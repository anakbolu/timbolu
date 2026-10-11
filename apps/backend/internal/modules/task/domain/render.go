package domain

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// RenderBlocks turns a stored message body into the text a model reads.
//
// It lives in the domain because both the workflow and the activities need it,
// and neither may depend on the other: the workflow is the deterministic half and
// the activities are the side-effecting half, so a shared helper has to sit
// below both.
//
// A block the model cannot read is summarised rather than dropped. A model that
// forgot it already produced a chart would produce it again, and a second chart
// costs a whole round.
func RenderBlocks(blocks []Block) string {
	parts := make([]string, 0, len(blocks))

	for _, block := range blocks {
		switch block.Type {
		case "text":
			var text struct {
				Markdown string `json:"markdown"`
			}
			if err := json.Unmarshal(block.Body, &text); err == nil {
				parts = append(parts, text.Markdown)
			}
		case "chart":
			parts = append(parts, "[grafik]")
		case "mermaid":
			parts = append(parts, "[diagram]")
		case "html":
			parts = append(parts, "[konten interaktif]")
		case "table":
			parts = append(parts, "[tabel]")
		case "draft":
			parts = append(parts, "[draf menunggu persetujuan]")
		}
	}

	return strings.Join(parts, "\n\n")
}

// WireID renders an id for a wire shape, or the empty string when there is none.
//
// Absence is an empty string rather than a zero uuid because encoding/json cannot
// omit a uuid.UUID — it is an array — so a zero value would be published and read
// as a real id.
func WireID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
