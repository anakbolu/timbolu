package container

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	authservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/service"
	workspaceservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/authn"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/oauth/google"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/ratelimit"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// Access-token defaults, used when the configuration omits a value.
const (
	defaultAccessTokenTTL = 15 * time.Minute
	defaultRefreshTTL     = 30 * 24 * time.Hour
	defaultVerifyTTL      = 24 * time.Hour
	defaultResetTTL       = time.Hour
	defaultStateTTL       = 10 * time.Minute
)

// openAuth builds the authentication primitives and the two services that need
// them. Every dependency is optional except the database: without it the auth
// and workspace services are left unwired and their routes answer 503 instead
// of panicking.
func (c *Container) openAuth(ctx context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.Auth == nil {
		c.Logger.Warn("auth is disabled: no database connection")
		return nil
	}

	issuer, err := authn.NewIssuer(cfg.JWT.Secret, durationMinutes(cfg.Auth.AccessTokenMinutes, defaultAccessTokenTTL))
	if err != nil {
		return fmt.Errorf("build access token issuer: %w", err)
	}
	c.Issuer = issuer

	states, err := authn.NewStateSigner(cfg.JWT.Secret, durationMinutes(cfg.Auth.StateMinutes, defaultStateTTL))
	if err != nil {
		return fmt.Errorf("build oauth state signer: %w", err)
	}
	c.States = states

	if c.Mailer == nil {
		if err := c.openMailer(cfg); err != nil {
			return err
		}
	}

	if c.Limiter == nil && c.Redis != nil {
		c.Limiter = ratelimit.New(c.Redis.Raw())
	}

	if c.Google == nil && strings.TrimSpace(cfg.Google.ClientID) != "" {
		client, err := google.New(google.Config{
			ClientID:     cfg.Google.ClientID,
			ClientSecret: cfg.Google.ClientSecret,
			RedirectURL:  cfg.Google.RedirectURL,
		})
		if err != nil {
			return fmt.Errorf("build google client: %w", err)
		}
		c.Google = client
	}

	var throttle authdomain.LoginThrottle
	if c.Limiter != nil {
		throttle = loginThrottle{limiter: c.Limiter, metrics: c.Metrics}
	}

	var googleProviderPort authdomain.GoogleProvider
	if c.Google != nil {
		googleProviderPort = googleProvider{client: c.Google}
	}

	c.Services.Auth = authservice.New(authservice.Deps{
		Repository: c.Repositories.Auth,
		Issuer:     tokenIssuer{issuer: issuer},
		States:     states,
		Mailer:     mailer{sender: c.Mailer},
		Throttle:   throttle,
		Google:     googleProviderPort,
		Passwords:  passwords{},
		Tokens:     tokens{},
		Logf:       c.Logger.Sugar().Errorf,
		Config: authservice.Config{
			RefreshTTL:               durationHours(cfg.Auth.RefreshHours, defaultRefreshTTL),
			VerificationTTL:          durationHours(cfg.Auth.VerificationHours, defaultVerifyTTL),
			ResetTTL:                 durationMinutes(cfg.Auth.ResetMinutes, defaultResetTTL),
			FrontendURL:              cfg.Application.FrontendURL,
			LoginThrottleCapacity:    cfg.Auth.LoginThrottleCapacity,
			LoginThrottleRefill:      cfg.Auth.LoginThrottleRefill,
			RequireEmailVerification: cfg.Auth.RequireEmailVerification,
		},
	})

	if c.Repositories.Agent != nil {
		c.Services.Agent = agentservice.New(agentservice.Deps{
			Repository: c.Repositories.Agent,
			Config:     agentservice.Config{MaxAgentsPerWorkspace: cfg.Plan.MaxAgentsPerWorkspace},
		})
	}

	c.Services.Workspace = workspaceservice.New(workspaceservice.Deps{
		Repository: c.Repositories.Workspace,
		Mailer:     mailer{sender: c.Mailer},
		Tokens:     tokens{},
		Config: workspaceservice.Config{
			FrontendURL: cfg.Application.FrontendURL,
		},
	})

	_ = ctx
	return nil
}

// mailSender is the sender the configuration selects, and whether delivering
// through it reaches the network.
type mailSender struct {
	sender mail.Sender
	// async marks a driver whose Send is a network call, and therefore belongs off
	// the request path.
	async bool
}

// openMailer builds the mail sender the configuration selects and hands it to
// the rest of the container. A driver whose Send reaches the network is put
// behind a worker pool: an unreachable mail server must not hold the request
// that triggered it, and a delivery that fails must not fail the registration it
// belongs to. The log driver stays synchronous, because printing a link costs
// nothing and is what a developer clicks.
func (c *Container) openMailer(cfg *config.Config) error {
	selected, err := selectMailer(cfg, c.Logger.Sugar().Infof)
	if err != nil {
		return err
	}

	c.Mailer = selected.sender
	if selected.async {
		c.AsyncMailer = mail.NewAsyncSender(c.Mailer, mail.AsyncOptions{
			Errorf: c.Logger.Sugar().Errorf,
		})
		c.Mailer = c.AsyncMailer
	}
	return nil
}

// selectMailer picks the driver. The log driver prints the links, which is how a
// developer completes the flow locally without an SMTP server.
func selectMailer(cfg *config.Config, logf func(format string, args ...any)) (mailSender, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Mail.Driver)) {
	case "", "log":
		return mailSender{sender: mail.NewLogSender(logf)}, nil
	case "smtp":
		sender, err := mail.NewSMTPSender(mail.SMTPConfig{
			Host:     cfg.Mail.Host,
			Port:     cfg.Mail.Port,
			Username: cfg.Mail.Username,
			Password: cfg.Mail.Password,
			From:     cfg.Mail.From,
			// Zero takes the mail package's own default.
			Timeout: time.Duration(cfg.Mail.TimeoutSeconds) * time.Second,
		})
		if err != nil {
			return mailSender{}, fmt.Errorf("build smtp sender: %w", err)
		}
		return mailSender{sender: sender, async: true}, nil
	default:
		return mailSender{}, fmt.Errorf("unknown mail driver %q", cfg.Mail.Driver)
	}
}
