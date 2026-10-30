package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

func TestProbeHTMLStream(t *testing.T) {
	arguments := map[string]any{
		"title":   "Simulasi diskon",
		"caption": "Konten interaktif dibuat Bolu",
		"html":    "<html><body><h1>Simulasi diskon</h1><script>parent.postMessage({type:'resize',height:220},'*');</script></body></html>",
	}
	payload, _ := json.Marshal(arguments)
	half := len(payload) / 2
	fmt.Printf("payload len=%d half=%d\nfirst=%q\nsecond=%q\n", len(payload), half, string(payload[:half]), string(payload[half:]))

	// What the adapter produces after merging the two fragments.
	merged := string(payload[:half]) + string(payload[half:])
	fmt.Println("merged equal:", merged == string(payload))

	var decoded map[string]any
	fmt.Println("decode err:", json.Unmarshal([]byte(merged), &decoded))

	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "Ini simulasi diskonnya."},
		{Type: llmdomain.EventToolCall, ToolCall: &llmdomain.ToolCall{ID: "call_1", Name: ToolRenderHTML, Arguments: json.RawMessage(merged)}},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: newMemoryStorage()})
	chunks, err := responder.Reply(context.Background(), chatdomain.Scope{WorkspaceID: uuid.New()}, chatdomain.ReplyRequest{
		Agent: chatdomain.AgentRef{ID: uuid.New(), Name: "Pinky"}, RepairBudget: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range chunks {
		fmt.Printf("kind=%s err=%s notice=%s\n", chunk.Kind, chunk.Err, chunk.Text)
	}
}
