package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// WireMessage renders one message for the API and for the event stream.
//
// It is the single place the wire shape is built, so the message a client
// receives in a response and the one it receives on the stream cannot drift
// apart — which is what let a user's message be published with a zero agent id
// and render as a Bolu's.
func WireMessage(message Message) MessagePayload {
	attachments := make([]AttachmentPayload, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		attachments = append(attachments, WireAttachment(attachment))
	}

	createdAt := ""
	if !message.CreatedAt.IsZero() {
		createdAt = message.CreatedAt.Format(time.RFC3339Nano)
	}

	return MessagePayload{
		MessageID:      message.ID.String(),
		ConversationID: message.ConversationID.String(),
		AgentID:        WireID(message.AuthorAgentID),
		UserID:         WireID(message.AuthorUserID),
		Status:         message.Status,
		Blocks:         WireBlocks(message.Blocks),
		Attachments:    attachments,
		TaskID:         WireID(message.TaskID),
		FinishReason:   message.FinishReason,
		CreatedAt:      createdAt,
	}
}

// WireAttachment renders one attachment for a client. The storage key stays on
// the server; the URL is what a client follows.
func WireAttachment(attachment Attachment) AttachmentPayload {
	return AttachmentPayload{
		ID:          attachment.ID.String(),
		Filename:    attachment.Filename,
		ContentType: attachment.ContentType,
		ByteSize:    attachment.ByteSize,
		URL:         "/api/attachments/" + attachment.ID.String(),
	}
}

// WireStagedAttachment renders a staged upload for the answer to `?staged=true`.
// It is not WireAttachment with other tags: a staged file is named by the id the
// send will carry, and the shape a message carries stays what it was.
func WireStagedAttachment(attachment Attachment) StagedAttachmentPayload {
	return StagedAttachmentPayload{
		AttachmentID: attachment.ID.String(),
		Name:         attachment.Filename,
		Mime:         attachment.ContentType,
		Size:         attachment.ByteSize,
		URL:          "/api/attachments/" + attachment.ID.String(),
	}
}

// WireBlocks renders a message body. An empty body is an empty list rather than
// null, so a client never has to handle both.
func WireBlocks(blocks []Block) []json.RawMessage {
	raw := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		if len(block.Body) == 0 {
			continue
		}
		raw = append(raw, block.Body)
	}
	return raw
}

// WireID renders an id, or the empty string when there is none.
//
// The empty string is what absence looks like on the wire: a zero uuid.UUID is
// an array, so `omitempty` would publish it rather than omit it.
func WireID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// WireState renders a Bolu's derived state for the wire.
func WireState(agentID uuid.UUID, state, reason string, taskID, draftID uuid.UUID) AgentStatePayload {
	return AgentStatePayload{
		AgentID: WireID(agentID),
		State:   state,
		Reason:  reason,
		TaskID:  WireID(taskID),
		DraftID: WireID(draftID),
	}
}
