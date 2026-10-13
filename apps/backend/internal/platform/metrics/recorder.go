package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The recording API: one method per measurement, taking plain values.
//
// They live on the provider rather than on the instruments so a caller never has to ask whether a
// provider exists — a deployment with metrics off must not have every call site carrying a branch,
// which is the shape that ends with one of them forgetting it. A nil provider records nothing.

// DraftReady records how long a draft took to be ready for a person.
func (p *Provider) DraftReady(ctx context.Context, seconds float64) {
	if p == nil || !p.Enabled() {
		return
	}

	p.Instruments.draftReady.Record(ctx, seconds)
}

// DraftDecision records what a person did with a draft.
//
// outcome is the deployment's own vocabulary — approved, revised, rejected — rather than a status a
// table happens to store, so a renamed column does not open a new time series.
func (p *Provider) DraftDecision(ctx context.Context, outcome string, revision bool) {
	if p == nil || !p.Enabled() {
		return
	}

	p.Instruments.draftDecisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", classify(outcome, draftOutcomes)),
		attribute.Bool("revised", revision),
	))
}

// TaskFinished records a task that reached a terminal state, with what it spent.
func (p *Provider) TaskFinished(ctx context.Context, trigger, status string, seconds float64, inputTokens, outputTokens, costMicros int64) {
	if p == nil || !p.Enabled() {
		return
	}

	attributes := metric.WithAttributes(
		attribute.String("trigger", classify(trigger, taskTriggers)),
		attribute.String("outcome", classify(status, taskOutcomes)),
	)

	p.Instruments.taskFinished.Add(ctx, 1, attributes)
	p.Instruments.taskTokens.Add(ctx, inputTokens, metric.WithAttributes(attribute.String("direction", "input")))
	p.Instruments.taskTokens.Add(ctx, outputTokens, metric.WithAttributes(attribute.String("direction", "output")))
	p.Instruments.taskCost.Add(ctx, costMicros)
	p.Instruments.taskDuration.Record(ctx, seconds, attributes)
}

// ToolCall records one tool call and whether it failed.
//
// It is labelled by the *integration* rather than by the tool name, because that is the question the
// chart exists to answer: "which integration is failing" is an incident, and which of its four
// operations failed is a second question the step rows answer exactly. The app is bounded by the
// registry, which is what makes it safe as a label.
func (p *Provider) ToolCall(ctx context.Context, app string, failed bool, seconds float64) {
	if p == nil || !p.Enabled() {
		return
	}

	attributes := metric.WithAttributes(attribute.String("app", classifyApp(app)))

	p.Instruments.toolCalls.Add(ctx, 1, attributes)
	if failed {
		p.Instruments.toolFailures.Add(ctx, 1, attributes)
	}
	p.Instruments.toolDuration.Record(ctx, seconds, attributes)
}

// ModelCall records one provider call.
func (p *Provider) ModelCall(ctx context.Context, provider, purpose string, fellBack bool, seconds float64) {
	if p == nil || !p.Enabled() {
		return
	}

	attributes := metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("purpose", classify(purpose, modelPurposes)),
	)

	p.Instruments.modelCalls.Add(ctx, 1, attributes)
	if fellBack {
		p.Instruments.modelFallbacks.Add(ctx, 1, attributes)
	}
	p.Instruments.modelLatency.Record(ctx, seconds, metric.WithAttributes(attribute.String("provider", provider)))
}

// RateLimited records a request a limit refused.
func (p *Provider) RateLimited(ctx context.Context, surface string) {
	if p == nil || !p.Enabled() {
		return
	}

	p.Instruments.rateLimited.Add(ctx, 1, metric.WithAttributes(
		attribute.String("surface", classify(surface, rateLimitSurfaces)),
	))
}

// TaskDispatched records work this deployment accepted.
//
// It is one half of the depth rather than an increment of it: the task is finished by a worker, in
// another process, so a single process cannot own the count. The two counters are scraped from the
// same fleet and the depth is their difference, which is also what makes it survive a restart —
// neither counter goes backwards.
func (p *Provider) TaskDispatched(ctx context.Context) {
	if p == nil || !p.Enabled() {
		return
	}

	p.Instruments.taskDispatched.Add(ctx, 1)
}

// TaskSettled records work that reached a terminal state.
func (p *Provider) TaskSettled(ctx context.Context) {
	if p == nil || !p.Enabled() {
		return
	}

	p.Instruments.taskSettled.Add(ctx, 1)
}

// ActivityRetry records an activity attempt past the first.
//
// It is recorded where the attempt happens rather than counted from a log, because "the retry rate
// went up" is the first sign of a provider or a queue degrading and it must be answerable without a
// search over rotated lines.
func (p *Provider) ActivityRetry(ctx context.Context, activity string, attempt int) {
	if p == nil || !p.Enabled() {
		return
	}

	p.Instruments.activityRetries.Add(ctx, 1, metric.WithAttributes(
		attribute.String("activity", classify(activity, activityNames)),
		attribute.String("attempt", attemptBucket(attempt)),
	))
}

// RetentionPruned records what a retention pass removed.
func (p *Provider) RetentionPruned(ctx context.Context, kind string, count int64) {
	if p == nil || !p.Enabled() || count == 0 {
		return
	}

	p.Instruments.retentionPruned.Add(ctx, count, metric.WithAttributes(
		attribute.String("kind", classify(kind, retentionKinds)),
	))
}

// ---------------------------------------------------------------- vocabularies

// The vocabularies a label is allowed to take. A value outside one of them becomes "other", which
// is what bounds cardinality: a reason string a future version of the product invents is one new
// series the day it is written, and an incident is not the moment to discover that the scrape
// budget is gone.
var (
	// The spellings mirror the approval module's own status values, which is what its service
	// reports: a status it renames becomes "other" rather than a new series, and the test below
	// is what says so out loud rather than leaving it to be discovered on a chart.
	draftOutcomes = newVocabulary("approved", "revise", "sent", "canceled")
	taskTriggers  = newVocabulary("chat", "routine", "webhook", "handoff")
	taskOutcomes  = newVocabulary("succeeded", "failed", "canceled")
	// The spellings are the model gateway's own Purpose values, which is what its callers set:
	// a purpose it renames becomes "other" rather than a new series, and the vocabulary test is
	// what says so out loud instead of leaving it to be discovered on a chart.
	modelPurposes     = newVocabulary("agent", "routing", "extraction", "embedding")
	rateLimitSurfaces = newVocabulary("login", "integration", "chat", "upload")
	retentionKinds    = newVocabulary("step", "attachment")
	integrationApps   = newVocabulary("gmail", "whatsapp")
	activityNames     = newVocabulary("LoadTaskContext", "CallModel", "RunTool", "RequestDraft",
		"CarryOutDraft", "RecordStep", "BeatTask", "CompleteTask", "PrepareHandoff",
		"ResumeTask", "MarkHandoffStarted", "ExtractConversation")
)

// vocabulary is a set of allowed label values.
type vocabulary map[string]bool

// newVocabulary builds a set from its members.
func newVocabulary(values ...string) vocabulary {
	set := make(vocabulary, len(values)+1)
	for _, value := range values {
		set[value] = true
	}
	// Every vocabulary accepts this, so a classification that fell back is itself a legal
	// value rather than a new series.
	set[otherLabel] = true

	return set
}

// attemptBucket turns an attempt number into a bounded label.
//
// The exact number is a series per retry depth, and a queue that is failing hard produces attempts
// that climb forever. What an operator acts on is the shape — the second attempt, or many.
func attemptBucket(attempt int) string {
	switch {
	case attempt <= 1:
		return "1"
	case attempt == 2:
		return "2"
	case attempt <= 5:
		return "3-5"
	default:
		return "6+"
	}
}

// otherLabel is where an unrecognised value lands.
const otherLabel = "other"

// classifyApp keeps an integration name inside its vocabulary.
//
// The set is the integrations this codebase ships, and anything else is grouped. That is not a
// shortcoming of the list: an integration connected over MCP carries whatever name the workspace gave
// its server, so an open label would be one time series per customer's own server — which is the
// exact shape the package comment says must not happen. The named ones are what an incident is
// triaged against; everything else is "other" on the chart and exact in the `integrations` and
// `task_steps` rows.
func classifyApp(app string) string {
	return classify(app, integrationApps)
}

// classify keeps a label inside its vocabulary.
func classify(value string, allowed vocabulary) string {
	if allowed[value] {
		return value
	}

	return otherLabel
}
