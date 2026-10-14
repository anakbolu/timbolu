package sealer

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/whatsapp-gw/internal/repository"
	"github.com/robzlabz/employeebot/packages/platform/kms"
	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// The sealer is the gateway's copy of the hub's key ring, and the claim under
// test is the one that makes moving the session a move rather than a re-link: a
// blob sealed under one workspace's data key opens under it and under nothing
// else, and the first writer of a data key is the one that keeps it.

// testKey is 32 bytes, so it is a valid key for the box.
const testKey = "0123456789abcdef0123456789abcdef"

// fakeKeys is a workspace key store in memory. It can be told to lose a key, so
// the mint-on-first-use path is reachable, and to answer with a different wrap
// after a save, so the race path is reachable too.
type fakeKeys struct {
	stored    map[uuid.UUID]repository.WrappedKey
	missing   bool
	raceWith  *repository.WrappedKey
	savedWith []repository.WrappedKey
}

func newFakeKeys() *fakeKeys {
	return &fakeKeys{stored: map[uuid.UUID]repository.WrappedKey{}}
}

func (f *fakeKeys) Key(_ context.Context, workspaceID uuid.UUID) (repository.WrappedKey, error) {
	if f.missing {
		return repository.WrappedKey{}, whatsappdomain.ErrNotFound
	}
	key, ok := f.stored[workspaceID]
	if !ok {
		return repository.WrappedKey{}, whatsappdomain.ErrNotFound
	}
	return key, nil
}

func (f *fakeKeys) SaveKey(_ context.Context, workspaceID uuid.UUID, key repository.WrappedKey) error {
	f.savedWith = append(f.savedWith, key)
	if f.raceWith != nil {
		// Another writer won the race: what is stored is not what this call
		// wrapped.
		f.stored[workspaceID] = *f.raceWith
		return nil
	}
	f.stored[workspaceID] = key
	return nil
}

func newSealer(t *testing.T, keys *fakeKeys) *Sealer {
	t.Helper()

	ring, err := kms.NewLocal("k1", testKey, nil)
	require.NoError(t, err)
	return New(ring, keys)
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	keys := newFakeKeys()
	sealer := newSealer(t, keys)
	workspaceID := uuid.New()

	sealed, err := sealer.Seal(context.Background(), workspaceID, []byte("device state"))
	require.NoError(t, err)
	require.NotEqual(t, []byte("device state"), sealed)

	opened, err := sealer.Open(context.Background(), workspaceID, sealed)
	require.NoError(t, err)
	require.Equal(t, []byte("device state"), opened)

	// The first seal minted the workspace's data key, which is the key a
	// workspace whose first act is linking a number would not otherwise have.
	require.Len(t, keys.savedWith, 1)
	require.Equal(t, "k1", keys.savedWith[0].KeyID)
}

func TestABlobDoesNotOpenUnderAnotherWorkspace(t *testing.T) {
	keys := newFakeKeys()
	sealer := newSealer(t, keys)

	sealed, err := sealer.Seal(context.Background(), uuid.New(), []byte("device state"))
	require.NoError(t, err)

	_, err = sealer.Open(context.Background(), uuid.New(), sealed)
	require.Error(t, err)
}

func TestASecondSealReusesTheMintedKey(t *testing.T) {
	keys := newFakeKeys()
	sealer := newSealer(t, keys)
	workspaceID := uuid.New()

	first, err := sealer.Seal(context.Background(), workspaceID, []byte("one"))
	require.NoError(t, err)
	_, err = sealer.Seal(context.Background(), workspaceID, []byte("two"))
	require.NoError(t, err)

	// Only the first seal minted: the key already in place is the key every
	// credential sealed under it needs.
	require.Len(t, keys.savedWith, 1)

	opened, err := sealer.Open(context.Background(), workspaceID, first)
	require.NoError(t, err)
	require.Equal(t, []byte("one"), opened)
}

func TestMintKeepsTheWrapAnotherWriterStored(t *testing.T) {
	// Two processes can mint at the same moment; the insert does nothing on
	// conflict, so the sealer must read back what is stored rather than trust what
	// it wrapped — otherwise one of them could not read its own blobs.
	other, err := kms.NewLocal("k1", testKey, nil)
	require.NoError(t, err)

	workspaceID := uuid.New()
	winnerDataKey, err := kms.NewDataKey()
	require.NoError(t, err)
	wrapped, err := other.Wrap(context.Background(), workspaceID, winnerDataKey)
	require.NoError(t, err)

	keys := newFakeKeys()
	keys.raceWith = &repository.WrappedKey{KeyID: wrapped.KeyID, Bytes: wrapped.Bytes}
	sealer := newSealer(t, keys)

	sealed, err := sealer.Seal(context.Background(), workspaceID, []byte("device state"))
	require.NoError(t, err)

	// The blob was sealed with the winner's key, so it opens with the ring the
	// store now reports — which is the whole point of re-reading.
	opened, err := sealer.Open(context.Background(), workspaceID, sealed)
	require.NoError(t, err)
	require.Equal(t, []byte("device state"), opened)
}

func TestAnUnreadableKeyStoreIsReported(t *testing.T) {
	ring, err := kms.NewLocal("k1", testKey, nil)
	require.NoError(t, err)

	sealer := New(ring, &failingKeys{err: errors.New("postgres is down")})
	_, err = sealer.Seal(context.Background(), uuid.New(), []byte("x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "postgres is down")
}

type failingKeys struct{ err error }

func (f *failingKeys) Key(context.Context, uuid.UUID) (repository.WrappedKey, error) {
	return repository.WrappedKey{}, f.err
}

func (f *failingKeys) SaveKey(context.Context, uuid.UUID, repository.WrappedKey) error { return f.err }
