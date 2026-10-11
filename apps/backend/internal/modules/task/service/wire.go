package service

import (
	"encoding/json"
	"time"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// Payload is the wire shape of a task event, and of the task endpoints.
//
// It is built in one place so a task read from the history endpoint and the same
// task arriving on the stream describe themselves identically — the lesson the
// chat module learned when a user's message published a zero agent id.
type PayloadShape struct {
	TaskID         string `json:"task_id"`
	AgentID        string `json:"agent_id"`
	ParentTaskID   string `json:"parent_task_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Trigger        string `json:"trigger"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	// Health is derived from the heartbeat: running, waiting, stuck, or done.
	Health string `json:"health"`
	// Depth is how far down a handoff chain the task is.
	Depth int `json:"depth"`
	// Summary is what the task did, in one line; it survives the raw steps.
	Summary string `json:"summary,omitempty"`
	// StoppedReason explains a task that stopped without finishing.
	StoppedReason string `json:"stopped_reason,omitempty"`
	// WaitingReason is what a parked task is waiting for.
	WaitingReason string `json:"waiting_reason,omitempty"`
	// WaitingDraftID links a parked task to the approval it waits on.
	WaitingDraftID string `json:"waiting_draft_id,omitempty"`
	WorkflowID     string `json:"workflow_id,omitempty"`
	StepCount      int    `json:"step_count"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	CostMicros     int64  `json:"cost_micros"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	HeartbeatAt    string `json:"heartbeat_at,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// Payload renders one task for a client.
func Payload(task domain.Task, now time.Time) PayloadShape {
	return PayloadShape{
		TaskID:         task.ID.String(),
		AgentID:        domain.WireID(task.AgentID),
		ParentTaskID:   domain.WireID(task.ParentTaskID),
		ConversationID: domain.WireID(task.ConversationID),
		Trigger:        task.Trigger,
		Title:          task.Title,
		Status:         task.Status,
		Health:         string(task.Health(now)),
		Depth:          task.Depth,
		Summary:        task.Summary,
		StoppedReason:  task.StoppedReason,
		WaitingReason:  task.WaitingReason,
		WaitingDraftID: domain.WireID(task.WaitingDraftID),
		WorkflowID:     task.WorkflowID,
		StepCount:      task.StepCount,
		InputTokens:    task.InputTokens,
		OutputTokens:   task.OutputTokens,
		CostMicros:     task.CostMicros,
		StartedAt:      wireTime(task.StartedAt),
		FinishedAt:     wireTime(task.FinishedAt),
		HeartbeatAt:    wireTime(task.HeartbeatAt),
		CreatedAt:      task.CreatedAt.Format(time.RFC3339Nano),
	}
}

// StepPayload is the wire shape of one recorded round.
type StepPayload struct {
	StepID    string          `json:"step_id"`
	Seq       int             `json:"seq"`
	Kind      string          `json:"kind"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolLabel string          `json:"tool_label,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Pruned    bool            `json:"pruned"`
	CreatedAt string          `json:"created_at"`
	TokensIn  int             `json:"input_tokens"`
	TokensOut int             `json:"output_tokens"`
}

// StepWire renders one step for a client. A pruned step keeps its shape and
// loses its contents, which is what makes the history readable after the
// retention job runs.
func StepWire(step domain.Step) StepPayload {
	payload := StepPayload{
		StepID:    step.ID.String(),
		Seq:       step.Seq,
		Kind:      step.Kind,
		ToolName:  step.ToolName,
		ToolLabel: step.ToolLabel,
		Pruned:    step.Pruned,
		CreatedAt: step.CreatedAt.Format(time.RFC3339Nano),
		TokensIn:  step.InputTokens,
		TokensOut: step.OutputTokens,
	}

	if !step.Pruned {
		payload.Input = step.Input
		payload.Output = step.Output
	}
	return payload
}

// taskEvent builds one event for the activity stream.
func taskEvent(scope domain.Scope, task domain.Task, eventType string, payload PayloadShape) domain.Event {
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte("{}")
	}

	return domain.Event{
		WorkspaceID:    scope.WorkspaceID,
		Type:           eventType,
		ActorAgentID:   task.AgentID,
		ConversationID: task.ConversationID,
		TaskID:         task.ID,
		Payload:        encoded,
	}
}

func wireTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}
