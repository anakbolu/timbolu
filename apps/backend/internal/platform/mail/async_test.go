package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeSender stands in for a mail server. It records what it was asked to send,
// fails its first `failures` attempts, and can be held until `release` is closed
// so a blocking server can be simulated. `started` is closed once the first
// attempt has begun, which lets a test wait for a worker to be busy rather than
// race it.
type fakeSender struct {
	mu       sync.Mutex
	sent     []Message
	attempts int
	failures int
	release  chan struct{}
	started  chan struct{}
	once     sync.Once
}

func (f *fakeSender) Send(ctx context.Context, message Message) error {
	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	release := f.release
	failures := f.failures
	f.mu.Unlock()

	f.once.Do(func() {
		if f.started != nil {
			close(f.started)
		}
	})

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if attempt <= failures {
		return errors.New("server said no")
	}

	f.mu.Lock()
	f.sent = append(f.sent, message)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSender) attemptsMade() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// logCollector records what the sender reported. It is safe to read while the
// workers are still writing.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCollector) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, format+" "+fmt.Sprint(args...))
}

func (c *logCollector) collected() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func testMessage() Message {
	return Message{
		To:      "owner@example.com",
		Subject: "Verifikasi email Bolu",
		Text:    "https://app.example.com/verify?token=abc",
	}
}

// TestAsyncSenderReturnsWithoutWaitingForDelivery is the whole point of the
// wrapper: the request that triggers a message never waits for the mail server,
// which may be unreachable.
func TestAsyncSenderReturnsWithoutWaitingForDelivery(t *testing.T) {
	release := make(chan struct{})
	inner := &fakeSender{release: release}
	sender := NewAsyncSender(inner, AsyncOptions{})

	start := time.Now()
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	elapsed := time.Since(start)

	require.Less(t, elapsed, 100*time.Millisecond, "Send must not wait for the delivery")
	close(release)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
	require.Equal(t, 1, inner.count(), "the queued message must still be delivered")
}

// TestAsyncSenderIgnoresTheRequestContext: the request is over — and its context
// cancelled — by the time the message leaves, so delivery must not depend on it.
func TestAsyncSenderIgnoresTheRequestContext(t *testing.T) {
	inner := &fakeSender{}
	sender := NewAsyncSender(inner, AsyncOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, sender.Send(ctx, testMessage()))
	cancel()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	require.NoError(t, sender.Close(drainCtx))
	require.Equal(t, 1, inner.count(), "a cancelled request must not cancel its message")
}

func TestAsyncSenderRetriesUntilTheMessageIsAccepted(t *testing.T) {
	inner := &fakeSender{failures: 2}
	sender := NewAsyncSender(inner, AsyncOptions{
		Attempts: 3,
		Backoff:  time.Millisecond,
	})

	require.NoError(t, sender.Send(context.Background(), testMessage()))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
	require.Equal(t, 1, inner.count())
	require.Equal(t, 3, inner.attemptsMade(), "two failures then the third attempt succeeds")
}

func TestAsyncSenderGivesUpAfterTheConfiguredAttempts(t *testing.T) {
	logs := &logCollector{}
	inner := &fakeSender{failures: 10}
	sender := NewAsyncSender(inner, AsyncOptions{
		Attempts: 2,
		Backoff:  time.Millisecond,
		Errorf:   logs.logf,
	})

	require.NoError(t, sender.Send(context.Background(), testMessage()))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
	require.Equal(t, 2, inner.attemptsMade())
	require.NotEmpty(t, logs.collected(), "giving up must be visible in the log")
}

// TestAsyncSenderReturnsAValidationErrorSynchronously: a malformed message is a
// bug in the request rather than a delivery problem, so the caller is told.
func TestAsyncSenderReturnsAValidationErrorSynchronously(t *testing.T) {
	inner := &fakeSender{}
	sender := NewAsyncSender(inner, AsyncOptions{})

	require.Error(t, sender.Send(context.Background(), Message{Subject: "no recipient"}))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
	require.Zero(t, inner.attemptsMade(), "a rejected message is never attempted")
}

// TestAsyncSenderDropsRatherThanBlocksOnAFullQueue: the caller is a request, and
// a request must not wait for a slow mail server.
func TestAsyncSenderDropsRatherThanBlocksOnAFullQueue(t *testing.T) {
	release := make(chan struct{})
	inner := &fakeSender{release: release, started: make(chan struct{})}
	logs := &logCollector{}
	sender := NewAsyncSender(inner, AsyncOptions{
		QueueSize: 1,
		Workers:   1,
		Errorf:    logs.logf,
	})

	// Wait for the single worker to be busy with the first message, so what the
	// next two do is the queue's behaviour rather than a race with the worker.
	start := time.Now()
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.Less(t, time.Since(start), 100*time.Millisecond)
	<-inner.started

	// The second message fills the queue; the third has nowhere to go.
	for range 2 {
		start := time.Now()
		require.NoError(t, sender.Send(context.Background(), testMessage()))
		require.Less(t, time.Since(start), 100*time.Millisecond, "a full queue must not block the caller")
	}

	lines := logs.collected()
	require.Len(t, lines, 1, "the dropped message must be reported")
	require.Contains(t, lines[0], "queue is full")

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
}

// TestAsyncSenderCloseDrainsTheQueue: a verification link queued by the last
// request before shutdown still leaves.
func TestAsyncSenderCloseDrainsTheQueue(t *testing.T) {
	inner := &fakeSender{}
	sender := NewAsyncSender(inner, AsyncOptions{Workers: 1})

	for range 5 {
		require.NoError(t, sender.Send(context.Background(), testMessage()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
	require.Equal(t, 5, inner.count(), "every queued message is delivered before Close returns")

	// After Close a message is dropped rather than delivered.
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.Equal(t, 5, inner.count())
}

func TestAsyncSenderCloseRespectsItsDeadline(t *testing.T) {
	release := make(chan struct{})
	inner := &fakeSender{release: release}
	sender := NewAsyncSender(inner, AsyncOptions{Workers: 1})
	require.NoError(t, sender.Send(context.Background(), testMessage()))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, sender.Close(ctx), context.DeadlineExceeded)
	close(release)
}

func TestAsyncSenderAppliesTheAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	inner := &fakeSender{release: release}
	logs := &logCollector{}
	sender := NewAsyncSender(inner, AsyncOptions{
		Attempts:       1,
		AttemptTimeout: 50 * time.Millisecond,
		Errorf:         logs.logf,
	})

	require.NoError(t, sender.Send(context.Background(), testMessage()))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))
	require.NotEmpty(t, logs.collected(), "an attempt that ran out of time must be reported")
	close(release)
}

// TestAsyncSenderNeverLogsTheBody: the body carries the one-time token, so a
// failure is reported with the recipient and the subject only.
func TestAsyncSenderNeverLogsTheBody(t *testing.T) {
	inner := &fakeSender{failures: 1}
	logs := &logCollector{}
	sender := NewAsyncSender(inner, AsyncOptions{
		Attempts: 1,
		Errorf:   logs.logf,
	})

	require.NoError(t, sender.Send(context.Background(), testMessage()))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sender.Close(ctx))

	lines := logs.collected()
	require.NotEmpty(t, lines)
	for _, line := range lines {
		require.NotContains(t, line, "verify?token=", "the body must never be logged")
		require.Contains(t, line, "owner@example.com", "the recipient must be in the report")
	}
}
