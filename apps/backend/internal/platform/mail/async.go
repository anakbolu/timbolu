package mail

import (
	"context"
	"sync"
	"time"
)

// Delivery defaults. A queue of 256 with two workers absorbs a burst of sign-ups
// without holding a request, and three attempts over roughly three seconds ride
// out a mail server that is restarting.
const (
	defaultAsyncQueueSize      = 256
	defaultAsyncWorkers        = 2
	defaultAsyncAttemptTimeout = 15 * time.Second
	defaultAsyncAttempts       = 3
	defaultAsyncBackoff        = time.Second
)

// AsyncOptions tunes an AsyncSender. Every field has a default, so the zero value
// is a usable sender.
type AsyncOptions struct {
	// QueueSize is how many messages may wait for a worker. When it is full a
	// message is dropped and reported rather than blocking the caller.
	QueueSize int
	// Workers is how many deliveries run at once.
	Workers int
	// AttemptTimeout bounds one delivery attempt, including the SMTP session.
	AttemptTimeout time.Duration
	// Attempts is how many times one message is tried before it is given up on.
	Attempts int
	// Backoff is the pause before the second attempt; each further attempt waits
	// twice as long.
	Backoff time.Duration
	// Errorf reports a dropped or failed delivery. It receives the recipient and
	// the subject, never the body, which carries the one-time tokens.
	Errorf func(format string, args ...any)
}

func (o AsyncOptions) withDefaults() AsyncOptions {
	if o.QueueSize <= 0 {
		o.QueueSize = defaultAsyncQueueSize
	}
	if o.Workers <= 0 {
		o.Workers = defaultAsyncWorkers
	}
	if o.AttemptTimeout <= 0 {
		o.AttemptTimeout = defaultAsyncAttemptTimeout
	}
	if o.Attempts <= 0 {
		o.Attempts = defaultAsyncAttempts
	}
	if o.Backoff <= 0 {
		o.Backoff = defaultAsyncBackoff
	}
	if o.Errorf == nil {
		o.Errorf = func(string, ...any) {}
	}
	return o
}

// AsyncSender hands a message to a worker pool and returns at once, so the
// request that triggered it never waits for an SMTP server.
//
// Delivery deliberately ignores the caller's context: the request is over — and
// its context cancelled — by the time the message leaves. Each attempt gets a
// detached context of its own, bounded by AttemptTimeout.
type AsyncSender struct {
	inner Sender
	opts  AsyncOptions

	// mu guards the queue against a send racing Close: Send holds it for reading,
	// Close takes it for writing before closing the channel, so no message is
	// ever handed to a channel that a worker has already left.
	mu     sync.RWMutex
	queue  chan Message
	closed bool
	wg     sync.WaitGroup
}

// NewAsyncSender wraps inner with a bounded queue and a fixed set of workers.
func NewAsyncSender(inner Sender, opts AsyncOptions) *AsyncSender {
	opts = opts.withDefaults()
	s := &AsyncSender{
		inner: inner,
		opts:  opts,
		queue: make(chan Message, opts.QueueSize),
	}
	s.wg.Add(opts.Workers)
	for range opts.Workers {
		go s.work()
	}
	return s
}

// Send validates the message and queues it. It returns the validation error to
// the caller, because a malformed message is a bug in the request rather than a
// delivery problem; once the message is queued the caller is done and nil is
// returned, whatever the mail server does afterwards.
func (s *AsyncSender) Send(_ context.Context, message Message) error {
	if err := validate(message); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		s.opts.Errorf("mail: dropped, sender is shutting down: to=%s subject=%q", message.To, message.Subject)
		return nil
	}

	select {
	case s.queue <- message:
	default:
		// A full queue means the mail server is slower than the traffic. The
		// caller is never blocked for it, and the loss is visible in the log.
		s.opts.Errorf("mail: queue is full (%d), dropped: to=%s subject=%q",
			cap(s.queue), message.To, message.Subject)
	}
	return nil
}

// Close stops accepting messages and waits for the queued ones to be delivered,
// or for ctx to expire. A message still being retried when the deadline passes is
// left to the process exiting.
func (s *AsyncSender) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// work delivers queued messages until the queue is closed and empty.
func (s *AsyncSender) work() {
	defer s.wg.Done()
	for message := range s.queue {
		s.deliver(message)
	}
}

// deliver attempts one message until it is accepted or the attempts run out.
func (s *AsyncSender) deliver(message Message) {
	backoff := s.opts.Backoff

	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.AttemptTimeout)
		err := s.inner.Send(ctx, message)
		cancel()

		if err == nil {
			return
		}
		if attempt >= s.opts.Attempts {
			s.opts.Errorf("mail: delivery failed after %d attempts: to=%s subject=%q: %v",
				attempt, message.To, message.Subject, err)
			return
		}
		s.opts.Errorf("mail: attempt %d failed, retrying in %s: to=%s subject=%q: %v",
			attempt, backoff, message.To, message.Subject, err)

		time.Sleep(backoff)
		backoff *= 2
	}
}
