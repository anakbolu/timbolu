package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestWireMessageOmitsTheAbsentAuthor is the guard on the bug that made a user's
// message render as a Bolu's.
//
// A zero uuid.UUID is an array, which encoding/json never treats as empty, so a
// typed identity field publishes "00000000-…" and a client reads it as an
// author. Absence has to be an empty string.
func TestWireMessageOmitsTheAbsentAuthor(t *testing.T) {
	userID := uuid.New()
	agentID := uuid.New()
	created := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	fromUser := WireMessage(Message{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		AuthorUserID:   userID,
		Blocks:         []Block{{Type: BlockText, Body: json.RawMessage(`{"type":"text","markdown":"halo"}`)}},
		Status:         MessageComplete,
		CreatedAt:      created,
	})

	encoded, err := json.Marshal(fromUser)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	require.NotContains(t, decoded, "agent_id", "a user message must carry no agent")
	require.Equal(t, userID.String(), decoded["user_id"])
	require.NotContains(t, string(encoded), "00000000-0000-0000-0000-000000000000")

	fromAgent := WireMessage(Message{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		AuthorAgentID:  agentID,
		Blocks:         []Block{{Type: BlockText, Body: json.RawMessage(`{"type":"text","markdown":"siap"}`)}},
		Status:         MessageStreaming,
		CreatedAt:      created,
	})

	encoded, err = json.Marshal(fromAgent)
	require.NoError(t, err)

	decoded = nil
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NotContains(t, decoded, "user_id", "a Bolu's message must carry no user")
	require.Equal(t, agentID.String(), decoded["agent_id"])
	require.Equal(t, MessageStreaming, decoded["status"])
	require.Equal(t, created.Format(time.RFC3339Nano), decoded["created_at"])

	// The body travels whole, so a client renders the event without a fetch.
	blocks, ok := decoded["blocks"].([]any)
	require.True(t, ok)
	require.Len(t, blocks, 1)
}

// TestWireMessageCarriesAttachments covers the read model of a message's files.
func TestWireMessageCarriesAttachments(t *testing.T) {
	attachment := Attachment{
		ID:          uuid.New(),
		Filename:    "daftar harga.pdf",
		ContentType: "application/pdf",
		ByteSize:    5,
	}

	payload := WireMessage(Message{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		AuthorUserID:   uuid.New(),
		Attachments:    []Attachment{attachment},
		Status:         MessageComplete,
	})

	require.Len(t, payload.Attachments, 1)
	require.Equal(t, attachment.ID.String(), payload.Attachments[0].ID)
	require.Equal(t, "daftar harga.pdf", payload.Attachments[0].Filename)
	require.Equal(t, int64(5), payload.Attachments[0].ByteSize)
	require.Contains(t, payload.Attachments[0].URL, attachment.ID.String())
}

// TestWireBlocksAreTheStoredDocuments keeps the delivered form and the stored
// form the same thing: a client stores what it renders.
func TestWireBlocksAreTheStoredDocuments(t *testing.T) {
	document := `{"type":"chart","spec":{"kind":"pie","slices":[{"name":"Kaos","value":10}]}}`

	payload := WireMessage(Message{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		AuthorAgentID:  uuid.New(),
		Blocks:         []Block{{Type: BlockChart, Body: json.RawMessage(document)}},
		Status:         MessageComplete,
	})

	require.Len(t, payload.Blocks, 1)
	require.JSONEq(t, document, string(payload.Blocks[0]))
	require.NotContains(t, string(payload.Blocks[0]), "Body", "the wrapper never reaches a client")
}

// TestWireMessageToleratesAnEmptyBody covers the placeholder a reply starts as.
func TestWireMessageToleratesAnEmptyBody(t *testing.T) {
	payload := WireMessage(Message{
		ID:             uuid.New(),
		ConversationID: uuid.New(),
		AuthorAgentID:  uuid.New(),
		Status:         MessageStreaming,
	})

	require.NotNil(t, payload.Blocks, "an empty body is an empty list, not null")
	require.Empty(t, payload.Blocks)
	require.NotNil(t, payload.Attachments)
}

// TestAgentStateOmitsTheAbsentWork keeps a client from reading a zero id as a
// task it can point at.
func TestAgentStateOmitsTheAbsentWork(t *testing.T) {
	payload := AgentStatePayload{AgentID: uuid.NewString(), State: StateIdle, Reason: "santai"}

	encoded, err := json.Marshal(payload)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NotContains(t, decoded, "task_id")
	require.NotContains(t, decoded, "draft_id")
	require.NotContains(t, string(encoded), "00000000-0000-0000-0000-000000000000")
}
