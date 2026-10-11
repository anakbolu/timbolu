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
	// CachePrefix marks the last message of a stable prefix (persona,
	// instructions, main memory) that the provider may cache. Adapters that
	// support it emit a cache breakpoint; the others ignore it.
	CachePrefix bool
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
	Model         string
	System        string
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
	MaxContextTokens  int
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

// UsageReader reads the aggregated usage of a workspace.
type UsageReader interface {
	// Daily returns per-day totals for the last days, most recent first.
	Daily(ctx context.Context, workspaceID uuid.UUID, since time.Time) ([]UsageDaily, error)
	// SumSince returns the tokens recorded since a moment, which is the durable
	// fallback when the live counter is unavailable.
	SumSince(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error)
	// CostSince returns the cost recorded since a moment, in micro-rupiah. It is
	// the durable fallback for the daily ceiling, and it reads the daily
	// aggregation rather than scanning the ledger.
	CostSince(ctx context.Context, workspaceID uuid.UUID, since time.Time) (int64, error)
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
