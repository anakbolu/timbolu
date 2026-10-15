package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// The lease that decides which gateway instance owns a number.
//
// Why this exists at all: a WhatsApp session driven by two processes sends every
// message twice and gets the number banned. So one number has one owner, and
// ownership is a fact with a deadline rather than a row in a table — an instance
// that dies must be replaced in seconds, not by an operator cleaning up.
//
// Why Redis: the value is liveness, not a record. A lease that survived the
// process that held it would be exactly the bug this prevents.
//
// The two scripts are atomic for the same reason the token bucket is: an acquire
// that read the key and then wrote it would let two instances both believe they
// hold the number, and that race is the one thing this must not have.

// acquireScript takes the lease when it is free, or extends it when it is
// already ours. It returns 1 when we hold it and 0 when somebody else does.
//
// KEYS[1] lease key
// ARGV[1] owner
// ARGV[2] ttl milliseconds
var acquireScript = redis.NewScript(`
local owner = redis.call('GET', KEYS[1])
if owner == false then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
if owner == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`)

// renewScript extends a lease only when it is still ours. It returns remaining
// milliseconds when we hold it and 0 when we do not, which is the answer a
// takeover produces — and a takeover is precisely the case a caller must notice.
//
// KEYS[1] lease key
// ARGV[1] owner
// ARGV[2] ttl milliseconds
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return tonumber(ARGV[2])
`)

// releaseScript gives the lease up only when it is ours, so a released instance
// cannot delete the lease a successor has already taken.
//
// KEYS[1] lease key
// ARGV[1] owner
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// LeasePrefix is what every lease key begins with, so a fleet's claims are
// recognisable in Redis and a test can count them.
const LeasePrefix = "whatsapp:lease:"

// RedisLease is the Redis-backed lease.
type RedisLease struct {
	client *redis.Client
}

// NewRedisLease builds the lease. A nil client answers "not held" rather than
// panicking: a deployment with no Redis cannot run a gateway anyway, and the
// engine says so at startup.
func NewRedisLease(client *redis.Client) *RedisLease {
	return &RedisLease{client: client}
}

// Acquire takes the lease when it is free or already ours.
func (l *RedisLease) Acquire(ctx context.Context, numberID uuid.UUID, owner string, ttl time.Duration) (bool, error) {
	if l == nil || l.client == nil {
		return false, nil
	}

	held, err := acquireScript.Run(ctx, l.client, []string{LeaseKey(numberID)}, owner, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("whatsapp: acquire lease: %w", err)
	}
	return held == 1, nil
}

// Renew extends a lease we hold, and answers false when we no longer hold it.
func (l *RedisLease) Renew(ctx context.Context, numberID uuid.UUID, owner string, ttl time.Duration) (bool, error) {
	if l == nil || l.client == nil {
		return false, nil
	}

	remaining, err := renewScript.Run(ctx, l.client, []string{LeaseKey(numberID)}, owner, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("whatsapp: renew lease: %w", err)
	}
	return remaining > 0, nil
}

// Release gives the lease up, so a takeover is immediate rather than a wait for
// the timeout.
func (l *RedisLease) Release(ctx context.Context, numberID uuid.UUID, owner string) error {
	if l == nil || l.client == nil {
		return nil
	}

	if _, err := releaseScript.Run(ctx, l.client, []string{LeaseKey(numberID)}, owner).Int(); err != nil {
		return fmt.Errorf("whatsapp: release lease: %w", err)
	}
	return nil
}

// LeaseKey is the Redis key one number's lease lives under.
func LeaseKey(numberID uuid.UUID) string { return LeasePrefix + numberID.String() }

// Compile-time check that the Redis lease satisfies the port the engine uses.
var _ leasePort = (*RedisLease)(nil)

// leasePort mirrors `whatsappdomain.Lease` so this package can assert the shape
// without the engine's own wiring in the way.
type leasePort interface {
	Acquire(ctx context.Context, numberID uuid.UUID, owner string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, numberID uuid.UUID, owner string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, numberID uuid.UUID, owner string) error
}
