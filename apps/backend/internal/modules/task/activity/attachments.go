package activity

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/attachtext"
)

// errNoAttachmentReader is what a file's note says when this deployment has
// nothing that can read attachments: the model is told the file is there, which
// is more useful than a note that says nothing.
var errNoAttachmentReader = errors.New("task: attachments cannot be read in this deployment")

// attachmentTurns reads the files of one round's messages.
//
// It is the activity's work rather than the workflow's because it touches object
// storage and a PDF parser: the workflow keeps references, and the content enters
// the prompt here, inside the call that is about to send it. The result is
// aligned with the messages, so the caller can render a turn without looking the
// file up again.
//
// The budget is spent newest first, and one renderer serves the whole round: what
// the person just sent is what the task is about, so it gets the room, and an old
// thread full of documents degrades into the notes that name them.
//
// A file is read once. The same attachment can sit on two messages — a person
// uploads it, then links it to what they say — and fetching it twice buys the
// model the same bytes twice: the newest mention keeps the content, and the older
// one only loses the file, not the text beside it.
func (a *Activities) attachmentTurns(ctx context.Context, scope domain.Scope, task domain.Task, messages []domain.Turn) []*attachtext.Turn {
	turns := make([]*attachtext.Turn, len(messages))
	renderer := attachtext.NewRenderer(a.deps.AttachmentLimits)

	read := make(map[uuid.UUID]bool)
	for i := len(messages) - 1; i >= 0; i-- {
		if len(messages[i].Attachments) == 0 {
			continue
		}

		var turn attachtext.Turn
		for _, ref := range messages[i].Attachments {
			if read[ref.ID] {
				continue
			}
			read[ref.ID] = true

			part := renderer.Render(a.readAttachment(ctx, scope, ref))
			if part.Err != nil {
				// A file that was meant to be read and was not is worth a line in
				// the log: the model only ever sees the note that stands in for it.
				a.taskLogger(ctx, task).Warn("an attachment could not be read",
					zap.String("attachment", ref.Filename),
					zap.Error(part.Err),
				)
			}
			turn.Add(part)
		}
		if turn.Empty() {
			continue
		}
		turns[i] = &turn
	}

	return turns
}

// readAttachment fetches one file's bytes, or reports why it could not.
//
// The size is checked before the object is fetched, so a file that is over the
// bound costs a note rather than a download. A deployment with no reader — the
// task module owns no attachments — names every file and reads none.
func (a *Activities) readAttachment(ctx context.Context, scope domain.Scope, ref domain.AttachmentRef) attachtext.File {
	file := attachtext.File{
		Filename:    ref.Filename,
		ContentType: ref.ContentType,
		ByteSize:    ref.ByteSize,
	}

	switch {
	case a.deps.Attachments == nil:
		file.Err = errNoAttachmentReader
	case a.deps.AttachmentLimits.Oversized(ref.ByteSize):
		file.Err = attachtext.ErrTooLarge
	default:
		content, err := a.deps.Attachments.ReadAttachment(ctx, scope, ref.ID)
		if err != nil {
			file.Err = err
			return file
		}
		file.Content = content.Content
	}

	return file
}

// carriedImages reports whether the rendered round would send a picture, which is
// what makes a chain's refusal worth a second try without them.
func carriedImages(turns []*attachtext.Turn) bool {
	for _, turn := range turns {
		if len(turn.Images()) > 0 {
			return true
		}
	}

	return false
}

// attachMessage renders one turn's text with what its files contribute, and the
// pictures when the chain can see them.
func attachMessage(message llmdomain.Message, attachments *attachtext.Turn, vision bool) llmdomain.Message {
	if text := attachments.Text(vision); text != "" {
		if message.Text == "" {
			message.Text = text
		} else {
			message.Text += "\n\n" + text
		}
	}
	if !vision {
		return message
	}

	for _, image := range attachments.Images() {
		message.Images = append(message.Images, llmdomain.Image{MediaType: image.MediaType, Data: image.Data})
	}

	return message
}

// saysNothing reports whether a rendered message contributes nothing to the
// request: no text, no tool call, no tool answer, no picture. Only a message whose
// whole content was a file that a newer message already carries can reach that
// state, and sending it as a blank turn is noise the model has to account for.
func saysNothing(message llmdomain.Message) bool {
	return strings.TrimSpace(message.Text) == "" &&
		len(message.ToolCalls) == 0 &&
		len(message.ToolResults) == 0 &&
		len(message.Images) == 0
}
