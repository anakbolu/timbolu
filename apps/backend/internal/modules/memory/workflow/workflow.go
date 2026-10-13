// Package workflow holds the durable half of memory extraction: one workflow per
// conversation that waits for the thread to stop moving and then asks the
// extractor to read it.
//
// The waiting is a Temporal timer rather than a sleep in the API process,
// because the point of the delay is that it survives a deploy: a conversation
// that goes quiet while a worker is being replaced is still extracted, and the
// quiet period is measured in wall-clock time rather than in process uptime.
package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// WorkflowName is what the worker registers and what the schedule names.
//
// It is the Go function's own name, deliberately: the worker registers a workflow
// by the name of the function it is handed, while the schedule starts one by this
// string. Two names that are only equal by coincidence is the failure mode where a
// workflow is scheduled, no worker recognises it, and the run retries until it
// times out — which is a silent stall rather than a failure. TestTheWorkflowNameIsTheFunctionName is what keeps them equal.
const WorkflowName = "MemoryExtractionWorkflow"

// Activity names, referenced by string so the worker registers them without the
// workflow sharing a function value with it.
const (
	// ActivityExtract reads the thread and writes what is worth keeping.
	ActivityExtract = "ExtractConversation"
)

// SignalActive is what a message sends. It resets the quiet period.
const SignalActive = "conversation-active"

// DefaultQuietPeriod is how long a conversation must be still when the caller
// does not configure one. Five minutes is longer than a reply and shorter than a
// coffee break: the thread has stopped, but the person has not left the screen.
const DefaultQuietPeriod = 5 * time.Minute

// maxQuietResets bounds how many times one workflow may be reset before it gives
// up. A conversation that keeps moving is not one to extract now, and each reset
// costs workflow history: past the bound the run ends and the next message starts
// a fresh one with an empty history.
const maxQuietResets = 200

// extractionTimeout bounds one pass: a small model call and one embedding call.
const extractionTimeout = 3 * time.Minute

// ExtractionInput is one conversation to watch. Every field is a string so a
// workflow started by an older deployment still deserialises.
type ExtractionInput struct {
	WorkspaceID    string
	ConversationID string
	// AgentID is the Bolu that answered in this thread: its switch decides
	// whether extraction runs, and its private layer is where role-specific
	// memories go.
	AgentID string
	// QuietPeriod is how long the thread must not move. It is carried in the
	// input so the configured period is the one that waits, rather than a
	// constant here disagreeing with the one in configuration.
	QuietPeriod time.Duration
}

// ExtractionResult is what one pass produced.
type ExtractionResult struct {
	Disabled bool
	Stored   int
	Merged   int
	Skipped  int
	Refused  int
}

// MemoryExtractionWorkflow waits for the conversation to go quiet and extracts
// once.
//
// The signals are what makes the wait a debounce rather than a delay: the caller
// starts the workflow with SignalWithStart on every message, so the first
// message starts a run and every later one pushes the timer out. When the timer
// finally fires, nothing has been said for the whole quiet period.
func MemoryExtractionWorkflow(ctx workflow.Context, input ExtractionInput) (ExtractionResult, error) {
	quiet := input.QuietPeriod
	if quiet <= 0 {
		quiet = DefaultQuietPeriod
	}

	active := workflow.GetSignalChannel(ctx, SignalActive)

	for resets := 0; ; resets++ {
		timer := workflow.NewTimer(ctx, quiet)
		reset := false

		selector := workflow.NewSelector(ctx)
		selector.AddFuture(timer, func(workflow.Future) {})
		selector.AddReceive(active, func(receive workflow.ReceiveChannel, _ bool) {
			var payload any
			receive.Receive(ctx, &payload)
			reset = true
		})
		selector.Select(ctx)

		if !reset {
			break
		}
		if resets >= maxQuietResets {
			// The thread never settled. Ending here is better than growing one
			// workflow's history without bound; the next message opens a new run.
			return ExtractionResult{}, nil
		}
	}

	extractCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: extractionTimeout,
		// A pass that fails is worth one retry: the model provider may have been
		// briefly unavailable, and the memory is not urgent.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    2,
		},
	})

	var result ExtractionResult
	if err := workflow.ExecuteActivity(extractCtx, ActivityExtract, input).Get(extractCtx, &result); err != nil {
		return ExtractionResult{}, err
	}

	return result, nil
}
