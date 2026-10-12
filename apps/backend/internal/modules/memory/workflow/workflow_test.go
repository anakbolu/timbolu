package workflow

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// TestTheWorkflowNameIsTheFunctionName is the same contract the task runtime
// carries for its activities, and it is not academic: the worker registers a
// workflow by the name of the function it is handed, while the schedule starts one
// by `WorkflowName`. When the two drift apart the workflow is scheduled, no worker
// recognises it, and the run retries until it times out — a silent stall that looks
// like a slow conversation rather than like a bug.
func TestTheWorkflowNameIsTheFunctionName(t *testing.T) {
	require.Equal(t, "MemoryExtractionWorkflow", WorkflowName)

	full := runtime.FuncForPC(reflect.ValueOf(MemoryExtractionWorkflow).Pointer()).Name()
	name := full[strings.LastIndex(full, ".")+1:]

	require.Equal(t, WorkflowName, name,
		"the workflow must be registered under the name the schedule starts it with")
}

// TestTheActivityNameIsTheMethodName is the same guard for the one activity.
func TestTheActivityNameIsTheMethodName(t *testing.T) {
	require.Equal(t, "ExtractConversation", ActivityExtract)
}

// stub carries the one activity name the workflow calls. The environment matches
// an expectation by the function that was registered, so the name has to exist as
// a real method even though the body never runs.
type stub struct{}

func (stub) ExtractConversation(context.Context, ExtractionInput) (ExtractionResult, error) {
	return ExtractionResult{}, nil
}

// harness is one test environment with the extraction activity scripted.
type harness struct {
	env *testsuite.TestWorkflowEnvironment
	// runs counts the passes the activity was asked for.
	runs int
	// result is what each pass answers.
	result ExtractionResult
	// inputs records what the activity was given, which is how "the configured
	// quiet period is the one that waits" is asserted.
	inputs []ExtractionInput
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(MemoryExtractionWorkflow)

	h := &harness{env: env}
	// The activity is registered under the exact name the workflow calls, which
	// is what lets the expectation be attached to it. Nothing of the stub body
	// runs: the pass is scripted below.
	env.RegisterActivityWithOptions(stub{}.ExtractConversation, activity.RegisterOptions{Name: ActivityExtract})
	env.OnActivity(ActivityExtract, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input ExtractionInput) (ExtractionResult, error) {
			h.runs++
			h.inputs = append(h.inputs, input)
			return h.result, nil
		})

	return h
}

func input(quiet time.Duration) ExtractionInput {
	return ExtractionInput{
		WorkspaceID:    "33333333-3333-3333-3333-333333333333",
		ConversationID: "55555555-5555-5555-5555-555555555555",
		AgentID:        "44444444-4444-4444-4444-444444444444",
		QuietPeriod:    quiet,
	}
}

// TestExtractionWaitsForTheQuietPeriod is the E10.5 gate: the conversation is
// read after it stops moving, not while it is being written.
func TestExtractionWaitsForTheQuietPeriod(t *testing.T) {
	h := newHarness(t)
	h.result = ExtractionResult{Stored: 1}

	h.env.ExecuteWorkflow(MemoryExtractionWorkflow, input(time.Minute))

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())

	var result ExtractionResult
	require.NoError(t, h.env.GetWorkflowResult(&result))
	require.Equal(t, 1, result.Stored)

	require.Equal(t, 1, h.runs)
	require.Equal(t, time.Minute, h.inputs[0].QuietPeriod)
}

// TestAMessagePushesTheTimerOut is what makes the wait a debounce: a message
// arriving after half the period must restart it, so a thread that keeps moving
// is never read in the middle.
func TestAMessagePushesTheTimerOut(t *testing.T) {
	h := newHarness(t)
	h.result = ExtractionResult{Stored: 2}

	quiet := time.Minute

	// Two messages after the first, then nothing: the third timer is the one
	// that fires.
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(SignalActive, nil)
	}, 40*time.Second)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(SignalActive, nil)
	}, 80*time.Second)

	// A probe at 100 virtual seconds asks whether the pass has already happened.
	// A timer started once and never restarted would have fired at 60s, so by
	// 100s the activity would have run; a debounce cannot have run until 60s after
	// the last signal at 80s.
	ranBeforeSettling := false
	h.env.RegisterDelayedCallback(func() {
		ranBeforeSettling = h.runs > 0
	}, 100*time.Second)

	h.env.ExecuteWorkflow(MemoryExtractionWorkflow, input(quiet))

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	require.False(t, ranBeforeSettling,
		"the quiet period must restart on every message, not run from the first one")
	require.Equal(t, 1, h.runs, "the pass runs once, after the thread settles")
}

// TestExtractionIsSkippedWhenTheBoluTurnedItOff: the activity answers
// `disabled`, and the workflow passes it through rather than retrying.
func TestExtractionIsSkippedWhenTheBoluTurnedItOff(t *testing.T) {
	h := newHarness(t)
	h.result = ExtractionResult{Disabled: true}

	h.env.ExecuteWorkflow(MemoryExtractionWorkflow, input(time.Second))

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())

	var result ExtractionResult
	require.NoError(t, h.env.GetWorkflowResult(&result))
	require.True(t, result.Disabled)
	require.Equal(t, 1, h.runs)
}

// TestTheQuietPeriodFallsBackToTheDefault: a caller that states nothing still
// gets the configured default rather than a zero timer that fires at once.
func TestTheQuietPeriodFallsBackToTheDefault(t *testing.T) {
	h := newHarness(t)

	h.env.ExecuteWorkflow(MemoryExtractionWorkflow, input(0))

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())

	var result ExtractionResult
	require.NoError(t, h.env.GetWorkflowResult(&result))
	require.Equal(t, 1, h.runs)
}

// TestAConversationThatNeverSettlesEnds is the bound: a thread that keeps moving
// must not grow one workflow's history without end.
func TestAConversationThatNeverSettlesEnds(t *testing.T) {
	h := newHarness(t)

	// A signal every second, against a period that is never reached because each
	// one restarts it. Past the bound the run ends without extracting.
	for i := range maxQuietResets + 5 {
		h.env.RegisterDelayedCallback(func() {
			h.env.SignalWorkflow(SignalActive, nil)
		}, time.Duration(i+1)*time.Second)
	}

	h.env.ExecuteWorkflow(MemoryExtractionWorkflow, input(time.Hour))

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	require.Zero(t, h.runs, "a conversation that never settled is not extracted")
}

// TestTheRunHandsTheActivityThePeriodItWasGiven is the other half of the contract
// the stub stack proved: the period the caller sets has to reach the activity, not
// only the workflow's own timer.
//
// The assertion is on the input the activity was handed rather than on a return
// value, because a period the workflow kept to itself is indistinguishable from one
// nobody set: the run waits correctly and then reads a thread it was told nothing
// about.
func TestTheRunHandsTheActivityThePeriodItWasGiven(t *testing.T) {
	h := newHarness(t)
	h.result = ExtractionResult{Stored: 1, Skipped: 2}

	h.env.ExecuteWorkflow(MemoryExtractionWorkflow, input(5*time.Second))

	require.True(t, h.env.IsWorkflowCompleted())
	require.NoError(t, h.env.GetWorkflowError())
	require.Equal(t, 1, h.runs)

	require.Equal(t, 5*time.Second, h.inputs[0].QuietPeriod,
		"the run must hand the activity the period it was given")
	require.Equal(t, "55555555-5555-5555-5555-555555555555", h.inputs[0].ConversationID)
}
