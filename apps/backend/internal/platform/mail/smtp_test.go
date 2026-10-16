package mail

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// silentListener accepts connections and never writes a greeting, which is the
// server that made registration wait for the kernel: the socket is open, so the
// dial succeeds, and nothing ever arrives.
func silentListener(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Held open and silent on purpose.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	return listener
}

func smtpSenderFor(t *testing.T, listener net.Listener, timeout time.Duration) *SMTPSender {
	t.Helper()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	sender, err := NewSMTPSender(SMTPConfig{
		Host:    host,
		Port:    atoi(t, port),
		From:    "Bolu <no-reply@bolu.id>",
		Timeout: timeout,
	})
	require.NoError(t, err)
	return sender
}

// TestSMTPSenderTimesOutOnAServerThatNeverSpeaks: the timeout bounds the whole
// session, not just the dial, so a server that accepts and goes quiet fails
// quickly instead of holding the caller.
func TestSMTPSenderTimesOutOnAServerThatNeverSpeaks(t *testing.T) {
	sender := smtpSenderFor(t, silentListener(t), 200*time.Millisecond)

	start := time.Now()
	err := sender.Send(context.Background(), testMessage())
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, time.Second, "a silent server must not hold the caller")
	require.Contains(t, err.Error(), "owner@example.com", "the failure names the recipient")
}

// TestSMTPSenderReturnsWhenTheContextIsCancelled: cancellation interrupts the
// session, it does not merely mark it — the pending read is closed underneath.
func TestSMTPSenderReturnsWhenTheContextIsCancelled(t *testing.T) {
	sender := smtpSenderFor(t, silentListener(t), time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sender.Send(ctx, testMessage())
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, time.Second, "cancellation must interrupt the session")
}

// TestSMTPSenderCompletesASession drives the hand-written session against a
// scripted server, so the EHLO, MAIL, RCPT, DATA and QUIT exchange and the
// payload are all exercised.
func TestSMTPSenderCompletesASession(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	type received struct {
		from string
		to   []string
		data string
	}
	got := make(chan received, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

		write("220 test.local ESMTP ready")
		var session received
		inData := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")

			if inData {
				if line == "." {
					inData = false
					got <- session
					write("250 OK: queued")
					continue
				}
				session.data += line + "\n"
				continue
			}

			switch {
			case strings.HasPrefix(line, "EHLO"):
				write("250-test.local")
				write("250 SIZE 10240000")
			case strings.HasPrefix(line, "MAIL FROM:"):
				session.from = strings.TrimPrefix(line, "MAIL FROM:")
				write("250 OK")
			case strings.HasPrefix(line, "RCPT TO:"):
				session.to = append(session.to, strings.TrimPrefix(line, "RCPT TO:"))
				write("250 OK")
			case line == "DATA":
				inData = true
				write("354 Start mail input")
			case line == "QUIT":
				write("221 Bye")
				return
			default:
				write("250 OK")
			}
		}
	}()

	sender := smtpSenderFor(t, listener, 5*time.Second)
	require.NoError(t, sender.Send(context.Background(), testMessage()))

	select {
	case session := <-got:
		require.Contains(t, session.from, "no-reply@bolu.id")
		require.Equal(t, []string{"<owner@example.com>"}, session.to)
		require.Contains(t, session.data, "Subject: Verifikasi email Bolu")
		require.Contains(t, session.data, "https://app.example.com/verify?token=abc")
	case <-time.After(2 * time.Second):
		t.Fatal("the session was never completed")
	}
}

func TestSMTPSenderDefaultsTheTimeout(t *testing.T) {
	sender, err := NewSMTPSender(SMTPConfig{Host: "smtp.example.com", From: "Bolu <no-reply@bolu.id>"})
	require.NoError(t, err)
	require.Equal(t, defaultSMTPTimeout, sender.cfg.Timeout, "a sender is always bounded")
}

func TestSMTPSenderReportsADeadServer(t *testing.T) {
	// A port nothing listens on: the dial fails at once with the reason, rather
	// than waiting for a timeout that will never fire.
	sender, err := NewSMTPSender(SMTPConfig{
		Host:    "127.0.0.1",
		Port:    1,
		From:    "Bolu <no-reply@bolu.id>",
		Timeout: time.Second,
	})
	require.NoError(t, err)

	require.Error(t, sender.Send(context.Background(), testMessage()))
}

func atoi(t *testing.T, value string) int {
	t.Helper()

	parsed, err := strconv.Atoi(value)
	require.NoError(t, err)
	return parsed
}
