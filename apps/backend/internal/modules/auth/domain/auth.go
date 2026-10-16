// Package domain defines the authentication contracts: the entities, the
// repository and service interfaces, and the request/response shapes. It
// imports only the standard library and github.com/google/uuid, so the module
// can be tested without HTTP, a database, or a provider.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errors returned by the service. Handlers map them to status codes, so the
// mapping lives in one place instead of being guessed per endpoint.
var (
	ErrEmailTaken          = errors.New("auth: email is already registered")
	ErrInvalidCredentials  = errors.New("auth: email or password is incorrect")
	ErrEmailNotVerified    = errors.New("auth: email is not verified")
	ErrInvalidToken        = errors.New("auth: token is invalid, expired, or already used")
	ErrTooManyAttempts     = errors.New("auth: too many attempts")
	ErrUserNotFound        = errors.New("auth: user not found")
	ErrWeakPassword        = errors.New("auth: password does not meet the policy")
	ErrGoogleNotConfigured = errors.New("auth: google sign-in is not configured")
	// ErrInvalidInput means the request itself is wrong (a malformed address).
	// Handlers answer 400 for it.
	ErrInvalidInput  = errors.New("auth: invalid input")
	ErrEmailMismatch = errors.New("auth: invitation email does not match the signed-in account")
	// ErrGoogleEmailUnverified means the provider did not assert the address,
	// so the account must not be linked or created from it.
	ErrGoogleEmailUnverified = errors.New("auth: google did not verify the email address")
)

// The platform role of an account, matching the check constraint on
// `users.role`.
//
// It is not the same thing as a workspace role, which is stored on `members`
// and answers "what may this person do inside this workspace". This one answers
// "may this person see the deployment", and only the operator surface at /admin
// reads it.
const (
	// RoleUser is every account: the default on the column, and what a
	// registration and an invitation both produce.
	RoleUser = "user"
	// RoleAdmin reaches the operator surface. It is granted by hand — see the
	// 000024 migration — and by another admin afterwards.
	RoleAdmin = "admin"
)

// IsAdmin reports whether a platform role may reach the operator surface. An
// unrecognised or empty role is not one, so a value that never went through the
// column's check constraint still fails closed.
func IsAdmin(role string) bool { return role == RoleAdmin }

// User is an account. The password hash never leaves the backend.
type User struct {
	ID            uuid.UUID
	Email         string
	EmailVerified bool
	HasGoogle     bool
	HasPassword   bool
	Onboarded     bool
	// Role is the platform role. It travels with the account rather than with a
	// session, so a promotion takes effect on the next request instead of on the
	// next sign-in.
	Role      string
	CreatedAt time.Time
}

// Session is what a successful sign-in returns: an access token for API calls
// and a refresh token for a new access token.
type Session struct {
	AccessToken     string
	AccessTokenExp  time.Time
	RefreshToken    string
	RefreshTokenExp time.Time
	User            User
}

// SessionRecord is a stored refresh session.
type SessionRecord struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// RegisterRequest is the sign-up payload.
type RegisterRequest struct {
	Email    string
	Password string
}

// LoginRequest carries the credentials and the client context recorded on the
// session.
type LoginRequest struct {
	Email     string
	Password  string
	UserAgent string
	IP        string
}

// GoogleCallbackRequest carries the code and the state returned by Google.
type GoogleCallbackRequest struct {
	Code      string
	State     string
	UserAgent string
	IP        string
}

// ResetPasswordRequest is the password reset payload.
type ResetPasswordRequest struct {
	Token       string
	NewPassword string
}

// Repository is the persistence contract. Implementations must run the writes
// inside the caller's scope so Row Level Security applies.
type Repository interface {
	// Users.
	CreateUser(ctx context.Context, email, passwordHash string) (User, error)
	CreateGoogleUser(ctx context.Context, email, subject string) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)
	UserByID(ctx context.Context, id uuid.UUID) (User, error)
	UserByGoogleSubject(ctx context.Context, subject string) (User, error)
	LinkGoogleSubject(ctx context.Context, userID uuid.UUID, subject string) (User, error)
	MarkEmailVerified(ctx context.Context, userID uuid.UUID) (User, error)
	UpdatePasswordHash(ctx context.Context, userID uuid.UUID, hash string) (User, error)
	MarkOnboarded(ctx context.Context, userID uuid.UUID) (User, error)
	// PasswordHash returns the stored hash for a user; kept out of User so the
	// hash is never carried around by accident.
	PasswordHash(ctx context.Context, userID uuid.UUID) (string, error)

	// Sessions.
	CreateSession(ctx context.Context, session NewSession) (SessionRecord, error)
	SessionByTokenHash(ctx context.Context, tokenHash string) (SessionRecord, error)
	SessionByID(ctx context.Context, id uuid.UUID) (SessionRecord, error)
	RevokeSession(ctx context.Context, id uuid.UUID) error
	RevokeSessionByTokenHash(ctx context.Context, tokenHash string) error
	RevokeSessionsForUser(ctx context.Context, userID uuid.UUID) (int64, error)

	// One-time tokens.
	CreateEmailVerificationToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error
	ConsumeEmailVerificationToken(ctx context.Context, tokenHash string) (uuid.UUID, error)
	CreatePasswordResetToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error
	ConsumePasswordResetToken(ctx context.Context, tokenHash string) (uuid.UUID, error)
}

// NewSession is a session to store.
type NewSession struct {
	UserID           uuid.UUID
	RefreshTokenHash string
	UserAgent        string
	IP               string
	ExpiresAt        time.Time
	RotatedFrom      *uuid.UUID
}

// Service is the authentication use case contract.
type Service interface {
	Register(ctx context.Context, req RegisterRequest) (User, error)
	VerifyEmail(ctx context.Context, token string) (User, error)
	ResendVerification(ctx context.Context, email string) error
	Login(ctx context.Context, req LoginRequest) (Session, error)
	Refresh(ctx context.Context, refreshToken string) (Session, error)
	Logout(ctx context.Context, refreshToken string) error
	RequestPasswordReset(ctx context.Context, email string) error
	ResetPassword(ctx context.Context, req ResetPasswordRequest) error
	GoogleAuthURL(ctx context.Context) (string, error)
	GoogleCallback(ctx context.Context, req GoogleCallbackRequest) (Session, error)
	CurrentUser(ctx context.Context, userID uuid.UUID) (User, error)
	// Authenticate verifies an access token and returns the account behind it.
	// It also checks that the session the token was issued for is still alive,
	// so signing out or resetting a password invalidates outstanding tokens.
	Authenticate(ctx context.Context, accessToken string) (User, error)
	// AssertVerified rejects an account that has not confirmed its email. The
	// workspace endpoints call it, so an unverified account cannot use the
	// features that require verification.
	AssertVerified(ctx context.Context, userID uuid.UUID) error
}
