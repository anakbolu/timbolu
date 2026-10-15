package container

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/authn"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/oauth/google"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/ratelimit"
	"github.com/robzlabz/employeebot/packages/platform/metrics"
)

// The adapters below translate a platform implementation into the port a module
// declares in its domain. They live in the container because it is the only
// package allowed to know both sides.

// tokenIssuer adapts the JWT issuer to the auth domain port.
type tokenIssuer struct {
	issuer *authn.Issuer
}

func (t tokenIssuer) Issue(userID, sessionID uuid.UUID) (string, time.Time, error) {
	return t.issuer.Issue(userID, sessionID)
}

func (t tokenIssuer) Verify(token string) (authdomain.AccessClaims, error) {
	claims, err := t.issuer.Verify(token)
	if err != nil {
		return authdomain.AccessClaims{}, err
	}
	return authdomain.AccessClaims{UserID: claims.UserID, SessionID: claims.SessionID}, nil
}

// mailer adapts the mail sender to the module ports. The ports take plain
// strings so a module never has to import the mail package.
type mailer struct {
	sender mail.Sender
}

func (m mailer) Send(ctx context.Context, to, subject, body string) error {
	return m.sender.Send(ctx, mail.Message{To: to, Subject: subject, Text: body})
}

// googleProvider adapts the Google client to the auth domain port.
type googleProvider struct {
	client *google.Client
}

func (g googleProvider) AuthCodeURL(state string) string {
	return g.client.AuthCodeURL(state)
}

func (g googleProvider) Exchange(ctx context.Context, code string) (authdomain.GoogleUser, error) {
	info, err := g.client.Exchange(ctx, code)
	if err != nil {
		if errors.Is(err, google.ErrEmailNotVerified) {
			return authdomain.GoogleUser{}, authdomain.ErrGoogleEmailUnverified
		}
		return authdomain.GoogleUser{}, err
	}
	return authdomain.GoogleUser{
		Subject:       info.Subject,
		Email:         info.Email,
		EmailVerified: info.EmailVerified,
	}, nil
}

// loginThrottle adapts the Redis limiter to the auth domain port.
//
// It carries the meter because a refusal is only visible here: the auth module asks a yes/no question
// and acts on the answer, so nothing above it can tell a throttled login from a successful one. The
// count is what says whether the ceiling is protecting the product or turning away its users.
type loginThrottle struct {
	limiter *ratelimit.Limiter
	metrics *metrics.Provider
}

func (l loginThrottle) Allow(ctx context.Context, key string, capacity int, refill float64) (authdomain.ThrottleResult, error) {
	result, err := l.limiter.Allow(ctx, key, capacity, refill)
	if err != nil {
		return authdomain.ThrottleResult{}, err
	}
	if !result.Allowed {
		l.metrics.RateLimited(ctx, "login")
	}
	return authdomain.ThrottleResult{Allowed: result.Allowed, RetryAfter: result.RetryAfter}, nil
}

func (l loginThrottle) Reset(ctx context.Context, key string) error {
	return l.limiter.Reset(ctx, key)
}

// passwords adapts the argon2id helpers to the auth domain port.
type passwords struct{}

func (passwords) Hash(password string) (string, error) { return authn.HashPassword(password) }

func (passwords) Verify(hash, password string) (bool, error) {
	return authn.VerifyPassword(hash, password)
}

func (passwords) Validate(password string) error { return authn.ValidatePassword(password) }

// tokens adapts the one-time token generator. It satisfies the Tokens port of
// both the auth and the workspace module.
type tokens struct{}

func (tokens) Generate() (string, string, error) { return authn.GenerateToken() }

// Compile-time checks that every adapter satisfies the port it is wired to.
var (
	_ authdomain.TokenIssuer    = tokenIssuer{}
	_ authdomain.Mailer         = mailer{}
	_ authdomain.GoogleProvider = googleProvider{}
	_ authdomain.LoginThrottle  = loginThrottle{}
	_ authdomain.Passwords      = passwords{}
	_ authdomain.Tokens         = tokens{}
	_ workspacedomain.Mailer    = mailer{}
	_ workspacedomain.Tokens    = tokens{}
)
