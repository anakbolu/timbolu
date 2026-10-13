package temporal

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The adapter is what puts the SDK's own lines — and a workflow's `workflow.GetLogger` lines — into
// the deployment's log, in its format and with its fields. A workflow's error that lands somewhere
// else is an error no log search finds.

func newBufferLogger(t *testing.T, level zapcore.Level) (*zap.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())

	return zap.New(zapcore.NewCore(encoder, zapcore.AddSync(buf), level)), buf
}

// decode reads one written line.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	line := map[string]any{}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line))

	return line
}

// TestTheSDKsFieldsBecomeTheDeploymentsFields: the SDK logs key/value pairs, and a workflow's logger
// arrives already carrying the workflow id. Those must be fields on the line rather than text.
func TestTheSDKsFieldsBecomeTheDeploymentsFields(t *testing.T) {
	logger, buf := newBufferLogger(t, zapcore.InfoLevel)

	sdk := NewZapLogger(logger).With("WorkflowID", "task-42", "WorkflowType", "AgentTaskWorkflow")
	sdk.Info("the workflow started", "TaskQueue", "bolu-agent")

	line := decode(t, buf)
	require.Equal(t, "the workflow started", line["msg"])
	require.Equal(t, "task-42", line["WorkflowID"])
	require.Equal(t, "AgentTaskWorkflow", line["WorkflowType"])
	require.Equal(t, "bolu-agent", line["TaskQueue"])
}

// TestAChildLoggerDoesNotChangeItsParent: `With` must return a new logger, because the SDK derives a
// workflow's logger from the worker's shared one — a mutation there would put one workflow's id on
// every other workflow's lines.
func TestAChildLoggerDoesNotChangeItsParent(t *testing.T) {
	logger, buf := newBufferLogger(t, zapcore.InfoLevel)

	parent := NewZapLogger(logger)
	child := parent.With("WorkflowID", "task-42")

	child.Info("from the child")
	parent.Info("from the parent")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	require.Len(t, lines, 2)

	childLine := map[string]any{}
	parentLine := map[string]any{}
	require.NoError(t, json.Unmarshal(lines[0], &childLine))
	require.NoError(t, json.Unmarshal(lines[1], &parentLine))

	require.Equal(t, "task-42", childLine["WorkflowID"])
	require.NotContains(t, parentLine, "WorkflowID")
}

// TestADanglingPairIsStillNamed: the SDK's own calls are variadic, and an odd one would otherwise drop
// the last key — which is how a line ends up missing exactly the field somebody was looking for.
func TestADanglingPairIsStillNamed(t *testing.T) {
	logger, buf := newBufferLogger(t, zapcore.InfoLevel)

	NewZapLogger(logger).Warn("something happened", "Attempt")

	line := decode(t, buf)
	require.Contains(t, line, "Attempt")
}

// TestTheLevelIsTheDeployments: a debug line of a workflow is frequent, and a deployment that runs at
// info must not pay for it — nor see it.
func TestTheLevelIsTheDeployments(t *testing.T) {
	logger, buf := newBufferLogger(t, zapcore.WarnLevel)

	sdk := NewZapLogger(logger)
	sdk.Debug("a detail")
	sdk.Info("a fact")
	sdk.Warn("a problem")

	require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("\n")))
	require.Equal(t, "a problem", decode(t, buf)["msg"])
}

// TestANilLoggerIsUsable: the SDK always has a logger, and a container assembled in a test passes
// none.
func TestANilLoggerIsUsable(t *testing.T) {
	require.NotPanics(t, func() { NewZapLogger(nil).Info("nothing to see") })
}
