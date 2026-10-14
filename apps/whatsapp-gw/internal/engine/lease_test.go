package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// E9.3's gate is this file: one number has one owner, and when the owner dies the
// next claimant takes over in seconds.
//
// The lease is tested against a real Redis because that is what makes it a lease
// rather than a variable: the property is "the value expires on its own", and a
// fake would only restate the test. The Redis client is pointed at the compose
// server, the way the rest of this deployment's integration tests reach theirs.

// redisFor opens the deployment's Redis, or skips the test when there is none.
func redisFor(t *testing.T) *redis.Client {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: redisAddr()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		require.NoError(t, client.Close())
		t.Skipf("no redis at %s: %v", redisAddr(), err)
	}

	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

// redisAddr is where the deployment's Redis listens. It follows the same
// convention the compose stack uses, and REDIS_ADDR overrides it.
func redisAddr() string {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:6379"
}

// TestOneInstanceHoldsTheNumber is the first half of the gate: the second
// claimant is refused while the first holds it, and refusing is the answer that
// matters — two clients on one session send everything twice.
func TestOneInstanceHoldsTheNumber(t *testing.T) {
	lease := NewRedisLease(redisFor(t))
	numberID := uuid.New()
	ctx := t.Context()
	t.Cleanup(func() { _ = lease.Release(ctx, numberID, "first") })

	held, err := lease.Acquire(ctx, numberID, "first", 30*time.Second)
	require.NoError(t, err)
	require.True(t, held)

	// The same instance asking again is not a conflict: a restart within the TTL
	// would otherwise refuse to serve its own number.
	held, err = lease.Acquire(ctx, numberID, "first", 30*time.Second)
	require.NoError(t, err)
	require.True(t, held, "the holder may re-take its own lease")

	held, err = lease.Acquire(ctx, numberID, "second", 30*time.Second)
	require.NoError(t, err)
	require.False(t, held, "a second instance must be refused while the first holds the number")
}

// TestADeadOwnerIsReplacedInSeconds is the second half, and the one the epic
// names: an instance that stops renewing loses the number, and the next claimant
// gets it without anybody cleaning up.
func TestADeadOwnerIsReplacedInSeconds(t *testing.T) {
	lease := NewRedisLease(redisFor(t))
	numberID := uuid.New()
	ctx := t.Context()

	// A deliberately short lease so the takeover is observed rather than waited
	// for. The production TTL is thirty seconds, and the property is the same.
	held, err := lease.Acquire(ctx, numberID, "dying", 300*time.Millisecond)
	require.NoError(t, err)
	require.True(t, held)

	// The dying instance renews once, which is what a live one does.
	held, err = lease.Renew(ctx, numberID, "dying", 300*time.Millisecond)
	require.NoError(t, err)
	require.True(t, held)

	// Then it stops. Nothing releases the lease — that is what a crash looks
	// like — and the successor has to be able to take it anyway.
	time.Sleep(450 * time.Millisecond)

	held, err = lease.Acquire(ctx, numberID, "successor", 30*time.Second)
	require.NoError(t, err)
	require.True(t, held, "the lease must expire on its own, or a crash leaves the number offline forever")

	// And the old owner finds out, which is what makes it stand down rather than
	// fight for a session it no longer owns.
	held, err = lease.Renew(ctx, numberID, "dying", 30*time.Second)
	require.NoError(t, err)
	require.False(t, held, "a successor's lease must not be renewable by the instance it replaced")
}

// TestACleanShutdownHandsOverImmediately: a released lease is free at once, so a
// rolling restart takes milliseconds rather than waiting out the TTL.
func TestACleanShutdownHandsOverImmediately(t *testing.T) {
	lease := NewRedisLease(redisFor(t))
	numberID := uuid.New()
	ctx := t.Context()

	held, err := lease.Acquire(ctx, numberID, "leaving", time.Hour)
	require.NoError(t, err)
	require.True(t, held)

	require.NoError(t, lease.Release(ctx, numberID, "leaving"))

	held, err = lease.Acquire(ctx, numberID, "arriving", time.Hour)
	require.NoError(t, err)
	require.True(t, held, "a released lease is free immediately")
}

// TestAReleaseCannotStealASuccessorsLease: the instance that lost a lease must not
// delete the one its successor holds. That would leave the number with no owner
// and a third instance free to claim it while the successor is still connected —
// two clients, which is the failure this whole file exists to prevent.
func TestAReleaseCannotStealASuccessorsLease(t *testing.T) {
	lease := NewRedisLease(redisFor(t))
	numberID := uuid.New()
	ctx := t.Context()
	t.Cleanup(func() { _ = lease.Release(ctx, numberID, "successor") })

	held, err := lease.Acquire(ctx, numberID, "predecessor", 300*time.Millisecond)
	require.NoError(t, err)
	require.True(t, held)

	time.Sleep(450 * time.Millisecond)

	held, err = lease.Acquire(ctx, numberID, "successor", time.Hour)
	require.NoError(t, err)
	require.True(t, held)

	// The predecessor's own cleanup runs late, as it would after a pause.
	require.NoError(t, lease.Release(ctx, numberID, "predecessor"))

	held, err = lease.Acquire(ctx, numberID, "third", time.Hour)
	require.NoError(t, err)
	require.False(t, held, "the predecessor's release must not have freed the successor's lease")
}

// TestTwoNumbersDoNotShareALease: the key names the number, so owning one says
// nothing about another.
func TestTwoNumbersDoNotShareALease(t *testing.T) {
	lease := NewRedisLease(redisFor(t))
	ctx := t.Context()
	first, second := uuid.New(), uuid.New()
	t.Cleanup(func() {
		_ = lease.Release(ctx, first, "instance")
		_ = lease.Release(ctx, second, "instance")
	})

	held, err := lease.Acquire(ctx, first, "instance", time.Hour)
	require.NoError(t, err)
	require.True(t, held)

	held, err = lease.Acquire(ctx, second, "instance", time.Hour)
	require.NoError(t, err)
	require.True(t, held, "a different number is a different lease")

	require.NotEqual(t, LeaseKey(first), LeaseKey(second))
}

// TestNoRedisIsNotAnError: a deployment without Redis cannot run a gateway, but
// the lease itself answers "not held" rather than panicking, so the process says
// what is missing instead of crashing on a nil dereference.
func TestNoRedisIsNotAnError(t *testing.T) {
	lease := NewRedisLease(nil)
	numberID := uuid.New()
	ctx := t.Context()

	held, err := lease.Acquire(ctx, numberID, "instance", time.Minute)
	require.NoError(t, err)
	require.False(t, held)

	held, err = lease.Renew(ctx, numberID, "instance", time.Minute)
	require.NoError(t, err)
	require.False(t, held)

	require.NoError(t, lease.Release(ctx, numberID, "instance"))
}
