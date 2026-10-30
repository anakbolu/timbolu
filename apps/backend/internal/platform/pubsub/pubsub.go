// Package pubsub publishes the activity stream over Redis.
//
// Redis is the fast path, not the record: an event is written to
// activity_events before it is published here, so a subscriber that misses a
// publish still finds the row when it reconnects. That is what makes the stream
// complete rather than merely live.
package pubsub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
)

// ChannelPrefix namespaces the per-workspace channels, so one tenant's events
// are one channel and a subscriber never filters another tenant's traffic.
const ChannelPrefix = "ws:"

// Channel is the Redis channel of one workspace.
func Channel(workspaceID uuid.UUID) string {
	return ChannelPrefix + workspaceID.String()
}

// Publisher publishes and subscribes to the workspace channels.
type Publisher struct {
	client *redis.Client
}

// New builds the publisher. A nil client is allowed: publishing then reports
// that the fast path is unavailable, which the event sink treats as a warning
// rather than a failure, because the row is already written.
func New(client *redis.Client) *Publisher {
	return &Publisher{client: client}
}

// Publish sends one event to the workspace channel.
func (p *Publisher) Publish(ctx context.Context, workspaceID uuid.UUID, event chatdomain.Event) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("pubsub: redis is not configured")
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("pubsub: encode event: %w", err)
	}
	if err := p.client.Publish(ctx, Channel(workspaceID), payload).Err(); err != nil {
		return fmt.Errorf("pubsub: publish: %w", err)
	}
	return nil
}

// Subscribe returns the live events of one workspace.
//
// The channel closes when the context ends. A subscriber is expected to have
// asked for the durable backlog first and to subscribe after, so the window
// between the two is covered by the replay rather than lost.
func (p *Publisher) Subscribe(ctx context.Context, workspaceID uuid.UUID) (<-chan chatdomain.Event, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("pubsub: redis is not configured")
	}

	subscription := p.client.Subscribe(ctx, Channel(workspaceID))
	// Waiting for the confirmation makes the caller's next step ordered: a
	// publish that happens after Subscribe returned is guaranteed to arrive.
	if _, err := subscription.Receive(ctx); err != nil {
		_ = subscription.Close()
		return nil, fmt.Errorf("pubsub: subscribe: %w", err)
	}

	events := make(chan chatdomain.Event, 64)
	go func() {
		defer close(events)
		defer func() { _ = subscription.Close() }()

		messages := subscription.Channel(redis.WithChannelSize(64))
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-messages:
				if !ok {
					return
				}

				var event chatdomain.Event
				if err := json.Unmarshal([]byte(message.Payload), &event); err != nil {
					// One unreadable payload must not end the subscription for
					// every event after it.
					continue
				}
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return events, nil
}
