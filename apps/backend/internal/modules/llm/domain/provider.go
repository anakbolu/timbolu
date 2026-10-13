// Package domain defines the LLM gateway contract: one internal message format,
// one Provider interface, and one Streaming event type.
//
// Nothing here knows about a provider. An adapter translates these types into
// its own wire format and translates the answers back, so the agent worker is
// written once and a provider can be swapped by configuration. The package
// imports only the standard library.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Role is who wrote a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// FinishReason is why the model stopped.
type FinishReason string

const (
	// FinishStop: the model finished its answer.
	FinishStop FinishReason = "stop"
	// FinishToolCalls: the model wants a tool called.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishLength: the output hit the token limit.
	FinishLength FinishReason = "length"
	// FinishContentFilter: the provider refused the content.
	FinishContentFilter FinishReason = "content_filter"
	// FinishUnknown: a reason this gateway does not model.
	FinishUnknown FinishReason = "unknown"
)

// Errors every adapter reports in the same way, so the gateway can decide to
// fall back without knowing the provider.
var (
	// ErrRateLimited means the provider asked us to slow down.
	ErrRateLimited = errors.New("llm: provider rate limited")
	// ErrProviderUnavailable means the provider is down or unreachable.
	ErrProviderUnavailable = errors.New("llm: provider unavailable")
	// ErrInvalidRequest means the request is malformed; retrying will not help.
	ErrInvalidRequest = errors.New("llm: invalid request")
	// ErrContextTooLong means the prompt does not fit the model's context.
	ErrContextTooLong = errors.New("llm: context too long")
	// ErrQuotaExceeded means the workspace used up its token allowance.
	ErrQuotaExceeded = errors.New("llm: workspace quota exceeded")
	// ErrProviderNotFound means the configured provider does not exist.
	ErrProviderNotFound = errors.New("llm: provider not configured")
	// ErrDuplicateProvider means the workspace already has a provider with that
	// name, or that another provider is already the default.
	ErrDuplicateProvider = errors.New("llm: provider name already exists")
	// ErrNoProvider means nothing is configured to answer the request.
	ErrNoProvider = errors.New("llm: no provider is configured")
	// ErrUsageNotRecorded means the call succeeded but its cost could not be
	// written. It is deliberately not retryable: the provider has already billed
	// the call, so trying again would bill it twice.
	ErrUsageNotRecorded = errors.New("llm: usage could not be recorded")
	// ErrUnsupported means the provider cannot do what the request asks.
	ErrUnsupported = errors.New("llm: provider does not support this request")
)

// Retryable reports whether falling back to another provider is worth trying.
// A malformed request or an over-long context fails everywhere, so it is not.
func Retryable(err error) bool {
	return errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrProviderUnavailable) ||
		errors.Is(err, ErrQuotaExceeded)
}

// Image is a picture sent to a vision model.
type Image struct {
	MediaType string // image/png, image/jpeg
	Data      string // base64, without a data: prefix
}

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is the answer to a ToolCall.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Message is one turn of the conversation in the internal format.
//
// A single type covers every role: an assistant message may carry Text and
// ToolCalls together, and a tool message carries ToolResults. The adapters
// project that onto whatever shape their provider wants.
type Message struct {
	Role        Role
	Text        string
	Images      []Image
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Tool is a callable capability offered to the model. Label comes from the tool
// catalogue and decides whether a call needs human approval, which is why it
// travels with the definition.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Label       string
}

// ToolChoice is how firmly the model must use a tool.
type ToolChoice string

const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceNone     ToolChoice = "none"
	ToolChoiceRequired ToolChoice = "required"
)

// ChatRequest is one completion request.
type ChatRequest struct {
	Model  string
	System string
	// Retrieved is what a search found for this request: a Bolu's private
	// memory, and the chunks of the workspace's documents that are close to the
	// question. It is data rather than instruction, and whoever produces it
	// marks it as such.
	//
	// It travels apart from System because the two are cached differently: the
	// system text is the stable prefix — persona, instructions, main memory —
	// that a provider with prompt caching keeps between calls, while this block
	// changes with the question. Joining them would spend a cache write on every
	// task and lose the hits the stable half earns.
	Retrieved     string
	Messages      []Message
	Tools         []Tool
	ToolChoice    ToolChoice
	ToolName      string // when ToolChoice pins one tool
	MaxTokens     int
	Temperature   *float64
	StopSequences []string
	// Metadata is carried into the usage record so a call can be traced back to
	// the task that caused it.
	Metadata RequestMetadata
}

// RequestMetadata ties a call to the work that caused it.
type RequestMetadata struct {
	WorkspaceID uuid.UUID
	AgentID     uuid.UUID
	TaskID      uuid.UUID
	// Purpose separates agent turns from routing, extraction, and embeddings so
	// the cost report can tell them apart.
	Purpose string
}

// Purpose values.
const (
	PurposeAgent      = "agent"
	PurposeRouting    = "routing"
	PurposeExtraction = "extraction"
	PurposeEmbedding  = "embedding"
)

// Usage is what a call cost, in tokens.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Total is the billable token count.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens
}

// ChatResponse is one completion.
type ChatResponse struct {
	Text         string
	ToolCalls    []ToolCall
	FinishReason FinishReason
	Usage        Usage
	// Model and Provider record who actually answered, which is what the usage
	// ledger stores when a fallback was used.
	Model    string
	Provider string
	// RawFinishReason keeps the provider's own string for diagnostics.
	RawFinishReason string
}

// HasToolCalls reports whether the model asked for a tool.
func (r ChatResponse) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// StreamEventType is the kind of a streaming event.
type StreamEventType string

const (
	// EventText carries a piece of the answer.
	EventText StreamEventType = "text"
	// EventToolCall carries a complete tool call. Providers that stream tool
	// arguments in fragments only emit this once the call is whole.
	EventToolCall StreamEventType = "tool_call"
	// EventUsage carries the token counts, usually once at the end.
	EventUsage StreamEventType = "usage"
	// EventDone is the last event of a successful stream.
	EventDone StreamEventType = "done"
	// EventError ends the stream with a failure.
	EventError StreamEventType = "error"
)

// StreamEvent is one normalised streaming event.
type StreamEvent struct {
	Type         StreamEventType
	Text         string
	ToolCall     *ToolCall
	Usage        *Usage
	FinishReason FinishReason
	Err          error
}

// Capabilities describes what a provider can do, so the gateway can reject a
// request that the chosen model cannot serve instead of failing at the provider.
type Capabilities struct {
	Tools             bool
	Vision            bool
	Streaming         bool
	PromptCaching     bool
	ParallelToolCalls bool
	// Embeddings says the provider has an embedding endpoint at all. A provider
	// without one is skipped by the embedding chain rather than asked and
	// refused, which is the difference between "try the next provider" and
	// "nothing here can do it".
	Embeddings       bool
	MaxContextTokens int
}

// Supports reports why a request cannot be served, or nil when it can.
func (c Capabilities) Supports(req ChatRequest) error {
	if len(req.Tools) > 0 && !c.Tools {
		return ErrUnsupported
	}
	if req.ToolChoice == ToolChoiceRequired || req.ToolName != "" {
		if !c.Tools {
			return ErrUnsupported
		}
	}
	for _, message := range req.Messages {
		if len(message.Images) > 0 && !c.Vision {
			return ErrUnsupported
		}
	}
	if c.MaxContextTokens > 0 && EstimateTokens(req) > c.MaxContextTokens {
		return ErrContextTooLong
	}
	return nil
}

// Provider is one configured way of reaching a model.
type Provider interface {
	// Name is the adapter name, e.g. "openai" or "anthropic".
	Name() string
	// Model is the model this provider is configured with.
	Model() string
	// Capabilities reports what the model can do.
	Capabilities() Capabilities
	// Chat performs a completion.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// Stream performs a completion and reports its progress. The returned
	// channel is closed when the stream ends; the last event is EventDone or
	// EventError.
	Stream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
	// Embed turns text into vectors.
	//
	// An adapter whose API has no embedding endpoint reports ErrUnsupported and
	// advertises Embeddings=false, so the chain can move on instead of stopping
	// on a capability the provider never had.
	Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error)
}

// EmbeddingDimensions is the width of the vectors this product stores. The
// bootstrap migration pins `vector(1536)` and the memory and knowledge tables
// are written with it, so a model that answers with another width is refused
// before a row exists rather than failing on the insert.
const EmbeddingDimensions = 1536

// EmbedRequest is one embedding call.
type EmbedRequest struct {
	// Model overrides the embedding model the deployment configured. Empty means
	// the deployment's model, and failing that the provider's own.
	Model string
	// Inputs is the text to embed, in order. The answer has one vector per
	// input, in the same order.
	Inputs   []string
	Metadata RequestMetadata
}

// EmbedResponse is what an embedding call answered.
type EmbedResponse struct {
	// Vectors is one vector per input, in the order the inputs were sent.
	Vectors [][]float32
	// Model and Provider record who actually answered, which is what the usage
	// ledger stores when a fallback was used.
	Model    string
	Provider string
	Usage    Usage
}

// UsageEntry is one row of the usage ledger.
type UsageEntry struct {
	WorkspaceID uuid.UUID
	AgentID     uuid.UUID
	TaskID      uuid.UUID
	Provider    string
	Model       string
	Purpose     string
	Usage       Usage
	CostMicros  int64
	CreatedAt   time.Time
}

// UsageDaily is one day of aggregated usage, which the dashboard and the quota
// screen read instead of scanning the ledger.
type UsageDaily struct {
	Day              time.Time
	Provider         string
	Model            string
	Purpose          string
	Calls            int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicros       int64
}

// Total is the billable token count of the day.
func (d UsageDaily) Total() int64 { return d.InputTokens + d.OutputTokens }

// UsageReport is what a workspace spent over a window, read three ways.
//
// The three are deliberately separate rather than one number and two breakdowns: `Totals` covers
// every call the workspace made, including the ones made outside any task — a memory extraction, an
// embedding, a routing decision — so a screen that presented the per-task rows as the whole of the
// spend would be showing a smaller number than the invoice. That gap is a fact about the product
// (work happens outside tasks) rather than a rounding error, and it is stated instead of hidden.
type UsageReport struct {
	// PeriodStartedAt is the first moment the window covers, in the deployment's own timezone
	// (Asia/Jakarta). It is resolved where the window is resolved, so a screen cannot disagree
	// with the totals about what the period was.
	PeriodStartedAt time.Time
	// Totals is every call in the window.
	Totals UsageTotals
	// ByAgent is the same spend grouped by the Bolu that made it, dearest first. A call made
	// outside any Bolu — there are none today, and the ledger permits them — lands in the row
	// whose AgentID is zero.
	ByAgent []UsageByAgent
	// ByTask is the same spend grouped by the task that caused it, dearest first. Calls made
	// outside a task are absent, which is why Totals and the sum of these rows differ.
	ByTask []UsageByTask
}

// UsageTotals is one window's spend.
type UsageTotals struct {
	Calls            int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicros       int64
}

// Total is the billable token count of the window.
func (t UsageTotals) Total() int64 { return t.InputTokens + t.OutputTokens }

// DefaultReportPage is how many Bolu and how many tasks one report lists when the caller does not
// say. It is a page rather than the whole of history: a workspace with ten thousand tasks has a
// report whose *totals* are the answer and whose rows are examples.
const DefaultReportPage = 50

// UsageByAgent is one Bolu's share of a window.
type UsageByAgent struct {
	AgentID      uuid.UUID
	AgentName    string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
}

// Total is the billable token count for this Bolu.
func (u UsageByAgent) Total() int64 { return u.InputTokens + u.OutputTokens }

// UsageByTask is one task's share of a window.
type UsageByTask struct {
	TaskID       uuid.UUID
	TaskTitle    string
	TaskStatus   string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
}

// Total is the billable token count for this task.
func (u UsageByTask) Total() int64 { return u.InputTokens + u.OutputTokens }

// UsageReader reads the aggregated usage of a workspace.
type UsageReader interface {
	// Daily returns per-day totals for the last days, most recent first.
	Daily(ctx context.Context, workspaceID uuid.UUID, since time.Time) ([]UsageDaily, error)
	// Report returns the period's totals over a window, with the per-Bolu and per-task
	// breakdowns the usage screen shows.
	Report(ctx context.Context, workspaceID uuid.UUID, since time.Time, limit int) (UsageReport, error)
	// SumSince returns the tokens recorded since a moment, which is the durable
	// fallback when the live counter is unavailable.
	SumSince(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error)
	// CostSince returns the cost recorded since a moment, in micro-rupiah. It is
	// the durable fallback for the daily ceiling, and it reads the daily
	// aggregation rather than scanning the ledger.
	CostSince(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error)
}

// Metrics is where the model gateway reports the calls it made.
//
// It is a port because the module owns no telemetry: the container implements it over whatever the
// deployment configured. What it carries is the provider, the purpose, and whether a fallback was
// needed — never the prompt, the answer, or a workspace. The identities live in `usage_ledger`,
// where the rows already are; a chart needs the *rate*: which provider is slow, and how often the
// chain had to move on.
type Metrics interface {
	// ModelCall reports one call that reached a provider. seconds is how long the provider took,
	// and fellBack is whether this deployment had already tried another one for the same
	// request — which is the number that explains an unexpected bill.
	ModelCall(ctx context.Context, provider, purpose string, fellBack bool, seconds float64)
}

// UsageRecorder persists what a call cost. Every provider call goes through it,
// which is what makes "no call is unrecorded" checkable.
type UsageRecorder interface {
	Record(ctx context.Context, entry UsageEntry) error
}

// QuotaChecker rejects a call when the workspace has used up its token
// allowance. EPIC 12 (#97) implements the policy; the gateway only asks.
type QuotaChecker interface {
	Check(ctx context.Context, workspaceID uuid.UUID) error
}

// QuotaCounter is the live spend of a workspace. It is a cache in front of the
// ledger, so a Redis restart costs accuracy but not correctness: the repository
// can always recompute the total from usage_ledger.
//
// It tracks two windows because the product bounds two: a token allowance per
// billing period, and a cost ceiling per day. A runaway task is stopped by the
// day before it can consume a month.
type QuotaCounter interface {
	// Spent is the tokens recorded for the current period.
	Spent(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	// Add records tokens spent by one call.
	Add(ctx context.Context, workspaceID uuid.UUID, tokens int64) error
	// SpentCostToday is the cost recorded for the current day, in micro-rupiah.
	SpentCostToday(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	// AddCost records what one call cost.
	AddCost(ctx context.Context, workspaceID uuid.UUID, costMicros int64) error
}

// QuotaPolicy reports what a workspace may spend. Zero means unlimited, and the
// two bounds are separate: a workspace may have tokens left for the month and
// still be out for the day.
//
// EPIC 12 (#97) replaces the configured values with the subscription's package.
type QuotaPolicy interface {
	// Allowance is the token allowance of the current period.
	Allowance(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	// DailyCostAllowance is the cost ceiling of one day, in micro-rupiah.
	DailyCostAllowance(ctx context.Context, workspaceID uuid.UUID) (int64, error)
}

// Auditor records what a person did, for the trail the product answers "who changed this"
// with. The approval module owns the table.
//
// A provider configuration holds an API key, so what this carries is deliberately narrow:
// the name, the adapter, the model, and whether a key is stored. The key itself is never
// passed here, which is why the entry is built from the redacted shape rather than from the
// configuration that was written.
type Auditor interface {
	Record(ctx context.Context, scope Scope, entry AuditEntry) error
}

// AuditEntry is one thing that happened, for the record.
type AuditEntry struct {
	Action      string
	ObjectType  string
	ObjectID    string
	ActorUserID uuid.UUID
	Metadata    map[string]any
}

// Clock reports the time, so cost and expiry rules are testable.
type Clock func() time.Time

// EstimateTokens is the cheap, provider-independent prompt size estimate used
// for the context check. Roughly four characters per token, which is close
// enough to reject a prompt that clearly does not fit.
func EstimateTokens(req ChatRequest) int {
	characters := len(req.System)
	for _, message := range req.Messages {
		characters += len(message.Text)
		for _, call := range message.ToolCalls {
			characters += len(call.Name) + len(call.Arguments)
		}
		for _, result := range message.ToolResults {
			characters += len(result.Content)
		}
	}
	for _, tool := range req.Tools {
		characters += len(tool.Name) + len(tool.Description) + len(tool.Schema)
	}
	return characters / 4
}
