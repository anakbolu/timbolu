package openai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/openai"
)

// TestEmbedTranslatesBothWays proves the embedding route of this wire format:
// the inputs travel as one request to /v1/embeddings, and the vectors come back
// paired with the text they belong to.
func TestEmbedTranslatesBothWays(t *testing.T) {
	var (
		path string
		body map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		// The provider answers in its own order, which the adapter has to
		// restore: a vector paired with the wrong text is worse than a failure.
		_, _ = w.Write([]byte(`{
			"model": "text-embedding-3-small",
			"data": [
				{"object":"embedding","index":1,"embedding":[0.3,0.4,0.5]},
				{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}
			],
			"usage": {"prompt_tokens": 11, "total_tokens": 11}
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := openai.New(openai.Config{
		BaseURL:            server.URL,
		APIKey:             "test-key",
		Model:              "gpt-4o-mini",
		SupportsEmbeddings: true,
		HTTPClient:         server.Client(),
	})
	require.NoError(t, err)

	response, err := client.Embed(context.Background(), domain.EmbedRequest{
		Model:  "text-embedding-3-small",
		Inputs: []string{"jam buka", "ongkir"},
	})
	require.NoError(t, err)

	require.Equal(t, "/v1/embeddings", path)
	require.Equal(t, "text-embedding-3-small", body["model"])
	require.Equal(t, []any{"jam buka", "ongkir"}, body["input"])

	require.Equal(t, "text-embedding-3-small", response.Model)
	require.Equal(t, 11, response.Usage.InputTokens)
	require.Len(t, response.Vectors, 2)
	require.Equal(t, []float32{0.1, 0.2, 0.3}, response.Vectors[0], "the vector must follow its input")
	require.Equal(t, []float32{0.3, 0.4, 0.5}, response.Vectors[1])
}

// TestEmbedRefusesWhenTheServerHasNoRoute: a gateway that copies the chat format
// but implements no embeddings answers 404, which is a retryable outage so the
// chain can move to a provider that can embed.
func TestEmbedRefusesWhenTheServerHasNoRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no such route","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(server.Close)

	client, err := openai.New(openai.Config{
		BaseURL:            server.URL,
		Model:              "gpt-4o-mini",
		SupportsEmbeddings: true,
		HTTPClient:         server.Client(),
	})
	require.NoError(t, err)

	_, err = client.Embed(context.Background(), domain.EmbedRequest{Inputs: []string{"halo"}})
	require.Error(t, err)
	require.True(t, domain.Retryable(err), "a missing route must fall to the next provider, not stop the chain")
}

// TestEmbedRejectsAMismatchedCount is why the count is checked: a provider that
// answers with fewer vectors than inputs would otherwise pair a vector with
// somebody else's text.
func TestEmbedRejectsAMismatchedCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`))
	}))
	t.Cleanup(server.Close)

	client, err := openai.New(openai.Config{
		BaseURL:            server.URL,
		Model:              "gpt-4o-mini",
		SupportsEmbeddings: true,
		HTTPClient:         server.Client(),
	})
	require.NoError(t, err)

	_, err = client.Embed(context.Background(), domain.EmbedRequest{Inputs: []string{"satu", "dua"}})
	require.Error(t, err)
}

// TestEmbedWithoutCapability: an adapter built without the route reports the
// capability as absent rather than trying.
func TestEmbedWithoutCapability(t *testing.T) {
	client, err := openai.New(openai.Config{Model: "gpt-4o-mini"})
	require.NoError(t, err)

	require.False(t, client.Capabilities().Embeddings)
	_, err = client.Embed(context.Background(), domain.EmbedRequest{Inputs: []string{"halo"}})
	require.ErrorIs(t, err, domain.ErrUnsupported)
}
