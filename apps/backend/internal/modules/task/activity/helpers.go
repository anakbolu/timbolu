package activity

import (
	"encoding/json"
	"strings"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
)

// HandoffTool is the tool that asks another Bolu to do part of the work.
//
// It is part of the runtime rather than an integration, so it is offered
// alongside the granted tools and the workflow performs it itself: the parent
// waits for the child's answer, which is what makes a chain of handoffs
// traceable rather than a conversation.
func HandoffTool() domain.Tool {
	return domain.Tool{
		Name: domain.ToolHandoff,
		Description: "Minta Bolu lain mengerjakan bagian pekerjaan ini, lalu pakai hasilnya. " +
			"Pakai hanya bila pekerjaannya memang bukan keahlianmu.",
		Schema: json.RawMessage(`{
			"type": "object",
			"required": ["agent", "instructions"],
			"properties": {
				"agent": {"type": "string", "description": "Nama Bolu, mis. Ijo"},
				"instructions": {"type": "string", "description": "Apa yang perlu dikerjakan dan data apa yang dibutuhkan"}
			}
		}`),
		Label: domain.LabelRead,
	}
}

// draftTitle names a draft from the arguments the tool was called with.
func draftTitle(request workflow.ToolRequest) string {
	var payload map[string]any
	if err := json.Unmarshal(request.Arguments, &payload); err != nil {
		return request.Name
	}

	for _, key := range []string{"title", "subject", "summary"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return request.Name
}

// firstLine takes the first line of a text, bounded, which is what a task title
// and a summary are.
func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		trimmed = trimmed[:index]
	}

	runes := []rune(strings.TrimSpace(trimmed))
	if len(runes) <= 120 {
		return string(runes)
	}
	return string(runes[:119]) + "…"
}

// truncated is what a tool's answer becomes when it is longer than the limit.
//
// It is a document rather than a fragment on purpose: a model handed the first N
// bytes of JSON would have to guess what was cut, and the guess is what costs the
// round. Saying so in the same shape is what lets it react.
type truncated struct {
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
	Preview   string `json:"preview"`
}

// truncate cuts a tool's answer at the limit.
//
// encoding/json replaces a byte that is not valid UTF-8 with U+FFFD, so a cut in
// the middle of a multi-byte rune still produces a document that parses.
func truncate(content []byte, limit int) []byte {
	if len(content) <= limit {
		return content
	}

	encoded, err := json.Marshal(truncated{
		Truncated: true,
		Bytes:     len(content),
		Preview:   string(content[:limit]),
	})
	if err != nil {
		return []byte(`{"truncated":true}`)
	}
	return encoded
}
