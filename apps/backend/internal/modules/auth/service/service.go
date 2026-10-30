// Package service implements the authentication use cases. It depends on the
// domain contracts and on the platform primitives (password hashing, access
// tokens, mail, rate limiting, Google), never on HTTP or SQL.
package service

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/domain"
)

// Config holds the authentication lifetimes and the login throttle.
type Config struct {
	// RefreshTTL is how long a refresh session stays valid.
	RefreshTTL time.Duration
	// VerificationTTL is how long an email verification link stays valid.
	VerificationTTL time.Duration
	// ResetTTL is how long a password reset link stays valid.
	ResetTTL time.Duration
	// FrontendURL is the base URL of the web app, used to build the links that
	// are emailed to the user.
	FrontendURL string
	// LoginThrottleCapacity and LoginThrottleRefill configure the per-email and
	// per-IP login buckets.
	LoginThrottleCapacity int
	LoginThrottleRefill   float64
	// RequireEmailVerification makes a new account confirm its address before it
	// may use the product. When false, Register marks the address verified at
	// once and sends no link: signing up is the whole of the gate.
	RequireEmailVerification bool
	// Clock is injectable so tests can control expiry.
	Clock func() time.Time
}

// Deps are the service dependencies. Everything except the repository is
// optional, so a deployment without Redis or Google still serves password
// authentication.
type Deps struct {
	Repository domain.Repository
	// Issuer signs access tokens.
	Issuer domain.TokenIssuer
	// States signs the OAuth state value.
	States domain.StateSigner
	// Mailer delivers the verification and reset links. Optional.
	Mailer domain.Mailer
	// Throttle limits repeated login attempts. Optional: without it the
	// credentials check still runs.
	Throttle domain.LoginThrottle
	// Google is the OIDC client. Optional: without it Google sign-in reports
	// not configured.
	Google domain.GoogleProvider
	// Passwords hashes and verifies account passwords.
	Passwords domain.Passwords
	// Tokens generates the one-time tokens emailed to users.
	Tokens domain.Tokens
	// Logf reports a failure that must not fail the request it belongs to, such
	// as a verification link that could not be delivered. Optional: without it
	// the failure is dropped.
	Logf   func(format string, args ...any)
	Config Config
}

type service struct {
	deps Deps
}

// New builds the auth service.
func New(deps Deps) domain.Service {
	if deps.Config.Clock == nil {
		deps.Config.Clock = time.Now
	}
	if deps.Config.RefreshTTL <= 0 {
		deps.Config.RefreshTTL = 30 * 24 * time.Hour
	}
	if deps.Config.VerificationTTL <= 0 {
		deps.Config.VerificationTTL = 24 * time.Hour
	}
	if deps.Config.ResetTTL <= 0 {
		deps.Config.ResetTTL = time.Hour
	}
	if deps.Config.LoginThrottleCapacity <= 0 {
		deps.Config.LoginThrottleCapacity = 5
	}
	if deps.Config.LoginThrottleRefill <= 0 {
		// One attempt every five seconds, so a burst is absorbed but a
		// legitimate user who mistyped can retry shortly.
		deps.Config.LoginThrottleRefill = 0.2
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	return &service{deps: deps}
}

// Register creates a password account and emails a verification link.
func (s *service) Register(ctx context.Context, req domain.RegisterRequest) (domain.User, error) {
	email, err := normalizeEmail(req.Email)
	if err != nil {
		return domain.User{}, err
	}
	if err := s.deps.Passwords.Validate(req.Password); err != nil {
		return domain.User{}, fmt.Errorf("%w: %w", domain.ErrWeakPassword, err)
	}

	hash, err := s.deps.Passwords.Hash(req.Password)
	if err != nil {
		return domain.User{}, err
	}

	user, err := s.deps.Repository.CreateUser(ctx, email, hash)
	if err != nil {
		return domain.User{}, err
	}

	if !s.deps.Config.RequireEmailVerification {
		// Verification is off in this deployment: the account is usable the moment
		// it exists, so the address is marked verified rather than left pending.
		return s.deps.Repository.MarkEmailVerified(ctx, user.ID)
	}

	body, err := s.issueVerificationToken(ctx, user)
	if err != nil {
		return domain.User{}, err
	}
	if err := s.send(ctx, user.Email, verificationSubject, body); err != nil {
		// The account and its token exist, so a mail server having a bad day must
		// not turn a successful sign-up into a failure the user cannot see past.
		// The link stays valid: resend-verification issues a fresh one.
		s.deps.Logf("auth: verification email to %s failed: %v", user.Email, err)
	}

	return user, nil
}

// ResendVerification sends a fresh verification link. It succeeds even when the
// account does not exist, so the endpoint cannot be used to discover addresses.
func (s *service) ResendVerification(ctx context.Context, email string) error {
	if !s.deps.Config.RequireEmailVerification {
		// There is nothing to verify in this deployment, and answering as if a
		// link were sent would be a lie the caller cannot see through.
		return nil
	}

	normalized, err := normalizeEmail(email)
	if err != nil {
		// A malformed address is answered like an unknown one, so the response
		// never depends on what the caller typed.
		return nil //nolint:nilerr // deliberate: the answer must not vary
	}

	user, err := s.deps.Repository.UserByEmail(ctx, normalized)
	if err != nil {
		// An unknown address is answered exactly like a known one, so the
		// endpoint cannot be used to discover accounts. The error is
		// deliberately dropped.
		if errors.Is(err, domain.ErrUserNotFound) {
			return nil //nolint:nilerr // deliberate: no account enumeration
		}
		return err
	}
	if user.EmailVerified {
		return nil
	}

	return s.sendVerification(ctx, user)
}

// VerifyEmail consumes a verification token and marks the account verified.
func (s *service) VerifyEmail(ctx context.Context, token string) (domain.User, error) {
	if strings.TrimSpace(token) == "" {
		return domain.User{}, domain.ErrInvalidToken
	}

	userID, err := s.deps.Repository.ConsumeEmailVerificationToken(ctx, domain.HashToken(token))
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			// No live token matched: already used, expired, or unknown. The
			// distinction is not useful to the caller.
			return domain.User{}, domain.ErrInvalidToken
		}
		return domain.User{}, err
	}

	return s.deps.Repository.MarkEmailVerified(ctx, userID)
}

// Login verifies credentials and opens a session.
func (s *service) Login(ctx context.Context, req domain.LoginRequest) (domain.Session, error) {
	email, err := normalizeEmail(req.Email)
	if err != nil {
		return domain.Session{}, domain.ErrInvalidCredentials
	}

	if err := s.allowLoginAttempt(ctx, email, req.IP); err != nil {
		return domain.Session{}, err
	}

	user, err := s.deps.Repository.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			return domain.Session{}, domain.ErrInvalidCredentials
		}
		return domain.Session{}, err
	}

	hash, err := s.deps.Repository.PasswordHash(ctx, user.ID)
	if err != nil {
		return domain.Session{}, err
	}
	if hash == "" {
		// Google-only account: there is no password to check.
		return domain.Session{}, domain.ErrInvalidCredentials
	}

	ok, err := s.deps.Passwords.Verify(hash, req.Password)
	if err != nil {
		return domain.Session{}, domain.ErrInvalidCredentials
	}
	if !ok {
		return domain.Session{}, domain.ErrInvalidCredentials
	}

	if !s.deps.Config.RequireEmailVerification && !user.EmailVerified {
		// This deployment does not ask for a confirmed address, so an account that
		// predates the switch is healed at sign-in rather than left able to
		// authenticate but refused everywhere afterwards.
		verified, err := s.deps.Repository.MarkEmailVerified(ctx, user.ID)
		if err != nil {
			return domain.Session{}, err
		}
		user = verified
	}

	session, err := s.openSession(ctx, user, req.UserAgent, req.IP, nil)
	if err != nil {
		return domain.Session{}, err
	}

	// A successful sign-in clears the throttle for this email and IP.
	s.resetLoginAttempts(ctx, email, req.IP)

	return session, nil
}

// Refresh rotates a refresh session: the presented token is revoked and a new
// one is issued, so a stolen token is usable at most once.
func (s *service) Refresh(ctx context.Context, refreshToken string) (domain.Session, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return domain.Session{}, domain.ErrInvalidToken
	}

	record, err := s.deps.Repository.SessionByTokenHash(ctx, domain.HashToken(refreshToken))
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			return domain.Session{}, domain.ErrInvalidToken
		}
		return domain.Session{}, err
	}
	if !s.sessionUsable(record) {
		return domain.Session{}, domain.ErrInvalidToken
	}

	user, err := s.deps.Repository.UserByID(ctx, record.UserID)
	if err != nil {
		return domain.Session{}, err
	}

	if err := s.deps.Repository.RevokeSession(ctx, record.ID); err != nil {
		return domain.Session{}, err
	}

	return s.openSession(ctx, user, "", "", &record.ID)
}

// Logout revokes the session behind a refresh token. It is idempotent: logging
// out twice, or with an unknown token, is not an error.
func (s *service) Logout(ctx context.Context, refreshToken string) error {
	if strings.TrimSpace(refreshToken) == "" {
		return nil
	}
	return s.deps.Repository.RevokeSessionByTokenHash(ctx, domain.HashToken(refreshToken))
}

// RequestPasswordReset emails a reset link. Like resend-verification, it always
// reports success so the endpoint cannot enumerate accounts.
func (s *service) RequestPasswordReset(ctx context.Context, email string) error {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return nil //nolint:nilerr // deliberate: the answer must not vary
	}

	user, err := s.deps.Repository.UserByEmail(ctx, normalized)
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			return nil
		}
		return err
	}

	raw, hash, err := s.deps.Tokens.Generate()
	if err != nil {
		return err
	}

	expiresAt := s.deps.Config.Clock().Add(s.deps.Config.ResetTTL)
	if err := s.deps.Repository.CreatePasswordResetToken(ctx, user.ID, hash, expiresAt); err != nil {
		return err
	}

	return s.send(ctx, user.Email, "Atur ulang kata sandi Bolu",
		fmt.Sprintf("Buka tautan ini untuk mengatur ulang kata sandi:\n\n%s/forgot-password?token=%s\n\nTautan berlaku %s.",
			strings.TrimRight(s.deps.Config.FrontendURL, "/"), raw, humanize(s.deps.Config.ResetTTL)))
}

// ResetPassword consumes a reset token, stores the new password, and revokes
// every existing session so a compromised session cannot outlive the reset.
func (s *service) ResetPassword(ctx context.Context, req domain.ResetPasswordRequest) error {
	if strings.TrimSpace(req.Token) == "" {
		return domain.ErrInvalidToken
	}
	if err := s.deps.Passwords.Validate(req.NewPassword); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrWeakPassword, err)
	}

	userID, err := s.deps.Repository.ConsumePasswordResetToken(ctx, domain.HashToken(req.Token))
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			return domain.ErrInvalidToken
		}
		return err
	}

	hash, err := s.deps.Passwords.Hash(req.NewPassword)
	if err != nil {
		return err
	}
	if _, err := s.deps.Repository.UpdatePasswordHash(ctx, userID, hash); err != nil {
		return err
	}

	_, err = s.deps.Repository.RevokeSessionsForUser(ctx, userID)
	return err
}

// GoogleAuthURL returns the consent URL with a signed state.
func (s *service) GoogleAuthURL(context.Context) (string, error) {
	if s.deps.Google == nil || s.deps.States == nil {
		return "", domain.ErrGoogleNotConfigured
	}

	state, err := s.deps.States.Sign()
	if err != nil {
		return "", err
	}

	return s.deps.Google.AuthCodeURL(state), nil
}

// GoogleCallback completes the Google flow: it verifies the state, exchanges
// the code, and either signs in, links to an existing account with the same
// email, or creates a new account.
func (s *service) GoogleCallback(ctx context.Context, req domain.GoogleCallbackRequest) (domain.Session, error) {
	if s.deps.Google == nil || s.deps.States == nil {
		return domain.Session{}, domain.ErrGoogleNotConfigured
	}
	if err := s.deps.States.Verify(req.State); err != nil {
		return domain.Session{}, fmt.Errorf("%w: %w", domain.ErrInvalidToken, err)
	}

	info, err := s.deps.Google.Exchange(ctx, req.Code)
	if err != nil {
		if errors.Is(err, domain.ErrGoogleEmailUnverified) {
			return domain.Session{}, fmt.Errorf("%w: provider did not verify the email", domain.ErrInvalidToken)
		}
		return domain.Session{}, err
	}

	email, err := normalizeEmail(info.Email)
	if err != nil {
		return domain.Session{}, err
	}

	user, err := s.deps.Repository.UserByGoogleSubject(ctx, info.Subject)
	if err != nil && !errors.Is(err, domain.ErrUserNotFound) {
		return domain.Session{}, err
	}

	if errors.Is(err, domain.ErrUserNotFound) {
		// Not linked yet. An account with the same email is linked rather than
		// duplicated.
		existing, lookupErr := s.deps.Repository.UserByEmail(ctx, email)
		switch {
		case lookupErr == nil:
			user, err = s.deps.Repository.LinkGoogleSubject(ctx, existing.ID, info.Subject)
			if err != nil {
				return domain.Session{}, err
			}
		case errors.Is(lookupErr, domain.ErrUserNotFound):
			user, err = s.deps.Repository.CreateGoogleUser(ctx, email, info.Subject)
			if err != nil {
				return domain.Session{}, err
			}
		default:
			return domain.Session{}, lookupErr
		}
	}

	return s.openSession(ctx, user, req.UserAgent, req.IP, nil)
}

// Authenticate verifies an access token and confirms its session is still
// usable, then returns the account.
func (s *service) Authenticate(ctx context.Context, accessToken string) (domain.User, error) {
	claims, err := s.deps.Issuer.Verify(accessToken)
	if err != nil {
		return domain.User{}, fmt.Errorf("%w: %w", domain.ErrInvalidToken, err)
	}

	record, err := s.deps.Repository.SessionByID(ctx, claims.SessionID)
	if err != nil {
		if errors.Is(err, domain.ErrUserNotFound) {
			return domain.User{}, domain.ErrInvalidToken
		}
		return domain.User{}, err
	}
	if !s.sessionUsable(record) || record.UserID != claims.UserID {
		return domain.User{}, domain.ErrInvalidToken
	}

	return s.deps.Repository.UserByID(ctx, claims.UserID)
}

// CurrentUser returns the account behind an access token.
func (s *service) CurrentUser(ctx context.Context, userID uuid.UUID) (domain.User, error) {
	if userID == uuid.Nil {
		return domain.User{}, domain.ErrUserNotFound
	}
	return s.deps.Repository.UserByID(ctx, userID)
}

// AssertVerified rejects an account whose email is not verified yet.
func (s *service) AssertVerified(ctx context.Context, userID uuid.UUID) error {
	user, err := s.CurrentUser(ctx, userID)
	if err != nil {
		return err
	}
	if !user.EmailVerified {
		return domain.ErrEmailNotVerified
	}
	return nil
}

// openSession creates the refresh session and issues the matching access token.
func (s *service) openSession(ctx context.Context, user domain.User, userAgent, ip string, rotatedFrom *uuid.UUID) (domain.Session, error) {
	raw, hash, err := s.deps.Tokens.Generate()
	if err != nil {
		return domain.Session{}, err
	}

	expiresAt := s.deps.Config.Clock().Add(s.deps.Config.RefreshTTL)
	record, err := s.deps.Repository.CreateSession(ctx, domain.NewSession{
		UserID:           user.ID,
		RefreshTokenHash: hash,
		UserAgent:        userAgent,
		IP:               ip,
		ExpiresAt:        expiresAt,
		RotatedFrom:      rotatedFrom,
	})
	if err != nil {
		return domain.Session{}, err
	}

	accessToken, accessExpiry, err := s.deps.Issuer.Issue(user.ID, record.ID)
	if err != nil {
		return domain.Session{}, err
	}

	return domain.Session{
		AccessToken:     accessToken,
		AccessTokenExp:  accessExpiry,
		RefreshToken:    raw,
		RefreshTokenExp: record.ExpiresAt,
		User:            user,
	}, nil
}

// sessionUsable reports whether a stored session may still be exchanged.
func (s *service) sessionUsable(record domain.SessionRecord) bool {
	if record.RevokedAt != nil {
		return false
	}
	return s.deps.Config.Clock().Before(record.ExpiresAt)
}

// allowLoginAttempt spends a token from the per-email and per-IP buckets. When
// Redis is not configured the throttle is skipped rather than blocking every
// sign-in.
func (s *service) allowLoginAttempt(ctx context.Context, email, ip string) error {
	if s.deps.Throttle == nil {
		return nil
	}

	keys := []string{"auth:login:email:" + email}
	if strings.TrimSpace(ip) != "" {
		keys = append(keys, "auth:login:ip:"+ip)
	}

	for _, key := range keys {
		result, err := s.deps.Throttle.Allow(ctx, key, s.deps.Config.LoginThrottleCapacity, s.deps.Config.LoginThrottleRefill)
		if err != nil {
			// A Redis failure degrades to "no throttling" rather than "nobody
			// can sign in": the credentials check still runs below.
			return nil //nolint:nilerr // deliberate: fail open on a limiter outage
		}
		if !result.Allowed {
			return fmt.Errorf("%w: retry in %s", domain.ErrTooManyAttempts, result.RetryAfter.Round(time.Second))
		}
	}

	return nil
}

func (s *service) resetLoginAttempts(ctx context.Context, email, ip string) {
	if s.deps.Throttle == nil {
		return
	}
	_ = s.deps.Throttle.Reset(ctx, "auth:login:email:"+email)
	if strings.TrimSpace(ip) != "" {
		_ = s.deps.Throttle.Reset(ctx, "auth:login:ip:"+ip)
	}
}

// verificationSubject is the subject of every verification email.
const verificationSubject = "Verifikasi email Bolu"

// sendVerification issues a one-time link and emails it.
func (s *service) sendVerification(ctx context.Context, user domain.User) error {
	body, err := s.issueVerificationToken(ctx, user)
	if err != nil {
		return err
	}
	return s.send(ctx, user.Email, verificationSubject, body)
}

// issueVerificationToken stores a one-time verification link for the account and
// returns the body of the email that carries it. It is separate from the delivery
// because the two failures mean different things: a token that could not be
// written is a broken request, a message that could not be sent is not.
func (s *service) issueVerificationToken(ctx context.Context, user domain.User) (string, error) {
	raw, hash, err := s.deps.Tokens.Generate()
	if err != nil {
		return "", err
	}

	expiresAt := s.deps.Config.Clock().Add(s.deps.Config.VerificationTTL)
	if err := s.deps.Repository.CreateEmailVerificationToken(ctx, user.ID, hash, expiresAt); err != nil {
		return "", err
	}

	return fmt.Sprintf("Buka tautan ini untuk memverifikasi email:\n\n%s/verify?token=%s\n\nTautan berlaku %s.",
		strings.TrimRight(s.deps.Config.FrontendURL, "/"), raw, humanize(s.deps.Config.VerificationTTL)), nil
}

func (s *service) send(ctx context.Context, to, subject, body string) error {
	if s.deps.Mailer == nil {
		return nil
	}
	return s.deps.Mailer.Send(ctx, to, subject, body)
}

// normalizeEmail validates and lowercases an address.
func normalizeEmail(email string) (string, error) {
	trimmed := strings.TrimSpace(email)
	if trimmed == "" {
		return "", fmt.Errorf("%w: email is required", domain.ErrInvalidInput)
	}
	if _, err := mail.ParseAddress(trimmed); err != nil {
		return "", fmt.Errorf("%w: email is not valid", domain.ErrInvalidInput)
	}
	return strings.ToLower(trimmed), nil
}

// humanize renders a duration the way a user reads it.
func humanize(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 hari"
		}
		return fmt.Sprintf("%d hari", days)
	case d >= time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 jam"
		}
		return fmt.Sprintf("%d jam", hours)
	default:
		return fmt.Sprintf("%d menit", int(d.Minutes()))
	}
}

var _ domain.Service = (*service)(nil)
