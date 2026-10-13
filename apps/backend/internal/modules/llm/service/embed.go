package service

import (
	"context"
	"fmt"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// Embed turns text into vectors, skipping the providers that have no embedding
// endpoint at all.
//
// The rule is a step stricter than Chat's: a provider that advertises no
// embeddings is passed over without being called, because asking a completion
// API for a vector is a request that cannot succeed. If nothing in the chain can
// embed, the caller is told so — which is a configuration fact a deployment
// fixes by pointing an [OI]-compatible provider at the workspace, not something
// a retry cures.
func (s *Service) Embed(ctx context.Context, scope domain.Scope, req domain.EmbedRequest) (domain.EmbedResponse, error) {
	if len(req.Inputs) == 0 {
		return domain.EmbedResponse{}, fmt.Errorf("%w: at least one input is required", domain.ErrInvalidRequest)
	}

	if err := s.Check(ctx, scope.WorkspaceID); err != nil {
		return domain.EmbedResponse{}, err
	}

	chain, err := s.Resolve(ctx, scope)
	if err != nil {
		return domain.EmbedResponse{}, err
	}

	var lastErr error

	for _, cfg := range chain {
		provider, err := s.deps.Factory.Build(cfg)
		if err != nil {
			// A configuration this deployment cannot serve is not something the
			// next provider fixes.
			return domain.EmbedResponse{}, err
		}
		if !provider.Capabilities().Embeddings {
			lastErr = fmt.Errorf("llm: %s tidak menyediakan embedding: %w", cfg.Name, domain.ErrUnsupported)
			continue
		}

		// The embedding model is a deployment fact, not something a workspace
		// configures per provider: the key is the workspace's, the model is the
		// one whose vectors fit the schema.
		call := req
		call.Model = firstNonEmpty(req.Model, s.deps.EmbeddingModel, cfg.Model)

		response, err := provider.Embed(ctx, call)
		if err != nil {
			lastErr = fmt.Errorf("llm: %s (%s): %w", cfg.Name, call.Model, err)
			if tryNext(err) {
				continue
			}
			return domain.EmbedResponse{}, lastErr
		}

		response.Provider = cfg.Adapter
		if response.Model == "" {
			response.Model = call.Model
		}

		if err := s.record(ctx, scope, cfg, domain.ChatResponse{
			Model: response.Model,
			Usage: response.Usage,
		}, req.Metadata); err != nil {
			return domain.EmbedResponse{}, err
		}

		return response, nil
	}

	if lastErr == nil {
		return domain.EmbedResponse{}, domain.ErrNoProvider
	}
	return domain.EmbedResponse{}, lastErr
}

// firstNonEmpty returns the first value that carries something.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
