package service

import (
	"context"
	"fmt"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// streamBuffer is how many events a consumer may fall behind before the
// provider's stream is throttled. One answer is a handful of events, so the
// buffer covers a slow renderer without holding a whole response in memory.
const streamBuffer = 16

// Chat runs one completion, falling back to the next provider when the one it
// chose fails in a way another might survive.
//
// The rule is deliberately narrow: a rate limit, an outage, a provider-side
// quota, or a model that cannot do what the request asks moves to the next
// provider. A malformed request fails everywhere, so it returns immediately
// rather than burning the whole chain.
func (s *Service) Chat(ctx context.Context, scope domain.Scope, req domain.ChatRequest) (domain.ChatResponse, error) {
	if err := s.Check(ctx, scope.WorkspaceID); err != nil {
		return domain.ChatResponse{}, err
	}

	chain, err := s.Resolve(ctx, scope)
	if err != nil {
		return domain.ChatResponse{}, err
	}

	var lastErr error

	for _, cfg := range chain {
		provider, err := s.deps.Factory.Build(cfg)
		if err != nil {
			// A configuration this deployment cannot serve is not something the
			// next provider fixes.
			return domain.ChatResponse{}, err
		}

		if err := provider.Capabilities().Supports(req); err != nil {
			lastErr = fmt.Errorf("llm: %s tidak bisa melayani permintaan: %w", cfg.Name, err)
			if tryNext(err) {
				continue
			}
			return domain.ChatResponse{}, lastErr
		}

		response, err := provider.Chat(ctx, req)
		if err != nil {
			lastErr = fmt.Errorf("llm: %s (%s): %w", cfg.Name, cfg.Model, err)
			if tryNext(err) {
				continue
			}
			return domain.ChatResponse{}, lastErr
		}

		// The provider that actually answered is what the ledger stores, so a
		// fallback is visible in the cost report rather than invisible.
		response.Provider = cfg.Adapter
		if response.Model == "" {
			response.Model = cfg.Model
		}

		if err := s.record(ctx, scope, cfg, response, req.Metadata); err != nil {
			return domain.ChatResponse{}, err
		}

		return response, nil
	}

	return domain.ChatResponse{}, lastErr
}

// Stream runs one completion and reports its progress.
//
// Falling back only happens before the first byte: once the caller has seen a
// fragment of an answer, restarting on another provider would duplicate it, so a
// later failure is reported as an error event and the caller decides what to do.
func (s *Service) Stream(ctx context.Context, scope domain.Scope, req domain.ChatRequest) (<-chan domain.StreamEvent, error) {
	if err := s.Check(ctx, scope.WorkspaceID); err != nil {
		return nil, err
	}

	chain, err := s.Resolve(ctx, scope)
	if err != nil {
		return nil, err
	}

	var lastErr error

	for _, cfg := range chain {
		provider, err := s.deps.Factory.Build(cfg)
		if err != nil {
			return nil, err
		}

		if err := provider.Capabilities().Supports(req); err != nil {
			lastErr = fmt.Errorf("llm: %s tidak bisa melayani permintaan: %w", cfg.Name, err)
			if tryNext(err) {
				continue
			}
			return nil, lastErr
		}

		events, err := provider.Stream(ctx, req)
		if err != nil {
			lastErr = fmt.Errorf("llm: %s (%s): %w", cfg.Name, cfg.Model, err)
			if tryNext(err) {
				continue
			}
			return nil, lastErr
		}

		// The first event is the last chance to switch providers: an error
		// before it means the provider never started answering.
		first, ok := <-events
		if !ok {
			lastErr = fmt.Errorf("llm: %s closed the stream without an event: %w", cfg.Name, domain.ErrProviderUnavailable)
			continue
		}
		if first.Type == domain.EventError && tryNext(first.Err) {
			lastErr = fmt.Errorf("llm: %s (%s): %w", cfg.Name, cfg.Model, first.Err)
			continue
		}

		out := make(chan domain.StreamEvent, streamBuffer)
		go s.pump(ctx, out, scope, cfg, req.Metadata, first, events)
		return out, nil
	}

	return nil, lastErr
}

// pump forwards one provider's events and records the cost when the answer
// finishes.
//
// The record is written before the terminal event is handed over, so a caller
// that sees a finished answer is looking at a stream whose cost is already in the
// ledger. When the write fails the stream ends with an error instead of a
// success: a call that cannot be billed is not one to report as done.
//
// The first event is passed in because Stream already read it to decide whether
// to fall back.
func (s *Service) pump(
	ctx context.Context,
	out chan<- domain.StreamEvent,
	scope domain.Scope,
	cfg domain.ProviderConfig,
	meta domain.RequestMetadata,
	first domain.StreamEvent,
	events <-chan domain.StreamEvent,
) {
	defer close(out)

	var usage domain.Usage

	// handle forwards one event. It reports whether the stream is over.
	handle := func(event domain.StreamEvent) bool {
		if event.Type == domain.EventUsage && event.Usage != nil {
			// The counts arrive before the terminal event, so the ledger row can
			// carry them.
			usage = *event.Usage
			return !send(ctx, out, event)
		}

		if event.Type != domain.EventDone && event.Type != domain.EventError {
			return !send(ctx, out, event)
		}

		if err := s.record(ctx, scope, cfg, s.response(cfg, usage), meta); err != nil {
			send(ctx, out, domain.StreamEvent{Type: domain.EventError, Err: err})
			return true
		}
		send(ctx, out, event)
		return true
	}

	if handle(first) {
		return
	}
	for event := range events {
		if handle(event) {
			return
		}
	}

	// The channel closed without a terminal event. Reporting an error is the
	// honest answer: the caller must not treat a truncated answer as complete.
	// The row is still written, with whatever counts were reported, so the call
	// is not missing from the ledger.
	if err := s.record(ctx, scope, cfg, s.response(cfg, usage), meta); err != nil {
		send(ctx, out, domain.StreamEvent{Type: domain.EventError, Err: err})
		return
	}
	send(ctx, out, domain.StreamEvent{
		Type: domain.EventError,
		Err:  fmt.Errorf("llm: %s closed the stream without a final event: %w", cfg.Name, domain.ErrProviderUnavailable),
	})
}

// response names the provider and the model that answered, which is what the
// ledger records when a fallback was used.
func (s *Service) response(cfg domain.ProviderConfig, usage domain.Usage) domain.ChatResponse {
	return domain.ChatResponse{Usage: usage, Model: cfg.Model, Provider: cfg.Adapter}
}

// send hands one event to the consumer, giving up when the caller's context ends
// so an abandoned stream cannot leak the goroutine.
func send(ctx context.Context, out chan<- domain.StreamEvent, event domain.StreamEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// record writes one usage row and moves the live counter.
//
// The counter is only a cache in front of the ledger, so a failed increment is
// not an error: the row above is the record, and the quota check recomputes from
// the ledger when the counter is unavailable.
func (s *Service) record(ctx context.Context, scope domain.Scope, cfg domain.ProviderConfig, response domain.ChatResponse, meta domain.RequestMetadata) error {
	if s.deps.Recorder == nil {
		return fmt.Errorf("%w: no usage recorder is configured", domain.ErrUsageNotRecorded)
	}

	purpose := meta.Purpose
	if purpose == "" {
		purpose = domain.PurposeAgent
	}

	entry := domain.UsageEntry{
		WorkspaceID: scope.WorkspaceID,
		AgentID:     scope.AgentID,
		TaskID:      meta.TaskID,
		Provider:    cfg.Adapter,
		Model:       response.Model,
		Purpose:     purpose,
		Usage:       response.Usage,
		CostMicros:  s.cost(response.Model, response.Usage),
	}

	if err := s.deps.Recorder.Record(ctx, entry); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrUsageNotRecorded, err)
	}

	if s.deps.Counter != nil {
		_ = s.deps.Counter.Add(ctx, scope.WorkspaceID, int64(response.Usage.Total()))
		// The day counter is moved with the same row, so the two windows cannot
		// disagree about what was spent.
		_ = s.deps.Counter.AddCost(ctx, scope.WorkspaceID, entry.CostMicros)
	}

	return nil
}

func (s *Service) cost(model string, usage domain.Usage) int64 {
	if s.deps.Costs == nil {
		return 0
	}
	return s.deps.Costs.Cost(model, usage)
}
