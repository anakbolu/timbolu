package service

import (
	"context"
	"encoding/json"
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

// backdate moves a row's created_at into the past. It is the only column the sweep
// selects on, so it is how a test has one pass take some files of a message and leave
// the others behind.
func (h *harness) backdate(t *testing.T, id uuid.UUID, age time.Duration) {
	t.Helper()

	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	attachment, ok := h.store.attachments[id]
	require.True(t, ok, "the row has to exist before it can be backdated")
	attachment.CreatedAt = time.Now().Add(-age)
	h.store.attachments[id] = attachment
}

// updatesFor returns the message.updated events a client would have seen for one
// message, which is what tells one event per pass from one event per file.
func (h *harness) updatesFor(t *testing.T, messageID uuid.UUID) []chatdomain.Event {
	t.Helper()

	var updates []chatdomain.Event
	for _, event := range h.store.eventsOfType(chatdomain.EventMessageUpdated) {
		if decodeMessagePayload(t, event).MessageID == messageID.String() {
			updates = append(updates, event)
		}
	}
	return updates
}

// requireEmptyAttachments asserts the payload carries an empty list rather than an
// absent or null field: a client replaces the whole list when the field is present, so
// an omitted one would leave a file on screen whose bytes are already gone.
func requireEmptyAttachments(t *testing.T, event chatdomain.Event) {
	t.Helper()

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(event.Payload, &payload))
	require.Contains(t, payload, "attachments", "the field has to be there for a client to clear the list")
	require.JSONEq(t, `[]`, string(payload["attachments"]))
}

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
	requireEmptyAttachments(t, event)
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

// TestPrunePublishesOneUpdatePerMessage: several files of one message expire in the
// same pass, and a client replaces the whole attachment list on every update it gets.
// One event per row would render states the message never had, so the pass ends with a
// single event per message carrying the list as it finally reads.
func TestPrunePublishesOneUpdatePerMessage(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	first, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "satu.png",
	}, pngOf("satu"))
	require.NoError(t, err)
	messageID := first.MessageID

	second, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID: messageID, Filename: "dua.png",
	}, pngOf("dua"))
	require.NoError(t, err)

	kept, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		MessageID: messageID, Filename: "tiga.png",
	}, pngOf("tiga"))
	require.NoError(t, err)

	// Two of the three are past the cutoff and one is not, which is the pass the
	// issue was filed about: one message, two rows, two intermediate states.
	h.backdate(t, first.ID, 72*time.Hour)
	h.backdate(t, second.ID, 72*time.Hour)
	before := len(h.updatesFor(t, messageID))

	result, err := h.service.PruneAttachments(context.Background(), h.scope,
		time.Now().Add(-24*time.Hour), chatdomain.DefaultPruneLimit)
	require.NoError(t, err)
	require.Equal(t, 2, result.Count)

	updates := h.updatesFor(t, messageID)
	require.Len(t, updates, before+1, "one pass is one event per message, however many files it took")

	payload := decodeMessagePayload(t, updates[len(updates)-1])
	require.Len(t, payload.Attachments, 1)
	require.Equal(t, kept.ID.String(), payload.Attachments[0].ID)
}

// TestPrunePublishesEveryMessageItTouched: a pass over several messages is one event
// each, and the message it emptied is one a client can clear rather than leave stale.
func TestPrunePublishesEveryMessageItTouched(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	first, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "satu.png",
	}, pngOf("satu"))
	require.NoError(t, err)

	second, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "dua.png",
	}, pngOf("dua"))
	require.NoError(t, err)
	require.NotEqual(t, first.MessageID, second.MessageID)

	h.backdate(t, first.ID, 72*time.Hour)
	h.backdate(t, second.ID, 72*time.Hour)
	before := map[uuid.UUID]int{
		first.MessageID:  len(h.updatesFor(t, first.MessageID)),
		second.MessageID: len(h.updatesFor(t, second.MessageID)),
	}

	result, err := h.service.PruneAttachments(context.Background(), h.scope,
		time.Now().Add(-24*time.Hour), chatdomain.DefaultPruneLimit)
	require.NoError(t, err)
	require.Equal(t, 2, result.Count)

	for _, messageID := range []uuid.UUID{first.MessageID, second.MessageID} {
		updates := h.updatesFor(t, messageID)
		require.Len(t, updates, before[messageID]+1, "each message it touched is told once")
		requireEmptyAttachments(t, updates[len(updates)-1])
	}
}

// TestPrunePublishesWhatItRemovedWhenAnotherFileFails: one object the store refuses
// must not hold back the messages the pass did change, because the next pass may fail
// on the same file and the files that are gone are gone now.
func TestPrunePublishesWhatItRemovedWhenAnotherFileFails(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	removed, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "satu.png",
	}, pngOf("satu"))
	require.NoError(t, err)

	stuck, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "dua.png",
	}, pngOf("dua"))
	require.NoError(t, err)

	// The stuck row is the older one, so the pass meets it first and keeps going.
	h.backdate(t, removed.ID, 48*time.Hour)
	h.backdate(t, stuck.ID, 96*time.Hour)
	h.storage.failDeleteKeys[stuck.StorageKey] = true

	result, err := h.service.PruneAttachments(context.Background(), h.scope,
		time.Now().Add(-24*time.Hour), chatdomain.DefaultPruneLimit)
	require.Error(t, err, "a file that could not be removed is reported")
	require.Equal(t, 1, result.Count)

	updates := h.updatesFor(t, removed.MessageID)
	require.Len(t, updates, 1, "the sweep's event is the only update: the upload opened the message")
	requireEmptyAttachments(t, updates[len(updates)-1])
	require.Empty(t, h.updatesFor(t, stuck.MessageID), "a row that stayed is a file still on screen")

	message, err := h.store.GetMessage(context.Background(), h.scope, stuck.MessageID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)
}

// TestPrunePublishesWhatItRemovedWhenTheCallerGivesUp: the sweep stops when the caller's context is
// cancelled, and the files it removed before that are gone from the bucket and from the rows — so
// the messages that render them have to be published anyway, on a context that outlives the
// request. A cancelled context reaching the event store is what the issue was filed about: the row
// goes, the update never arrives, and the file stays on screen with a download that no longer
// answers.
func TestPrunePublishesWhatItRemovedWhenTheCallerGivesUp(t *testing.T) {
	h := newHarness(t)
	conversation := h.directConversation(t)

	removed, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "satu.png",
	}, pngOf("satu"))
	require.NoError(t, err)

	kept, err := h.service.Attach(context.Background(), h.scope, chatdomain.AttachRequest{
		ConversationID: conversation.ID, Filename: "dua.png",
	}, pngOf("dua"))
	require.NoError(t, err)
	require.NotEqual(t, removed.MessageID, kept.MessageID)

	// The pass meets the older row first and is cancelled the moment that row is gone, which is
	// what a caller that gave up between two rows looks like.
	h.backdate(t, removed.ID, 96*time.Hour)
	h.backdate(t, kept.ID, 48*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.store.afterDelete = cancel

	result, err := h.service.PruneAttachments(ctx, h.scope,
		time.Now().Add(-24*time.Hour), chatdomain.DefaultPruneLimit)
	require.ErrorIs(t, err, context.Canceled, "the pass reports that it was cut short")
	require.Equal(t, 1, result.Count, "only the row the pass reached went")

	updates := h.updatesFor(t, removed.MessageID)
	require.Len(t, updates, 1, "the message whose file went is published even though the caller gave up")
	requireEmptyAttachments(t, updates[0])

	require.Empty(t, h.updatesFor(t, kept.MessageID), "a row that stayed is a file still on screen")
	message, err := h.store.GetMessage(context.Background(), h.scope, kept.MessageID)
	require.NoError(t, err)
	require.Len(t, message.Attachments, 1)
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
