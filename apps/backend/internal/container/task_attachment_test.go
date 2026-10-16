package container

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain/mocks"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// TestTaskAttachmentReaderReadsInsideTheWorkspaceScope is the seam's contract:
// the activity names a file, and the chat module resolves it in the workspace the
// task belongs to, so a task can never read another tenant's upload.
func TestTaskAttachmentReaderReadsInsideTheWorkspaceScope(t *testing.T) {
	service := chatmocks.NewService(t)
	userID, workspaceID, attachmentID := uuid.New(), uuid.New(), uuid.New()

	service.EXPECT().
		AttachmentContent(mock.Anything, chatdomain.Scope{UserID: userID, WorkspaceID: workspaceID}, attachmentID).
		Return(chatdomain.Attachment{
			ID: attachmentID, StorageKey: "attachments/stok.txt",
			Filename: "stok.txt", ContentType: "text/plain", ByteSize: 22,
		}, []byte("Stok kaos merah: 7 pcs"), nil).
		Once()

	reader := taskAttachmentReader{container: &Container{Services: &Services{Chat: service}}}
	file, err := reader.ReadAttachment(context.Background(),
		taskdomain.Scope{UserID: userID, WorkspaceID: workspaceID}, attachmentID)
	require.NoError(t, err)

	require.Equal(t, taskdomain.AttachmentContent{
		Filename:    "stok.txt",
		ContentType: "text/plain",
		Content:     []byte("Stok kaos merah: 7 pcs"),
	}, file)
}

// TestTaskAttachmentReaderReportsAReadThatFailed: the activity turns this into a
// note in the prompt, so what matters here is that the failure arrives rather
// than being swallowed into an empty file.
func TestTaskAttachmentReaderReportsAReadThatFailed(t *testing.T) {
	service := chatmocks.NewService(t)
	attachmentID := uuid.New()

	service.EXPECT().
		AttachmentContent(mock.Anything, mock.Anything, attachmentID).
		Return(chatdomain.Attachment{}, nil, chatdomain.ErrStorageUnavailable).
		Once()

	reader := taskAttachmentReader{container: &Container{Services: &Services{Chat: service}}}
	_, err := reader.ReadAttachment(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, attachmentID)
	require.ErrorIs(t, err, chatdomain.ErrStorageUnavailable)
}

// TestTaskAttachmentReaderResolvesTheChatModuleWhenAFileIsRead is issue #235's
// unit-level gate: the task runtime is assembled before the chat module, so a
// reader that captured the service at assembly time would be nil in every
// process. This one holds the container and answers at call time, which is what
// makes the order irrelevant.
func TestTaskAttachmentReaderResolvesTheChatModuleWhenAFileIsRead(t *testing.T) {
	c := &Container{Logger: zap.NewNop(), Services: &Services{}}
	reader := c.taskAttachmentReader()
	require.NotNil(t, reader, "the activity set must always be handed a reader")

	// The assembly the container actually performs: the reader exists before the
	// chat module does.
	_, err := reader.ReadAttachment(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, uuid.New())
	require.ErrorIs(t, err, errNoChatModule, "a deployment with no chat module cannot read an upload")

	attachmentID := uuid.New()
	service := chatmocks.NewService(t)
	service.EXPECT().
		AttachmentContent(mock.Anything, mock.Anything, attachmentID).
		Return(chatdomain.Attachment{Filename: "stok.txt", ContentType: "text/plain"}, []byte("7 pcs"), nil).
		Once()
	c.Services.Chat = service

	file, err := reader.ReadAttachment(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, attachmentID)
	require.NoError(t, err, "the module built after the reader is the one that answers")
	require.Equal(t, []byte("7 pcs"), file.Content)
}
