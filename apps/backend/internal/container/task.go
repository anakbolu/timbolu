package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	billingdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	taskactivity "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/activity"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	taskservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/pubsub"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// openTask builds the task runtime: the repository, the service that opens a
// task, and the ports the workflow's activities call.
//
// Every port is optional and the runtime degrades rather than failing: without
// the agent registry a task cannot be opened at all, without the model gateway
// it cannot think, and without a draft gate it refuses anything that reaches
// outside Bolu. Each of those is reported at startup, because a Bolu that
// silently does nothing is worse than one that says why.
func (c *Container) openTask(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.Task == nil {
		c.Logger.Warn("the task runtime is disabled: no database connection")
		return nil
	}

	// The starter is what hands a task to Temporal. Without it a task is
	// recorded and left queued, which is the honest state for a deployment that
	// runs the API without a worker.
	var starter taskdomain.Starter
	if c.Temporal != nil {
		starter = taskStarter{client: c.Temporal}
	} else {
		c.Logger.Warn("tasks will be recorded but never run: temporal is not configured")
	}

	deps := taskservice.Deps{
		Metrics: c.metricsFor(),
		Tasks:   c.Repositories.Task,
		Steps:   c.Repositories.Task,
		Starter: starter,
		Events:  c.taskEventSink(),
		Budget:  c.budget(),
		Limits:  taskLimits(cfg),
		Logger:  c.Logger,
	}
	if c.Services.Agent != nil {
		deps.Agents = taskAgentDirectory{service: c.Services.Agent}
	}

	c.Services.Task = taskservice.New(deps)

	// The activity set is built even when the API process never runs it: the
	// worker binary shares the container, and building it here is what keeps one
	// assembly point.
	c.Tasks = taskactivity.New(taskactivity.Deps{
		Tasks:    c.Repositories.Task,
		Steps:    c.Repositories.Task,
		Agents:   c.taskAgentDirectory(),
		Tools:    c.toolRegistry(),
		Executor: c.toolExecutor(),
		Drafts:   c.draftGate(),
		Memory:   c.memoryRecallForTask(),
		Gateway:  c.Services.LLM,
		Costs:    c.costTable(),
		Budget:   c.budget(),
		Events:   c.taskEventSink(),
		Reply:    c.taskReplyWriter(),
		// The files of a message reach the prompt through the chat module, which
		// owns them: the activity asks for the bytes, and the workflow only ever
		// held the reference.
		Attachments:      c.taskAttachmentReader(),
		AttachmentLimits: attachmentLimits(cfg),
		Outcomes:         taskOutcomeSink{container: c},
		Namer:            c.taskAgentNamer(),
		Limits:           taskLimits(cfg),
		Logger:           c.Logger,
		// The activity set starts the spans of a task's rounds and tool calls, under the
		// activity span the Temporal interceptor opens.
		Tracer:  c.telemetryTracer(),
		Metrics: c.metricsFor(),
	})

	return nil
}

// taskLimits reads the configured bounds, falling back to the product defaults.
func taskLimits(cfg *config.Config) taskdomain.Limits {
	if cfg == nil {
		return taskdomain.DefaultLimits
	}

	return taskdomain.Limits{
		MaxSteps:           cfg.Tasks.MaxSteps,
		MaxTokens:          cfg.Tasks.MaxTokens,
		MaxHandoffDepth:    cfg.Tasks.MaxHandoffDepth,
		MaxToolResultBytes: cfg.Tasks.MaxToolResultBytes,
	}.Normalise()
}

// agentService is the registry, or nil when this deployment has none.
//
// Every port below reads it through this one accessor, so a container that was
// never assembled answers "not configured" instead of dereferencing a nil
// aggregate: the same rule App and Close follow.
func (c *Container) agentService() agentdomain.Service {
	if c == nil || c.Services == nil {
		return nil
	}
	return c.Services.Agent
}

// taskAgentDirectory adapts the agent registry to the task module's port. It is
// nil-safe: a deployment without the registry gets a nil port, and the service
// then relies on the foreign key alone.
func (c *Container) taskAgentDirectory() taskdomain.AgentDirectory {
	service := c.agentService()
	if service == nil {
		return nil
	}
	return taskAgentDirectory{service: service}
}

// taskAgentNamer is the same adapter, asked a different question: which Bolu a
// handoff named.
func (c *Container) taskAgentNamer() taskdomain.AgentNamer {
	service := c.agentService()
	if service == nil {
		return nil
	}
	return taskAgentDirectory{service: service}
}

// toolRegistry reports the tools a Bolu was actually granted.
func (c *Container) toolRegistry() taskdomain.ToolRegistry {
	service := c.agentService()
	if service == nil {
		return nil
	}
	return toolRegistry{service: service, container: c}
}

// draftGate turns a write_external action into a draft a human decides on.
//
// It is the approval module's own service, adapted: the task proposes an action
// and the policy decides whether a person is asked. Without it a task refuses the
// action instead of taking it, which is the safe direction.
func (c *Container) draftGate() taskdomain.DraftGate {
	if c == nil || c.Services == nil || c.Services.Approval == nil {
		return nil
	}
	return approvalGate{service: c.Services.Approval}
}

// costTable prices a model call. It is the gateway's own table, so a task and
// the ledger price a call the same way and a task's own total cannot disagree
// with the invoice.
func (c *Container) costTable() taskdomain.CostTable {
	if c == nil {
		return nil
	}
	return c.Costs
}

// budget refuses work a workspace can no longer afford.
//
// It is the gateway's own pre-flight check, asked a second time and earlier: the
// gateway refuses the call, and this refuses the round, so a task stops at a
// clean boundary with a reason rather than halfway through a model call. Both
// bounds the policy knows are asked — the period's tokens and the day's cost —
// which is what keeps the two places from disagreeing about either.
func (c *Container) budget() taskdomain.Budget {
	if c == nil || c.Services == nil {
		return nil
	}
	checker, ok := c.Services.LLM.(llmdomain.QuotaChecker)
	if !ok {
		return nil
	}
	return quotaBudget{checker: checker}
}

// quotaBudget adapts the gateway's quota check to the task module's port.
type quotaBudget struct {
	checker llmdomain.QuotaChecker
}

// Allow implements taskdomain.Budget.
func (b quotaBudget) Allow(ctx context.Context, workspaceID uuid.UUID) error {
	return b.checker.Check(ctx, workspaceID)
}

// taskEventSink adapts the chat module's event store to the task module's port,
// so a task event reaches the feed and the chat thread through the same durable
// stream.
func (c *Container) taskEventSink() taskdomain.EventSink {
	if c == nil || c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}
	sink := chatEventSink{store: c.Repositories.Chat}
	if c.Redis != nil {
		sink.publisher = pubsub.New(c.Redis.Raw())
	}
	return sink
}

// taskReplyWriter adapts the conversation store to the task module's reply port,
// which is how a task's answer lands in the thread it came from.
func (c *Container) taskReplyWriter() taskdomain.ReplyWriter {
	if c == nil || c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}
	return taskReplyWriter{messages: c.Repositories.Chat, events: c.Repositories.Chat, sink: c.taskEventSink()}
}

// taskAttachmentReader reads a message's files for the task runtime.
//
// The chat service is resolved when a file is read rather than captured here,
// because the task runtime is assembled before the chat module: a task exists to
// answer a chat message, so the chat service needs the task runtime to dispatch
// one. A reader captured at assembly time is therefore nil in every process, and
// every file would be named instead of read — which is what issue #235 was.
func (c *Container) taskAttachmentReader() taskdomain.AttachmentReader {
	if c == nil {
		return nil
	}

	return taskAttachmentReader{container: c}
}

// errNoChatModule is what reading a file answers in a deployment that runs no
// conversation module: nothing owns the uploads, so nothing can fetch one.
var errNoChatModule = errors.New("chat: the conversation module is not configured")

// taskAttachmentReader adapts the chat service to the port the task module
// declares.
//
// The chat service already resolves an attachment inside its workspace scope and
// refuses one that is not there, so this is an adapter rather than a second
// implementation: a file is read the same way whichever path asked for it.
type taskAttachmentReader struct {
	container *Container
}

// ReadAttachment implements taskdomain.AttachmentReader.
func (r taskAttachmentReader) ReadAttachment(ctx context.Context, scope taskdomain.Scope, id uuid.UUID) (taskdomain.AttachmentContent, error) {
	if r.container == nil || r.container.Services == nil || r.container.Services.Chat == nil {
		return taskdomain.AttachmentContent{}, errNoChatModule
	}

	attachment, content, err := r.container.Services.Chat.AttachmentContent(ctx, chatScopeOf(scope), id)
	if err != nil {
		return taskdomain.AttachmentContent{}, err
	}

	return taskdomain.AttachmentContent{
		Filename:    attachment.Filename,
		ContentType: attachment.ContentType,
		Content:     content,
	}, nil
}

// ------------------------------------------------------------------- starter

// taskStarter starts and signals the workflow that runs a task.
//
// It lives in the container because the container is the only package allowed
// to know both the task module and the Temporal client: the module declares the
// port and never learns which engine answers it.
type taskStarter struct {
	client *temporal.Client
}

// Start opens the workflow for one task.
func (s taskStarter) Start(ctx context.Context, scope taskdomain.Scope, input taskdomain.StartInput) (taskdomain.StartResult, error) {
	if s.client == nil || s.client.Client() == nil {
		return taskdomain.StartResult{}, errors.New("task: temporal is not configured")
	}

	options := s.client.StartOptions(workflowID(input.TaskID))
	run, err := s.client.Client().ExecuteWorkflow(ctx, options, workflow.WorkflowName, workflow.TaskInput{
		TaskID:         input.TaskID.String(),
		WorkspaceID:    scope.WorkspaceID.String(),
		AgentID:        input.AgentID.String(),
		ConversationID: wireUUID(input.ConversationID),
		ReplyMessageID: wireUUID(input.ReplyMessageID),
		Title:          input.Title,
		Prompt:         input.Prompt,
		Depth:          input.Depth,
	})
	if err != nil {
		return taskdomain.StartResult{}, err
	}

	return taskdomain.StartResult{WorkflowID: run.GetID(), RunID: run.GetRunID()}, nil
}

// Signal delivers a decision to a running task. A workflow that already
// finished answers "not found", which is not an error: the decision arrived
// after the task stopped caring.
func (s taskStarter) Signal(ctx context.Context, _ taskdomain.Scope, taskID uuid.UUID, name string, payload any) error {
	if s.client == nil || s.client.Client() == nil {
		return errors.New("task: temporal is not configured")
	}

	return s.client.Client().SignalWorkflow(ctx, workflowID(taskID), "", name, payload)
}

// workflowID is the deterministic id of one task's workflow, which is what makes
// a restart resume the task instead of opening a second one.
func workflowID(taskID uuid.UUID) string { return "task-" + taskID.String() }

func wireUUID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// ------------------------------------------------------------------ adapters

// taskAgentDirectory adapts the agent registry to the task module's ports.
type taskAgentDirectory struct {
	service agentdomain.Service
}

// Agent implements taskdomain.AgentDirectory.
func (d taskAgentDirectory) Agent(ctx context.Context, scope taskdomain.Scope, id uuid.UUID) (taskdomain.AgentRef, error) {
	agent, err := d.service.Get(ctx, agentScopeOf(scope), id)
	if err != nil {
		return taskdomain.AgentRef{}, err
	}
	return toTaskAgentRef(agent), nil
}

// Active implements taskdomain.AgentDirectory.
func (d taskAgentDirectory) Active(ref taskdomain.AgentRef) bool {
	return ref.Active()
}

// ByName implements taskdomain.AgentNamer. The lookup is by name because that is
// what the model knows: it is told the roster, not the ids.
func (d taskAgentDirectory) ByName(ctx context.Context, scope taskdomain.Scope, name string) (taskdomain.AgentRef, error) {
	agents, err := d.service.List(ctx, agentScopeOf(scope))
	if err != nil {
		return taskdomain.AgentRef{}, err
	}

	wanted := strings.ToLower(strings.TrimSpace(name))
	for _, agent := range agents {
		if strings.ToLower(agent.Name) == wanted {
			return toTaskAgentRef(agent), nil
		}
	}
	return taskdomain.AgentRef{}, agentdomain.ErrAgentNotFound
}

func agentScopeOf(scope taskdomain.Scope) agentdomain.Scope {
	return agentdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

func toTaskAgentRef(agent agentdomain.Agent) taskdomain.AgentRef {
	return taskdomain.AgentRef{
		ID:           agent.ID,
		Name:         agent.Name,
		Role:         agent.Role,
		Persona:      agent.Persona,
		Tone:         agent.Tone,
		Stored:       agent.Status,
		Display:      agent.Display,
		Tools:        agent.Tools,
		DefaultModel: agent.DefaultModel,
	}
}

// toolRegistry reports the tools a Bolu was granted.
//
// Two filters apply and both are real: the grant (which integrations the Bolu was given) and an
// executor (something that can run the tool). The grant filter is the registry's own query — the
// workspace's catalogue and the Bolu's profile, joined against the grants. The executor filter is that
// a tool the hub cannot run is not offered: a catalogue row without an adapter is a migration that
// outran its code, and offering it teaches the model to call something that fails.
//
// The schema comes from the adapter that runs the tool, which is what makes the arguments a model sends
// the arguments the tool expects. The catalogue carries the name, the label, and the description —
// what a screen shows — and the adapter carries the schema, because a schema is code.
type toolRegistry struct {
	service agentdomain.Service
	// schemas is the hub's registry, resolved lazily: the runtime is assembled after the hub, and a
	// deployment with no hub offers the catalogue rows with an open schema rather than refusing them.
	container *Container
}

// Tools implements taskdomain.ToolRegistry.
func (r toolRegistry) Tools(ctx context.Context, scope taskdomain.Scope, agentID uuid.UUID) ([]taskdomain.Tool, error) {
	granted, err := r.service.AllowedTools(ctx, agentScopeOf(scope), agentID)
	if err != nil {
		return nil, err
	}

	tools := make([]taskdomain.Tool, 0, len(granted))
	for _, tool := range granted {
		schema, runnable := r.schemaOf(tool.Name)
		if !runnable {
			// Nothing in this deployment can run it: the catalogue knows a tool no adapter claims.
			// Offering it would be teaching the model to call something that cannot work.
			continue
		}

		tools = append(tools, taskdomain.Tool{
			Name:        tool.Name,
			Integration: tool.Integration,
			Description: tool.Description,
			Schema:      schema,
			Label:       tool.Label,
		})
	}

	return tools, nil
}

// schemaOf answers the schema an adapter declares for one tool.
//
// The second answer is "something can run this", which is the filter above rather than a detail: a
// catalogue row with no adapter behind it is offered to nobody, and a deployment with no hub at all
// offers its rows with an open schema instead of hiding them — the API and the worker assemble
// separately, and a worker without a hub is a deployment that cannot run tools, which its own executor
// says when a call arrives.
func (r toolRegistry) schemaOf(name string) (json.RawMessage, bool) {
	if r.container != nil && r.container.Services != nil && r.container.Services.Integration != nil {
		spec, _, ok := r.container.Services.Integration.Registry().Lookup(name)
		if !ok {
			return nil, false
		}

		return spec.Schema, true
	}

	return openSchema, true
}

// openSchema accepts any argument object. A tool with no adapter to ask is offered with it, which is
// permissive rather than wrong: the hub validates the arguments against the real schema when the call
// is made, so a wrong argument is refused either way.
var openSchema = json.RawMessage(`{"type":"object","additionalProperties":true}`)

// chatEventSink writes a task event to the durable stream the whole product
// reads, and publishes it to whoever is connected.
type chatEventSink struct {
	store     chatdomain.EventStore
	publisher chatdomain.EventPublisher
}

// Emit implements taskdomain.EventSink.
//
// The durable row is written first: the live publish is the fast path, and a
// publish that fails costs latency and never the event.
func (s chatEventSink) Emit(ctx context.Context, event taskdomain.Event) error {
	stored, err := s.store.AppendEvent(ctx, chatdomain.Event{
		WorkspaceID:    event.WorkspaceID,
		Type:           event.Type,
		ActorAgentID:   event.ActorAgentID,
		ActorUserID:    event.ActorUserID,
		ConversationID: event.ConversationID,
		TaskID:         event.TaskID,
		DraftID:        event.DraftID,
		Payload:        event.Payload,
	})
	if err != nil {
		return err
	}

	if s.publisher == nil {
		return nil
	}
	return s.publisher.Publish(ctx, stored.WorkspaceID, stored)
}

// taskReplyWriter reads the thread a task answers and writes the answer back
// into it.
type taskReplyWriter struct {
	messages chatdomain.MessageRepository
	events   chatdomain.EventStore
	sink     taskdomain.EventSink
}

// History implements taskdomain.ReplyWriter.
func (w taskReplyWriter) History(ctx context.Context, scope taskdomain.Scope, conversationID uuid.UUID, limit int) ([]taskdomain.Message, error) {
	stored, err := w.messages.LatestMessages(ctx, chatScopeOf(scope), conversationID, limit)
	if err != nil {
		return nil, err
	}

	messages := make([]taskdomain.Message, 0, len(stored))
	for _, message := range stored {
		messages = append(messages, toTaskMessage(message))
	}
	return messages, nil
}

// Write implements taskdomain.ReplyWriter: it fills the placeholder the chat
// module wrote, so the answer appears where the question was asked.
func (w taskReplyWriter) Write(ctx context.Context, scope taskdomain.Scope, write taskdomain.WriteReply) error {
	if write.MessageID == uuid.Nil {
		return nil
	}

	message, err := w.messages.GetMessage(ctx, chatScopeOf(scope), write.MessageID)
	if err != nil {
		return err
	}

	message.Blocks = toChatBlocks(write.Blocks)
	message.Status = write.Status
	message.FinishReason = write.FinishReason
	message.TaskID = write.TaskID

	if _, err := w.messages.Update(ctx, chatScopeOf(scope), message); err != nil {
		return err
	}

	if w.sink == nil {
		return nil
	}

	// The stream learns the answer grew, which is what a connected client
	// renders as the Bolu finishing its message. The payload is the same wire
	// shape the message endpoint returns, so the two cannot drift.
	payload, err := json.Marshal(chatdomain.WireMessage(message))
	if err != nil {
		//nolint:nilerr // the answer is already stored; a payload that will not encode costs the live update, not the answer
		return nil
	}

	return w.sink.Emit(ctx, taskdomain.Event{
		WorkspaceID:    message.WorkspaceID,
		Type:           chatdomain.EventMessageUpdated,
		ActorAgentID:   message.AuthorAgentID,
		ConversationID: message.ConversationID,
		TaskID:         message.TaskID,
		Payload:        payload,
	})
}

func chatScopeOf(scope taskdomain.Scope) chatdomain.Scope {
	return chatdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

func toTaskMessage(message chatdomain.Message) taskdomain.Message {
	blocks := make([]taskdomain.Block, 0, len(message.Blocks))
	for _, block := range message.Blocks {
		blocks = append(blocks, taskdomain.Block{Type: block.Type, Body: block.Body})
	}

	// Only the reference travels: the activity reads the bytes when it renders
	// the prompt, so a workflow's state never holds a file.
	attachments := make([]taskdomain.AttachmentRef, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		attachments = append(attachments, taskdomain.AttachmentRef{
			ID:          attachment.ID,
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			ByteSize:    attachment.ByteSize,
		})
	}

	return taskdomain.Message{
		ID:             message.ID,
		ConversationID: message.ConversationID,
		AgentID:        message.AuthorAgentID,
		UserID:         message.AuthorUserID,
		Blocks:         blocks,
		Attachments:    attachments,
	}
}

func toChatBlocks(blocks []taskdomain.Block) []chatdomain.Block {
	out := make([]chatdomain.Block, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, chatdomain.Block{Type: block.Type, Body: block.Body})
	}
	return out
}

// -------------------------------------------------------------- chat → task

// chatTaskStarter adapts the task runtime to the port the chat module declares,
// which is what turns a chat message into a durable task.
type chatTaskStarter struct {
	service taskdomain.Service
}

// Start implements chatdomain.TaskStarter.
func (s chatTaskStarter) Start(ctx context.Context, scope chatdomain.Scope, req chatdomain.StartRequest) (uuid.UUID, error) {
	task, err := s.service.Dispatch(ctx, taskdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, taskdomain.DispatchRequest{
		AgentID:        req.AgentID,
		ConversationID: req.ConversationID,
		ReplyMessageID: req.MessageID,
		Trigger:        req.Trigger,
		Title:          req.Title,
		Prompt:         req.Title,
	})
	if err != nil {
		// The container is where the two modules meet, so it is where one module's
		// refusal becomes the other's. A workspace that cannot spend is a message the
		// chat module does not know about, and a screen needs a sentence it can act on
		// rather than an internal failure.
		if reason, refused := budgetRefusal(err); refused {
			return uuid.Nil, fmt.Errorf("%w: %s", chatdomain.ErrNoBudget, reason)
		}
		return uuid.Nil, err
	}
	return task.ID, nil
}

// budgetRefusal reports whether an error is the workspace's package refusing to pay, and
// the sentence a person should read.
//
// Both halves of the package are here: an allowance that is spent, and a subscription
// past its grace window. They are different instructions to a person — buy a top-up, or
// pay the invoice — and both mean the work cannot be opened.
func budgetRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, billingdomain.ErrQuotaExceeded):
		return userSentence(err, billingdomain.ErrQuotaExceeded), true
	case errors.Is(err, billingdomain.ErrReadOnly):
		return userSentence(err, billingdomain.ErrReadOnly), true
	default:
		return "", false
	}
}

// userSentence is what a person should read, taken out of an error's own words.
//
// The sentinel is searched for rather than only trimmed from the front, because by the
// time the refusal reaches here it carries the wrappers of every layer it passed through
// — the model gateway's own check wraps it before the task runtime sees it — and what a
// person reads should be the sentence the package wrote rather than the list of packages
// that noticed.
func userSentence(err error, prefix error) string {
	text := err.Error()
	if index := strings.Index(text, prefix.Error()); index >= 0 {
		text = text[index+len(prefix.Error()):]
	}

	message := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), ":"))
	if message == "" {
		return prefix.Error()
	}
	return message
}

// Compile-time checks that every adapter satisfies the port it is wired to.
var (
	_ taskdomain.AgentDirectory = taskAgentDirectory{}
	_ taskdomain.AgentNamer     = taskAgentDirectory{}
	_ taskdomain.ToolRegistry   = toolRegistry{}
	_ taskdomain.EventSink      = chatEventSink{}
	_ taskdomain.ReplyWriter    = taskReplyWriter{}
	_ taskdomain.Starter        = taskStarter{}
	_ chatdomain.TaskStarter    = chatTaskStarter{}
	_ taskdomain.Budget         = quotaBudget{}
)
