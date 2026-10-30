package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/task/workflow"
)

// recordingThreads is the chat module's side of a thread: it records what it was
// asked to open and answers with a thread, or with a refusal.
type recordingThreads struct {
	asked []domain.ThreadRequest
	err   error
}

func (r *recordingThreads) OpenThread(_ context.Context, _ domain.Scope, req domain.ThreadRequest) (domain.Thread, error) {
	r.asked = append(r.asked, req)
	if r.err != nil {
		return domain.Thread{}, r.err
	}
	return domain.Thread{ConversationID: uuid.New(), TaskID: uuid.New(), Title: req.Title}, nil
}

// chatTask opens a task that answers a chat, which is the only kind a thread can
// be opened from.
func (h *harness) chatTask(t *testing.T, depth int) domain.Task {
	t.Helper()

	task, err := h.tasks.Create(context.Background(), h.scope, domain.NewTask{
		AgentID:        uuid.MustParse(agentID),
		ConversationID: uuid.New(),
		Trigger:        domain.TriggerChat,
		Title:          "Posting carousel",
		Depth:          depth,
	})
	require.NoError(t, err)
	return task
}

func toolNames(tools []domain.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestLoadTaskContextOffersAThreadOnlyFromAChat: the tool is offered to a task a
// person asked for in a chat, and to no task that has nowhere to open one from or
// that another Bolu handed over.
func TestLoadTaskContextOffersAThreadOnlyFromAChat(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Threads = &recordingThreads{} })

	load := func(task domain.Task) []string {
		loaded, err := h.activities.LoadTaskContext(context.Background(), workflow.TaskInput{
			TaskID: task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(),
		})
		require.NoError(t, err)
		return toolNames(loaded.Tools)
	}

	require.Contains(t, load(h.chatTask(t, 0)), domain.ToolStartThread)
	require.NotContains(t, load(h.chatTask(t, 1)), domain.ToolStartThread, "a handed-over task is someone else's work")
	require.NotContains(t, load(h.task), domain.ToolStartThread, "a task with no chat has nowhere to open a thread")

	without := newHarness(t, domain.DefaultLimits)
	loaded, err := without.activities.LoadTaskContext(context.Background(), workflow.TaskInput{
		TaskID: without.chatTask(t, 0).ID.String(), WorkspaceID: without.scope.WorkspaceID.String(),
	})
	require.NoError(t, err)
	require.NotContains(t, toolNames(loaded.Tools), domain.ToolStartThread, "no chat module, no thread")
}

// TestStartThreadOpensTheThreadFromTheTasksChat: the thread is opened from the
// conversation the task answers, carried by the same Bolu, and names the task.
func TestStartThreadOpensTheThreadFromTheTasksChat(t *testing.T) {
	threads := &recordingThreads{}
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Threads = threads })
	task := h.chatTask(t, 0)

	result, err := h.activities.StartThread(context.Background(), workflow.ThreadRequest{
		TaskID:      task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Title:       "Publish carousel",
		Brief:       "Siapkan No 43",
	})
	require.NoError(t, err)
	require.Empty(t, result.Refusal)
	require.NotEmpty(t, result.ConversationID)
	require.Equal(t, "Publish carousel", result.Title)

	require.Len(t, threads.asked, 1)
	asked := threads.asked[0]
	require.Equal(t, task.ConversationID, asked.ConversationID)
	require.Equal(t, task.AgentID, asked.AgentID)
	require.Equal(t, task.ID, asked.ParentTaskID)
}

// TestStartThreadRefusesRatherThanFailing: every way a thread cannot be opened is
// a sentence the model reads, not an error that ends the task.
func TestStartThreadRefusesRatherThanFailing(t *testing.T) {
	threads := &recordingThreads{}
	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) { deps.Threads = threads })

	empty, err := h.activities.StartThread(context.Background(), workflow.ThreadRequest{
		TaskID: h.chatTask(t, 0).ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), Title: "x",
	})
	require.NoError(t, err)
	require.Contains(t, empty.Refusal, "brief")

	noChat, err := h.activities.StartThread(context.Background(), workflow.ThreadRequest{
		TaskID: h.task.ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), Title: "x", Brief: "y",
	})
	require.NoError(t, err)
	require.Contains(t, noChat.Refusal, "obrolan")

	threads.err = errors.New("chat: a thread cannot open another thread")
	refused, err := h.activities.StartThread(context.Background(), workflow.ThreadRequest{
		TaskID: h.chatTask(t, 0).ID.String(), WorkspaceID: h.scope.WorkspaceID.String(), Title: "x", Brief: "y",
	})
	require.NoError(t, err)
	require.Contains(t, refused.Refusal, "another thread")
}
