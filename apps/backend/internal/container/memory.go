package container

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/activity"
	memorydomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	memoryservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/workflow"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// openMemory builds the memory module: what a Bolu remembers, what it knows, and
// the extraction that reads a conversation once it stops moving.
//
// It runs before the task runtime and before the conversations, because both ask
// it for something: a task asks what a Bolu remembers before it answers, and a
// message tells the extractor the thread is still alive.
//
// Without a model gateway the module still serves its reads and its deletes; the
// writes that need a vector answer with the reason instead. That is the honest
// state for a deployment with no embedding provider, and it is reported at
// startup.
func (c *Container) openMemory(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.Memory == nil {
		c.Logger.Warn("memory is disabled: no database connection")
		return nil
	}

	var embedder memorydomain.Embedder
	if c.Services.LLM != nil {
		embedder = c.Services.LLM
	} else {
		c.Logger.Warn("memory writes are disabled: the model gateway is not configured, so nothing can be embedded")
	}

	deps := memoryservice.Deps{
		Repository: c.Repositories.Memory,
		Embedder:   embedder,
		Model:      c.Services.LLM,
		Threads:    c.memoryConversations(),
		Auditor:    c.memoryAuditor(),
		Scheduler:  c.memoryScheduler(cfg),
		Limits:     memoryLimits(cfg),
		Logger:     c.Logger,
	}

	service := memoryservice.New(deps)
	c.Memory = service
	c.Services.Memory = service

	// The bounds are logged because two of them are invisible from the outside: a
	// retrieval that returns nothing looks like a workspace with no memory, and an
	// extraction that never runs looks like a slow conversation. Both are the
	// configured value, so a deployment can check what it is running with.
	limits := deps.Limits.Normalise()
	c.Logger.Info("memory is enabled",
		zap.Int("main_tokens", limits.MainTokens),
		zap.Float64("similarity", limits.Similarity),
		zap.Duration("quiet_period", limits.QuietPeriod),
		zap.Bool("embeddings_available", embedder != nil),
	)

	// The activity set is built in every process for the same reason the task
	// runtime's is: the worker binary shares the container, and one assembly
	// point is what keeps the two from disagreeing about what is registered.
	c.MemoryTasks = activity.New(activity.Deps{Memories: service, Logger: c.Logger})

	return nil
}

// memoryLimits reads the configured bounds, falling back to the product defaults.
func memoryLimits(cfg *config.Config) memorydomain.Limits {
	if cfg == nil {
		return memorydomain.DefaultLimits
	}

	return memorydomain.Limits{
		MainTokens:         cfg.Memory.MainTokens,
		RecallMemories:     cfg.Memory.RecallMemories,
		RecallKnowledge:    cfg.Memory.RecallKnowledge,
		Similarity:         cfg.Memory.Similarity,
		QuietPeriod:        time.Duration(cfg.Memory.QuietSeconds) * time.Second,
		ExtractionMessages: cfg.Memory.ExtractionMessages,
		ChunkRunes:         cfg.Memory.ChunkRunes,
		ChunkOverlapRunes:  cfg.Memory.ChunkOverlapRunes,
		MaxContentRunes:    cfg.Memory.MaxContentRunes,
	}.Normalise()
}

// memoryConversations adapts the conversation store to the reader the extractor
// uses. It renders the blocks to the text a model reads, so the memory module
// never learns what a block is.
func (c *Container) memoryConversations() memorydomain.Conversation {
	if c == nil || c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}
	return memoryConversations{messages: c.Repositories.Chat, agents: c.Services.Agent}
}

type memoryConversations struct {
	messages chatdomain.MessageRepository
	agents   agentdomain.Service
}

// Recent implements memorydomain.Conversation: the last messages of a thread,
// oldest first, as plain text.
func (m memoryConversations) Recent(ctx context.Context, scope memorydomain.Scope, conversationID uuid.UUID, limit int) ([]memorydomain.Utterance, error) {
	stored, err := m.messages.LatestMessages(ctx, chatScopeForMemory(scope), conversationID, limit)
	if err != nil {
		return nil, err
	}

	names := m.agentNames(ctx, scope)

	utterances := make([]memorydomain.Utterance, 0, len(stored))
	for _, message := range stored {
		text := strings.TrimSpace(renderChatBlocks(message.Blocks))
		if text == "" {
			// A placeholder the task has not filled yet is not something the
			// extractor can read, and an empty turn would look like a message
			// that said nothing.
			continue
		}

		utterances = append(utterances, memorydomain.Utterance{
			MessageID: message.ID,
			AgentID:   message.AuthorAgentID,
			AgentName: names[message.AuthorAgentID],
			Text:      text,
			CreatedAt: message.CreatedAt,
		})
	}

	return utterances, nil
}

// agentNames maps the Bolu of a workspace to their names, so the transcript says
// who spoke rather than naming an id. A registry that is not configured leaves
// the names empty, which the extractor still reads as "a Bolu".
func (m memoryConversations) agentNames(ctx context.Context, scope memorydomain.Scope) map[uuid.UUID]string {
	names := map[uuid.UUID]string{}
	if m.agents == nil {
		return names
	}

	agents, err := m.agents.List(ctx, agentdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	})
	if err != nil {
		return names
	}

	for _, agent := range agents {
		names[agent.ID] = agent.Name
	}
	return names
}

// memoryAuditor adapts the approval repository to the audit port.
//
// The trail lives in the approval module's table, and the module that owns a
// table is the module that writes it: the container is the only package allowed
// to know both, which is what this adapter is for.
func (c *Container) memoryAuditor() memorydomain.Auditor {
	if c == nil || c.Repositories == nil || c.Repositories.Approval == nil {
		return nil
	}
	return memoryAuditor{repository: c.Repositories.Approval}
}

// auditRecorder is the one method of the approval repository this adapter uses.
// Naming it separately is what keeps the memory module's trail writer from
// depending on the approval module's whole contract, which it has no business
// with: it writes one entry and reads nothing.
type auditRecorder interface {
	RecordAudit(ctx context.Context, scope approvaldomain.Scope, entry approvaldomain.AuditEntry) error
}

type memoryAuditor struct {
	repository auditRecorder
}

// Record implements memorydomain.Auditor.
func (a memoryAuditor) Record(ctx context.Context, scope memorydomain.Scope, entry memorydomain.AuditEntry) error {
	return a.repository.RecordAudit(ctx, approvaldomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, approvaldomain.AuditEntry{
		Action:       entry.Action,
		ObjectType:   entry.ObjectType,
		ObjectID:     entry.ObjectID,
		ActorUserID:  entry.ActorUserID,
		ActorAgentID: entry.ActorAgentID,
		Metadata:     entry.Metadata,
	})
}

// memoryScheduler starts the extraction workflow that waits for a conversation
// to go quiet.
//
// SignalWithStart is what makes the debounce work with one call: the first
// message starts a run, and every later one signals the run that already exists.
// The workflow id is derived from the conversation, so two messages a second
// apart reach the same workflow instead of opening two.
//
// The quiet period is read from the configuration here rather than from the
// service, because the service does not exist yet when this is built: taking it
// from `c.Memory` would capture the nil interface and every conversation would wait
// for the product default instead of the configured one.
func (c *Container) memoryScheduler(cfg *config.Config) memorydomain.Scheduler {
	if c == nil || c.Temporal == nil {
		return nil
	}
	return memoryScheduler{client: c.Temporal, quiet: memoryLimits(cfg).QuietPeriod}
}

type memoryScheduler struct {
	client *temporal.Client
	quiet  time.Duration
}

// ConversationActive implements memorydomain.Scheduler.
func (s memoryScheduler) ConversationActive(ctx context.Context, scope memorydomain.Scope, conversationID, agentID uuid.UUID) error {
	if s.client == nil || s.client.Client() == nil {
		return nil
	}
	if conversationID == uuid.Nil {
		return nil
	}
	quiet := s.quiet
	if quiet <= 0 {
		quiet = workflow.DefaultQuietPeriod
	}

	_, err := s.client.Client().SignalWithStartWorkflow(
		ctx,
		memoryWorkflowID(conversationID),
		workflow.SignalActive,
		nil,
		s.options(conversationID),
		workflow.WorkflowName,
		workflow.ExtractionInput{
			WorkspaceID:    scope.WorkspaceID.String(),
			ConversationID: conversationID.String(),
			AgentID:        wireUUID(agentID),
			QuietPeriod:    quiet,
		},
	)
	return err
}

// options are what one extraction run is started with.
//
// The id is reused when the previous run already finished, and that is the whole
// point of the debounce: a conversation is extracted once per quiet period, so a
// thread that keeps being written must be able to open a fresh run. A task's id
// must not be reused — a duplicate dispatch there has to be refused rather than
// replace a finished task's history — which is why the policy is set here instead
// of in StartOptions, where both schedules would share it.
func (s memoryScheduler) options(conversationID uuid.UUID) client.StartWorkflowOptions {
	options := s.client.StartOptions(memoryWorkflowID(conversationID))
	options.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE
	return options
}

// memoryWorkflowID is the deterministic id of one conversation's extraction run.
func memoryWorkflowID(conversationID uuid.UUID) string {
	return "memory-" + conversationID.String()
}

// memoryRecallForTask adapts the memory service to the task runtime's port.
func (c *Container) memoryRecallForTask() taskdomain.MemoryRecall {
	if c == nil || c.Memory == nil {
		return nil
	}
	return memoryRecall{service: c.Memory}
}

type memoryRecall struct {
	service memorydomain.Service
}

// Recall implements taskdomain.MemoryRecall.
func (r memoryRecall) Recall(ctx context.Context, scope taskdomain.Scope, request taskdomain.RecallRequest) (taskdomain.Recall, error) {
	recall, err := r.service.Recall(ctx, memoryScopeFor(scope), memorydomain.RecallRequest{
		AgentID: request.AgentID,
		TaskID:  request.TaskID,
		Query:   request.Query,
	})
	if err != nil {
		return taskdomain.Recall{}, err
	}

	return taskdomain.Recall{
		Stable:    recall.Stable,
		Retrieved: recall.Retrieved,
		Used:      recall.Used,
	}, nil
}

// memoryForChat adapts the memory service to the two questions the chat module
// asks about a message.
func (c *Container) memoryForChat() chatdomain.Memory {
	if c == nil || c.Memory == nil {
		return nil
	}
	return memoryChat{service: c.Memory}
}

type memoryChat struct {
	service memorydomain.Service
}

// Remember implements chatdomain.Memory.
func (m memoryChat) Remember(ctx context.Context, scope chatdomain.Scope, req chatdomain.RememberRequest) (chatdomain.RememberResult, error) {
	result, err := m.service.Remember(ctx, memoryScopeForChat(scope), memorydomain.RememberRequest{
		Text:      req.Text,
		AgentID:   req.AgentID,
		MessageID: req.MessageID,
	})
	if err != nil {
		return chatdomain.RememberResult{}, err
	}

	return chatdomain.RememberResult{
		Handled:      result.Handled,
		Confirmation: result.Confirmation,
		Refused:      result.Refused,
	}, nil
}

// ConversationActive implements chatdomain.Memory.
func (m memoryChat) ConversationActive(ctx context.Context, scope chatdomain.Scope, conversationID, agentID uuid.UUID) error {
	return m.service.Active(ctx, memoryScopeForChat(scope), conversationID, agentID)
}

func memoryScopeFor(scope taskdomain.Scope) memorydomain.Scope {
	return memorydomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

func memoryScopeForChat(scope chatdomain.Scope) memorydomain.Scope {
	return memorydomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

func chatScopeForMemory(scope memorydomain.Scope) chatdomain.Scope {
	return chatdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

// RegisterMemoryTasks installs the extraction workflow and its activity on the
// agent worker.
func RegisterMemoryTasks(w workerish, c *Container) {
	if c == nil || c.MemoryTasks == nil {
		return
	}

	w.RegisterWorkflow(workflow.MemoryExtractionWorkflow)
	w.RegisterActivity(c.MemoryTasks)
}

// workerish is the part of the SDK worker this file needs. It is a local
// interface so the registration can be tested without a server.
type workerish interface {
	RegisterWorkflow(workflowFn any)
	RegisterActivity(activityFn any)
}

// renderChatBlocks turns a stored message body into the text a model reads.
//
// The rendering is the task module's, shared rather than reimplemented: the
// extractor has to read the conversation the same way the task runtime does, or
// the two would disagree about what was said — and a chart would be two different
// words depending on who asked.
func renderChatBlocks(blocks []chatdomain.Block) string {
	return taskdomain.RenderBlocks(toTaskBlocks(blocks))
}

func toTaskBlocks(blocks []chatdomain.Block) []taskdomain.Block {
	out := make([]taskdomain.Block, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, taskdomain.Block{Type: block.Type, Body: block.Body})
	}
	return out
}
