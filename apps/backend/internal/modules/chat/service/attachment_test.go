package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
)

// pngOf, pdfOf, exeOf, and textOf build content a sniffer reads as those types while
// still carrying the text a test names the file with, so two uploads can be told apart
// without hand-writing a whole file.
func pngOf(text string) []byte { return append([]byte("\x89PNG\r\n\x1a\n"), text...) }
func pdfOf(text string) []byte { return append([]byte("%PDF-1.4\n"), text...) }
func exeOf(text string) []byte { return append([]byte("MZ\x90\x00"), text...) }

// TestAnUploadOpensItsMessageOnlyOnceTheBytesAreStored: a file sent without a message
// used to open one first, so a store that failed left an empty bubble and a client that
// had been told "message.new" saw it change. Now the message and the file are one write,
// and the only event is the new message, already carrying the file.
func TestAnUploadOpensItsMessageOnlyOnceTheBytesAreStored(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	attachment, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID,
		Filename:       "foto.png",
	}, pngOf("foto"))
	require.NoError(t, err)

	message, err := h.store.GetMessage(context.Background(), h.scope, attachment.MessageID)
	require.NoError(t, err)
	require.Equal(t, conversation.ID, message.ConversationID)
	require.Equal(t, h.scope.UserID, message.AuthorUserID, "the person who uploaded wrote the message")
	require.Empty(t, message.Blocks, "the file is the whole message")
	require.Len(t, message.Attachments, 1)
	require.Equal(t, "foto.png", message.Attachments[0].Filename)

	require.Len(t, h.store.messagesOf(conversation.ID), 1, "the upload opened exactly one message")

	event := h.waitForEvent(t, chatdomain.EventMessageNew, func(event chatdomain.Event) bool {
		return decodeMessagePayload(t, event).MessageID == message.ID.String()
	})
	require.Equal(t, h.scope.UserID.String(), decodeMessagePayload(t, event).UserID)
	require.Empty(t, h.store.eventsOfType(chatdomain.EventMessageUpdated),
		"a client is never told about a message and then told it changed")
}

// TestAFailedUploadLeavesNothing: the store is down, so there is no row, no object, and
// nothing published — the request fails whole.
func TestAFailedUploadLeavesNothing(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)
	h.store.failAttachmentWrite = true

	_, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID,
		Filename:       "foto.png",
	}, pngOf("foto"))
	require.Error(t, err)

	require.Empty(t, h.store.messagesOf(conversation.ID), "no carrier message survives the failure")
	require.Empty(t, h.store.eventsOfType(chatdomain.EventMessageNew))
	require.Empty(t, h.store.eventsOfType(chatdomain.EventMessageUpdated))
	require.Empty(t, h.storage.objects, "the bytes are removed again when the row cannot be written")
}

// TestARefusedUploadLeavesNothing: the type is checked before the first write, so a
// refused file never reaches storage and never opens a message.
func TestARefusedUploadLeavesNothing(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	_, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID,
		Filename:       "x.exe",
	}, exeOf("payload"))
	require.ErrorIs(t, err, chatdomain.ErrFileTypeNotAllowed)

	require.Empty(t, h.store.messagesOf(conversation.ID))
	require.Empty(t, h.storage.objects)
	require.Empty(t, h.store.events)
}

// TestTheFileTypeAllowlistIsCheckedOnBothNameAndContent: an extension is what a client
// types and the content is what the bytes are, so a file passes only when both agree.
func TestTheFileTypeAllowlistIsCheckedOnBothNameAndContent(t *testing.T) {
	tests := map[string]struct {
		filename string
		content  []byte
		allowed  bool
	}{
		"a png named png":                 {filename: "foto.png", content: pngOf("foto"), allowed: true},
		"a pdf named pdf":                 {filename: "nota.pdf", content: pdfOf("nota"), allowed: true},
		"text named txt":                  {filename: "catatan.txt", content: []byte("halo"), allowed: true},
		"an executable named exe":         {filename: "x.exe", content: exeOf("payload")},
		"an executable disguised as png":  {filename: "foto.png", content: exeOf("payload")},
		"an elf named png":                {filename: "foto.png", content: append([]byte("\x7fELF"), make([]byte, 32)...)},
		"a shell script named sh":         {filename: "deploy.sh", content: []byte("#!/bin/sh\necho hi\n")},
		"text where an image was claimed": {filename: "foto.png", content: []byte("not an image")},
		"a png where a pdf was claimed":   {filename: "nota.pdf", content: pngOf("foto")},
		"an unknown extension":            {filename: "data.parquet", content: []byte("PAR1")},
		"no extension at all":             {filename: "catatan", content: []byte("halo")},
		"no name at all":                  {filename: "", content: []byte("halo")},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			conversation := h.directConversation(t)

			attachment, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
				ConversationID: conversation.ID,
				Filename:       test.filename,
			}, test.content)

			if test.allowed {
				require.NoError(t, err)
				require.Equal(t, test.filename, attachment.Filename)
				return
			}

			require.ErrorIs(t, err, chatdomain.ErrFileTypeNotAllowed)
			require.Empty(t, h.store.messagesOf(conversation.ID))
			require.Empty(t, h.storage.objects)
		})
	}
}

// TestTheStoredTypeComesFromTheExtension: the type a download endpoint later serves is the
// one the extension names, and it is written with the row — there is no path by which a
// client's own header reaches storage, which is what keeps a file from being served as
// something it is not.
func TestTheStoredTypeComesFromTheExtension(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	attachment, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID,
		Filename:       "catatan.txt",
	}, []byte("halo"))
	require.NoError(t, err)

	require.Equal(t, "text/plain", attachment.ContentType)

	stored, err := h.service.Attachment(context.Background(), h.scope, attachment.ID)
	require.NoError(t, err)
	require.Equal(t, "text/plain", stored.ContentType)

	for key, object := range h.storage.objects {
		require.True(t, strings.HasSuffix(key, "catatan.txt"))
		require.Equal(t, []byte("halo"), object)
	}
}

// TestTheAllowlistCanOnlyNarrow: a deployment names the extensions it wants, and anything
// it names that the service has no content rule for is ignored — an allowlist that could
// name an executable would not be a policy but a hole.
func TestTheAllowlistCanOnlyNarrow(t *testing.T) {
	h := newHarness(t, func(deps *Deps) {
		deps.Config.AttachmentAllowedExtensions = []string{".PNG", "exe", "parquet", "png"}
	})
	conversation := h.directConversation(t)

	require.Equal(t, []string{"png"}, h.service.settings.AttachmentAllowedExtensions,
		"the dot is stripped, the duplicate dropped, and the executable and the unknown type refused")

	_, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "foto.png",
	}, pngOf("foto"))
	require.NoError(t, err)

	_, err = h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "catatan.txt",
	}, []byte("halo"))
	require.ErrorIs(t, err, chatdomain.ErrFileTypeNotAllowed, "a type outside the list is refused")
}

// TestAnUnusableAllowlistFallsBackToTheDefault: a typo in a deployment's list must not
// take the chat down, so a list with nothing usable in it is the default list.
func TestAnUnusableAllowlistFallsBackToTheDefault(t *testing.T) {
	require.Equal(t, defaultAttachmentExtensions, allowedExtensions(nil, zap.NewNop()))
	require.Equal(t, defaultAttachmentExtensions, allowedExtensions([]string{"", "."}, zap.NewNop()))
	require.Equal(t, defaultAttachmentExtensions, allowedExtensions([]string{"exe", "parquet"}, zap.NewNop()))
	require.Equal(t, []string{"png", "pdf"}, allowedExtensions([]string{"png", "pdf", "dll"}, zap.NewNop()))
}

// TestPrunePublishesTheMessageItEmptied: the sweep removes the row and the element the
// message renders in one write, and a client has to be told, because the file would
// otherwise stay on screen with a download that no longer answers.
func TestPrunePublishesTheMessageItEmptied(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	attachment, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "foto.png",
	}, pngOf("foto"))
	require.NoError(t, err)
	messageID := attachment.MessageID

	result, err := h.service.PruneAttachments(context.Background(), h.scope,
		time.Now().Add(time.Minute), chatdomain.DefaultPruneLimit)
	require.NoError(t, err)
	require.Equal(t, 1, result.Count)

	message, err := h.store.GetMessage(context.Background(), h.scope, messageID)
	require.NoError(t, err)
	require.Empty(t, message.Attachments, "the read model is what a client renders, so it goes with the row")

	event := h.waitForEvent(t, chatdomain.EventMessageUpdated, func(event chatdomain.Event) bool {
		payload := decodeMessagePayload(t, event)
		return payload.MessageID == messageID.String()
	})
	require.Empty(t, decodeMessagePayload(t, event).Attachments)
}

// TestAPrunedMessageKeepsItsOtherFiles: one sweep removes one row, and the message it
// belongs to must not lose the files that are still there.
func TestAPrunedMessageKeepsItsOtherFiles(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	first, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "satu.png",
	}, pngOf("satu"))
	require.NoError(t, err)

	second, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID: first.MessageID, Filename: "dua.png",
	}, pngOf("dua"))
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)

	// The row is removed on its own, which is what the sweep does once per row.
	message, err := h.store.Delete(context.Background(), h.scope, first.ID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)
	require.Equal(t, second.ID, message.Attachments[0].ID)

	// A row that is already gone converges rather than failing: the sweep runs again
	// after a partial failure and must not report work it already did.
	again, err := h.store.Delete(context.Background(), h.scope, first.ID)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, again.ID, "there is nothing to publish the second time")
}

// TestAnUploadNeedsAThreadToLandIn: the file opens its own message, so it has to say
// which conversation that message goes in.
func TestAnUploadNeedsAThreadToLandIn(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		Filename: "foto.png",
	}, pngOf("foto"))
	require.ErrorIs(t, err, chatdomain.ErrInvalidInput)
	require.Empty(t, h.storage.objects)
}
