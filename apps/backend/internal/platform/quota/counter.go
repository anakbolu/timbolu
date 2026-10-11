// Package quota keeps the live token spend of a workspace in Redis.
//
// The ledger in Postgres is the record of what was spent; this counter is the
// fast read the pre-flight check makes before every call, so a workspace that
// exhausted its allowance is rejected before the provider is billed. Because it
// is only a cache, a Redis outage costs accuracy and not correctness: the caller
// can always recompute the total from the ledger.
package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces the period counters, so they can be flushed together when
// the period rolls over.
const keyPrefix = "quota:spent:"

// costKeyPrefix namespaces the day counters, which answer a different question:
// how much a workspace has spent today, in micro-rupiah. A runaway task is
// stopped by the day before it can consume a month.
const costKeyPrefix = "quota:cost:"

// ttl keeps a counter a little longer than the period it belongs to, so the last
// day of a period still reads the right total.
const ttl = 40 * 24 * time.Hour

// addScript increments the counter and gives it a lifetime if it has none. Both
// steps must be atomic: a key that is incremented but never expires would leak
// one key per workspace per period forever.
var addScript = redis.NewScript(`
local total = redis.call('INCRBY', KEYS[1], ARGV[1])
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return total
`)

// Counter is the Redis-backed live spend of a workspace.
type Counter struct {
	client *redis.Client
	clock  func() time.Time
}

// New builds the counter. A nil client is allowed: Spent and Add then report
// that the counter is not configured, and the caller falls back to the ledger.
func New(client *redis.Client) *Counter {
	return &Counter{client: client, clock: time.Now}
}

// SpentCostToday returns the cost recorded for the workspace today.
func (c *Counter) SpentCostToday(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if c == nil || c.client == nil {
		return 0, fmt.Errorf("quota: redis is not configured")
	}

	total, err := c.client.Get(ctx, c.costKey(workspaceID)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Nothing spent today, which is the normal state of a new day.
			return 0, nil
		}
		return 0, fmt.Errorf("quota: read today's cost: %w", err)
	}
	return total, nil
}

// AddCost records what one call cost, in micro-rupiah.
func (c *Counter) AddCost(ctx context.Context, workspaceID uuid.UUID, costMicros int64) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("quota: redis is not configured")
	}
	if costMicros <= 0 {
		// A call that cost nothing must not create a counter.
		return nil
	}

	if _, err := addScript.Run(ctx, c.client, []string{c.costKey(workspaceID)}, costMicros, ttl.Milliseconds()).Result(); err != nil {
		return fmt.Errorf("quota: add today's cost: %w", err)
	}
	return nil
}

// Spent returns the tokens recorded for the workspace in the current period.
func (c *Counter) Spent(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	if c == nil || c.client == nil {
		return 0, fmt.Errorf("quota: redis is not configured")
	}

	total, err := c.client.Get(ctx, c.key(workspaceID)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// Nothing spent yet, which is the normal state of a new workspace.
			return 0, nil
		}
		return 0, fmt.Errorf("quota: read spend: %w", err)
	}
	return total, nil
}

// Add records tokens spent by one call.
func (c *Counter) Add(ctx context.Context, workspaceID uuid.UUID, tokens int64) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("quota: redis is not configured")
	}
	if tokens <= 0 {
		// A call that reported no usage must not create a counter.
		return nil
	}

	if _, err := addScript.Run(ctx, c.client, []string{c.key(workspaceID)}, tokens, ttl.Milliseconds()).Result(); err != nil {
		return fmt.Errorf("quota: add spend: %w", err)
	}
	return nil
}

// Reset clears the counter of a workspace, which the period rollover and the
// tests use.
func (c *Counter) Reset(ctx context.Context, workspaceID uuid.UUID) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("quota: redis is not configured")
	}
	if err := c.client.Del(ctx, c.key(workspaceID)).Err(); err != nil {
		return fmt.Errorf("quota: reset spend: %w", err)
	}
	return nil
}

// key is the period the spend belongs to. The period is the calendar month in
// Jakarta, which is what the product sells today; EPIC 12 (#97) points it at the
// invoice period once subscriptions exist.
func (c *Counter) key(workspaceID uuid.UUID) string {
	now := time.Now()
	if c != nil && c.clock != nil {
		now = c.clock()
	}
	jakarta := now.In(jakartaLocation)
	return fmt.Sprintf("%s%s:%s", keyPrefix, workspaceID, jakarta.Format("2006-01"))
}

// costKey is the day the spend belongs to. The day is the calendar day in
// Jakarta, which is where the customers are and therefore where their day
// boundary falls.
func (c *Counter) costKey(workspaceID uuid.UUID) string {
	now := time.Now()
	if c != nil && c.clock != nil {
		now = c.clock()
	}
	jakarta := now.In(jakartaLocation)
	return fmt.Sprintf("%s%s:%s", costKeyPrefix, workspaceID, jakarta.Format("2006-01-02"))
}

// jakartaLocation is where the customers are, so their month boundary is the one
// the quota follows.
var jakartaLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// A container without the time zone database: UTC+7 is the correct
		// offset and never changes, so the fallback is exact.
		return time.FixedZone("WIB", 7*60*60)
	}
	return location
}()

// SetClock replaces the clock, which lets a test cross a month boundary without
// waiting.
func (c *Counter) SetClock(clock func() time.Time) {
	if c != nil {
		c.clock = clock
	}
}
