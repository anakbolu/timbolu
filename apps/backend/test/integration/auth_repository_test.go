package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// newAuthRepo opens a repository against a migrated database.
func newAuthRepo(t *testing.T, dsn string) *authrepo.Repository {
	t.Helper()

	pool, err := database.New(t.Context(), database.Config{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return authrepo.New(pool.PgxPool())
}

func TestAuthRepositoryUsers(t *testing.T) {
	ctx := t.Context()
	repo := newAuthRepo(t, appDatabase(t))

	t.Run("create and read back", func(t *testing.T) {
		user, err := repo.CreateUser(ctx, "Owner@Example.com", "$argon2id$hash")
		require.NoError(t, err)
		require.NotEqual(t, uuid.Nil, user.ID)
		require.False(t, user.EmailVerified)
		require.True(t, user.HasPassword)
		require.False(t, user.HasGoogle)

		// The lookup is case insensitive, so a user who types their address
		// differently still signs in.
		found, err := repo.UserByEmail(ctx, "owner@example.com")
		require.NoError(t, err)
		require.Equal(t, user.ID, found.ID)

		byID, err := repo.UserByID(ctx, user.ID)
		require.NoError(t, err)
		require.Equal(t, user.ID, byID.ID)

		hash, err := repo.PasswordHash(ctx, user.ID)
		require.NoError(t, err)
		require.Equal(t, "$argon2id$hash", hash)
	})

	t.Run("a duplicate address is refused", func(t *testing.T) {
		_, err := repo.CreateUser(ctx, "owner@example.com", "$argon2id$hash")
		require.ErrorIs(t, err, domain.ErrEmailTaken)

		// The uniqueness is on the lowercased address.
		_, err = repo.CreateUser(ctx, "OWNER@EXAMPLE.COM", "$argon2id$hash")
		require.ErrorIs(t, err, domain.ErrEmailTaken)
	})

	t.Run("an unknown account is reported as not found", func(t *testing.T) {
		_, err := repo.UserByEmail(ctx, "nobody@example.com")
		require.ErrorIs(t, err, domain.ErrUserNotFound)

		_, err = repo.UserByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrUserNotFound)
	})

	t.Run("verification, password change and onboarding are recorded", func(t *testing.T) {
		user, err := repo.CreateUser(ctx, "verify@example.com", "$argon2id$hash")
		require.NoError(t, err)

		verified, err := repo.MarkEmailVerified(ctx, user.ID)
		require.NoError(t, err)
		require.True(t, verified.EmailVerified)

		updated, err := repo.UpdatePasswordHash(ctx, user.ID, "$argon2id$new")
		require.NoError(t, err)
		require.True(t, updated.HasPassword)

		hash, err := repo.PasswordHash(ctx, user.ID)
		require.NoError(t, err)
		require.Equal(t, "$argon2id$new", hash)

		onboarded, err := repo.MarkOnboarded(ctx, user.ID)
		require.NoError(t, err)
		require.True(t, onboarded.Onboarded)

		// Marking twice keeps the first timestamp.
		again, err := repo.MarkOnboarded(ctx, user.ID)
		require.NoError(t, err)
		require.Equal(t, onboarded.Onboarded, again.Onboarded)
	})

	t.Run("a google identity is created and linked", func(t *testing.T) {
		google, err := repo.CreateGoogleUser(ctx, "google@example.com", "subject-1")
		require.NoError(t, err)
		require.True(t, google.EmailVerified, "the provider asserted the address")
		require.True(t, google.HasGoogle)
		require.False(t, google.HasPassword)

		found, err := repo.UserByGoogleSubject(ctx, "subject-1")
		require.NoError(t, err)
		require.Equal(t, google.ID, found.ID)

		// Linking attaches the identity to an existing password account.
		password, err := repo.CreateUser(ctx, "linked@example.com", "$argon2id$hash")
		require.NoError(t, err)
		require.False(t, password.HasGoogle)

		linked, err := repo.LinkGoogleSubject(ctx, password.ID, "subject-2")
		require.NoError(t, err)
		require.True(t, linked.HasGoogle)
		require.True(t, linked.EmailVerified, "linking also confirms the address")
		require.True(t, linked.HasPassword, "the password stays usable")

		// A subject can only be linked once.
		other, err := repo.CreateUser(ctx, "other@example.com", "$argon2id$hash")
		require.NoError(t, err)
		_, err = repo.LinkGoogleSubject(ctx, other.ID, "subject-2")
		require.ErrorIs(t, err, domain.ErrEmailTaken)
	})
}

func TestAuthRepositorySessions(t *testing.T) {
	ctx := t.Context()
	dsn := appDatabase(t)
	repo := newAuthRepo(t, dsn)
	pool := poolFor(t, dsn)

	user, err := repo.CreateUser(ctx, "session@example.com", "$argon2id$hash")
	require.NoError(t, err)

	session, err := repo.CreateSession(ctx, domain.NewSession{
		UserID:           user.ID,
		RefreshTokenHash: "hash-1",
		UserAgent:        "test-agent",
		IP:               "10.0.0.1",
		ExpiresAt:        time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, session.ID)
	require.Nil(t, session.RevokedAt)

	byHash, err := repo.SessionByTokenHash(ctx, "hash-1")
	require.NoError(t, err)
	require.Equal(t, session.ID, byHash.ID)

	byID, err := repo.SessionByID(ctx, session.ID)
	require.NoError(t, err)
	require.Equal(t, user.ID, byID.UserID)

	_, err = repo.SessionByTokenHash(ctx, "unknown-hash")
	require.ErrorIs(t, err, domain.ErrUserNotFound)

	t.Run("revoking by id", func(t *testing.T) {
		require.NoError(t, repo.RevokeSession(ctx, session.ID))

		revoked, err := repo.SessionByID(ctx, session.ID)
		require.NoError(t, err)
		require.NotNil(t, revoked.RevokedAt)

		// Revoking again is not an error, and does not move the timestamp.
		require.NoError(t, repo.RevokeSession(ctx, session.ID))
	})

	t.Run("revoking by token hash", func(t *testing.T) {
		second, err := repo.CreateSession(ctx, domain.NewSession{
			UserID:           user.ID,
			RefreshTokenHash: "hash-2",
			ExpiresAt:        time.Now().Add(time.Hour),
		})
		require.NoError(t, err)

		require.NoError(t, repo.RevokeSessionByTokenHash(ctx, "hash-2"))

		revoked, err := repo.SessionByID(ctx, second.ID)
		require.NoError(t, err)
		require.NotNil(t, revoked.RevokedAt)
	})

	t.Run("rotation records the session it replaced", func(t *testing.T) {
		rotated, err := repo.CreateSession(ctx, domain.NewSession{
			UserID:           user.ID,
			RefreshTokenHash: "hash-3",
			ExpiresAt:        time.Now().Add(time.Hour),
			RotatedFrom:      &session.ID,
		})
		require.NoError(t, err)
		require.NotEqual(t, session.ID, rotated.ID, "rotation issues a new session, it does not reuse the old one")

		// The chain is what makes a replayed refresh token detectable.
		var rotatedFrom *uuid.UUID
		require.NoError(t, pool.PgxPool().QueryRow(ctx,
			"SELECT rotated_from FROM sessions WHERE id = $1", rotated.ID).Scan(&rotatedFrom))
		require.NotNil(t, rotatedFrom)
		require.Equal(t, session.ID, *rotatedFrom)
	})

	t.Run("revoking every session of a user", func(t *testing.T) {
		other, err := repo.CreateUser(ctx, "other-sessions@example.com", "$argon2id$hash")
		require.NoError(t, err)
		_, err = repo.CreateSession(ctx, domain.NewSession{
			UserID:           other.ID,
			RefreshTokenHash: "hash-4",
			ExpiresAt:        time.Now().Add(time.Hour),
		})
		require.NoError(t, err)

		revoked, err := repo.RevokeSessionsForUser(ctx, other.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), revoked)

		// Running it again finds nothing live.
		revoked, err = repo.RevokeSessionsForUser(ctx, other.ID)
		require.NoError(t, err)
		require.Zero(t, revoked)
	})
}

func TestAuthRepositoryOneTimeTokens(t *testing.T) {
	ctx := t.Context()
	repo := newAuthRepo(t, appDatabase(t))

	user, err := repo.CreateUser(ctx, "tokens@example.com", "$argon2id$hash")
	require.NoError(t, err)

	t.Run("email verification is single use", func(t *testing.T) {
		require.NoError(t, repo.CreateEmailVerificationToken(ctx, user.ID, "verify-hash", time.Now().Add(time.Hour)))

		userID, err := repo.ConsumeEmailVerificationToken(ctx, "verify-hash")
		require.NoError(t, err)
		require.Equal(t, user.ID, userID)

		// The second attempt finds no live token.
		_, err = repo.ConsumeEmailVerificationToken(ctx, "verify-hash")
		require.ErrorIs(t, err, domain.ErrUserNotFound)
	})

	t.Run("an expired verification token is refused", func(t *testing.T) {
		require.NoError(t, repo.CreateEmailVerificationToken(ctx, user.ID, "expired-hash", time.Now().Add(-time.Minute)))

		_, err := repo.ConsumeEmailVerificationToken(ctx, "expired-hash")
		require.ErrorIs(t, err, domain.ErrUserNotFound)
	})

	t.Run("password reset is single use", func(t *testing.T) {
		require.NoError(t, repo.CreatePasswordResetToken(ctx, user.ID, "reset-hash", time.Now().Add(time.Hour)))

		userID, err := repo.ConsumePasswordResetToken(ctx, "reset-hash")
		require.NoError(t, err)
		require.Equal(t, user.ID, userID)

		_, err = repo.ConsumePasswordResetToken(ctx, "reset-hash")
		require.ErrorIs(t, err, domain.ErrUserNotFound)
	})

	t.Run("an expired reset token is refused", func(t *testing.T) {
		require.NoError(t, repo.CreatePasswordResetToken(ctx, user.ID, "stale-hash", time.Now().Add(-time.Second)))

		_, err := repo.ConsumePasswordResetToken(ctx, "stale-hash")
		require.ErrorIs(t, err, domain.ErrUserNotFound)
	})
}

// TestAuthRepositoryWithoutAPool keeps the "no database" state an error instead
// of a panic.
func TestAuthRepositoryWithoutAPool(t *testing.T) {
	ctx := context.Background()
	repo := authrepo.New(nil)

	_, err := repo.CreateUser(ctx, "a@example.com", "hash")
	require.ErrorContains(t, err, "not configured")

	_, err = repo.UserByEmail(ctx, "a@example.com")
	require.ErrorContains(t, err, "not configured")

	_, err = repo.CreateSession(ctx, domain.NewSession{})
	require.ErrorContains(t, err, "not configured")

	_, err = repo.RevokeSessionsForUser(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")

	require.ErrorContains(t, repo.CreateEmailVerificationToken(ctx, uuid.New(), "h", time.Now()), "not configured")
	require.ErrorContains(t, repo.CreatePasswordResetToken(ctx, uuid.New(), "h", time.Now()), "not configured")
	_, err = repo.ConsumePasswordResetToken(ctx, "h")
	require.ErrorContains(t, err, "not configured")
	_, err = repo.ConsumeEmailVerificationToken(ctx, "h")
	require.ErrorContains(t, err, "not configured")
	_, err = repo.SessionByID(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")
	_, err = repo.SessionByTokenHash(ctx, "h")
	require.ErrorContains(t, err, "not configured")
	require.ErrorContains(t, repo.RevokeSession(ctx, uuid.New()), "not configured")
	require.ErrorContains(t, repo.RevokeSessionByTokenHash(ctx, "h"), "not configured")
	_, err = repo.UserByID(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")
	_, err = repo.UserByGoogleSubject(ctx, "s")
	require.ErrorContains(t, err, "not configured")
	_, err = repo.CreateGoogleUser(ctx, "a@example.com", "s")
	require.ErrorContains(t, err, "not configured")
	_, err = repo.LinkGoogleSubject(ctx, uuid.New(), "s")
	require.ErrorContains(t, err, "not configured")
	_, err = repo.MarkEmailVerified(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")
	_, err = repo.UpdatePasswordHash(ctx, uuid.New(), "h")
	require.ErrorContains(t, err, "not configured")
	_, err = repo.MarkOnboarded(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")
	_, err = repo.PasswordHash(ctx, uuid.New())
	require.ErrorContains(t, err, "not configured")
}
