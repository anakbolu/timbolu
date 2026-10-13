package activity

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// The task runtime's spans and its log fields.
//
// The Temporal interceptor already gives every activity a span of its own, which is what connects
// the workflow's history to the code that ran it. What is added here is the semantics on top of
// that skeleton: which Bolu, which task, which tool, which model, and how much it cost — the
// questions somebody reading a trace of a slow or wrong answer actually asks.
//
// Two rules hold across all of it. **The identifiers and the counts travel; the content does not**:
// a tool's arguments and its result, a prompt, and an answer are the customer's words, and the same
// reason the audit trail keeps a metadata map rather than a payload applies here. And **the log
// fields are on every line a task writes**: workspace_id, agent_id, task_id, and workflow_id, so a
// line can be followed from the trace to the log and back.

// span opens the span for one step of a task.
func (a *Activities) span(ctx context.Context, name string, task domain.Task, extra ...attribute.KeyValue) (context.Context, trace.Span) {
	if a.deps.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}

	attributes := []attribute.KeyValue{
		attribute.String("workspace_id", task.WorkspaceID.String()),
		attribute.String("task_id", task.ID.String()),
		attribute.String("agent_id", task.AgentID.String()),
		attribute.Int("task_depth", task.Depth),
	}
	if workflowID := workflowIDOf(ctx); workflowID != "" {
		attributes = append(attributes, attribute.String("workflow_id", workflowID))
	}

	return a.deps.Tracer.Start(ctx, name, trace.WithAttributes(append(attributes, extra...)...))
}

// workflowIDOf reads the workflow execution this activity belongs to.
//
// It is read from Temporal rather than passed through the request: the workflow id is what a person
// quotes when they report a task that misbehaved, and the runtime already knows it — asking the
// caller to send it back would be a field that can be dropped.
func workflowIDOf(ctx context.Context) string {
	if !activity.IsActivity(ctx) {
		// A unit test calling the activity directly is not inside a workflow, and asking
		// Temporal for the execution would panic.
		return ""
	}

	return activity.GetInfo(ctx).WorkflowExecution.ID
}

// taskLogger is the logger a task's lines are written through.
//
// The four fields are always together and always in the same names, which is the point: a log
// search for one task's lines is one query, and a line that carries only some of them is a line
// that cannot be followed.
func (a *Activities) taskLogger(ctx context.Context, task domain.Task) *zap.Logger {
	if a.deps.Logger == nil {
		return zap.NewNop()
	}

	fields := []zap.Field{
		zap.String("workspace_id", task.WorkspaceID.String()),
		zap.String("agent_id", task.AgentID.String()),
		zap.String("task_id", task.ID.String()),
	}
	if workflowID := workflowIDOf(ctx); workflowID != "" {
		fields = append(fields, zap.String("workflow_id", workflowID))
	}

	return a.deps.Logger.With(fields...)
}

// toolSpan opens the span for one tool call.
//
// The tool's arguments are never attributes. A tool call is where a customer's data enters the
// run — an email body, a chat message, a file's contents — and a trace is a store the product's own
// retention does not reach.
func (a *Activities) toolSpan(ctx context.Context, task domain.Task, name string) (context.Context, trace.Span) {
	return a.span(ctx, "tool "+name, task, attribute.String("tool_name", name))
}

// end records the error, if any, and closes the span.
func end(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "the step failed")
	}
	span.End()
}
