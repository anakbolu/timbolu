package activity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/attachtext"
)

// stubAttachments serves the files of a test's messages by attachment id.
type stubAttachments struct {
	files map[uuid.UUID]domain.AttachmentContent
	// errs fails a read, which is what a missing object or a refused scope looks
	// like from the activity's side.
	errs map[uuid.UUID]error
	// reads counts the calls, which is how "an oversized file is never fetched"
	// is asserted.
	reads int
}

func (s *stubAttachments) ReadAttachment(_ context.Context, _ domain.Scope, id uuid.UUID) (domain.AttachmentContent, error) {
	s.reads++
	if err := s.errs[id]; err != nil {
		return domain.AttachmentContent{}, err
	}

	file, ok := s.files[id]
	if !ok {
		return domain.AttachmentContent{}, fmt.Errorf("task: attachment %s not found", id)
	}

	return file, nil
}

// textRef is a stored text file's reference, as the chat module writes it.
func textRef(content string) (domain.AttachmentRef, domain.AttachmentContent) {
	id := uuid.New()
	ref := domain.AttachmentRef{
		ID: id, Filename: "stok.txt", ContentType: "text/plain", ByteSize: int64(len(content)),
	}

	return ref, domain.AttachmentContent{Filename: "stok.txt", ContentType: "text/plain", Content: []byte(content)}
}

// pngRef is a picture's reference. The bytes are a PNG header and nothing else,
// which is all the sniffing looks at: the stored content type is not trusted.
func pngRef() (domain.AttachmentRef, domain.AttachmentContent) {
	content := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	id := uuid.New()
	ref := domain.AttachmentRef{
		ID: id, Filename: "foto.png", ContentType: "image/png", ByteSize: int64(len(content)),
	}

	return ref, domain.AttachmentContent{Filename: "foto.png", ContentType: "image/png", Content: content}
}

// callModel runs one round for the harness's task.
func callModel(t *testing.T, h *harness, turns ...domain.Turn) (workflow.ModelReply, error) {
	t.Helper()

	return h.activities.CallModel(context.Background(), workflow.ModelRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		AgentID:     h.task.AgentID.String(),
		System:      "Kamu Oren.",
		Messages:    turns,
	})
}

// TestCallModelReadsTheAttachmentsOfAMessage is the task half of #187: a message
// that carried a file reaches the model with the file's content in it.
func TestCallModelReadsTheAttachmentsOfAMessage(t *testing.T) {
	ref, file := textRef("Stok kaos merah: 7 pcs")
	attachments := &stubAttachments{files: map[uuid.UUID]domain.AttachmentContent{ref.ID: file}}

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "stoknya 7", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	// The message is nothing but the file, which is what an upload with no words
	// looks like once the workflow has rendered it.
	reply, err := callModel(t, h, domain.Turn{Role: llmdomain.RoleUser, Attachments: []domain.AttachmentRef{ref}})
	require.NoError(t, err)
	require.Equal(t, "stoknya 7", reply.Text)
	require.Equal(t, 1, attachments.reads)

	require.Len(t, captured.Messages, 1)
	require.Regexp(t, `(?m)^\[Lampiran: stok\.txt \(text/plain, 22 B\) #[0-9a-f]{8}\]$`, captured.Messages[0].Text,
		"the prompt names the file the way a model can cite it")
	require.Regexp(t, `(?m)^\[/Lampiran #[0-9a-f]{8}\]$`, captured.Messages[0].Text,
		"the file's text is closed by a marker of its own")
	require.Contains(t, captured.Messages[0].Text, "Stok kaos merah: 7 pcs")
	require.Empty(t, captured.Messages[0].Images, "a text file is text, not a picture")
}

// TestCallModelSendsAPictureAsAnImage: a picture travels as vision input rather
// than as a line about a picture.
func TestCallModelSendsAPictureAsAnImage(t *testing.T) {
	ref, file := pngRef()
	attachments := &stubAttachments{files: map[uuid.UUID]domain.AttachmentContent{ref.ID: file}}

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "kelihatan", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	_, err := callModel(t, h, domain.Turn{Role: llmdomain.RoleUser, Text: "cek ini", Attachments: []domain.AttachmentRef{ref}})
	require.NoError(t, err)

	images := captured.Messages[0].Images
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MediaType)
	require.Equal(t, base64.StdEncoding.EncodeToString(file.Content), images[0].Data)
	require.Contains(t, captured.Messages[0].Text, "[Gambar foto.png (image/png, 24 B)]")
}

// TestCallModelRetriesWithoutImagesWhenTheChainCannotSeeThem is the fallback: a
// task whose chain has no vision still finishes, with the picture named as one it
// could not read.
func TestCallModelRetriesWithoutImagesWhenTheChainCannotSeeThem(t *testing.T) {
	ref, file := pngRef()
	attachments := &stubAttachments{files: map[uuid.UUID]domain.AttachmentContent{ref.ID: file}}

	var (
		calls   []llmdomain.ChatRequest
		served  int
		refused bool
	)
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			calls = append(calls, req)
			if len(req.Messages) > 0 && len(req.Messages[0].Images) > 0 {
				refused = true
				return llmdomain.ChatResponse{}, fmt.Errorf("llm: the provider cannot read images: %w", llmdomain.ErrUnsupported)
			}
			served++
			return llmdomain.ChatResponse{Text: "fotonya tidak bisa kubaca", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	reply, err := callModel(t, h, domain.Turn{Role: llmdomain.RoleUser, Text: "cek ini", Attachments: []domain.AttachmentRef{ref}})
	require.NoError(t, err, "a chain without vision must not fail the task")
	require.True(t, refused, "the first attempt carried the picture")
	require.Equal(t, 1, served)
	require.Equal(t, "fotonya tidak bisa kubaca", reply.Text)

	require.Len(t, calls, 2)
	require.Contains(t, calls[1].Messages[0].Text,
		"[Gambar foto.png tidak bisa dibaca: model yang aktif tidak mendukung gambar]")
	require.Empty(t, calls[1].Messages[0].Images)
}

// TestCallModelNamesAFileItCouldNotRead: a file the reader cannot produce is a
// note in the prompt, not a failed round.
func TestCallModelNamesAFileItCouldNotRead(t *testing.T) {
	ref, _ := textRef("Stok kaos merah: 7 pcs")
	attachments := &stubAttachments{errs: map[uuid.UUID]error{ref.ID: errors.New("storage: connection refused")}}

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "berkasnya tidak terbaca", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	_, err := callModel(t, h, domain.Turn{Role: llmdomain.RoleUser, Text: "cek", Attachments: []domain.AttachmentRef{ref}})
	require.NoError(t, err)

	require.Contains(t, captured.Messages[0].Text, "[Lampiran stok.txt tidak bisa dibaca: storage: connection refused]")
}

// TestCallModelDoesNotReadAnOversizedFile: the bound is checked against the
// reference's size, so a huge file costs a note rather than a download.
func TestCallModelDoesNotReadAnOversizedFile(t *testing.T) {
	ref, _ := textRef("Stok kaos merah: 7 pcs")
	ref.ByteSize = 40 << 20
	attachments := &stubAttachments{}

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		deps.AttachmentLimits = attachtext.Limits{MaxFileBytes: 1024}
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "oke", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	_, err := callModel(t, h, domain.Turn{Role: llmdomain.RoleUser, Text: "cek", Attachments: []domain.AttachmentRef{ref}})
	require.NoError(t, err)

	require.Zero(t, attachments.reads, "an oversized file is never fetched")
	require.Contains(t, captured.Messages[0].Text, "berkas lebih besar dari batas baca")
}

// TestCallModelWithoutAReaderNamesTheFiles: a deployment that reads no
// attachments still tells the model the file is there.
func TestCallModelWithoutAReaderNamesTheFiles(t *testing.T) {
	ref, _ := textRef("Stok kaos merah: 7 pcs")

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "oke", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	_, err := callModel(t, h, domain.Turn{Role: llmdomain.RoleUser, Text: "cek", Attachments: []domain.AttachmentRef{ref}})
	require.NoError(t, err)

	require.Contains(t, captured.Messages[0].Text,
		"[Lampiran stok.txt tidak bisa dibaca: task: attachments cannot be read in this deployment]")
}

// TestAttachmentBudgetIsSpentOnTheNewestMessage: an old thread full of documents
// must not crowd out the file the task was opened with.
func TestAttachmentBudgetIsSpentOnTheNewestMessage(t *testing.T) {
	older, olderFile := textRef("AAAA")
	newer, newerFile := textRef("BBBB")
	attachments := &stubAttachments{files: map[uuid.UUID]domain.AttachmentContent{
		older.ID: olderFile, newer.ID: newerFile,
	}}

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		// Room for one file's section only, which makes the order visible.
		deps.AttachmentLimits = attachtext.Limits{MaxCharsTotal: 80}
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "oke", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	_, err := callModel(t, h,
		domain.Turn{Role: llmdomain.RoleUser, Text: "yang lama", Attachments: []domain.AttachmentRef{older}},
		domain.Turn{Role: llmdomain.RoleUser, Text: "yang baru", Attachments: []domain.AttachmentRef{newer}},
	)
	require.NoError(t, err)

	require.Len(t, captured.Messages, 2)
	require.Contains(t, captured.Messages[1].Text, "BBBB", "the newest file is the one that gets the room")
	require.NotContains(t, captured.Messages[0].Text, "AAAA")
	require.Contains(t, captured.Messages[0].Text, "batas total konteks lampiran tercapai")
}

// TestCallModelReadsAFileOnceWhenTwoMessagesCarryIt is issue #239 on the task
// side: a file uploaded on its own is a message of its own, and the send that used
// it through attachment_ids carries the same reference again. Both turns are in the
// round, so without a rule the file is fetched twice, its text is spent twice, and
// its picture is sent as two images.
func TestCallModelReadsAFileOnceWhenTwoMessagesCarryIt(t *testing.T) {
	ref, file := pngRef()
	attachments := &stubAttachments{files: map[uuid.UUID]domain.AttachmentContent{ref.ID: file}}

	var captured llmdomain.ChatRequest
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Attachments = attachments
		deps.Gateway = capturingGateway{onChat: func(_ llmdomain.Scope, req llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			captured = req
			return llmdomain.ChatResponse{Text: "kelihatan 7", FinishReason: llmdomain.FinishStop}, nil
		}}
	})

	_, err := callModel(t, h,
		domain.Turn{Role: llmdomain.RoleUser, Attachments: []domain.AttachmentRef{ref}},
		domain.Turn{Role: llmdomain.RoleUser, Text: "stoknya berapa?", Attachments: []domain.AttachmentRef{ref}},
	)
	require.NoError(t, err)

	require.Equal(t, 1, attachments.reads, "one file is fetched once")
	require.Len(t, captured.Messages, 1, "the file travels on the message that was written")
	require.Equal(t, "stoknya berapa?", captured.Messages[0].Text[:len("stoknya berapa?")])
	require.Len(t, captured.Messages[0].Images, 1, "one picture is one image part")
	require.Equal(t, 1, strings.Count(captured.Messages[0].Text, "[Gambar foto.png"),
		"the picture is named once")
}
