package service

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/attachtext"
)

// attachmentMessage builds a stored user message carrying the given files, the
// shape Send writes when someone uploads something with no words.
func attachmentMessage(text string, attachments ...chatdomain.Attachment) chatdomain.Message {
	message := chatdomain.Message{
		ID:           uuid.New(),
		AuthorUserID: uuid.New(),
		Attachments:  attachments,
	}
	if text != "" {
		message.Blocks = []chatdomain.Block{{Type: chatdomain.BlockText, Body: jsonTextBlock(text)}}
	}

	return message
}

// storeText puts a text file in the store and returns the attachment row that
// points at it.
func storeText(t *testing.T, storage *memoryStorage, key, filename, content string) chatdomain.Attachment {
	t.Helper()

	object, err := storage.Put(context.Background(), key, "text/plain", []byte(content))
	require.NoError(t, err)

	return chatdomain.Attachment{
		ID: uuid.New(), Filename: filename, ContentType: "text/plain",
		StorageKey: object.Key, ByteSize: object.ByteSize,
	}
}

// replyOnce runs the responder to its end, failing the test on any error chunk.
func replyOnce(t *testing.T, responder *ModelResponder, scope chatdomain.Scope, history []chatdomain.Message) {
	t.Helper()

	chunks, err := responder.Reply(context.Background(), scope, chatdomain.ReplyRequest{
		Agent: chatdomain.AgentRef{ID: uuid.New(), Name: "Oren"},
		// The last message is the prompt, as the service sends it.
		Message: history[len(history)-1], History: history, RepairBudget: 1,
	})
	require.NoError(t, err)
	for chunk := range chunks {
		require.NotEqual(t, chatdomain.ChunkError, chunk.Kind, "the reply must not fail: %s", chunk.Err)
	}
}

// TestResponderIncludesAttachmentOnlyMessage is the half of #187 that is about a
// message with no words: an upload that is the message must still be answered
// with the file in hand, not skipped as an empty turn.
func TestResponderIncludesAttachmentOnlyMessage(t *testing.T) {

	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	attachment := storeText(t, storage, "attachments/w/1/stok.txt", "stok.txt", "Stok kaos merah: 7 pcs")

	message := attachmentMessage("", attachment)
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "stoknya 7"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})
	replyOnce(t, responder, scope, []chatdomain.Message{message})

	calls := gateway.recordedCalls()
	require.Len(t, calls, 1)
	require.Len(t, calls[0].Messages, 1, "the attachment-only message must not be dropped")
	require.Contains(t, calls[0].Messages[0].Text, "Stok kaos merah: 7 pcs")
	require.Equal(t, llmdomain.RoleUser, calls[0].Messages[0].Role)
}

// TestResponderSendsImagesToAVisionModel asserts the picture travels as an
// image: a base64 payload on the user's turn, not a description of one.
func TestResponderSendsImagesToAVisionModel(t *testing.T) {
	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	content := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	object, err := storage.Put(context.Background(), "attachments/w/2/foto.png", "image/png", content)
	require.NoError(t, err)

	message := attachmentMessage("cek ini", chatdomain.Attachment{
		ID: uuid.New(), Filename: "foto.png", ContentType: "image/png",
		StorageKey: object.Key, ByteSize: object.ByteSize,
	})
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "kelihatan"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})
	replyOnce(t, responder, scope, []chatdomain.Message{message})

	images := gateway.recordedCalls()[0].Messages[0].Images
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MediaType)
	require.Equal(t, base64.StdEncoding.EncodeToString(content), images[0].Data)
}

// TestResponderRetriesWithoutImagesWhenTheChainCannotSeeThem is the fallback: a
// Bolu whose model has no vision still answers, with the photo named as one it
// could not read.
func TestResponderRetriesWithoutImagesWhenTheChainCannotSeeThem(t *testing.T) {
	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	content := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	object, err := storage.Put(context.Background(), "attachments/w/2/foto.png", "image/png", content)
	require.NoError(t, err)

	message := attachmentMessage("cek ini", chatdomain.Attachment{
		ID: uuid.New(), Filename: "foto.png", ContentType: "image/png",
		StorageKey: object.Key, ByteSize: object.ByteSize,
	})
	gateway := &scriptedGateway{
		refuseImages: true,
		rounds: [][]llmdomain.StreamEvent{{
			{Type: llmdomain.EventText, Text: "fotonya tidak bisa kubaca"},
			{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
		}},
	}

	core, logs := observer.New(zap.WarnLevel)
	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage, Logger: zap.New(core)})

	chunks, err := responder.Reply(context.Background(), scope, chatdomain.ReplyRequest{
		Agent: chatdomain.AgentRef{ID: uuid.New(), Name: "Oren"}, Message: message,
		History: []chatdomain.Message{message}, RepairBudget: 1,
	})
	require.NoError(t, err)

	var (
		text    string
		notices []string
	)
	for chunk := range chunks {
		require.NotEqual(t, chatdomain.ChunkError, chunk.Kind, "the reply must survive a chain without vision")
		switch chunk.Kind {
		case chatdomain.ChunkText:
			text += chunk.Text
		case chatdomain.ChunkNotice:
			notices = append(notices, chunk.Text)
		}
	}

	require.Equal(t, "fotonya tidak bisa kubaca", text)
	require.NotEmpty(t, notices, "the user is told the picture was not read")
	require.Equal(t, 1, logs.Len(), "the fallback is worth one warning")

	calls := gateway.recordedCalls()
	require.Len(t, calls, 2, "one refused attempt, one without the picture")
	require.Len(t, calls[0].Messages[0].Images, 1)
	require.Empty(t, calls[1].Messages[0].Images)
	require.Contains(t, calls[1].Messages[0].Text, "[Gambar foto.png tidak bisa dibaca: model yang aktif tidak mendukung gambar]")
}

// TestResponderNotesAFileItCouldNotRead: a file the store cannot produce is a
// note, not a failed reply.
func TestResponderNotesAFileItCouldNotRead(t *testing.T) {
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	message := attachmentMessage("cek ini", chatdomain.Attachment{
		ID: uuid.New(), Filename: "hilang.txt", ContentType: "text/plain",
		StorageKey: "attachments/w/1/hilang.txt", ByteSize: 10,
	})
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "berkasnya tidak ada"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	core, logs := observer.New(zap.WarnLevel)
	responder := NewModelResponder(ResponderDeps{
		Gateway: gateway, Storage: newMemoryStorage(), Logger: zap.New(core),
	})
	replyOnce(t, responder, scope, []chatdomain.Message{message})

	require.Contains(t, gateway.recordedCalls()[0].Messages[0].Text, "[Lampiran hilang.txt tidak bisa dibaca:")
	require.Equal(t, 1, logs.Len())
}

// TestResponderDoesNotFetchAnOversizedFile: the bound is checked against the
// stored size, so a huge file costs a note rather than a download.
func TestResponderDoesNotFetchAnOversizedFile(t *testing.T) {
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	message := attachmentMessage("cek ini", chatdomain.Attachment{
		ID: uuid.New(), Filename: "besar.csv", ContentType: "text/csv",
		StorageKey: "attachments/w/1/besar.csv", ByteSize: 40 << 20,
	})
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "oke"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	storage := &countingStorage{ObjectStore: newMemoryStorage()}
	responder := NewModelResponder(ResponderDeps{
		Gateway: gateway, Storage: storage, Limits: attachtext.Limits{MaxFileBytes: 1024},
	})
	replyOnce(t, responder, scope, []chatdomain.Message{message})

	require.Zero(t, storage.gets, "an oversized file is never fetched")
	require.Contains(t, gateway.recordedCalls()[0].Messages[0].Text, "berkas lebih besar dari batas baca")
}

// TestResponderNamesAFileItCannotReadWhenThereIsNoStorage: a deployment without
// object storage still tells the model the file is there.
func TestResponderNamesAFileItCannotReadWhenThereIsNoStorage(t *testing.T) {
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	message := attachmentMessage("cek ini", chatdomain.Attachment{
		ID: uuid.New(), Filename: "stok.txt", ContentType: "text/plain",
		StorageKey: "attachments/w/1/stok.txt", ByteSize: 22,
	})
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "oke"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway})
	replyOnce(t, responder, scope, []chatdomain.Message{message})

	require.Contains(t, gateway.recordedCalls()[0].Messages[0].Text,
		"[Lampiran stok.txt tidak bisa dibaca: chat: object storage is not configured]")
}

// TestResponderSpendsTheBudgetOnTheNewestMessage: an old thread full of
// documents must not crowd out the file someone just sent.
func TestResponderSpendsTheBudgetOnTheNewestMessage(t *testing.T) {
	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	older := storeText(t, storage, "attachments/w/1/lama.txt", "lama.txt", "AAAA")
	newer := storeText(t, storage, "attachments/w/2/baru.txt", "baru.txt", "BBBB")

	first := attachmentMessage("yang lama", older)
	second := attachmentMessage("yang baru", newer)
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "oke"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{
		Gateway: gateway, Storage: storage,
		// Room for one file's section only, which makes the order visible.
		Limits: attachtext.Limits{MaxCharsTotal: 80},
	})
	replyOnce(t, responder, scope, []chatdomain.Message{first, second})

	messages := gateway.recordedCalls()[0].Messages
	require.Len(t, messages, 2)
	require.Contains(t, messages[1].Text, "BBBB", "the newest file is the one that gets the room")
	require.NotContains(t, messages[0].Text, "AAAA")
	require.Contains(t, messages[0].Text, "batas total konteks lampiran tercapai")
}

// TestResponderRendersAFileOnceWhenItWasUploadedAndThenLinked is issue #239: an
// upload with no message of its own becomes one, and the send that uses it through
// attachment_ids attaches the same file again. Both turns are in the history, so
// without a rule the file's text is spent twice and its picture is sent twice —
// once per turn — while only one of them is what the person meant to say.
func TestResponderRendersAFileOnceWhenItWasUploadedAndThenLinked(t *testing.T) {
	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}

	content := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	object, err := storage.Put(context.Background(), "attachments/w/3/foto.png", "image/png", content)
	require.NoError(t, err)
	foto := chatdomain.Attachment{
		ID: uuid.New(), Filename: "foto.png", ContentType: "image/png",
		StorageKey: object.Key, ByteSize: object.ByteSize,
	}

	// The upload is a message of its own, and the send that used it carries it
	// again: two turns, one file.
	uploaded := attachmentMessage("", foto)
	linked := attachmentMessage("stoknya berapa?", foto)
	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventText, Text: "kelihatan 7"},
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}

	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})
	replyOnce(t, responder, scope, []chatdomain.Message{uploaded, linked})

	calls := gateway.recordedCalls()
	require.Len(t, calls, 1)

	// The attachment-only turn has nothing left to say, and the file travels on
	// the message that was actually written.
	messages := calls[0].Messages
	require.Len(t, messages, 1, "the file is one turn, not two")
	require.Equal(t, "stoknya berapa?", strings.SplitN(messages[0].Text, "\n", 2)[0])
	require.Len(t, messages[0].Images, 1, "one picture is one image part")
	require.Equal(t, 1, strings.Count(messages[0].Text, "[Gambar foto.png"),
		"the picture is named once")
}

// TestResponderKeepsAttachmentsOutOfTheSystemPrompt is the boundary the issue
// asks for: files are content, and the instruction that governs the answer stays
// what the deployment wrote.
func TestResponderKeepsAttachmentsOutOfTheSystemPrompt(t *testing.T) {
	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New(), UserID: uuid.New()}
	attachment := storeText(t, storage, "attachments/w/1/stok.txt", "stok.txt", "Stok kaos merah: 7 pcs")
	message := attachmentMessage("cek", attachment)

	gateway := &scriptedGateway{rounds: [][]llmdomain.StreamEvent{{
		{Type: llmdomain.EventDone, FinishReason: llmdomain.FinishStop},
	}}}
	responder := NewModelResponder(ResponderDeps{Gateway: gateway, Storage: storage})
	replyOnce(t, responder, scope, []chatdomain.Message{message})

	require.NotContains(t, gateway.recordedCalls()[0].System, "Stok kaos merah")
	// The model is told the same thing the renderer enforces: what sits between
	// the markers is a file's content, so an instruction found in it is data too.
	require.Contains(t, gateway.recordedCalls()[0].System, "tidak bisa dipercaya")
}

// countingStorage counts the fetches, which is how "an oversized file is never
// downloaded" is asserted.
type countingStorage struct {
	chatdomain.ObjectStore
	gets int
}

func (s *countingStorage) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.ObjectStore.Get(ctx, key)
}

// A file whose content cannot be read must not stop the reply either way: the
// responder reports it and the model still answers.
func TestConversationTurnsRendersEveryMessage(t *testing.T) {
	storage := newMemoryStorage()
	scope := chatdomain.Scope{WorkspaceID: uuid.New()}
	attachment := storeText(t, storage, "attachments/w/1/stok.txt", "stok.txt", "Stok kaos merah: 7 pcs")

	responder := NewModelResponder(ResponderDeps{Gateway: &scriptedGateway{}, Storage: storage})
	fromAgent := attachmentMessage("halo")
	fromAgent.AuthorAgentID = uuid.New()

	turns := responder.conversationTurns(context.Background(), scope, []chatdomain.Message{
		attachmentMessage("", attachment),
		fromAgent,
	})

	require.Len(t, turns, 2)
	require.False(t, turns[0].empty())
	require.Nil(t, turns[0].attachments.Images())
	// The section's two markers carry one nonce, so the file's own text cannot
	// end the block it is quoted in.
	nonces := regexp.MustCompile(`#([0-9a-f]{8})\]`).FindAllStringSubmatch(turns[0].attachments.Text(true), -1)
	require.Len(t, nonces, 2)
	require.Equal(t, nonces[0][1], nonces[1][1])
	require.Equal(t,
		"[Lampiran: stok.txt (text/plain, 22 B) #"+nonces[0][1]+"]\nStok kaos merah: 7 pcs\n[/Lampiran #"+nonces[0][1]+"]",
		turns[0].attachments.Text(true))
	require.Equal(t, llmdomain.RoleUser, turns[0].message(true).Role)
	require.Equal(t, llmdomain.RoleAssistant, turns[1].message(true).Role)
}

// An empty message with no files contributes nothing, which is the one case a
// turn is left out of the request.
func TestTurnMessagesSkipsEmptyTurns(t *testing.T) {
	require.Empty(t, turnMessages([]conversationTurn{{role: llmdomain.RoleUser, text: "  "}}, true))
}

// A storage failure inside the renderer must not be mistaken for a missing file
// with no reason: the reason reaches the model.
func TestReadAttachmentReportsTheStorageError(t *testing.T) {
	responder := NewModelResponder(ResponderDeps{Gateway: &scriptedGateway{}, Storage: failingStorage{}})

	file := responder.readAttachment(context.Background(), chatdomain.Scope{}, chatdomain.Attachment{
		Filename: "stok.txt", ContentType: "text/plain", ByteSize: 4, StorageKey: "k",
	})

	require.ErrorIs(t, file.Err, errStorageDown)
}

var errStorageDown = errors.New("storage: connection refused")

type failingStorage struct{}

func (failingStorage) Put(context.Context, string, string, []byte) (chatdomain.StoredObject, error) {
	return chatdomain.StoredObject{}, errStorageDown
}
func (failingStorage) Get(context.Context, string) ([]byte, error) { return nil, errStorageDown }
func (failingStorage) Delete(context.Context, string) error        { return errStorageDown }
func (failingStorage) List(context.Context, string) ([]string, error) {
	return nil, errStorageDown
}
func (failingStorage) Driver() string { return "failing" }

var _ chatdomain.ObjectStore = failingStorage{}
