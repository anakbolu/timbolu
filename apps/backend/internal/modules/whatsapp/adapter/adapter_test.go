package adapter_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/whatsapp/adapter"
	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The adapter is where the approval gate's involvement is decided, so that is what
// this file is about: the tool that sends a message is labelled `write_external`,
// and the label is the only thing that makes a person decide before it leaves.

// fakeNumbers records what the adapter asked the module to do.
type fakeNumbers struct {
	enqueued   whatsappdomain.EnqueueRequest
	enqueueErr error
	messages   []whatsappdomain.Inbound
	numberID   uuid.UUID
	historyErr error
}

func (f *fakeNumbers) Enqueue(_ context.Context, _ whatsappdomain.Scope, request whatsappdomain.EnqueueRequest) (whatsappdomain.Outbox, error) {
	f.enqueued = request
	if f.enqueueErr != nil {
		return whatsappdomain.Outbox{}, f.enqueueErr
	}
	return whatsappdomain.Outbox{ID: uuid.New(), NumberID: request.NumberID, Status: whatsappdomain.OutboxQueued}, nil
}

func (f *fakeNumbers) History(context.Context, whatsappdomain.Scope, uuid.UUID, string, int) ([]whatsappdomain.Inbound, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.messages, nil
}

func (f *fakeNumbers) NumberByIntegration(context.Context, whatsappdomain.Scope, uuid.UUID) (uuid.UUID, error) {
	if f.numberID == uuid.Nil {
		return uuid.New(), nil
	}
	return f.numberID, nil
}

// tool returns one declared tool by name.
func tool(t *testing.T, name string) integrationdomain.ToolSpec {
	t.Helper()

	for _, spec := range adapter.New(&fakeNumbers{}).Tools() {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("no tool named %q", name)
	return integrationdomain.ToolSpec{}
}

// callRender performs one adapter call.
func callRender(t *testing.T, numbers *fakeNumbers, toolName string, arguments any) (integrationdomain.Result, error) {
	t.Helper()

	encoded, err := json.Marshal(arguments)
	require.NoError(t, err)

	return adapter.New(numbers).Execute(t.Context(), integrationdomain.Call{
		Scope:         integrationdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
		IntegrationID: uuid.New(),
		Tool:          toolName,
		Arguments:     encoded,
	})
}

// TestSendingIsAnActionThatLeaves is E9.5's gate at the adapter's boundary, and it
// is the whole reason the tool exists as a tool: the label is what makes the
// approval gate hold the message until a person decides. Rendering it as a read
// would send a customer an unreviewed reply.
func TestSendingIsAnActionThatLeaves(t *testing.T) {
	spec := tool(t, adapter.ToolSend)

	require.Equal(t, integrationdomain.LabelWriteExternal, spec.Label)
	require.NoError(t, spec.Valid())

	// The schema has to say what the model must provide, or the model guesses.
	var schema struct {
		Required []string                   `json:"required"`
		Props    map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(spec.Schema, &schema))
	require.ElementsMatch(t, []string{"to", "text"}, schema.Required)
	require.Contains(t, schema.Props, "to")
	require.Contains(t, schema.Props, "text")
}

// TestReadingIsNotAnAction: looking a conversation up changes nothing outside the
// workspace, so nobody is asked. Rendering it as a write would train people to
// approve without reading.
func TestReadingIsNotAnAction(t *testing.T) {
	spec := tool(t, adapter.ToolHistory)

	require.Equal(t, integrationdomain.LabelRead, spec.Label)
	require.NoError(t, spec.Valid())
}

// TestTheAdapterCarriesItsOwnCredential: a linked number has no token to store, so
// the adapter declares that a call needs none. That declaration is what stops the
// executor from refusing every call against a connection whose token columns are
// empty.
func TestTheAdapterCarriesItsOwnCredential(t *testing.T) {
	var credentialled integrationdomain.Credentialled = adapter.New(&fakeNumbers{})
	require.False(t, credentialled.NeedsToken())
}

// TestTheMessageIsQueuedRatherThanSent: the adapter hands the message to the
// outbox, which is where the pace, the typing indicator, and the hourly ceiling
// live. Sending here would bypass all three.
func TestTheMessageIsQueuedRatherThanSent(t *testing.T) {
	numbers := &fakeNumbers{}
	result, err := callRender(t, numbers, adapter.ToolSend, map[string]any{
		"to":   "+62 812-3456-789",
		"text": "Halo Kak, pesanannya sudah dikirim ya.",
	})
	require.NoError(t, err)

	require.Equal(t, "+62 812-3456-789", numbers.enqueued.ToJID, "the number travels as the caller wrote it")
	require.Equal(t, whatsappdomain.KindText, numbers.enqueued.Kind)
	require.Contains(t, numbers.enqueued.Text, "sudah dikirim")
	require.Contains(t, result.Summary, "antrean")
	require.Contains(t, string(result.Content), `"queued":true`)
}

// TestSendingNeedsARecipientAndABody: the schema already refuses these, and the
// adapter refuses them again because the schema is a deployment's to configure and
// the check is not.
func TestSendingNeedsARecipientAndABody(t *testing.T) {
	_, err := callRender(t, &fakeNumbers{}, adapter.ToolSend, map[string]any{"to": "", "text": "halo"})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)

	_, err = callRender(t, &fakeNumbers{}, adapter.ToolSend, map[string]any{"to": "628999", "text": "   "})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)
}

// TestAnOfflineNumberIsReportedRatherThanSwallowed: the message could not be
// queued, so the call fails and the draft's task sees why. Recording it as sent
// would be the one failure a customer notices.
func TestAnOfflineNumberIsReportedRatherThanSwallowed(t *testing.T) {
	numbers := &fakeNumbers{enqueueErr: whatsappdomain.ErrNotConnected}

	_, err := callRender(t, numbers, adapter.ToolSend, map[string]any{"to": "628999", "text": "halo"})
	require.ErrorIs(t, err, whatsappdomain.ErrNotConnected)
}

// TestTheHistoryReadsOldestFirst is what makes a conversation readable: a model
// given the newest message first answers the wrong thing.
func TestTheHistoryReadsTheConversation(t *testing.T) {
	numbers := &fakeNumbers{messages: []whatsappdomain.Inbound{
		{ID: uuid.New(), WaMessageID: "msg-1", SenderJID: "628999@s.whatsapp.net", SenderName: "Bu Sari",
			Kind: whatsappdomain.KindText, Text: "Ada kaos polos ukuran L?"},
		{ID: uuid.New(), WaMessageID: "msg-2", ChatJID: "628999@s.whatsapp.net", IsFromMe: true,
			Kind: whatsappdomain.KindText, Text: "Ada Kak, stoknya masih."},
		{ID: uuid.New(), WaMessageID: "msg-3", SenderJID: "628999@s.whatsapp.net",
			Kind: whatsappdomain.KindImage, Text: "ini ukurannya",
			Media: whatsappdomain.Media{Kind: whatsappdomain.KindImage, Status: whatsappdomain.MediaReady}},
	}}

	result, err := callRender(t, numbers, adapter.ToolHistory, map[string]any{"chat": "628999", "limit": 10})
	require.NoError(t, err)

	var rendered struct {
		Chat     string `json:"chat"`
		Messages []struct {
			From  string `json:"from"`
			Ours  bool   `json:"ours"`
			Text  string `json:"text"`
			Media *struct {
				Kind   string `json:"kind"`
				Status string `json:"status"`
			} `json:"media"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(result.Content, &rendered))

	require.Equal(t, "628999", rendered.Chat)
	require.Len(t, rendered.Messages, 3)
	require.Equal(t, "Ada kaos polos ukuran L?", rendered.Messages[0].Text, "the oldest first")
	require.True(t, rendered.Messages[1].Ours, "a message this number sent is marked as ours")
	require.NotNil(t, rendered.Messages[2].Media, "an attachment is visible as one")
	require.Equal(t, whatsappdomain.MediaReady, rendered.Messages[2].Media.Status)
}

// TestTheHistoryNeedsAConversation: reading "some conversation" is not a question
// with an answer.
func TestTheHistoryNeedsAConversation(t *testing.T) {
	_, err := callRender(t, &fakeNumbers{}, adapter.ToolHistory, map[string]any{"chat": "", "limit": 5})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)
}

// TestAnUnknownToolIsRefused: the registry only offers the declared tools, and a
// call for anything else is a bug rather than something to attempt.
func TestAnUnknownToolIsRefused(t *testing.T) {
	_, err := callRender(t, &fakeNumbers{}, "whatsapp.delete_everything", map[string]any{})
	require.ErrorIs(t, err, integrationdomain.ErrUnknownTool)
}

// TestTheMessageEventIsDeclared: a routine watches `app.thing`, so the trigger has
// to be declared with the same name the module announces. A drift between the two
// is a routine that never fires and nothing that says why.
func TestTheMessageEventIsDeclared(t *testing.T) {
	triggers := adapter.New(&fakeNumbers{}).Triggers()
	require.Len(t, triggers, 1)

	require.Equal(t, adapter.App, triggers[0].App)
	require.Equal(t, integrationdomain.TriggerWebhook, triggers[0].Kind)
	require.Equal(t, whatsappdomain.EventMessageReceived, triggers[0].Event)
	require.NoError(t, triggers[0].Valid())
}

// TestTheAdapterNamesItsApplication: the app key is the prefix of every tool name
// and the key a connection is stored under, so it has to match.
func TestTheAdapterNamesItsApplication(t *testing.T) {
	require.Equal(t, "whatsapp", adapter.App)
	require.Equal(t, adapter.App, adapter.New(&fakeNumbers{}).App())

	for _, spec := range adapter.New(&fakeNumbers{}).Tools() {
		require.Equal(t, adapter.App, spec.App())
	}
}
