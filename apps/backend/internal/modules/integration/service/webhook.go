package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// WebhookURL returns the address a provider should call for one connection.
//
// It is built from a signed reference rather than from the ids themselves: the
// workspace is a UUID that appears in API responses, so an unsigned URL would let
// a genuine delivery for one workspace be replayed against another. The value is
// verified in-process, which is also why the endpoint needs no database read
// before it knows its tenant.
func (s *service) WebhookURL(ctx context.Context, scope domain.Scope, integrationID uuid.UUID, baseURL string) (string, error) {
	if integrationID == uuid.Nil {
		return "", fmt.Errorf("%w: an integration id is required", domain.ErrInvalidInput)
	}
	if s.deps.Refs == nil {
		return "", fmt.Errorf("%w: no reference codec is configured", domain.ErrInvalidInput)
	}

	connection, err := s.deps.Repository.Get(ctx, scope, integrationID)
	if err != nil {
		return "", err
	}

	value, err := s.deps.Refs.SignRef(domain.WebhookRef{
		WorkspaceID:   scope.WorkspaceID,
		IntegrationID: integrationID,
		App:           connection.App,
	})
	if err != nil {
		return "", fmt.Errorf("integration: sign webhook reference: %w", err)
	}

	return strings.TrimRight(baseURL, "/") + "/api/webhooks/" + connection.App + "?ref=" + value, nil
}

// ReceiveWebhook verifies a delivery and queues it, reporting how many events
// were newly accepted. A delivery that was already queued reports zero, which is
// a success: the provider did its job, and telling it otherwise would make it
// retry forever.
//
// Two things are checked and both are needed. The signed reference proves the URL
// was issued by this deployment for a specific connection, which is what names
// the tenant without a session. The adapter's own verification proves the request
// came from the provider, because the URL alone is only a capability.
//
// Nothing slow happens here: the provider is waiting on a 200, and a body that
// takes work to read belongs in the worker.
func (s *service) ReceiveWebhook(ctx context.Context, app string, request domain.WebhookRequest) (int, error) {
	trimmed := strings.TrimSpace(app)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: an app is required", domain.ErrInvalidInput)
	}
	if s.deps.Refs == nil || s.deps.Queue == nil {
		return 0, fmt.Errorf("%w: webhooks are not configured", domain.ErrInvalidInput)
	}

	ref, err := s.deps.Refs.VerifyRef(request.Query["ref"])
	if err != nil {
		return 0, err
	}
	// The URL names the app it was issued for, so a delivery for one application
	// cannot be replayed at another's endpoint.
	if ref.App != trimmed {
		return 0, fmt.Errorf("%w: the reference is for %q", domain.ErrSignatureInvalid, ref.App)
	}

	adapter, ok := s.webhookAdapter(trimmed)
	if !ok {
		return 0, fmt.Errorf("%w: %q", domain.ErrNoWebhook, trimmed)
	}

	events, err := adapter.VerifyWebhook(ctx, request)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		// A genuine request that carries nothing to act on. It is not an error,
		// and the provider should not retry it.
		return 0, fmt.Errorf("%w: the delivery carries no event", domain.ErrInvalidInput)
	}

	// One delivery may carry several events. Each is deduplicated on its own key
	// rather than the batch colliding on one.
	accepted := 0
	// fired is the event kinds this delivery actually added, deduplicated: one
	// delivery carrying the same kind twice starts the routines watching it once.
	fired := map[string]bool{}

	for index, event := range events {
		stored, err := s.deps.Queue.Enqueue(ctx, domain.EnqueueInput{
			WorkspaceID:   ref.WorkspaceID,
			IntegrationID: ref.IntegrationID,
			App:           trimmed,
			DeliveryID:    deliveryID(trimmed, event, request.ReceivedAt.UnixNano(), index, len(events)),
			Event:         event.Event,
			Payload:       event.Payload,
		})
		if err != nil {
			if errors.Is(err, domain.ErrDeliveryExists) {
				// A retry. The delivery is already queued, so the answer is the
				// same as the first time: accepted, and the provider stops.
				s.deps.Logger.Info("a webhook delivery arrived twice",
					zap.String("app", trimmed),
					zap.String("event", event.Event),
				)
				continue
			}
			return accepted, err
		}
		_ = stored
		accepted++
		fired[event.Event] = true
	}

	// The routines a workspace set up for "when this happens" are started here,
	// because this is the moment the event arrived: a trigger routine has no
	// schedule, so nothing else would ever start it.
	//
	// A routine that could not be started does not fail the delivery. The provider
	// is waiting on a 200 and its event is already queued; the delivery stays
	// unclaimed, and the next one of the same kind finds both — which is what makes
	// this self-healing rather than lossy.
	s.startTriggers(ctx, domain.Scope{WorkspaceID: ref.WorkspaceID}, fired)

	return accepted, nil
}

// startTriggers hands each newly queued event kind to the routines watching it.
func (s *service) startTriggers(ctx context.Context, scope domain.Scope, events map[string]bool) {
	if s.deps.Triggers == nil {
		return
	}

	for event := range events {
		started, err := s.deps.Triggers.Fire(ctx, scope, event)
		if err != nil {
			s.deps.Logger.Warn("could not start the routines watching a delivery",
				zap.String("event", event), zap.Error(err))
			continue
		}
		if started > 0 {
			s.deps.Logger.Info("a delivery started the routines watching it",
				zap.String("event", event), zap.Int("routines", started))
		}
	}
}

// deliveryID is the key a delivery is deduplicated on.
//
// The provider's own id is what makes a retry harmless. A provider that sends
// none leaves nothing to deduplicate on, so the arrival time stands in: it keeps
// two deliveries apart, and the only cost is that a retry of an unidentified
// delivery is queued twice rather than once.
func deliveryID(app string, event domain.TriggerEvent, receivedAt int64, index, total int) string {
	id := event.EventID
	if id == "" {
		id = fmt.Sprintf("%s@%d", app, receivedAt)
	}
	if total > 1 {
		// One delivery carrying several events must not collapse them onto one
		// key.
		return fmt.Sprintf("%s#%d", id, index)
	}
	return id
}

// PendingWebhooks returns the oldest deliveries a workspace has not acted on.
func (s *service) PendingWebhooks(ctx context.Context, scope domain.Scope, limit int) ([]domain.QueuedEvent, error) {
	if s.deps.Queue == nil {
		return nil, fmt.Errorf("%w: webhooks are not configured", domain.ErrInvalidInput)
	}
	return s.deps.Queue.Pending(ctx, scope, limit)
}

// ClaimPendingEvents reads and claims the deliveries of one event kind.
//
// It is what a routine watching that event consumes, and the claim happens in the
// same step as the read: two routines watching one event must not both act on one
// delivery.
func (s *service) ClaimPendingEvents(ctx context.Context, scope domain.Scope, event string, limit int) ([]domain.QueuedEvent, error) {
	if s.deps.Queue == nil {
		return nil, fmt.Errorf("%w: webhooks are not configured", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(event) == "" {
		return nil, fmt.Errorf("%w: an event name is required", domain.ErrInvalidInput)
	}

	return s.deps.Queue.ClaimEvents(ctx, scope, strings.TrimSpace(event), limit)
}

// WebhookProcessed marks a delivery as acted on.
func (s *service) WebhookProcessed(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if s.deps.Queue == nil {
		return fmt.Errorf("%w: webhooks are not configured", domain.ErrInvalidInput)
	}
	return s.deps.Queue.Processed(ctx, scope, id)
}

// WebhookFailed records an attempt that did not work, so a delivery that keeps
// failing is visible rather than silently retried forever.
func (s *service) WebhookFailed(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string) error {
	if s.deps.Queue == nil {
		return fmt.Errorf("%w: webhooks are not configured", domain.ErrInvalidInput)
	}
	return s.deps.Queue.Failed(ctx, scope, id, reason)
}

// webhookAdapter finds an adapter that receives pushes.
func (s *service) webhookAdapter(app string) (domain.TriggerAdapter, bool) {
	if s.deps.Registry == nil {
		return nil, false
	}

	adapter, ok := s.deps.Registry.Adapter(app)
	if !ok {
		return nil, false
	}

	trigger, ok := adapter.(domain.TriggerAdapter)
	return trigger, ok
}
