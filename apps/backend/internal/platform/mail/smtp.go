package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// defaultSMTPTimeout bounds one delivery when the configuration names none. A
// mail server that is unreachable must cost a bounded moment, never the kernel's
// own give-up time: a registration that waits two minutes for a dial is a
// registration the user has abandoned.
const defaultSMTPTimeout = 10 * time.Second

// SMTPConfig holds the SMTP connection settings.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// Timeout bounds the dial and every read and write of the session. Zero
	// takes defaultSMTPTimeout.
	Timeout time.Duration
}

// SMTPSender delivers messages through an SMTP server.
type SMTPSender struct {
	cfg SMTPConfig
}

// NewSMTPSender builds an SMTP sender. Host and From are required: a sender
// that silently drops mail is worse than a startup failure.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, fmt.Errorf("mail: smtp host is required")
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, fmt.Errorf("mail: smtp from address is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultSMTPTimeout
	}
	return &SMTPSender{cfg: cfg}, nil
}

// Timeout reports the bound this sender applies to one delivery.
func (s *SMTPSender) Timeout() time.Duration {
	return s.cfg.Timeout
}

// Send delivers the message with STARTTLS on the standard submission port.
//
// The session is driven by hand rather than through smtp.SendMail because that
// helper has neither a deadline nor a context: a server that accepts the
// connection and then stops talking would hold the caller until the operating
// system gave up on it.
func (s *SMTPSender) Send(ctx context.Context, message Message) error {
	if err := s.deliver(ctx, message); err != nil {
		return fmt.Errorf("mail: send to %s: %w", message.To, err)
	}
	return nil
}

// deliver opens one bounded SMTP session and sends message through it.
func (s *SMTPSender) deliver(ctx context.Context, message Message) error {
	deadline := time.Now().Add(s.cfg.Timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	address := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	conn, err := (&net.Dialer{Timeout: s.cfg.Timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}

	// The deadline covers the whole session: net/smtp sets none of its own, so a
	// server that greets and then goes quiet fails here instead of hanging.
	_ = conn.SetDeadline(deadline)

	// net/smtp has no context support, so cancellation closes the connection out
	// from under the session, which fails the pending read at once.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = client.Close() }()

	if err := client.Hello("localhost"); err != nil {
		return err
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return err
		}
	}
	// A server that does not advertise AUTH is not asked for it, which is what
	// smtp.SendMail does with the same configuration.
	if s.cfg.Username != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := client.Mail(s.cfg.From); err != nil {
		return err
	}
	if err := client.Rcpt(message.To); err != nil {
		return err
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(buildPayload(s.cfg.From, message)); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	return client.Quit()
}

// buildPayload writes a minimal RFC 5322 message with UTF-8 headers.
func buildPayload(from string, message Message) []byte {
	var builder strings.Builder
	builder.WriteString("From: " + from + "\r\n")
	builder.WriteString("To: " + message.To + "\r\n")
	builder.WriteString("Subject: " + message.Subject + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(message.Text)
	return []byte(builder.String())
}
