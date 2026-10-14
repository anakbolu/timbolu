package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/authn"
)

// The primitives are used for real: they are pure and fast, and using them keeps
// the tests honest about the hashes and tokens the service produces. Only the
// repository and the outbound collaborators are mocked.

const testSecret = "test-secret"

type harness struct {
	service  domain.Service
	repo     *mocks.Repository
	mailer   *mocks.Mailer
	throttle *mocks.LoginThrottle
	google   *mocks.GoogleProvider
	states   *mocks.StateSigner
	issuer   *authn.Issuer
	now      time.Time
}

func newHarness(t *testing.T, opts ...func(*Deps)) *harness {
	t.Helper()

	issuer, err := authn.NewIssuer(testSecret, 15*time.Minute)
	require.NoError(t, err)

	h := &harness{
		repo:     mocks.NewRepository(t),
		mailer:   mocks.NewMailer(t),
		throttle: mocks.NewLoginThrottle(t),
		google:   mocks.NewGoogleProvider(t),
		states:   mocks.NewStateSigner(t),
		issuer:   issuer,
		now:      time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
	}

	deps := Deps{
		Repository: h.repo,
		Issuer:     issuerAdapter{h.issuer},
		States:     h.states,
		Mailer:     h.mailer,
		Throttle:   h.throttle,
		Google:     h.google,
		Passwords:  passwordsAdapter{},
		Tokens:     tokensAdapter{},
		Config: Config{
			RefreshTTL:               30 * 24 * time.Hour,
			VerificationTTL:          24 * time.Hour,
			ResetTTL:                 time.Hour,
			FrontendURL:              "https://app.example.com",
			RequireEmailVerification: true,
			Clock:                    func() time.Time { return h.now },
		},
	}
	for _, opt := range opts {
		opt(&deps)
	}

	h.service = New(deps)
	return h
}

// passwordsAdapter and tokensAdapter are the production primitives, wired
// directly: the container adapters only translate types.
type passwordsAdapter struct{}

func (passwordsAdapter) Hash(password string) (string, error) { return authn.HashPassword(password) }
func (passwordsAdapter) Verify(hash, password string) (bool, error) {
	return authn.VerifyPassword(hash, password)
}
func (passwordsAdapter) Validate(password string) error { return authn.ValidatePassword(password) }

type tokensAdapter struct{}

func (tokensAdapter) Generate() (string, string, error) { return authn.GenerateToken() }

// issuerAdapter is the same translation the container performs: the platform
// issuer returns its own claims type, the port returns the domain one.
type issuerAdapter struct{ issuer *authn.Issuer }

func (a issuerAdapter) Issue(userID, sessionID uuid.UUID) (string, time.Time, error) {
	return a.issuer.Issue(userID, sessionID)
}

func (a issuerAdapter) Verify(token string) (domain.AccessClaims, error) {
	claims, err := a.issuer.Verify(token)
	if err != nil {
		return domain.AccessClaims{}, err
	}
	return domain.AccessClaims{UserID: claims.UserID, SessionID: claims.SessionID}, nil
}

// allowLogin makes the throttle accept one attempt.
func (h *harness) allowLogin() {
	h.throttle.EXPECT().Allow(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(domain.ThrottleResult{Allowed: true}, nil).Maybe()
}

func mustHash(t *testing.T, password string) string {
	t.Helper()

	hash, err := authn.HashPassword(password)
	require.NoError(t, err)
	return hash
}

func verifiedUser(t *testing.T, email string) domain.User {
	t.Helper()

	return domain.User{
		ID:            uuid.New(),
		Email:         email,
		EmailVerified: true,
		HasPassword:   true,
		CreatedAt:     time.Now(),
	}
}

func TestRegister(t *testing.T) {
	t.Run("creates the account and emails a verification link", func(t *testing.T) {
		h := newHarness(t)
		user := domain.User{ID: uuid.New(), Email: "owner@example.com"}

		var storedHash string
		h.repo.EXPECT().CreateUser(mock.Anything, "owner@example.com", mock.Anything).
			RunAndReturn(func(_ context.Context, _ string, hash string) (domain.User, error) {
				storedHash = hash
				return user, nil
			}).Once()

		var tokenHash string
		h.repo.EXPECT().CreateEmailVerificationToken(mock.Anything, user.ID, mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _ uuid.UUID, hash string, expiresAt time.Time) error {
				tokenHash = hash
				require.WithinDuration(t, h.now.Add(24*time.Hour), expiresAt, time.Second)
				return nil
			}).Once()

		h.mailer.EXPECT().Send(mock.Anything, "owner@example.com", mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _, _, body string) error {
				require.Contains(t, body, "https://app.example.com/verify?token=")
				require.Contains(t, body, "Tautan berlaku 1 hari")
				return nil
			}).Once()

		created, err := h.service.Register(context.Background(), domain.RegisterRequest{
			Email:    "  Owner@Example.com ",
			Password: "a-good-password",
		})
		require.NoError(t, err)
		require.Equal(t, user.ID, created.ID)

		require.NotEmpty(t, storedHash)
		require.True(t, strings.HasPrefix(storedHash, "$argon2id$"), "the password must be argon2id hashed")
		require.NotEqual(t, "a-good-password", storedHash)
		require.NotEmpty(t, tokenHash, "a verification token must be stored")
		require.NotContains(t, tokenHash, "verify?token=", "only the hash is stored, not the link")
	})

	t.Run("marks the account verified and sends nothing when verification is off", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) { d.Config.RequireEmailVerification = false })
		user := domain.User{ID: uuid.New(), Email: "owner@example.com"}

		h.repo.EXPECT().CreateUser(mock.Anything, "owner@example.com", mock.Anything).
			Return(user, nil).Once()
		h.repo.EXPECT().MarkEmailVerified(mock.Anything, user.ID).
			Return(domain.User{ID: user.ID, Email: user.Email, EmailVerified: true}, nil).Once()

		created, err := h.service.Register(context.Background(), domain.RegisterRequest{
			Email:    "owner@example.com",
			Password: "a-good-password",
		})
		require.NoError(t, err)
		require.True(t, created.EmailVerified, "a deployment without verification creates a usable account")
	})

	t.Run("rejects a weak password before touching the database", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Register(context.Background(), domain.RegisterRequest{
			Email:    "owner@example.com",
			Password: "short",
		})
		require.ErrorIs(t, err, domain.ErrWeakPassword)
	})

	t.Run("rejects an invalid address", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Register(context.Background(), domain.RegisterRequest{
			Email:    "not-an-address",
			Password: "a-good-password",
		})
		require.Error(t, err)
	})

	t.Run("reports an address that is already registered", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().CreateUser(mock.Anything, mock.Anything, mock.Anything).
			Return(domain.User{}, domain.ErrEmailTaken).Once()

		_, err := h.service.Register(context.Background(), domain.RegisterRequest{
			Email:    "owner@example.com",
			Password: "a-good-password",
		})
		require.ErrorIs(t, err, domain.ErrEmailTaken)
	})
}

func TestVerifyEmail(t *testing.T) {
	t.Run("consumes the token and marks the account verified", func(t *testing.T) {
		h := newHarness(t)
		userID := uuid.New()

		h.repo.EXPECT().ConsumeEmailVerificationToken(mock.Anything, domain.HashToken("the-token")).
			Return(userID, nil).Once()
		h.repo.EXPECT().MarkEmailVerified(mock.Anything, userID).
			Return(domain.User{ID: userID, Email: "owner@example.com", EmailVerified: true}, nil).Once()

		user, err := h.service.VerifyEmail(context.Background(), "the-token")
		require.NoError(t, err)
		require.True(t, user.EmailVerified)
	})

	t.Run("a token that is unknown, used, or expired is rejected", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().ConsumeEmailVerificationToken(mock.Anything, mock.Anything).
			Return(uuid.Nil, domain.ErrUserNotFound).Once()

		_, err := h.service.VerifyEmail(context.Background(), "used-token")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("an empty token is rejected without a query", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.VerifyEmail(context.Background(), "  ")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})
}

func TestLogin(t *testing.T) {
	const password = "a-good-password"

	t.Run("opens a session on correct credentials", func(t *testing.T) {
		h := newHarness(t)
		h.allowLogin()

		user := verifiedUser(t, "owner@example.com")
		h.repo.EXPECT().UserByEmail(mock.Anything, "owner@example.com").Return(user, nil).Once()
		h.repo.EXPECT().PasswordHash(mock.Anything, user.ID).Return(mustHash(t, password), nil).Once()

		var storedHash string
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, session domain.NewSession) (domain.SessionRecord, error) {
				storedHash = session.RefreshTokenHash
				require.Equal(t, user.ID, session.UserID)
				require.Equal(t, "test-agent", session.UserAgent)
				require.Equal(t, "10.0.0.1", session.IP)
				require.Nil(t, session.RotatedFrom)
				return domain.SessionRecord{ID: uuid.New(), UserID: user.ID, ExpiresAt: session.ExpiresAt}, nil
			}).Once()
		h.throttle.EXPECT().Reset(mock.Anything, mock.Anything).Return(nil).Maybe()

		session, err := h.service.Login(context.Background(), domain.LoginRequest{
			Email:     "owner@example.com",
			Password:  password,
			UserAgent: "test-agent",
			IP:        "10.0.0.1",
		})
		require.NoError(t, err)
		require.NotEmpty(t, session.AccessToken)
		require.NotEmpty(t, session.RefreshToken)
		require.NotEqual(t, session.RefreshToken, storedHash, "the raw refresh token must not be stored")
		require.Equal(t, domain.HashToken(session.RefreshToken), storedHash)

		claims, err := h.issuer.Verify(session.AccessToken)
		require.NoError(t, err)
		require.Equal(t, user.ID, claims.UserID)
	})

	t.Run("unknown address and wrong password give the same error", func(t *testing.T) {
		for name, setup := range map[string]func(*harness){
			"unknown address": func(h *harness) {
				h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).
					Return(domain.User{}, domain.ErrUserNotFound).Once()
			},
			"wrong password": func(h *harness) {
				user := verifiedUser(t, "owner@example.com")
				h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(user, nil).Once()
				h.repo.EXPECT().PasswordHash(mock.Anything, user.ID).Return(mustHash(t, password), nil).Once()
			},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.allowLogin()
				setup(h)

				_, err := h.service.Login(context.Background(), domain.LoginRequest{
					Email:    "owner@example.com",
					Password: "the-wrong-password",
				})
				require.ErrorIs(t, err, domain.ErrInvalidCredentials)
			})
		}
	})

	t.Run("a google-only account has no password to check", func(t *testing.T) {
		h := newHarness(t)
		h.allowLogin()

		user := domain.User{ID: uuid.New(), Email: "owner@example.com", HasGoogle: true}
		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(user, nil).Once()
		h.repo.EXPECT().PasswordHash(mock.Anything, user.ID).Return("", nil).Once()

		_, err := h.service.Login(context.Background(), domain.LoginRequest{
			Email:    "owner@example.com",
			Password: password,
		})
		require.ErrorIs(t, err, domain.ErrInvalidCredentials)
	})

	t.Run("an unverified account can sign in but is not verified", func(t *testing.T) {
		h := newHarness(t)
		h.allowLogin()

		user := domain.User{ID: uuid.New(), Email: "owner@example.com", HasPassword: true}
		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(user, nil).Once()
		h.repo.EXPECT().PasswordHash(mock.Anything, user.ID).Return(mustHash(t, password), nil).Once()
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: user.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()
		h.throttle.EXPECT().Reset(mock.Anything, mock.Anything).Return(nil).Maybe()

		session, err := h.service.Login(context.Background(), domain.LoginRequest{
			Email:    "owner@example.com",
			Password: password,
		})
		require.NoError(t, err)
		require.False(t, session.User.EmailVerified)

		// The features that need verification stay closed.
		h.repo.EXPECT().UserByID(mock.Anything, user.ID).Return(user, nil).Once()
		require.ErrorIs(t, h.service.AssertVerified(context.Background(), user.ID), domain.ErrEmailNotVerified)
	})

	t.Run("heals an unverified account when the deployment does not require verification", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) { d.Config.RequireEmailVerification = false })
		h.allowLogin()

		user := domain.User{ID: uuid.New(), Email: "owner@example.com", HasPassword: true}
		verified := domain.User{ID: user.ID, Email: user.Email, EmailVerified: true, HasPassword: true}

		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(user, nil).Once()
		h.repo.EXPECT().PasswordHash(mock.Anything, user.ID).Return(mustHash(t, password), nil).Once()
		h.repo.EXPECT().MarkEmailVerified(mock.Anything, user.ID).Return(verified, nil).Once()
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: user.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()
		h.throttle.EXPECT().Reset(mock.Anything, mock.Anything).Return(nil).Maybe()

		session, err := h.service.Login(context.Background(), domain.LoginRequest{
			Email:    "owner@example.com",
			Password: password,
		})
		require.NoError(t, err)
		require.True(t, session.User.EmailVerified, "an account that predates the switch is healed at sign-in")
	})

	t.Run("too many attempts are refused", func(t *testing.T) {
		h := newHarness(t)
		h.throttle.EXPECT().Allow(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.ThrottleResult{Allowed: false, RetryAfter: 5 * time.Second}, nil).Once()

		_, err := h.service.Login(context.Background(), domain.LoginRequest{
			Email:    "owner@example.com",
			Password: password,
		})
		require.ErrorIs(t, err, domain.ErrTooManyAttempts)
	})

	t.Run("a failing throttle does not lock the user out", func(t *testing.T) {
		h := newHarness(t)
		h.throttle.EXPECT().Allow(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(domain.ThrottleResult{}, errors.New("redis is down")).Maybe()

		user := verifiedUser(t, "owner@example.com")
		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(user, nil).Once()
		h.repo.EXPECT().PasswordHash(mock.Anything, user.ID).Return(mustHash(t, password), nil).Once()
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: user.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()
		h.throttle.EXPECT().Reset(mock.Anything, mock.Anything).Return(errors.New("redis is down")).Maybe()

		_, err := h.service.Login(context.Background(), domain.LoginRequest{
			Email:    "owner@example.com",
			Password: password,
		})
		require.NoError(t, err, "a limiter outage must not block a valid sign-in")
	})
}

func TestRefreshRotatesTheSession(t *testing.T) {
	t.Run("the presented token is revoked and a new one is issued", func(t *testing.T) {
		h := newHarness(t)
		user := verifiedUser(t, "owner@example.com")
		oldSession := uuid.New()

		h.repo.EXPECT().SessionByTokenHash(mock.Anything, domain.HashToken("old-refresh")).
			Return(domain.SessionRecord{ID: oldSession, UserID: user.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()
		h.repo.EXPECT().UserByID(mock.Anything, user.ID).Return(user, nil).Once()
		h.repo.EXPECT().RevokeSession(mock.Anything, oldSession).Return(nil).Once()

		var rotatedFrom *uuid.UUID
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, session domain.NewSession) (domain.SessionRecord, error) {
				rotatedFrom = session.RotatedFrom
				return domain.SessionRecord{ID: uuid.New(), UserID: user.ID, ExpiresAt: session.ExpiresAt}, nil
			}).Once()

		session, err := h.service.Refresh(context.Background(), "old-refresh")
		require.NoError(t, err)
		require.NotEqual(t, "old-refresh", session.RefreshToken)
		require.NotNil(t, rotatedFrom)
		require.Equal(t, oldSession, *rotatedFrom, "the new session must record what it replaced")
	})

	t.Run("an unknown token is rejected", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().SessionByTokenHash(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{}, domain.ErrUserNotFound).Once()

		_, err := h.service.Refresh(context.Background(), "unknown")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("a revoked session is rejected", func(t *testing.T) {
		h := newHarness(t)
		revoked := h.now.Add(-time.Minute)

		h.repo.EXPECT().SessionByTokenHash(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: uuid.New(), ExpiresAt: h.now.Add(time.Hour), RevokedAt: &revoked}, nil).Once()

		_, err := h.service.Refresh(context.Background(), "revoked")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("an expired session is rejected", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().SessionByTokenHash(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: uuid.New(), ExpiresAt: h.now.Add(-time.Minute)}, nil).Once()

		_, err := h.service.Refresh(context.Background(), "expired")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("an empty token is rejected without a query", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Refresh(context.Background(), "")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})
}

func TestLogout(t *testing.T) {
	t.Run("revokes the session behind the token", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().RevokeSessionByTokenHash(mock.Anything, domain.HashToken("refresh")).Return(nil).Once()

		require.NoError(t, h.service.Logout(context.Background(), "refresh"))
	})

	t.Run("is idempotent for a missing token", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.service.Logout(context.Background(), ""))
	})
}

func TestPasswordReset(t *testing.T) {
	t.Run("sends a link for a known address", func(t *testing.T) {
		h := newHarness(t)
		user := verifiedUser(t, "owner@example.com")

		h.repo.EXPECT().UserByEmail(mock.Anything, "owner@example.com").Return(user, nil).Once()
		h.repo.EXPECT().CreatePasswordResetToken(mock.Anything, user.ID, mock.Anything, mock.Anything).
			Return(nil).Once()
		h.mailer.EXPECT().Send(mock.Anything, "owner@example.com", mock.Anything, mock.Anything).
			RunAndReturn(func(_ context.Context, _, _, body string) error {
				require.Contains(t, body, "https://app.example.com/forgot-password?token=")
				return nil
			}).Once()

		require.NoError(t, h.service.RequestPasswordReset(context.Background(), "owner@example.com"))
	})

	// An unknown address must look exactly like a known one, otherwise the
	// endpoint becomes an account enumeration oracle.
	t.Run("reports success for an unknown address", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(domain.User{}, domain.ErrUserNotFound).Once()

		require.NoError(t, h.service.RequestPasswordReset(context.Background(), "nobody@example.com"))
	})

	t.Run("stores the new password and revokes every session", func(t *testing.T) {
		h := newHarness(t)
		userID := uuid.New()

		h.repo.EXPECT().ConsumePasswordResetToken(mock.Anything, domain.HashToken("reset-token")).
			Return(userID, nil).Once()

		var newHash string
		h.repo.EXPECT().UpdatePasswordHash(mock.Anything, userID, mock.Anything).
			RunAndReturn(func(_ context.Context, _ uuid.UUID, hash string) (domain.User, error) {
				newHash = hash
				return domain.User{ID: userID}, nil
			}).Once()
		h.repo.EXPECT().RevokeSessionsForUser(mock.Anything, userID).Return(int64(2), nil).Once()

		err := h.service.ResetPassword(context.Background(), domain.ResetPasswordRequest{
			Token:       "reset-token",
			NewPassword: "the-new-password",
		})
		require.NoError(t, err)

		ok, err := authn.VerifyPassword(newHash, "the-new-password")
		require.NoError(t, err)
		require.True(t, ok, "the stored hash must match the new password")
	})

	t.Run("rejects an invalid token", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().ConsumePasswordResetToken(mock.Anything, mock.Anything).
			Return(uuid.Nil, domain.ErrUserNotFound).Once()

		err := h.service.ResetPassword(context.Background(), domain.ResetPasswordRequest{
			Token:       "used",
			NewPassword: "the-new-password",
		})
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("rejects a weak password before consuming the token", func(t *testing.T) {
		h := newHarness(t)

		err := h.service.ResetPassword(context.Background(), domain.ResetPasswordRequest{
			Token:       "reset-token",
			NewPassword: "short",
		})
		require.ErrorIs(t, err, domain.ErrWeakPassword)
	})
}

func TestGoogleCallback(t *testing.T) {
	t.Run("signs in an account that is already linked", func(t *testing.T) {
		h := newHarness(t)
		user := verifiedUser(t, "owner@example.com")
		user.HasGoogle = true

		h.states.EXPECT().Verify("state").Return(nil).Once()
		h.google.EXPECT().Exchange(mock.Anything, "code").
			Return(domain.GoogleUser{Subject: "sub-1", Email: "owner@example.com", EmailVerified: true}, nil).Once()
		h.repo.EXPECT().UserByGoogleSubject(mock.Anything, "sub-1").Return(user, nil).Once()
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: user.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()

		session, err := h.service.GoogleCallback(context.Background(), domain.GoogleCallbackRequest{Code: "code", State: "state"})
		require.NoError(t, err)
		require.Equal(t, user.ID, session.User.ID)
	})

	t.Run("links to the existing account with the same address", func(t *testing.T) {
		h := newHarness(t)
		existing := verifiedUser(t, "owner@example.com")

		h.states.EXPECT().Verify("state").Return(nil).Once()
		h.google.EXPECT().Exchange(mock.Anything, "code").
			Return(domain.GoogleUser{Subject: "sub-2", Email: "Owner@Example.com", EmailVerified: true}, nil).Once()
		h.repo.EXPECT().UserByGoogleSubject(mock.Anything, "sub-2").
			Return(domain.User{}, domain.ErrUserNotFound).Once()
		h.repo.EXPECT().UserByEmail(mock.Anything, "owner@example.com").Return(existing, nil).Once()
		h.repo.EXPECT().LinkGoogleSubject(mock.Anything, existing.ID, "sub-2").
			Return(domain.User{ID: existing.ID, Email: existing.Email, EmailVerified: true, HasGoogle: true}, nil).Once()
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: existing.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()

		session, err := h.service.GoogleCallback(context.Background(), domain.GoogleCallbackRequest{Code: "code", State: "state"})
		require.NoError(t, err)
		require.Equal(t, existing.ID, session.User.ID, "the Google identity must attach to the existing account")
	})

	t.Run("creates an account when nothing matches", func(t *testing.T) {
		h := newHarness(t)
		created := verifiedUser(t, "new@example.com")

		h.states.EXPECT().Verify("state").Return(nil).Once()
		h.google.EXPECT().Exchange(mock.Anything, "code").
			Return(domain.GoogleUser{Subject: "sub-3", Email: "new@example.com", EmailVerified: true}, nil).Once()
		h.repo.EXPECT().UserByGoogleSubject(mock.Anything, "sub-3").Return(domain.User{}, domain.ErrUserNotFound).Once()
		h.repo.EXPECT().UserByEmail(mock.Anything, "new@example.com").Return(domain.User{}, domain.ErrUserNotFound).Once()
		h.repo.EXPECT().CreateGoogleUser(mock.Anything, "new@example.com", "sub-3").Return(created, nil).Once()
		h.repo.EXPECT().CreateSession(mock.Anything, mock.Anything).
			Return(domain.SessionRecord{ID: uuid.New(), UserID: created.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()

		session, err := h.service.GoogleCallback(context.Background(), domain.GoogleCallbackRequest{Code: "code", State: "state"})
		require.NoError(t, err)
		require.Equal(t, created.ID, session.User.ID)
	})

	t.Run("rejects a bad state before calling Google", func(t *testing.T) {
		h := newHarness(t)
		h.states.EXPECT().Verify("tampered").Return(authn.ErrInvalidState).Once()

		_, err := h.service.GoogleCallback(context.Background(), domain.GoogleCallbackRequest{Code: "code", State: "tampered"})
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("rejects an address the provider did not verify", func(t *testing.T) {
		h := newHarness(t)
		h.states.EXPECT().Verify("state").Return(nil).Once()
		h.google.EXPECT().Exchange(mock.Anything, "code").
			Return(domain.GoogleUser{}, domain.ErrGoogleEmailUnverified).Once()

		_, err := h.service.GoogleCallback(context.Background(), domain.GoogleCallbackRequest{Code: "code", State: "state"})
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("reports that Google is not configured", func(t *testing.T) {
		h := newHarness(t, func(deps *Deps) { deps.Google = nil })

		if _, err := h.service.GoogleAuthURL(context.Background()); !errors.Is(err, domain.ErrGoogleNotConfigured) {
			t.Fatalf("expected ErrGoogleNotConfigured, got %v", err)
		}
		_, err := h.service.GoogleCallback(context.Background(), domain.GoogleCallbackRequest{Code: "code", State: "state"})
		require.ErrorIs(t, err, domain.ErrGoogleNotConfigured)
	})
}

func TestGoogleAuthURL(t *testing.T) {
	h := newHarness(t)
	h.states.EXPECT().Sign().Return("signed-state", nil).Once()
	h.google.EXPECT().AuthCodeURL("signed-state").Return("https://accounts.google.com/o/oauth2/v2/auth?state=signed-state").Once()

	url, err := h.service.GoogleAuthURL(context.Background())
	require.NoError(t, err)
	require.Contains(t, url, "state=signed-state")
}

func TestAuthenticate(t *testing.T) {
	t.Run("accepts a token whose session is alive", func(t *testing.T) {
		h := newHarness(t)
		user := verifiedUser(t, "owner@example.com")
		sessionID := uuid.New()

		token, _, err := h.issuer.Issue(user.ID, sessionID)
		require.NoError(t, err)

		h.repo.EXPECT().SessionByID(mock.Anything, sessionID).
			Return(domain.SessionRecord{ID: sessionID, UserID: user.ID, ExpiresAt: h.now.Add(time.Hour)}, nil).Once()
		h.repo.EXPECT().UserByID(mock.Anything, user.ID).Return(user, nil).Once()

		got, err := h.service.Authenticate(context.Background(), token)
		require.NoError(t, err)
		require.Equal(t, user.ID, got.ID)
	})

	// Signing out must invalidate the access token immediately, not only when it
	// expires.
	t.Run("rejects a token whose session was revoked", func(t *testing.T) {
		h := newHarness(t)
		userID, sessionID := uuid.New(), uuid.New()
		revoked := h.now.Add(-time.Minute)

		token, _, err := h.issuer.Issue(userID, sessionID)
		require.NoError(t, err)

		h.repo.EXPECT().SessionByID(mock.Anything, sessionID).
			Return(domain.SessionRecord{ID: sessionID, UserID: userID, ExpiresAt: h.now.Add(time.Hour), RevokedAt: &revoked}, nil).Once()

		_, err = h.service.Authenticate(context.Background(), token)
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("rejects a token for a session that does not exist", func(t *testing.T) {
		h := newHarness(t)
		token, _, err := h.issuer.Issue(uuid.New(), uuid.New())
		require.NoError(t, err)

		h.repo.EXPECT().SessionByID(mock.Anything, mock.Anything).Return(domain.SessionRecord{}, domain.ErrUserNotFound).Once()

		_, err = h.service.Authenticate(context.Background(), token)
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})

	t.Run("rejects a malformed token", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.service.Authenticate(context.Background(), "not-a-token")
		require.ErrorIs(t, err, domain.ErrInvalidToken)
	})
}

func TestResendVerification(t *testing.T) {
	t.Run("sends a new link to an unverified account", func(t *testing.T) {
		h := newHarness(t)
		user := domain.User{ID: uuid.New(), Email: "owner@example.com"}

		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).Return(user, nil).Once()
		h.repo.EXPECT().CreateEmailVerificationToken(mock.Anything, user.ID, mock.Anything, mock.Anything).Return(nil).Once()
		h.mailer.EXPECT().Send(mock.Anything, user.Email, mock.Anything, mock.Anything).Return(nil).Once()

		require.NoError(t, h.service.ResendVerification(context.Background(), user.Email))
	})

	t.Run("stays silent for an already verified account", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).
			Return(domain.User{ID: uuid.New(), EmailVerified: true}, nil).Once()

		require.NoError(t, h.service.ResendVerification(context.Background(), "owner@example.com"))
	})

	t.Run("stays silent for an unknown address", func(t *testing.T) {
		h := newHarness(t)
		h.repo.EXPECT().UserByEmail(mock.Anything, mock.Anything).
			Return(domain.User{}, domain.ErrUserNotFound).Once()

		require.NoError(t, h.service.ResendVerification(context.Background(), "nobody@example.com"))
	})
}
