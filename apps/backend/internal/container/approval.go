package container

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	approvaldomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/domain"
	approvalrepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/repository"
	approvalservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/approval/service"
	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	taskdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	workspacerepo "github.com/anakbolu/timbolu/apps/backend/internal/modules/workspace/repository"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/pubsub"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/temporal"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// openApproval builds the approval runtime: the draft gate, the policy screen,
// and the audit trail.
//
// It is assembled before the task runtime because the task runtime asks it
// whether an action may leave, which is the product's central guardrail: without
// this, a Bolu that decides to send an email has nothing stopping it.
func (c *Container) openApproval(_ context.Context, _ *config.Config, o *options) error {
	if c.Repositories == nil || c.Repositories.Approval == nil {
		c.Logger.Warn("the approval gate is disabled: no database connection")
		return nil
	}

	c.Services.Approval = approvalservice.New(approvalservice.Deps{
		Drafts: c.Repositories.Approval,
		Sender: c.approvalSender(),
		Cards:  c.approvalCards(),
		Events: c.approvalEventSink(),
		Hours:  workspaceHours{workspace: c.Repositories.Workspace},
		Logger: c.Logger,
		// Only a test overrides this. The working-hours rule reads the clock, so a
		// test that drives an approval through has to pin it, or the same test passes
		// during Jakarta's working day and fails after 21:00.
		Clock:   o.clock,
		Metrics: c.metricsFor(),
	})

	// The approval repository is handed to the workspace repository as its second
	// provisioner, so a new workspace never exists without a policy for every
	// action that reaches outside it.
	if c.Repositories.Workspace != nil {
		c.Repositories.Workspace.SetPolicyProvisioner(c.Repositories.Approval)
	}

	return nil
}

// approvalSender carries an approved action out. It lives in executor.go, beside
// the integration hub it asks to perform the action.

// approvalCards writes the draft card into the conversation a task came from.
func (c *Container) approvalCards() approvaldomain.Cards {
	if c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}
	return approvalCards{messages: c.Repositories.Chat, sink: c.approvalEventSink()}
}

// approvalEventSink adapts the chat module's event store to the approval port, so
// a decision reaches the dashboard and the chat card through the same durable
// stream everything else uses.
func (c *Container) approvalEventSink() approvaldomain.EventSink {
	if c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}
	sink := chatEventSink{store: c.Repositories.Chat}
	if c.Redis != nil {
		sink.publisher = pubsub.New(c.Redis.Raw())
	}
	return approvalEventSink{sink: sink}
}

// approvalGate adapts the approval service to the port the task runtime declares.
//
// The task module never learns which module decides: it proposes an action and
// this answers whether a person is needed.
type approvalGate struct {
	service approvaldomain.Service
}

// RequestDraft implements taskdomain.DraftGate.
func (g approvalGate) RequestDraft(ctx context.Context, scope taskdomain.Scope, request taskdomain.DraftRequest) (taskdomain.Draft, error) {
	outcome, err := g.service.Request(ctx, approvaldomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, approvaldomain.Request{
		TaskID:         request.TaskID,
		AgentID:        request.AgentID,
		ConversationID: request.ConversationID,
		ActionKind:     request.ActionKind,
		Title:          request.Title,
		Summary:        request.Title,
		Payload:        request.Payload,
	})
	if err != nil {
		return taskdomain.Draft{}, err
	}

	// A policy that allowed the action unattended is reported as an auto draft
	// that already ran, so the workflow records what happened instead of parking
	// on a signal nobody will send.
	return taskdomain.Draft{
		ID:      outcome.DraftID,
		Title:   outcome.Title,
		Status:  outcome.Status,
		Note:    outcome.Reason,
		Auto:    outcome.Executed,
		Content: outcome.Content,
	}, nil
}

// CarryOut implements taskdomain.DraftGate.
func (g approvalGate) CarryOut(ctx context.Context, scope taskdomain.Scope, draftID uuid.UUID) (taskdomain.DraftExecution, error) {
	outcome, err := g.service.CarryOut(ctx, approvaldomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, draftID)
	if err != nil {
		// The approval module's errors are translated into the task module's own:
		// the runtime has to be able to tell a permanent state from a hiccup
		// without importing the module that decided it.
		switch {
		case errors.Is(err, approvaldomain.ErrNoSender):
			return taskdomain.DraftExecution{}, taskdomain.ErrDraftNoSender
		case errors.Is(err, approvaldomain.ErrNotApproved):
			return taskdomain.DraftExecution{}, taskdomain.ErrDraftNotApproved
		}
		return taskdomain.DraftExecution{}, err
	}

	held := outcome.NotBefore != nil && !outcome.Executed
	return taskdomain.DraftExecution{
		Status:    outcome.Status,
		Content:   outcome.Content,
		Held:      held,
		NotBefore: outcome.NotBefore,
		Reason:    outcome.Reason,
	}, nil
}

// approvalCards puts the card in the thread and rewrites it after a decision.
type approvalCards struct {
	messages chatdomain.MessageRepository
	sink     approvaldomain.EventSink
}

// Write implements approvaldomain.Cards.
//
// The card is its own message, so a decision rewrites one message rather than
// editing the task's answer, and a reload shows what happened.
func (c approvalCards) Write(ctx context.Context, scope approvaldomain.Scope, draft approvaldomain.Draft) (uuid.UUID, error) {
	if draft.ConversationID == uuid.Nil {
		return uuid.Nil, nil
	}

	message, err := c.messages.Append(ctx, chatScopeFor(scope), chatdomain.Message{
		ConversationID: draft.ConversationID,
		AuthorAgentID:  draft.AgentID,
		Blocks:         []chatdomain.Block{draftCardBlock(draft)},
		TaskID:         draft.TaskID,
		Status:         chatdomain.MessageComplete,
	})
	if err != nil {
		return uuid.Nil, err
	}

	c.emit(ctx, message, draft)
	return message.ID, nil
}

// Rewrite implements approvaldomain.Cards.
func (c approvalCards) Rewrite(ctx context.Context, scope approvaldomain.Scope, messageID uuid.UUID, draft approvaldomain.Draft) error {
	stored, err := c.messages.GetMessage(ctx, chatScopeFor(scope), messageID)
	if err != nil {
		return err
	}

	stored.Blocks = []chatdomain.Block{draftCardBlock(draft)}
	if _, err := c.messages.Update(ctx, chatScopeFor(scope), stored); err != nil {
		return err
	}

	c.emit(ctx, stored, draft)
	return nil
}

// emit publishes the card's new state, which is how it changes without a reload.
func (c approvalCards) emit(ctx context.Context, message chatdomain.Message, draft approvaldomain.Draft) {
	if c.sink == nil {
		return
	}

	payload, err := json.Marshal(chatdomain.WireMessage(message))
	if err != nil {
		//nolint:nilerr // the card is already stored; a payload that will not encode costs the live update, not the card
		return
	}

	// The card is already stored, so a publish that fails costs the live update
	// and nothing else: the thread shows the new state on the next read.
	_ = c.sink.Emit(ctx, approvaldomain.Event{
		WorkspaceID:    message.WorkspaceID,
		Type:           chatdomain.EventMessageUpdated,
		ActorAgentID:   draft.AgentID,
		ConversationID: draft.ConversationID,
		TaskID:         draft.TaskID,
		DraftID:        draft.ID,
		Payload:        payload,
	})
}

// chatScopeFor renders the approval module's scope as the chat module's, which
// is a different type for the same two ids.
func chatScopeFor(scope approvaldomain.Scope) chatdomain.Scope {
	return chatdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

// wireDraftTime renders a moment, or the empty string when there is none.
func wireDraftTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

// truncateRunes bounds one field of the card, so a long body does not make the
// message unreadable in the thread.
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

// draftCardBlock renders the card as a chat block.
//
// The block carries the fields the card shows and the status it is in, which is
// what makes the same message render as a question before a decision and as a
// record after one.
func draftCardBlock(draft approvaldomain.Draft) chatdomain.Block {
	fields := make([][]string, 0, 4)
	for _, key := range []string{"to", "subject", "body", "amount", "message"} {
		var payload map[string]any
		if err := json.Unmarshal(draft.Payload, &payload); err != nil {
			break
		}
		value, ok := payload[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			fields = append(fields, []string{key, truncateRunes(text, 200)})
		}
	}

	body, err := json.Marshal(map[string]any{
		"type":        "draft",
		"draft_id":    draft.ID.String(),
		"action_kind": draft.ActionKind,
		"title":       draft.Title,
		"summary":     draft.Summary,
		"status":      draft.Status,
		"revision":    draft.Revision,
		"fields":      fields,
		// A held action says why and until when, so the card answers "why has
		// this not gone out" without opening the dashboard.
		"blocked_reason": draft.BlockedReason,
		"not_before":     wireDraftTime(draft.NotBefore),
	})
	if err != nil {
		body = []byte(`{"type":"draft"}`)
	}

	return chatdomain.Block{Type: chatdomain.BlockDraft, Body: body}
}

// approvalEventSink adapts the chat event sink to the approval port.
type approvalEventSink struct {
	sink chatEventSink
}

// Emit implements approvaldomain.EventSink.
func (s approvalEventSink) Emit(ctx context.Context, event approvaldomain.Event) error {
	return s.sink.Emit(ctx, taskdomain.Event{
		WorkspaceID:    event.WorkspaceID,
		Type:           event.Type,
		ActorAgentID:   event.ActorAgentID,
		ActorUserID:    event.ActorUserID,
		ConversationID: event.ConversationID,
		TaskID:         event.TaskID,
		DraftID:        event.DraftID,
		Payload:        event.Payload,
	})
}

// workspaceHours reads the window a workspace lets actions leave in.
type workspaceHours struct {
	workspace *workspacerepo.Repository
}

// WorkingHours implements the approval service's reader.
func (h workspaceHours) WorkingHours(ctx context.Context, workspaceID uuid.UUID) (approvaldomain.WorkingHours, error) {
	if h.workspace == nil {
		return approvaldomain.WorkingHours{}, errors.New("approval: the workspace repository is not configured")
	}

	window, err := h.workspace.WorkingHours(ctx, workspaceID)
	if err != nil {
		return approvaldomain.WorkingHours{}, err
	}

	return approvaldomain.WorkingHours{
		Start:    window.Start,
		End:      window.End,
		Timezone: window.Timezone,
	}, nil
}

// approvalSignaler hands a decision to the running workflow.
//
// It lives in the container because the container is the only package allowed to
// know both the approval module and the Temporal client.
type approvalSignaler struct {
	client *temporal.Client
}

// SignalApproval implements the handler's Signaler.
func (s approvalSignaler) SignalApproval(ctx context.Context, scope approvaldomain.Scope, taskID, draftID uuid.UUID, approved bool, note string) error {
	if s.client == nil || s.client.Client() == nil {
		return errors.New("approval: temporal is not configured")
	}

	// The payload mirrors the workflow's own signal type. It is built here rather
	// than imported because the handler must not depend on the workflow package.
	payload := map[string]any{
		"DraftID":  draftID.String(),
		"Approved": approved,
		"Note":     note,
	}

	return s.client.Client().SignalWorkflow(ctx, workflowID(taskID), "", signalApprovalName, payload)
}

// signalApprovalName is the workflow signal a decision travels on. It is named
// here so the container and the workflow cannot drift apart silently.
const signalApprovalName = "approval"

// Compile-time checks that every adapter satisfies the port it is wired to.
var (
	_ taskdomain.DraftGate      = approvalGate{}
	_ approvaldomain.Cards      = approvalCards{}
	_ approvaldomain.EventSink  = approvalEventSink{}
	_ approvaldomain.Repository = (*approvalrepo.Repository)(nil)
	_ approvaldomain.Service    = (*approvalservice.Service)(nil)
)
