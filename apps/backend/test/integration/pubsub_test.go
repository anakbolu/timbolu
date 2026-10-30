package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/pubsub"
)

// TestChannelIsNamespacedByWorkspace is the isolation rule the stream depends on:
// one tenant's events are one channel, so a subscriber never filters another
// tenant's traffic and cannot accidentally forward it.
func TestChannelIsNamespacedByWorkspace(t *testing.T) {
	first := uuid.New()
	second := uuid.New()

	require.Equal(t, "ws:"+first.String(), pubsub.Channel(first))
	require.NotEqual(t, pubsub.Channel(first), pubsub.Channel(second))
}

// TestPublishAndSubscribeRoundTrip is the E5.7 gate on the fast path.
func TestPublishAndSubscribeRoundTrip(t *testing.T) {
	client := newRedisRawClient(t)
	publisher := pubsub.New(client)

	workspace := uuid.New()
	other := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := publisher.Subscribe(ctx, workspace)
	require.NoError(t, err)

	// A subscriber of another workspace must never see this one's event.
	foreign, err := publisher.Subscribe(ctx, other)
	require.NoError(t, err)

	event := chatdomain.Event{
		WorkspaceID:    workspace,
		Type:           chatdomain.EventMessageNew,
		ConversationID: uuid.New(),
		Payload:        json.RawMessage(`{"message_id":"x"}`),
	}
	require.NoError(t, publisher.Publish(ctx, workspace, event))

	select {
	case received := <-events:
		require.Equal(t, workspace, received.WorkspaceID)
		require.Equal(t, chatdomain.EventMessageNew, received.Type)
		require.JSONEq(t, `{"message_id":"x"}`, string(received.Payload))
	case <-time.After(10 * time.Second):
		require.Fail(t, "the event never arrived")
	}

	select {
	case leaked := <-foreign:
		require.Failf(t, "another workspace received an event", "%s", leaked.Type)
	case <-time.After(300 * time.Millisecond):
		// Nothing arrived, which is the point.
	}
}

// TestSubscribeEndsWithItsContext keeps an abandoned subscriber from leaking.
func TestSubscribeEndsWithItsContext(t *testing.T) {
	publisher := pubsub.New(newRedisRawClient(t))

	ctx, cancel := context.WithCancel(context.Background())
	events, err := publisher.Subscribe(ctx, uuid.New())
	require.NoError(t, err)

	cancel()

	select {
	case _, ok := <-events:
		require.False(t, ok, "the channel closes when the context ends")
	case <-time.After(10 * time.Second):
		require.Fail(t, "the subscription did not end")
	}
}

// TestAnUnreadableFrameDoesNotEndTheSubscription is the property that keeps one
// bad publish from silencing a workspace.
func TestAnUnreadableFrameDoesNotEndTheSubscription(t *testing.T) {
	client := newRedisRawClient(t)
	publisher := pubsub.New(client)

	workspace := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := publisher.Subscribe(ctx, workspace)
	require.NoError(t, err)

	// Something else writes to the channel: a frame this package did not build.
	require.NoError(t, client.Publish(ctx, pubsub.Channel(workspace), "not json").Err())

	good := chatdomain.Event{WorkspaceID: workspace, Type: chatdomain.EventAgentState}
	require.NoError(t, publisher.Publish(ctx, workspace, good))

	select {
	case received := <-events:
		require.Equal(t, chatdomain.EventAgentState, received.Type, "the readable event still arrives")
	case <-time.After(10 * time.Second):
		require.Fail(t, "the subscription ended on the unreadable frame")
	}
}

// TestPublishRequiresRedis keeps the fail-visible behaviour of a deployment
// without the fast path: the event is still stored, and the caller is told the
// publish did not happen.
func TestPublishRequiresRedis(t *testing.T) {
	require.Error(t, pubsub.New(nil).Publish(context.Background(), uuid.New(), chatdomain.Event{}))

	_, err := pubsub.New(nil).Subscribe(context.Background(), uuid.New())
	require.Error(t, err)
}
