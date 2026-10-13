package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// embeddingResponse is the answer of the embeddings route of this wire format.
type embeddingResponse struct {
	Model string          `json:"model"`
	Data  []embeddingData `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *wireError `json:"error"`
}

type embeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Embed turns text into vectors.
//
// The inputs travel as one request rather than one per string: embedding a
// document chunk by chunk costs a round trip each and reports the same cost, and
// the answer is what the caller indexes.
func (c *Client) Embed(ctx context.Context, req domain.EmbedRequest) (domain.EmbedResponse, error) {
	if !c.cfg.SupportsEmbeddings {
		return domain.EmbedResponse{}, fmt.Errorf("%w: %s has no embedding route", domain.ErrUnsupported, c.Name())
	}
	if len(req.Inputs) == 0 {
		return domain.EmbedResponse{}, fmt.Errorf("%w: at least one input is required", domain.ErrInvalidRequest)
	}

	model := req.Model
	if model == "" {
		model = c.cfg.Model
	}

	body, err := json.Marshal(map[string]any{"model": model, "input": req.Inputs})
	if err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%w: encode request: %w", domain.ErrInvalidRequest, err)
	}

	resp, err := c.post(ctx, "/v1/embeddings", body, false)
	if err != nil {
		return domain.EmbedResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%w: read response: %w", domain.ErrProviderUnavailable, err)
	}

	var payload embeddingResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%w: decode response: %w", domain.ErrProviderUnavailable, err)
	}
	if payload.Error != nil {
		return domain.EmbedResponse{}, payload.Error.err()
	}
	// A count that does not match the inputs would silently pair a vector with
	// the wrong text, which is worse than a failed call: every later search would
	// answer with somebody else's words.
	if len(payload.Data) != len(req.Inputs) {
		return domain.EmbedResponse{}, fmt.Errorf("%w: provider returned %d vectors for %d inputs",
			domain.ErrProviderUnavailable, len(payload.Data), len(req.Inputs))
	}

	// The index is what pairs a vector with its input, so the order is restored
	// from it rather than assumed.
	sort.Slice(payload.Data, func(i, j int) bool { return payload.Data[i].Index < payload.Data[j].Index })

	vectors := make([][]float32, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.Index < 0 || item.Index >= len(req.Inputs) {
			return domain.EmbedResponse{}, fmt.Errorf("%w: provider returned index %d for %d inputs",
				domain.ErrProviderUnavailable, item.Index, len(req.Inputs))
		}
		vectors = append(vectors, item.Embedding)
	}

	usage := domain.Usage{}
	if payload.Usage != nil {
		usage.InputTokens = payload.Usage.PromptTokens
	}

	answered := payload.Model
	if answered == "" {
		answered = model
	}

	return domain.EmbedResponse{Vectors: vectors, Model: answered, Usage: usage}, nil
}
