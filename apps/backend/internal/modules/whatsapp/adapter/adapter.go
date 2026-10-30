// Package adapter exposes a linked WhatsApp number as a tool the hub can run and
// an event a routine can watch.
//
// It is an adapter in the hub's sense: the model sees tools with names and
// schemas, and nothing downstream learns that WhatsApp is not Gmail. The sending
// tool is labelled `write_external`, which is what puts every message through the
// approval gate — a reply to a customer is an action that leaves the workspace,
// and the gate is the whole reason that label exists.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// App is the application key this adapter answers for.
const App = "whatsapp"

// Tool names.
const (
	// ToolSend is a message to a customer. It is `write_external`: somebody
	// decides before it leaves.
	ToolSend = "whatsapp.send_message"
	// ToolHistory reads what a customer wrote earlier, which is what makes a
	// second question about the same order answerable.
	ToolHistory = "whatsapp.read_messages"
)

// Numbers is the part of the module this adapter uses.
//
// It is declared here rather than taken as the whole service because an adapter
// should be able to say exactly what it needs: enqueue a message, and read a
// conversation. Anything wider would let a tool implementation reach parts of the
// product that are not its business.
type Numbers interface {
	// Enqueue records one message to send.
	Enqueue(ctx context.Context, scope whatsappdomain.Scope, request whatsappdomain.EnqueueRequest) (whatsappdomain.Outbox, error)
	// History returns what one conversation has said, oldest first, for the
	// connection a call names.
	History(ctx context.Context, scope whatsappdomain.Scope, integrationID uuid.UUID, chatJID string, limit int) ([]whatsappdomain.Inbound, error)
	// NumberByIntegration answers which number a connection speaks as.
	NumberByIntegration(ctx context.Context, scope whatsappdomain.Scope, integrationID uuid.UUID) (uuid.UUID, error)
}

// Adapter is the WhatsApp integration.
type Adapter struct {
	numbers Numbers
}

// New builds the adapter.
func New(numbers Numbers) *Adapter {
	return &Adapter{numbers: numbers}
}

// App implements integrationdomain.Adapter.
func (a *Adapter) App() string { return App }

// NeedsToken implements integrationdomain.Credentialled.
//
// The credential a WhatsApp call needs is the session, and the session lives in
// the gateway that holds the connection open. There is no token for the hub to
// store, so it is not asked for one — and a call is still refused for a
// connection the workspace never made, because the executor looks the connection
// up before it dispatches.
func (a *Adapter) NeedsToken() bool { return false }

// Tools implements integrationdomain.Adapter.
func (a *Adapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{
		{
			Name:  ToolSend,
			Label: integrationdomain.LabelWriteExternal,
			Description: "Kirim pesan WhatsApp ke seorang pelanggan. Pesannya akan " +
				"menunggu persetujuan sebelum benar-benar terkirim.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"to": {"type": "string", "description": "Nomor tujuan dalam format internasional, contoh 628123456789"},
					"text": {"type": "string", "description": "Isi pesan yang akan dikirim"}
				},
				"required": ["to", "text"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        ToolHistory,
			Label:       integrationdomain.LabelRead,
			Description: "Baca percakapan WhatsApp dengan satu nomor, dari yang paling awal.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"chat": {"type": "string", "description": "Nomor pelanggan, contoh 628123456789"},
					"limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "Berapa pesan terakhir yang dibaca"}
				},
				"required": ["chat"],
				"additionalProperties": false
			}`),
		},
	}
}

// Triggers implements integrationdomain.Adapter.
func (a *Adapter) Triggers() []integrationdomain.TriggerSpec {
	return []integrationdomain.TriggerSpec{{
		App:         App,
		Kind:        integrationdomain.TriggerWebhook,
		Event:       whatsappdomain.EventMessageReceived,
		Description: "Pesan WhatsApp masuk",
	}}
}

// Execute implements integrationdomain.Adapter.
func (a *Adapter) Execute(ctx context.Context, call integrationdomain.Call) (integrationdomain.Result, error) {
	if a.numbers == nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: no whatsapp runtime is configured", whatsappdomain.ErrNotConfigured)
	}

	scope := whatsappdomain.Scope{
		UserID:      call.Scope.UserID,
		WorkspaceID: call.Scope.WorkspaceID,
	}

	switch call.Tool {
	case ToolSend:
		return a.send(ctx, scope, call)
	case ToolHistory:
		return a.history(ctx, scope, call)
	default:
		return integrationdomain.Result{}, fmt.Errorf("%w: %s", integrationdomain.ErrUnknownTool, call.Tool)
	}
}

// sendArguments is what the sending tool accepts.
type sendArguments struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// send queues one message.
//
// It queues rather than sends: the message goes out on the number's own pace,
// with the typing indicator and the hourly ceiling applied, which is the
// difference between answering a customer and blasting one.
func (a *Adapter) send(ctx context.Context, scope whatsappdomain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	var args sendArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	numberID, err := a.numberFor(ctx, scope, call.IntegrationID)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	to := strings.TrimSpace(args.To)
	if to == "" {
		return integrationdomain.Result{}, fmt.Errorf("%w: pesan tidak menyebut nomor tujuan", whatsappdomain.ErrInvalidInput)
	}
	if strings.TrimSpace(args.Text) == "" {
		return integrationdomain.Result{}, fmt.Errorf("%w: pesan kosong", whatsappdomain.ErrInvalidInput)
	}

	queued, err := a.numbers.Enqueue(ctx, scope, whatsappdomain.EnqueueRequest{
		NumberID: numberID,
		ToJID:    to,
		Kind:     whatsappdomain.KindText,
		Text:     args.Text,
	})
	if err != nil {
		return integrationdomain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"queued":     true,
		"message_id": queued.ID.String(),
		"to":         to,
		"status":     queued.Status,
	})
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("whatsapp: render result: %w", err)
	}

	return integrationdomain.Result{
		Content: content,
		Summary: "Pesan WhatsApp masuk antrean untuk " + to,
	}, nil
}

// historyArguments is what the reading tool accepts.
type historyArguments struct {
	Chat  string `json:"chat"`
	Limit int    `json:"limit"`
}

// history reads one conversation.
func (a *Adapter) history(ctx context.Context, scope whatsappdomain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	var args historyArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	chat := strings.TrimSpace(args.Chat)
	if chat == "" {
		return integrationdomain.Result{}, fmt.Errorf("%w: percakapan mana yang dibaca belum disebut", whatsappdomain.ErrInvalidInput)
	}

	limit := args.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	messages, err := a.numbers.History(ctx, scope, call.IntegrationID, chat, limit)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	rendered := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		entry := map[string]any{
			"from":  message.SenderJID,
			"at":    message.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"kind":  message.Kind,
			"ours":  message.IsFromMe,
			"group": message.IsGroup,
			"text":  message.Text,
		}
		if message.SenderName != "" {
			entry["name"] = message.SenderName
		}
		if message.Media.Status != "" {
			entry["media"] = map[string]any{
				"kind":   message.Media.Kind,
				"status": message.Media.Status,
			}
		}
		rendered = append(rendered, entry)
	}

	content, err := json.Marshal(map[string]any{"chat": chat, "messages": rendered})
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("whatsapp: render result: %w", err)
	}

	return integrationdomain.Result{
		Content: content,
		Summary: fmt.Sprintf("%d pesan terakhir dengan %s", len(rendered), chat),
	}, nil
}

// numberFor resolves which number a call speaks as.
//
// It is read from the connection the call names rather than from the arguments:
// an approved action was approved for one connection, and letting the model pick
// the number would let a reply to one customer go out of another's line.
func (a *Adapter) numberFor(ctx context.Context, scope whatsappdomain.Scope, integrationID uuid.UUID) (uuid.UUID, error) {
	if integrationID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: aksi ini tidak menyebut sambungannya", whatsappdomain.ErrInvalidInput)
	}

	return a.numbers.NumberByIntegration(ctx, scope, integrationID)
}

// Compile-time checks that the adapter is what the hub asks for.
var (
	_ integrationdomain.Adapter       = (*Adapter)(nil)
	_ integrationdomain.Credentialled = (*Adapter)(nil)
)
