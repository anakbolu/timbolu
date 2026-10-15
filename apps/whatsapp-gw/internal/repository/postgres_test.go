package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/packages/platform/database"
	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// gatewayPool opens the module's pool against a test database.
func gatewayPool(t *testing.T, dsn string) *database.Pool {
	t.Helper()

	pool, err := database.New(t.Context(), database.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

// seed writes the rows the tests read, through a raw pool rather than through the
// repository, so a test's arrange step cannot pass because the code under test is
// wrong.
type seed struct {
	pool *pgxpool.Pool
}

func newSeed(t *testing.T, dsn string) *seed {
	t.Helper()

	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return &seed{pool: pool}
}

func (s *seed) workspace(t *testing.T) uuid.UUID {
	t.Helper()

	id := uuid.New()
	_, err := s.pool.Exec(t.Context(), `INSERT INTO workspaces (id) VALUES ($1)`, id)
	require.NoError(t, err)

	return id
}

func (s *seed) number(t *testing.T, workspaceID uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	_, err := s.pool.Exec(t.Context(),
		`INSERT INTO whatsapp_numbers (id, workspace_id) VALUES ($1, $2)`, id, workspaceID)
	require.NoError(t, err)

	return id
}

func (s *seed) directory(t *testing.T, numberID, workspaceID uuid.UUID, enabled bool, createdAt time.Time) {
	t.Helper()

	_, err := s.pool.Exec(t.Context(),
		`INSERT INTO whatsapp_directory (number_id, workspace_id, enabled, created_at) VALUES ($1, $2, $3, $4)`,
		numberID, workspaceID, enabled, createdAt)
	require.NoError(t, err)
}

// TestServingListsEnabledNumbersInCreationOrder proves the fleet's read is the
// routing pair, that a parked number is left out, and that the order is the
// directory's own — a fleet that started numbers in an arbitrary order would
// reconnect them in one too.
func TestServingListsEnabledNumbersInCreationOrder(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	repo := New(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)
	first := s.number(t, workspaceID)
	parked := s.number(t, workspaceID)
	second := s.number(t, workspaceID)

	base := time.Now().UTC().Add(-time.Hour)
	s.directory(t, second, workspaceID, true, base.Add(2*time.Minute))
	s.directory(t, first, workspaceID, true, base)
	s.directory(t, parked, workspaceID, false, base.Add(time.Minute))

	assignments, err := repo.Serving(ctx)
	require.NoError(t, err)

	require.Equal(t, []whatsappdomain.Assignment{
		{NumberID: first, WorkspaceID: workspaceID},
		{NumberID: second, WorkspaceID: workspaceID},
	}, assignments)
}

// TestSaveThenLoadRoundTripsASession is the pair the engine leans on: a device
// blob written at link time is the blob read back at every reconnect.
func TestSaveThenLoadRoundTripsASession(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	repo := New(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)
	numberID := s.number(t, workspaceID)

	scope := whatsappdomain.Scope{WorkspaceID: workspaceID}
	sealed := []byte{1, 2, 3, 4}

	require.NoError(t, repo.Save(ctx, scope, whatsappdomain.Session{
		NumberID:    numberID,
		WorkspaceID: workspaceID,
		Sealed:      sealed,
		JID:         "628123@server",
	}))

	session, err := repo.Load(ctx, scope, numberID)
	require.NoError(t, err)

	require.Equal(t, numberID, session.NumberID)
	require.Equal(t, workspaceID, session.WorkspaceID)
	require.Equal(t, sealed, session.Sealed)
	require.Equal(t, "628123@server", session.JID)
	require.False(t, session.UpdatedAt.IsZero())
}

// TestSaveReplacesAnExistingSession covers the upsert: relinking a number
// replaces the device blob rather than adding a second one.
func TestSaveReplacesAnExistingSession(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	repo := New(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)
	numberID := s.number(t, workspaceID)
	scope := whatsappdomain.Scope{WorkspaceID: workspaceID}

	require.NoError(t, repo.Save(ctx, scope, whatsappdomain.Session{
		NumberID: numberID, WorkspaceID: workspaceID, Sealed: []byte{1}, JID: "old@server",
	}))
	require.NoError(t, repo.Save(ctx, scope, whatsappdomain.Session{
		NumberID: numberID, WorkspaceID: workspaceID, Sealed: []byte{2}, JID: "new@server",
	}))

	session, err := repo.Load(ctx, scope, numberID)
	require.NoError(t, err)
	require.Equal(t, []byte{2}, session.Sealed)
	require.Equal(t, "new@server", session.JID)
}

// TestLoadReportsAMissingSession proves "not found" stays the module's sentinel
// rather than the driver's, because the engine switches on it.
func TestLoadReportsAMissingSession(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	repo := New(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)
	numberID := s.number(t, workspaceID)

	_, err := repo.Load(ctx, whatsappdomain.Scope{WorkspaceID: workspaceID}, numberID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
}

// TestLoadDoesNotCrossWorkspaces proves the workspace predicate is applied: a
// session stored under one tenant is not readable under another.
func TestLoadDoesNotCrossWorkspaces(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	repo := New(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	owner := s.workspace(t)
	other := s.workspace(t)
	numberID := s.number(t, owner)

	require.NoError(t, repo.Save(ctx, whatsappdomain.Scope{WorkspaceID: owner}, whatsappdomain.Session{
		NumberID: numberID, WorkspaceID: owner, Sealed: []byte{9},
	}))

	_, err := repo.Load(ctx, whatsappdomain.Scope{WorkspaceID: other}, numberID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
}

// TestForgetSessionRemovesTheDevice is the unlink path: the blob is gone, and a
// second forget is a no-op rather than an error.
func TestForgetSessionRemovesTheDevice(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	repo := New(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)
	numberID := s.number(t, workspaceID)
	scope := whatsappdomain.Scope{WorkspaceID: workspaceID}

	require.NoError(t, repo.Save(ctx, scope, whatsappdomain.Session{
		NumberID: numberID, WorkspaceID: workspaceID, Sealed: []byte{1},
	}))
	require.NoError(t, repo.ForgetSession(ctx, scope, numberID))

	_, err := repo.Load(ctx, scope, numberID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	require.NoError(t, repo.ForgetSession(ctx, scope, numberID))
}

// TestKeyReturnsTheStoredWrap is the read the sealer makes before it opens a blob.
func TestKeyReturnsTheStoredWrap(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	store := NewKeyStore(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)
	wrapped := []byte("wrapped-data-key")
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_keys (workspace_id, wrapped_data_key, kms_key_id) VALUES ($1, $2, $3)`,
		workspaceID, wrapped, "kms-1")
	require.NoError(t, err)

	key, err := store.Key(ctx, workspaceID)
	require.NoError(t, err)
	require.Equal(t, WrappedKey{KeyID: "kms-1", Bytes: wrapped}, key)
}

// TestKeyReportsAMissingKey keeps the first-use state a sentinel: the sealer
// mints a key on that answer, so a driver error here would be a link that fails
// rather than a key that is created.
func TestKeyReportsAMissingKey(t *testing.T) {
	dsn := postgresDSN(t)
	store := NewKeyStore(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	_, err := store.Key(t.Context(), s.workspace(t))
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
}

// TestSaveKeyKeepsTheFirstWrap proves the insert does nothing on conflict: a wrap
// minted by the hub in the same moment must not be replaced, because replacing it
// would make every credential sealed under it unreadable.
func TestSaveKeyKeepsTheFirstWrap(t *testing.T) {
	ctx := t.Context()
	dsn := postgresDSN(t)
	store := NewKeyStore(gatewayPool(t, dsn))
	s := newSeed(t, dsn)

	workspaceID := s.workspace(t)

	require.NoError(t, store.SaveKey(ctx, workspaceID, WrappedKey{KeyID: "hub", Bytes: []byte("first")}))
	require.NoError(t, store.SaveKey(ctx, workspaceID, WrappedKey{KeyID: "gateway", Bytes: []byte("second")}))

	key, err := store.Key(ctx, workspaceID)
	require.NoError(t, err)
	require.Equal(t, WrappedKey{KeyID: "hub", Bytes: []byte("first")}, key)
}

// TestMethodsReportAMissingPool keeps the unconfigured state an error rather than
// a panic: a gateway with no database must fail a call, not the process.
func TestMethodsReportAMissingPool(t *testing.T) {
	ctx := context.Background()
	scope := whatsappdomain.Scope{WorkspaceID: uuid.New()}
	numberID := uuid.New()

	repo := New(nil)
	_, err := repo.Load(ctx, scope, numberID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)
	require.ErrorIs(t, repo.Save(ctx, scope, whatsappdomain.Session{}), whatsappdomain.ErrNotConfigured)
	require.ErrorIs(t, repo.ForgetSession(ctx, scope, numberID), whatsappdomain.ErrNotConfigured)

	_, err = repo.Serving(ctx)
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)

	store := NewKeyStore(nil)
	_, err = store.Key(ctx, scope.WorkspaceID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)
	require.ErrorIs(t, store.SaveKey(ctx, scope.WorkspaceID, WrappedKey{}), whatsappdomain.ErrNotConfigured)
}

// TestTranslateKeepsAnUnrelatedErrorWrapped proves only the driver's "no rows"
// becomes the module's sentinel; anything else is reported as itself, so a broken
// connection is not mistaken for a missing row.
func TestTranslateKeepsAnUnrelatedErrorWrapped(t *testing.T) {
	require.ErrorIs(t, translate(pgx.ErrNoRows, "load session"), whatsappdomain.ErrNotFound)

	boom := errors.New("boom")
	err := translate(boom, "load session")
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, whatsappdomain.ErrNotFound)
}
