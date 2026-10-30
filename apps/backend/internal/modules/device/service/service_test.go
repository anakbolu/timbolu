package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/service"
)

// This file is the module's rules rather than Postgres's: a repeated token updates one row, a body
// that is not an address is refused, and another account's token is not this account's to delete.
// The repository is a stub precisely because none of that is storage's job — the upsert's one-row
// promise is the service's contract even though the SQL enforces it.

// stubRepository keeps the registry in memory with the same key the unique index uses.
type stubRepository struct {
	mu    sync.Mutex
	rows  map[string]domain.Device
	clock time.Time
}

func newStubRepository() *stubRepository {
	return &stubRepository{
		rows:  map[string]domain.Device{},
		clock: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
	}
}

func (s *stubRepository) Register(_ context.Context, scope domain.Scope, request domain.RegisterRequest) (domain.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clock = s.clock.Add(time.Minute)
	key := scope.UserID.String() + "|" + request.Token

	if existing, ok := s.rows[key]; ok {
		existing.Platform = request.Platform
		existing.AppVersion = request.AppVersion
		existing.LastSeenAt = s.clock
		s.rows[key] = existing

		return existing, nil
	}

	device := domain.Device{
		ID:          uuid.New(),
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
		Platform:    request.Platform,
		Token:       request.Token,
		AppVersion:  request.AppVersion,
		LastSeenAt:  s.clock,
		CreatedAt:   s.clock,
	}
	s.rows[key] = device

	return device, nil
}

func (s *stubRepository) Delete(_ context.Context, scope domain.Scope, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := scope.UserID.String() + "|" + token
	if _, ok := s.rows[key]; !ok {
		return domain.ErrNotFound
	}

	delete(s.rows, key)

	return nil
}

func (s *stubRepository) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.rows)
}

func (s *stubRepository) row(scope domain.Scope, token string) (domain.Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	device, ok := s.rows[scope.UserID.String()+"|"+token]

	return device, ok
}

// harness is the service over the in-memory registry.
type harness struct {
	repo  *stubRepository
	svc   *service.Service
	scope domain.Scope
}

func newHarness() *harness {
	repo := newStubRepository()

	return &harness{
		repo:  repo,
		svc:   service.New(service.Deps{Repository: repo}),
		scope: domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// TestRegisteringTheSameTokenTwiceKeepsOneRow: a phone re-registers on every launch, so the second
// registration must move `last_seen_at` on the row that is already there rather than add an address
// the future sender would push to twice.
func TestRegisteringTheSameTokenTwiceKeepsOneRow(t *testing.T) {
	h := newHarness()
	request := domain.RegisterRequest{
		Token:      "ExponentPushToken[abc123]",
		Platform:   domain.PlatformIOS,
		AppVersion: "1.0.0",
	}

	first, err := h.svc.Register(context.Background(), h.scope, request)
	require.NoError(t, err)

	request.AppVersion = "1.1.0"
	request.Platform = domain.PlatformAndroid
	second, err := h.svc.Register(context.Background(), h.scope, request)
	require.NoError(t, err)

	require.Equal(t, 1, h.repo.count())
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.CreatedAt, second.CreatedAt)
	require.True(t, second.LastSeenAt.After(first.LastSeenAt))
	require.Equal(t, "1.1.0", second.AppVersion)
	require.Equal(t, domain.PlatformAndroid, second.Platform)

	stored, ok := h.repo.row(h.scope, request.Token)
	require.True(t, ok)
	require.Equal(t, second.ID, stored.ID)
	require.Equal(t, second.LastSeenAt, stored.LastSeenAt)
}

// TestRegisterRefusesWhatIsNotAnAddress covers the three bodies a client can send that are not a
// registration: a platform the app does not run on, no token, and a token longer than a push address.
func TestRegisterRefusesWhatIsNotAnAddress(t *testing.T) {
	cases := map[string]domain.RegisterRequest{
		"unknown platform": {Token: "ExponentPushToken[abc123]", Platform: "web"},
		"empty token":      {Token: "   ", Platform: domain.PlatformAndroid},
		"token too long":   {Token: strings.Repeat("a", domain.TokenMaxLength+1), Platform: domain.PlatformAndroid},
	}

	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()

			_, err := h.svc.Register(context.Background(), h.scope, request)

			require.ErrorIs(t, err, domain.ErrInvalidInput)
			require.Zero(t, h.repo.count())
		})
	}
}

// TestDeletingAnotherAccountsTokenIsNotFound: the row is invisible rather than forbidden, which is
// what keeps the endpoint from confirming that somebody else registered a token.
func TestDeletingAnotherAccountsTokenIsNotFound(t *testing.T) {
	h := newHarness()
	token := "ExponentPushToken[abc123]"

	_, err := h.svc.Register(context.Background(), h.scope, domain.RegisterRequest{
		Token:    token,
		Platform: domain.PlatformIOS,
	})
	require.NoError(t, err)

	other := domain.Scope{UserID: uuid.New(), WorkspaceID: h.scope.WorkspaceID}

	err = h.svc.Delete(context.Background(), other, token)

	require.ErrorIs(t, err, domain.ErrNotFound)
	require.Equal(t, 1, h.repo.count())

	// The owner can still remove it, which is the other half of "the token is the account's".
	require.NoError(t, h.svc.Delete(context.Background(), h.scope, token))
	require.Zero(t, h.repo.count())
}

// TestAScopeWithoutAWorkspaceIsRefused: the route is tenant-scoped, and a registration carries the
// workspace it came from. Answering without one would be a write nobody asked for.
func TestAScopeWithoutAWorkspaceIsRefused(t *testing.T) {
	h := newHarness()

	_, err := h.svc.Register(context.Background(), domain.Scope{UserID: uuid.New()}, domain.RegisterRequest{
		Token:    "ExponentPushToken[abc123]",
		Platform: domain.PlatformIOS,
	})

	require.True(t, errors.Is(err, domain.ErrInvalidInput))
	require.Zero(t, h.repo.count())
}
