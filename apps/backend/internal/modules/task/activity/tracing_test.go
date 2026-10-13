package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/workflow"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/testsuite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The task runtime's spans, asserted where they are started: one per model round and one per tool
// call, under the request's trace. The other half of what these tests check is what a span must
// *not* carry — a tool's arguments and a prompt are the customer's words, and a trace is a store the
// product's retention policy does not reach.

// recordingTracer gives a harness a tracer whose spans can be read back.
func recordingTracer(t *testing.T) (*tracetest.InMemoryExporter, trace.Tracer) {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	return exporter, provider.Tracer("bolu")
}

// withTracing returns the harness mutation that installs the tracer.
func withTracing(tracer trace.Tracer) func(*Deps) {
	return func(deps *Deps) { deps.Tracer = tracer }
}

// attributesOf reads one span's attributes into a map, so a test can ask for one by name.
func attributesOf(t *testing.T, span tracetest.SpanStub) map[string]string {
	t.Helper()

	attributes := map[string]string{}
	for _, attribute := range span.Attributes {
		attributes[string(attribute.Key)] = attribute.Value.String()
	}

	return attributes
}

// TestAToolCallIsASpanNamedForTheTool: the question a trace answers about tools is "which one was
// slow", so the span is named for the tool, and it carries the task and the Bolu it belonged to.
func TestAToolCallIsASpanNamedForTheTool(t *testing.T) {
	exporter, tracer := recordingTracer(t)

	h := newHarness(t, domain.DefaultLimits, withTracing(tracer))
	h.executor.content = json.RawMessage(`{"messages":[{"text":"pesanan pelanggan"}]}`)

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		CallID:      "call-1",
		Name:        "whatsapp.list_messages",
		Arguments:   json.RawMessage(`{"chat":"Penjualan","secret":"nomor pelanggan 08123"}`),
	})
	require.NoError(t, err)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)

	span := spans[0]
	require.Equal(t, "tool whatsapp.list_messages", span.Name)

	attributes := attributesOf(t, span)
	require.Equal(t, h.task.ID.String(), attributes["task_id"])
	require.Equal(t, h.scope.WorkspaceID.String(), attributes["workspace_id"])
	require.Equal(t, h.task.AgentID.String(), attributes["agent_id"])
	require.Equal(t, "whatsapp.list_messages", attributes["tool_name"])
	require.Equal(t, "false", attributes["is_error"])

	// And the arguments and the result are nowhere on it.
	for key, value := range attributes {
		require.NotContains(t, value, "nomor pelanggan", "attribute %s carries a tool argument", key)
		require.NotContains(t, value, "pesanan pelanggan", "attribute %s carries a tool result", key)
	}
}

// TestAToolTheDeploymentCannotServeIsStillASpan: an activity that ran and refused is what an
// incident is looking for. The span is opened before the checks rather than after them precisely so
// a refusal is not an unexplained gap in the trace — which is what the smoke run showed when the
// span opened after the executor check and the whole tool call left no trace behind.
func TestAToolTheDeploymentCannotServeIsStillASpan(t *testing.T) {
	exporter, tracer := recordingTracer(t)

	h := newHarness(t, domain.DefaultLimits, withTracing(tracer))
	h.activities.deps.Executor = nil

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.search",
		Arguments:   json.RawMessage(`{"q":"pesanan"}`),
	})
	require.ErrorIs(t, err, domain.ErrToolNotAvailable)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1, "the refusal is a span, not a gap")
	require.Equal(t, "tool gmail.search", spans[0].Name)
	require.Equal(t, codes.Error, spans[0].Status.Code)
	require.NotEmpty(t, spans[0].Events, "why it was refused is recorded")
	require.Equal(t, h.task.ID.String(), attributesOf(t, spans[0])["task_id"])
}

// TestAToolThatReportsAnErrorDoesNotFailTheRuntime: the model is told what went wrong and decides
// what to do, so the span records the fact without being marked failed — a distinction that matters
// when somebody reads a trace to find out whether the deployment or the tool was at fault.
func TestAToolThatReportsAnErrorDoesNotFailTheRuntime(t *testing.T) {
	exporter, tracer := recordingTracer(t)

	h := newHarness(t, domain.DefaultLimits, withTracing(tracer))
	h.executor.content = json.RawMessage(`{"error":"nomor tidak terdaftar"}`)
	h.executor.err = nil

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "whatsapp.send",
		Arguments:   json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "false", attributesOf(t, spans[0])["is_error"],
		"a result the tool returned is not an error it raised")
}

// TestAModelRoundIsASpanThatNamesTheModelThatAnswered: the provider and the model are the answering
// ones rather than the configured ones, which is what makes a fallback visible in a trace — and the
// prompt is not on it.
func TestAModelRoundIsASpanThatNamesTheModelThatAnswered(t *testing.T) {
	exporter, tracer := recordingTracer(t)

	h := newHarness(t, domain.DefaultLimits, func(deps *Deps) {
		deps.Tracer = tracer
		deps.Gateway = capturingGateway{onChat: func(llmdomain.Scope, llmdomain.ChatRequest) (llmdomain.ChatResponse, error) {
			return llmdomain.ChatResponse{
				Text:     "Ada 2 pesanan.",
				Usage:    llmdomain.Usage{InputTokens: 120, OutputTokens: 30},
				Model:    "gpt-4o-mini",
				Provider: "openai",
				// A tool call, so the count is asserted as well as the text.
				ToolCalls:    []llmdomain.ToolCall{{ID: "call-1", Name: "gmail.search"}},
				FinishReason: llmdomain.FinishToolCalls,
			}, nil
		}}
	})

	_, err := h.activities.CallModel(context.Background(), workflow.ModelRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		AgentID:     h.task.AgentID.String(),
		System:      "Kamu Oren, penjual di Toko Sinar.",
		Messages: []domain.Turn{
			{Role: llmdomain.RoleUser, Text: "Rekap pesanan hari ini"},
		},
	})
	require.NoError(t, err)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "task.model_round", spans[0].Name)

	attributes := attributesOf(t, spans[0])
	require.Equal(t, h.task.ID.String(), attributes["task_id"])
	require.Equal(t, "openai", attributes["provider"])
	require.Equal(t, "gpt-4o-mini", attributes["model"])
	require.Equal(t, "120", attributes["input_tokens"])
	require.Equal(t, "30", attributes["output_tokens"])
	require.Equal(t, "1", attributes["tool_calls"])
	require.Equal(t, "1", attributes["history_messages"])

	for key, value := range attributes {
		require.NotContains(t, value, "Rekap pesanan hari ini", "attribute %s carries the prompt", key)
		require.NotContains(t, value, "Kamu Oren", "attribute %s carries the system text", key)
	}
}

// TestANestedStepIsPartOfTheContextsTrace: the activity's own span is the parent, so a task's rounds
// and tools hang under the workflow step rather than starting traces of their own.
func TestANestedStepIsPartOfTheContextsTrace(t *testing.T) {
	exporter, tracer := recordingTracer(t)

	h := newHarness(t, domain.DefaultLimits, withTracing(tracer))
	h.executor.content = json.RawMessage(`{"ok":true}`)

	ctx, parent := tracer.Start(context.Background(), "activity:RunTool")
	defer parent.End()

	_, err := h.activities.RunTool(ctx, workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.search",
		Arguments:   json.RawMessage(`{"q":"pesanan"}`),
	})
	require.NoError(t, err)

	// Only the child has finished, so it is the one the exporter holds: the parent is still
	// open, which is exactly the state the runtime leaves it in.
	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "tool gmail.search", spans[0].Name)

	require.Equal(t, parent.SpanContext().TraceID(), spans[0].SpanContext.TraceID())
	require.Equal(t, parent.SpanContext().SpanID(), spans[0].Parent.SpanID(),
		"the workflow step is the parent of the tool call")
}

// TestATaskWithoutATracerStillRuns: a deployment with telemetry off must not have a task that fails
// because its spans have nowhere to go.
func TestATaskWithoutATracerStillRuns(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)
	h.executor.content = json.RawMessage(`{"ok":true}`)

	_, err := h.activities.RunTool(context.Background(), workflow.ToolRequest{
		TaskID:      h.task.ID.String(),
		WorkspaceID: h.scope.WorkspaceID.String(),
		Name:        "gmail.search",
		Arguments:   json.RawMessage(`{"q":"pesanan"}`),
	})
	require.NoError(t, err)
	require.Len(t, h.executor.calls, 1)
}

// TestTheTaskLoggerCarriesTheFourFields: workspace, agent, task, and workflow are what a log search
// needs, and a line missing one of them cannot be found by it. The workflow id is read from the
// activity's own context rather than passed in, so the test runs the activity through Temporal's own
// environment — the path that produces it — instead of asserting a value handed to the logger.
func TestTheTaskLoggerCarriesTheFourFields(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	h.activities.deps.Logger = zap.New(zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.InfoLevel))

	// One function, registered and executed — the environment looks an activity up by the type
	// name it derives from the function, so a second closure would be an unregistered one.
	logThroughTaskLogger := func(ctx context.Context) error {
		h.activities.taskLogger(ctx, h.task).Warn("something happened")

		return nil
	}

	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(logThroughTaskLogger)

	_, err := env.ExecuteActivity(logThroughTaskLogger)
	require.NoError(t, err)

	var line map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line))

	require.Equal(t, h.scope.WorkspaceID.String(), line["workspace_id"])
	require.Equal(t, h.task.AgentID.String(), line["agent_id"])
	require.Equal(t, h.task.ID.String(), line["task_id"])
	require.NotEmpty(t, line["workflow_id"], "the workflow id comes from the activity context")
}

// TestAStepOutsideAnActivityLogsWithoutAWorkflowID: the guard that keeps a unit test — or any code
// that calls an activity as a plain function — from asking Temporal for an execution it does not
// have, which would panic rather than log.
func TestAStepOutsideAnActivityLogsWithoutAWorkflowID(t *testing.T) {
	h := newHarness(t, domain.DefaultLimits)

	require.Empty(t, workflowIDOf(context.Background()))
	require.NotPanics(t, func() { h.activities.taskLogger(context.Background(), h.task) })
}
