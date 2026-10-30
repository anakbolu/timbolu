// Package sealer seals and opens a device blob with the workspace's own data key.
//
// It is the gateway's copy of the key ring the hub already keeps: the same
// key-management key, the same per-workspace data key, and the same associated
// data — so a session sealed by the API before this process existed still opens
// here, which is what makes moving the session a move rather than a re-link.
package sealer

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/repository"
	"github.com/anakbolu/timbolu/packages/platform/crypto"
	"github.com/anakbolu/timbolu/packages/platform/kms"
	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// Keys is where a workspace's wrapped data key is stored.
type Keys interface {
	Key(ctx context.Context, workspaceID uuid.UUID) (repository.WrappedKey, error)
	SaveKey(ctx context.Context, workspaceID uuid.UUID, key repository.WrappedKey) error
}

// Sealer implements whatsappdomain.Sealer over the deployment's key ring.
type Sealer struct {
	keys  *kms.Local
	store Keys
}

// New builds the sealer.
func New(keys *kms.Local, store Keys) *Sealer {
	return &Sealer{keys: keys, store: store}
}

// Compile-time check that the sealer is the port the engine declares.
var _ whatsappdomain.Sealer = (*Sealer)(nil)

// Seal encrypts a device blob.
func (s *Sealer) Seal(ctx context.Context, workspaceID uuid.UUID, plaintext []byte) ([]byte, error) {
	box, err := s.box(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	// Go strings carry arbitrary bytes, so the blob survives the cast exactly.
	sealed, err := box.EncryptWith(string(plaintext), associated(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("whatsapp: seal session: %w", err)
	}

	return sealed, nil
}

// Open decrypts a device blob.
func (s *Sealer) Open(ctx context.Context, workspaceID uuid.UUID, ciphertext []byte) ([]byte, error) {
	box, err := s.box(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	opened, err := box.DecryptWith(ciphertext, associated(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open session: %w", err)
	}

	return []byte(opened), nil
}

// box returns the secret box for a workspace's data key.
func (s *Sealer) box(ctx context.Context, workspaceID uuid.UUID) (*crypto.Box, error) {
	dataKey, err := s.dataKey(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	return crypto.New(dataKey)
}

// dataKey resolves the workspace's data key, creating it the first time the
// gateway seals something.
//
// A workspace whose first act is linking a number has no data key yet — the hub
// mints one when a person connects a mailbox, and this is the same key for the
// same workspace — so the mint happens here too. What this never does is replace
// an existing wrap: a key already in place is the key every credential sealed
// under it needs, and convergence after a rotation stays the hub's job, where the
// tokens that must be re-sealed are read.
func (s *Sealer) dataKey(ctx context.Context, workspaceID uuid.UUID) ([]byte, error) {
	stored, err := s.store.Key(ctx, workspaceID)
	if errors.Is(err, whatsappdomain.ErrNotFound) {
		return s.mint(ctx, workspaceID)
	}
	if err != nil {
		return nil, err
	}

	return s.keys.Unwrap(ctx, workspaceID, kms.Wrapped{KeyID: stored.KeyID, Bytes: stored.Bytes})
}

// mint creates a workspace's data key and stores the wrapped form.
func (s *Sealer) mint(ctx context.Context, workspaceID uuid.UUID) ([]byte, error) {
	dataKey, err := kms.NewDataKey()
	if err != nil {
		return nil, err
	}

	wrapped, err := s.keys.Wrap(ctx, workspaceID, dataKey)
	if err != nil {
		return nil, err
	}

	if err := s.store.SaveKey(ctx, workspaceID, repository.WrappedKey{
		KeyID: wrapped.KeyID,
		Bytes: wrapped.Bytes,
	}); err != nil {
		return nil, err
	}

	// The insert does nothing when another writer won the race, so what is stored
	// now is the authority rather than what this call wrapped: a data key that two
	// processes disagreed about would leave one of them unable to read its own
	// blobs.
	stored, err := s.store.Key(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if stored.KeyID == wrapped.KeyID && bytes.Equal(stored.Bytes, wrapped.Bytes) {
		return dataKey, nil
	}

	return s.keys.Unwrap(ctx, workspaceID, kms.Wrapped{KeyID: stored.KeyID, Bytes: stored.Bytes})
}

// associated is the data a seal is bound to.
//
// It is the same string the hub binds a token seal to, and it has to be: sessions
// sealed by the API before this process owned them carry it, and a different
// prefix would turn every linked number into one that needs relinking. The
// workspace is bound in so a blob cannot be moved to another tenant's row and
// opened there.
func associated(workspaceID uuid.UUID) []byte {
	return []byte("integration-tokens:" + workspaceID.String())
}
