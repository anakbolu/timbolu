package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestAMemoryOmitsWhatIsAbsent is why the wire shape is written out by hand:
// `encoding/json` cannot omit a `uuid.UUID`, so a typed field would publish
// `00000000-0000-0000-0000-000000000000` for a main memory and a client would read
// it as a Bolu whose id is all zeroes.
func TestAMemoryOmitsWhatIsAbsent(t *testing.T) {
	memory := Memory{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Content:     "Jam buka 08.00-21.00",
		Kind:        KindFact,
		CreatedBy:   SourceUser,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	raw, err := json.Marshal(memory)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	require.NotContains(t, decoded, "agent_id", "a main memory belongs to no Bolu")
	require.NotContains(t, decoded, "superseded_by", "a current memory was replaced by nothing")
	require.Equal(t, LayerMain, decoded["layer"], "the layer is stated so a client does not infer it from an absent id")
	require.Equal(t, memory.ID.String(), decoded["id"])
}

// TestAMemoryNamesItsLayerAndItsReplacement: a private memory that was replaced
// says both, which is what the screen groups and flags on.
func TestAMemoryNamesItsLayerAndItsReplacement(t *testing.T) {
	agentID := uuid.New()
	newer := uuid.New()

	raw, err := json.Marshal(Memory{
		ID:           uuid.New(),
		WorkspaceID:  uuid.New(),
		AgentID:      agentID,
		Content:      "Kamu selalu menulis tanggal di tagihan",
		Kind:         KindInstruction,
		SupersededBy: newer,
		CreatedBy:    SourceExtraction,
		Similarity:   0.93,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	require.Equal(t, agentID.String(), decoded["agent_id"])
	require.Equal(t, newer.String(), decoded["superseded_by"])
	require.Equal(t, LayerPrivate, decoded["layer"])
	require.Equal(t, 0.93, decoded["similarity"])
}

// TestAKnowledgeChunkOmitsAnAbsentLayer is the same rule for a document chunk.
func TestAKnowledgeChunkOmitsAnAbsentLayer(t *testing.T) {
	raw, err := json.Marshal(KnowledgeItem{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Source:      "katalog.pdf",
		ChunkIndex:  0,
		Content:     "Ongkir Bandung 15 ribu",
		Status:      StatusReady,
		CreatedAt:   time.Now(),
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	require.NotContains(t, decoded, "agent_id")
	require.Equal(t, StatusReady, decoded["status"])
}
