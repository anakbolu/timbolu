package service

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// TestReproTheModelReadsTheAttachments is issue #187: a user message with files
// must reach the model with the files' content, not only its text.
func TestReproTheModelReadsTheAttachments(t *testing.T) {
	storage := newMemoryStorage()
	ctx := context.Background()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}

	txt, err := storage.Put(ctx, "attachments/w/1/stok.txt", "text/plain", []byte("Stok kaos merah: 7 pcs"))
	require.NoError(t, err)
	png, err := storage.Put(ctx, "attachments/w/2/foto.png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...))
	require.NoError(t, err)

	message := chatdomain.Message{
		ID: uuid.New(), AuthorUserID: scope.UserID,
		Blocks: []chatdomain.Block{{Type: chatdomain.BlockText, Body: json.RawMessage(`{"type":"text","markdown":"Tolong cek lampiran ini"}`)}},
		Attachments: []chatdomain.Attachment{
			{ID: uuid.New(), Filename: "stok.txt", ContentType: "text/plain", StorageKey: txt.Key, ByteSize: txt.ByteSize},
			{ID: uuid.New(), Filename: "foto.png", ContentType: "image/png", StorageKey: png.Key, ByteSize: png.ByteSize},
		},
	}

	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "ok"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}
	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})
	chunks, err := responder.Reply(ctx, scope, chatdomain.ReplyRequest{
		Agent: chatdomain.AgentRef{ID: uuid.New(), Name: "Oren"}, Message: message,
		History: []chatdomain.Message{message}, RepairBudget: 1,
	})
	require.NoError(t, err)
	for range chunks {
	}

	calls := gateway.recordedCalls()
	require.NotEmpty(t, calls)
	request := calls[0]

	// Evidence: the user turns as the gateway receives them, image bytes elided.
	if path := os.Getenv("REPRO_DUMP"); path != "" {
		type img struct {
			MediaType string
			Bytes     int
		}
		type turn struct {
			Role   string
			Text   string
			Images []img
		}
		var dump []turn
		for _, m := range request.Messages {
			tt := turn{Role: string(m.Role), Text: m.Text}
			for _, i := range m.Images {
				tt.Images = append(tt.Images, img{i.MediaType, len(i.Data)})
			}
			dump = append(dump, tt)
		}
		raw, _ := json.MarshalIndent(map[string]any{"messages": dump}, "", "  ")
		_ = os.WriteFile(path, raw, 0o644)
	}

	var text strings.Builder
	images := 0
	for _, m := range request.Messages {
		text.WriteString(m.Text)
		images += len(m.Images)
	}
	require.Contains(t, text.String(), "Stok kaos merah: 7 pcs", "the text file's content must reach the model")
	require.Contains(t, text.String(), "stok.txt")
	require.Equal(t, 1, images, "the picture goes to a vision-capable chain as an image")
}
