package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/platform/mail"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// TestSelectMailerMarksTheNetworkDriverAsync: the SMTP driver's Send reaches the
// network, and it is the async wrapper that keeps that off the request path. A
// driver marked synchronous here is a registration that waits for a mail server.
func TestSelectMailerMarksTheNetworkDriverAsync(t *testing.T) {
	selected, err := selectMailer(mailConfigFor("log"), func(string, ...any) {})
	require.NoError(t, err)
	require.False(t, selected.async, "the log driver writes to the log and nothing else")

	selected, err = selectMailer(mailConfigFor("smtp"), func(string, ...any) {})
	require.NoError(t, err)
	require.True(t, selected.async, "the smtp driver belongs off the request path")

	smtpSender, ok := selected.sender.(*mail.SMTPSender)
	require.True(t, ok)
	require.Equal(t, 3*time.Second, smtpSender.Timeout(), "MAIL_TIMEOUT_SECONDS reaches the sender")

	_, err = selectMailer(mailConfigFor("carrier-pigeon"), func(string, ...any) {})
	require.ErrorContains(t, err, "unknown mail driver")
}

// TestOpenMailerPutsTheQueueInFrontOfTheNetworkDriver: what the container hands
// the services is the point. A container that builds the queue but leaves Mailer
// pointing at the SMTP sender is a registration that waits for a mail server —
// the original latency bug, one wiring mistake away.
func TestOpenMailerPutsTheQueueInFrontOfTheNetworkDriver(t *testing.T) {
	c := &Container{Logger: zap.NewNop()}
	require.NoError(t, c.openMailer(mailConfigFor("smtp")))

	queued, ok := c.Mailer.(*mail.AsyncSender)
	require.True(t, ok, "the smtp driver must reach the services behind the queue, got %T", c.Mailer)
	require.Same(t, queued, c.AsyncMailer)

	// Nothing listens on the configured port, so a synchronous Send would return
	// the dial error here. A queued one accepts the message and returns.
	require.NoError(t, c.Mailer.Send(context.Background(), mail.Message{
		To:      "owner@example.com",
		Subject: "Verifikasi email Bolu",
		Text:    "https://app.example.com/verify?token=abc",
	}))
}

// TestOpenMailerLeavesTheLogDriverSynchronous: printing a link costs nothing, so
// there is nothing to hand off, and a container that queued it would hold the
// link behind a worker for no reason.
func TestOpenMailerLeavesTheLogDriverSynchronous(t *testing.T) {
	c := &Container{Logger: zap.NewNop()}
	require.NoError(t, c.openMailer(mailConfigFor("log")))

	_, ok := c.Mailer.(*mail.AsyncSender)
	require.False(t, ok, "the log driver needs no worker pool")
	require.Nil(t, c.AsyncMailer)
}

// TestContainerDrainsQueuedMailOnClose: a verification link queued by the last
// request before shutdown still leaves.
func TestContainerDrainsQueuedMailOnClose(t *testing.T) {
	c := newTestContainer(t)

	inner := &recordingMailer{}
	c.AsyncMailer = mail.NewAsyncSender(inner, mail.AsyncOptions{})
	c.Mailer = c.AsyncMailer
	require.NoError(t, c.Mailer.Send(context.Background(), mail.Message{
		To:      "owner@example.com",
		Subject: "Verifikasi email Bolu",
		Text:    "https://app.example.com/verify?token=abc",
	}))

	c.Close()

	require.Len(t, inner.sent, 1, "Close must drain the queue")
	require.Equal(t, "owner@example.com", inner.sent[0].To)
	require.Nil(t, c.AsyncMailer)
}

// mailConfigFor is the configuration a deployment of one driver would load.
func mailConfigFor(driver string) *config.Config {
	cfg := testConfig()
	cfg.Mail = config.MailConfig{
		Driver:         driver,
		Host:           "127.0.0.1",
		Port:           2525,
		From:           "Bolu <no-reply@bolu.id>",
		TimeoutSeconds: 3,
	}
	return cfg
}
