package container

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/ratelimit"
)

// callLimits paces the calls a workspace makes on one integration.
//
// A provider publishes its own quota, and a routine that runs every hour must not
// spend a day's worth of it. The bucket is keyed by the connection rather than by
// the app, because two Gmail accounts are two quotas; the ceiling comes from the
// app, because the quota belongs to the provider.
type callLimits struct {
	limiter *ratelimit.Limiter
	// perMinute is the ceiling per app, already parsed.
	perMinute map[string]int
	// fallback applies to an app with no ceiling of its own. Zero means unlimited.
	fallback int
	logger   *zap.Logger
}

// newCallLimits builds the limiter from configuration.
//
// A malformed pair is refused rather than ignored: a ceiling that silently did
// not apply is a quota that silently gets spent.
func newCallLimits(cfg *config.Config, limiter *ratelimit.Limiter, logger *zap.Logger) (*callLimits, error) {
	if limiter == nil {
		return nil, nil
	}

	perMinute, err := parseRateLimits(cfg.Integrations.RateLimits)
	if err != nil {
		return nil, err
	}

	limits := &callLimits{
		limiter:   limiter,
		perMinute: perMinute,
		fallback:  cfg.Integrations.DefaultCallsPerMinute,
		logger:    logger,
	}

	if len(perMinute) > 0 || limits.fallback > 0 {
		logger.Info("integration calls are paced",
			zap.Int("default_per_minute", limits.fallback),
			zap.Int("apps_with_their_own_ceiling", len(perMinute)),
		)
	}

	return limits, nil
}

// parseRateLimits reads the `app:perMinute` pairs.
func parseRateLimits(encoded string) (map[string]int, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return nil, nil
	}

	limits := map[string]int{}
	for _, pair := range strings.Split(trimmed, ",") {
		entry := strings.TrimSpace(pair)
		if entry == "" {
			continue
		}

		app, value, found := strings.Cut(entry, ":")
		if !found {
			return nil, fmt.Errorf("integration: rate limit %q must be `app:perMinute`", entry)
		}

		app = strings.TrimSpace(app)
		perMinute, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("integration: rate limit %q has no number: %w", entry, err)
		}
		if app == "" || perMinute <= 0 {
			return nil, fmt.Errorf("integration: rate limit %q must name an app and a positive number", entry)
		}

		limits[app] = perMinute
	}

	if len(limits) == 0 {
		return nil, nil
	}
	return limits, nil
}

// Allow takes one token for a connection.
//
// An app with no ceiling is not paced at all, which is the honest reading of a
// deployment that configured none: pacing it at some invented number would refuse
// work nobody asked to have refused.
func (c *callLimits) Allow(ctx context.Context, app string, integrationID uuid.UUID) (integrationdomain.LimitDecision, error) {
	if c == nil || c.limiter == nil {
		return integrationdomain.LimitDecision{Allowed: true}, nil
	}

	perMinute := c.ceiling(app)
	if perMinute <= 0 {
		return integrationdomain.LimitDecision{Allowed: true}, nil
	}

	// The bucket is the connection's, so two accounts of one provider do not
	// share a quota.
	key := "integration:" + app + ":" + integrationID.String()

	result, err := c.limiter.Allow(ctx, key, perMinute, float64(perMinute)/60)
	if err != nil {
		// A limiter that cannot answer must not stop the work: Redis being
		// unreachable is a reason to lose pacing, not a reason to lose the task.
		c.logger.Warn("the call limiter could not answer, so the call is allowed",
			zap.String("app", app),
			zap.Error(err),
		)
		return integrationdomain.LimitDecision{Allowed: true}, nil
	}

	return integrationdomain.LimitDecision{
		Allowed:    result.Allowed,
		RetryAfter: result.RetryAfter,
		PerMinute:  perMinute,
	}, nil
}

// ceiling is the calls a minute an app allows.
func (c *callLimits) ceiling(app string) int {
	if perMinute, ok := c.perMinute[app]; ok {
		return perMinute
	}
	return c.fallback
}

// compile-time check that the adapter satisfies the port it is wired to.
var _ integrationdomain.CallLimiter = (*callLimits)(nil)
