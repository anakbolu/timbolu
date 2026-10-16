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

	reader := taskAttachmentReader{service: service}
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

	reader := taskAttachmentReader{service: service}
	_, err := reader.ReadAttachment(context.Background(), taskdomain.Scope{WorkspaceID: uuid.New()}, attachmentID)
	require.ErrorIs(t, err, chatdomain.ErrStorageUnavailable)
}

// TestTaskAttachmentReaderIsAbsentWithoutTheChatModule: a deployment that runs no
// conversation module has nothing that can read an upload, and says so by handing
// the activity no reader at all.
func TestTaskAttachmentReaderIsAbsentWithoutTheChatModule(t *testing.T) {
	c := &Container{Logger: zap.NewNop(), Services: &Services{}}
	require.Nil(t, c.taskAttachmentReader())

	service := chatmocks.NewService(t)
	c.Services.Chat = service
	require.NotNil(t, c.taskAttachmentReader())
}
