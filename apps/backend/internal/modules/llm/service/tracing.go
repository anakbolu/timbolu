package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// The model calls' spans.
//
// What a span carries is what an incident needs and what a bill is argued with: which workspace and
// which Bolu, which provider and which model, how many tokens, whether a fallback happened, and how
// many tool calls came back. **The prompt and the answer are never attributes.** They are the
// customer's words, and a trace is a second store with its own retention and its own access — one
// that the product's own retention policy does not reach. The same rule the audit trail follows
// applies here: identifiers, counts, and outcomes, never content.

// startChatSpan opens the span for one completion, or the no-op span when telemetry is not wired.
func (s *Service) startChatSpan(ctx context.Context, name string, scope domain.Scope, req domain.ChatRequest) (context.Context, trace.Span) {
	if s.deps.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}

	attributes := []attribute.KeyValue{
		attribute.String("workspace_id", scope.WorkspaceID.String()),
		attribute.Int("message_count", len(req.Messages)),
		attribute.Int("tool_count", len(req.Tools)),
		// How big the request was, in the unit the bill is in — which is what makes a slow or
		// an expensive call explicable without keeping any of its text.
		attribute.Int("prompt_tokens_estimate", domain.EstimateTokens(req)),
		attribute.Int("max_tokens", req.MaxTokens),
	}
	if scope.AgentID != uuid.Nil {
		attributes = append(attributes, attribute.String("agent_id", scope.AgentID.String()))
	}

	return s.deps.Tracer.Start(ctx, name, trace.WithAttributes(attributes...))
}

// finishChatSpan records who answered and what it cost, and closes it.
//
// The provider and the model are the *answering* ones rather than the requested ones, which is the
// point: a trace that named the configured provider would hide a fallback, and a fallback is
// exactly what somebody reading the trace is looking for.
func finishChatSpan(span trace.Span, response domain.ChatResponse, attempts int) {
	if span == nil {
		return
	}

	span.SetAttributes(
		attribute.String("provider", response.Provider),
		attribute.String("model", response.Model),
		attribute.Int("input_tokens", response.Usage.InputTokens),
		attribute.Int("output_tokens", response.Usage.OutputTokens),
		attribute.Int("tool_calls", len(response.ToolCalls)),
		attribute.String("finish_reason", string(response.FinishReason)),
		attribute.Int("attempts", attempts),
	)
	span.End()
}

// failChatSpan records why a completion failed and closes it.
func failChatSpan(span trace.Span, attempts int, err error) {
	if span == nil {
		return
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "the model call failed")
	}
	span.SetAttributes(attribute.Int("attempts", attempts))
	span.End()
}

// metric reports one call that reached a provider.
//
// The purpose comes from the request's own metadata rather than from the call site, so a caller that
// labels a call "extraction" is charted as extraction: the label is the caller's intent, and
// inventing one here would file an extraction under the agent's turns and hide what the fleet
// actually spends on each.
func (s *Service) metric(ctx context.Context, provider string, meta domain.RequestMetadata, attempts int, elapsed time.Duration) {
	if s.deps.Metrics == nil {
		return
	}

	purpose := meta.Purpose
	if purpose == "" {
		purpose = domain.PurposeAgent
	}

	s.deps.Metrics.ModelCall(ctx, provider, purpose, attempts > 1, elapsed.Seconds())
}

// startEmbedSpan opens the span for one embedding call.
func (s *Service) startEmbedSpan(ctx context.Context, scope domain.Scope, req domain.EmbedRequest) (context.Context, trace.Span) {
	if s.deps.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}

	attributes := []attribute.KeyValue{
		attribute.String("workspace_id", scope.WorkspaceID.String()),
		attribute.Int("inputs", len(req.Inputs)),
	}
	if scope.AgentID != uuid.Nil {
		attributes = append(attributes, attribute.String("agent_id", scope.AgentID.String()))
	}

	return s.deps.Tracer.Start(ctx, "llm.embed", trace.WithAttributes(attributes...))
}
