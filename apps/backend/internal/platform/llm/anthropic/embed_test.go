package anthropic_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/anthropic"
)

// TestEmbedIsRefused: this API has no embedding endpoint, so the capability is
// reported as absent and a call says so instead of failing at the provider.
//
// A provider that advertised embeddings and then failed would turn a workspace's
// memory write into a write that half happened, which is why the capability is
// what the chain reads.
func TestEmbedIsRefused(t *testing.T) {
	client, err := anthropic.New(anthropic.Config{
		BaseURL: "http://localhost:1",
		APIKey:  "test-key",
		Model:   "claude-3-5-haiku-latest",
	})
	require.NoError(t, err)

	require.False(t, client.Capabilities().Embeddings)

	_, err = client.Embed(context.Background(), domain.EmbedRequest{Inputs: []string{"halo"}})
	require.ErrorIs(t, err, domain.ErrUnsupported)
}
