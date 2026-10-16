package service

import (
	"context"
	"strings"

	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/attachtext"
)

// conversationTurn is one stored message as the model will read it: the text it
// already had, and whatever its attachments contribute.
//
// The two are kept apart rather than joined at once because the contribution is
// not final until the chain is known: a model that cannot see pictures reads the
// same turn with every picture replaced by the line that says so.
type conversationTurn struct {
	role llmdomain.Role
	text string
	// attachments is nil for a message without files, which is the common case.
	attachments *attachtext.Turn
}

// message renders the turn for a model that can, or cannot, see pictures.
func (t conversationTurn) message(vision bool) llmdomain.Message {
	message := llmdomain.Message{Role: t.role, Text: t.text}

	if attachments := t.attachments.Text(vision); attachments != "" {
		if strings.TrimSpace(message.Text) == "" {
			message.Text = attachments
		} else {
			message.Text += "\n\n" + attachments
		}
	}
	if vision {
		for _, image := range t.attachments.Images() {
			message.Images = append(message.Images, llmdomain.Image{MediaType: image.MediaType, Data: image.Data})
		}
	}

	return message
}

// empty reports whether the turn says nothing at all, which is the only case a
// message is left out of the request. A message that is nothing but files is not
// empty: its files are what it says.
func (t conversationTurn) empty() bool {
	return strings.TrimSpace(t.text) == "" && t.attachments.Empty()
}

// conversationTurns maps stored messages onto the model's message format, files
// included.
//
// The budget is spent newest first, and one renderer serves the whole request:
// what a person just sent is what they are asking about, so it is the file that
// gets the room, and an older thread full of documents degrades into the notes
// that name them.
func (r *ModelResponder) conversationTurns(ctx context.Context, scope chatdomain.Scope, history []chatdomain.Message) []conversationTurn {
	turns := make([]conversationTurn, len(history))
	for i, message := range history {
		role := llmdomain.RoleAssistant
		if !message.FromAgent() {
			role = llmdomain.RoleUser
		}

		turns[i] = conversationTurn{role: role, text: renderBlocksForModel(message.Blocks)}
	}

	renderer := attachtext.NewRenderer(r.deps.Limits)
	for i := len(history) - 1; i >= 0; i-- {
		if len(history[i].Attachments) == 0 {
			continue
		}

		var turn attachtext.Turn
		for _, attachment := range history[i].Attachments {
			part := renderer.Render(r.readAttachment(ctx, scope, attachment))
			if part.Err != nil {
				r.deps.Logger.Warn("an attachment could not be read",
					zap.String("attachment", attachment.Filename),
					zap.String("workspace_id", scope.WorkspaceID.String()),
					zap.Error(part.Err),
				)
			}
			turn.Add(part)
		}
		turns[i].attachments = &turn
	}

	return turns
}

// turnMessages renders the turns as the request the model is sent.
func turnMessages(turns []conversationTurn, vision bool) []llmdomain.Message {
	messages := make([]llmdomain.Message, 0, len(turns))
	for _, turn := range turns {
		if turn.empty() {
			continue
		}
		messages = append(messages, turn.message(vision))
	}

	return messages
}

// carriesImages reports whether the request would carry a picture, which is what
// makes a refusal by the chain worth a second try without them.
func carriesImages(turns []conversationTurn) bool {
	for _, turn := range turns {
		if len(turn.attachments.Images()) > 0 {
			return true
		}
	}

	return false
}

// readAttachment fetches one file's bytes, or reports why it could not.
//
// The size is checked before the object is fetched, so a file that is over the
// bound costs a note rather than a download.
func (r *ModelResponder) readAttachment(ctx context.Context, scope chatdomain.Scope, attachment chatdomain.Attachment) attachtext.File {
	file := attachtext.File{
		Filename:    attachment.Filename,
		ContentType: attachment.ContentType,
		ByteSize:    attachment.ByteSize,
	}

	switch {
	case r.deps.Storage == nil:
		// A deployment without object storage can still answer: the file is
		// named, and the model is told it could not be read.
		file.Err = chatdomain.ErrStorageUnavailable
	case r.deps.Limits.Oversized(attachment.ByteSize):
		file.Err = attachtext.ErrTooLarge
	default:
		content, err := r.deps.Storage.Get(ctx, attachment.StorageKey)
		if err != nil {
			file.Err = err
			return file
		}
		file.Content = content
	}

	return file
}
