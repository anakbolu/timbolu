// Package repository implements the auth data access with pgx and the
// sqlc-generated queries.
//
// The auth tables (users, sessions, one-time tokens) are not tenant-scoped and
// carry no Row Level Security: access is controlled by knowing the token or the
// credentials. Tenant isolation starts at members/workspaces.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/auth/repository/sqlcgen"
)

// uniqueViolation is the PostgreSQL error code for a unique constraint breach.
const uniqueViolation = "23505"

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *pgxpool.Pool
}

// New builds the repository.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) queries() (*sqlcgen.Queries, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("auth: database is not configured")
	}
	return sqlcgen.New(r.pool), nil
}

// CreateUser stores a password account.
func (r *Repository) CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.CreateUser(ctx, sqlcgen.CreateUserParams{Email: email, PasswordHash: passwordHash})
	if err != nil {
		if isUniqueViolation(err) {
			return domain.User{}, domain.ErrEmailTaken
		}
		return domain.User{}, fmt.Errorf("auth: create user: %w", err)
	}
	return toUser(user), nil
}

// CreateGoogleUser stores a Google account, already verified.
func (r *Repository) CreateGoogleUser(ctx context.Context, email, subject string) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.CreateGoogleUser(ctx, sqlcgen.CreateGoogleUserParams{Email: email, GoogleSubject: &subject})
	if err != nil {
		if isUniqueViolation(err) {
			return domain.User{}, domain.ErrEmailTaken
		}
		return domain.User{}, fmt.Errorf("auth: create google user: %w", err)
	}
	return toUser(user), nil
}

// UserByEmail looks up an account by address, case insensitively.
func (r *Repository) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.GetUserByEmail(ctx, email)
	if err != nil {
		return domain.User{}, mapNotFound(err, "user by email")
	}
	return toUser(user), nil
}

// UserByID looks up an account by id.
func (r *Repository) UserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.GetUserByID(ctx, id)
	if err != nil {
		return domain.User{}, mapNotFound(err, "user by id")
	}
	return toUser(user), nil
}

// UserByGoogleSubject looks up an account by its Google identity.
func (r *Repository) UserByGoogleSubject(ctx context.Context, subject string) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.GetUserByGoogleSubject(ctx, &subject)
	if err != nil {
		return domain.User{}, mapNotFound(err, "user by google subject")
	}
	return toUser(user), nil
}

// LinkGoogleSubject attaches a Google identity to an existing account.
func (r *Repository) LinkGoogleSubject(ctx context.Context, userID uuid.UUID, subject string) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.LinkGoogleSubject(ctx, sqlcgen.LinkGoogleSubjectParams{ID: userID, GoogleSubject: &subject})
	if err != nil {
		if isUniqueViolation(err) {
			return domain.User{}, domain.ErrEmailTaken
		}
		return domain.User{}, fmt.Errorf("auth: link google subject: %w", err)
	}
	return toUser(user), nil
}

// MarkEmailVerified records that the address was confirmed.
func (r *Repository) MarkEmailVerified(ctx context.Context, userID uuid.UUID) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.MarkEmailVerified(ctx, userID)
	if err != nil {
		return domain.User{}, mapNotFound(err, "mark email verified")
	}
	return toUser(user), nil
}

// UpdatePasswordHash replaces the stored password hash.
func (r *Repository) UpdatePasswordHash(ctx context.Context, userID uuid.UUID, hash string) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.UpdatePasswordHash(ctx, sqlcgen.UpdatePasswordHashParams{ID: userID, PasswordHash: hash})
	if err != nil {
		return domain.User{}, mapNotFound(err, "update password hash")
	}
	return toUser(user), nil
}

// MarkOnboarded records that the account finished onboarding.
func (r *Repository) MarkOnboarded(ctx context.Context, userID uuid.UUID) (domain.User, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.User{}, err
	}

	user, err := queries.MarkOnboarded(ctx, userID)
	if err != nil {
		return domain.User{}, mapNotFound(err, "mark onboarded")
	}
	return toUser(user), nil
}

// MarkOnboardedInTx records the same fact inside a transaction the caller owns,
// which is how onboarding writes it together with the workspace it just created.
// A crash between the two would otherwise leave an account that owns a workspace
// its session does not believe in.
func (r *Repository) MarkOnboardedInTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if tx == nil {
		return fmt.Errorf("auth: marking onboarded needs a transaction")
	}

	if _, err := sqlcgen.New(tx).MarkOnboarded(ctx, userID); err != nil {
		return mapNotFound(err, "mark onboarded")
	}
	return nil
}

// PasswordHash returns the stored hash, or an empty string for an account
// without a password (Google-only).
func (r *Repository) PasswordHash(ctx context.Context, userID uuid.UUID) (string, error) {
	queries, err := r.queries()
	if err != nil {
		return "", err
	}

	user, err := queries.GetUserByID(ctx, userID)
	if err != nil {
		return "", mapNotFound(err, "password hash")
	}
	return user.PasswordHash, nil
}

// CreateSession stores a refresh session.
func (r *Repository) CreateSession(ctx context.Context, session domain.NewSession) (domain.SessionRecord, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.SessionRecord{}, err
	}

	created, err := queries.CreateSession(ctx, sqlcgen.CreateSessionParams{
		UserID:           session.UserID,
		RefreshTokenHash: session.RefreshTokenHash,
		UserAgent:        session.UserAgent,
		Ip:               session.IP,
		ExpiresAt:        session.ExpiresAt,
		RotatedFrom:      session.RotatedFrom,
	})
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("auth: create session: %w", err)
	}
	return toSession(created), nil
}

// SessionByTokenHash finds a session by the hash of its refresh token.
func (r *Repository) SessionByTokenHash(ctx context.Context, tokenHash string) (domain.SessionRecord, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.SessionRecord{}, err
	}

	session, err := queries.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		return domain.SessionRecord{}, mapNotFound(err, "session by token hash")
	}
	return toSession(session), nil
}

// SessionByID finds a session by id, used to validate an access token.
func (r *Repository) SessionByID(ctx context.Context, id uuid.UUID) (domain.SessionRecord, error) {
	queries, err := r.queries()
	if err != nil {
		return domain.SessionRecord{}, err
	}

	session, err := queries.GetSessionByID(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, mapNotFound(err, "session by id")
	}
	return toSession(session), nil
}

// RevokeSession marks one session as revoked.
func (r *Repository) RevokeSession(ctx context.Context, id uuid.UUID) error {
	queries, err := r.queries()
	if err != nil {
		return err
	}

	if _, err := queries.RevokeSession(ctx, id); err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}

// RevokeSessionByTokenHash revokes the session a refresh token belongs to.
func (r *Repository) RevokeSessionByTokenHash(ctx context.Context, tokenHash string) error {
	queries, err := r.queries()
	if err != nil {
		return err
	}

	if _, err := queries.RevokeSessionByTokenHash(ctx, tokenHash); err != nil {
		return fmt.Errorf("auth: revoke session by token: %w", err)
	}
	return nil
}

// RevokeSessionsForUser revokes every live session of a user, used after a
// password reset.
func (r *Repository) RevokeSessionsForUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	queries, err := r.queries()
	if err != nil {
		return 0, err
	}

	revoked, err := queries.RevokeSessionsForUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke sessions for user: %w", err)
	}
	return revoked, nil
}

// CreateEmailVerificationToken stores a one-time verification token hash.
func (r *Repository) CreateEmailVerificationToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	queries, err := r.queries()
	if err != nil {
		return err
	}

	_, err = queries.CreateEmailVerificationToken(ctx, sqlcgen.CreateEmailVerificationTokenParams{
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return fmt.Errorf("auth: create verification token: %w", err)
	}
	return nil
}

// ConsumeEmailVerificationToken marks the token used and returns its owner. A
// second call finds nothing, which is what enforces single use.
func (r *Repository) ConsumeEmailVerificationToken(ctx context.Context, tokenHash string) (uuid.UUID, error) {
	queries, err := r.queries()
	if err != nil {
		return uuid.Nil, err
	}

	token, err := queries.ConsumeEmailVerificationToken(ctx, tokenHash)
	if err != nil {
		return uuid.Nil, mapNotFound(err, "consume verification token")
	}
	return token.UserID, nil
}

// CreatePasswordResetToken stores a one-time reset token hash.
func (r *Repository) CreatePasswordResetToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	queries, err := r.queries()
	if err != nil {
		return err
	}

	_, err = queries.CreatePasswordResetToken(ctx, sqlcgen.CreatePasswordResetTokenParams{
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return fmt.Errorf("auth: create reset token: %w", err)
	}
	return nil
}

// ConsumePasswordResetToken marks the token used and returns its owner.
func (r *Repository) ConsumePasswordResetToken(ctx context.Context, tokenHash string) (uuid.UUID, error) {
	queries, err := r.queries()
	if err != nil {
		return uuid.Nil, err
	}

	token, err := queries.ConsumePasswordResetToken(ctx, tokenHash)
	if err != nil {
		return uuid.Nil, mapNotFound(err, "consume reset token")
	}
	return token.UserID, nil
}

func toUser(user sqlcgen.User) domain.User {
	return domain.User{
		ID:            user.ID,
		Email:         user.Email,
		EmailVerified: user.EmailVerifiedAt != nil,
		HasGoogle:     user.GoogleSubject != nil && *user.GoogleSubject != "",
		HasPassword:   user.PasswordHash != "",
		Onboarded:     user.OnboardedAt != nil,
		Role:          user.Role,
		CreatedAt:     user.CreatedAt,
	}
}

func toSession(session sqlcgen.Session) domain.SessionRecord {
	record := domain.SessionRecord{
		ID:        session.ID,
		UserID:    session.UserID,
		ExpiresAt: session.ExpiresAt,
	}
	if session.RevokedAt != nil {
		record.RevokedAt = session.RevokedAt
	}
	return record
}

// mapNotFound turns "no rows" into the domain error the service expects, so the
// service never has to import pgx.
func mapNotFound(err error, operation string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrUserNotFound
	}
	return fmt.Errorf("auth: %s: %w", operation, err)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// Compile-time check that the repository satisfies the domain contract.
var _ domain.Repository = (*Repository)(nil)
