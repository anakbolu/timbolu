// Package kms wraps the key that protects every workspace's data key.
//
// This is the outer half of the envelope. The inner half is the data key, which
// encrypts the tokens themselves; this package never sees a token. Keeping the
// two apart is what makes a rotation cheap: replacing the key here re-wraps the
// data keys, and the ciphertext of every stored token is left alone.
//
// A deployment supplies the key in the environment (see KmsConfig). A cloud
// key-management service would be another implementation of Manager, which is
// why the interface takes a workspace id and returns the id of the key it used.
package kms

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/crypto"
)

// DataKeySize is the length of a workspace data key: the same AES-256 key the
// secret box takes.
const DataKeySize = crypto.KeySize

var (
	// ErrNoKey means no key-management key is configured, so no token can be
	// sealed or opened.
	ErrNoKey = errors.New("kms: no key-management key configured")
	// ErrUnknownKey means a wrap names a key this deployment no longer holds.
	// The message says which, because that is the whole diagnosis: the operator
	// rotated and dropped the old key from the configuration.
	ErrUnknownKey = errors.New("kms: the key that wrapped this value is not configured")
)

// Wrapped is a data key sealed by a key-management key, together with the id of
// the key that sealed it. The id travels with the value so a rotation can tell
// an old wrap from a new one without opening either.
type Wrapped struct {
	KeyID string
	Bytes []byte
}

// Manager seals and opens data keys.
type Manager interface {
	// Wrap seals a data key under the active key.
	Wrap(ctx context.Context, workspaceID uuid.UUID, dataKey []byte) (Wrapped, error)
	// Unwrap opens a data key sealed by the named key, which may be a retired
	// one. That is what keeps an existing connection working across a rotation.
	Unwrap(ctx context.Context, workspaceID uuid.UUID, wrapped Wrapped) ([]byte, error)
	// ActiveKeyID is the id a new wrap records.
	ActiveKeyID() string
}

// Local wraps data keys with keys held in the process environment.
//
// The workspace id is mixed into the associated data of every wrap, so a value
// sealed for one workspace cannot be opened as another's even if the stored rows
// are swapped. It costs nothing and removes a whole class of mistake.
type Local struct {
	active crypto.Box
	// retired holds the keys a previous rotation used. They open, never seal.
	retired  map[string]*crypto.Box
	activeID string
}

// NewLocal builds a manager from the active key and any retired ones.
//
// Retired keys are given as id-to-encoded-key pairs. The order does not matter;
// the ids are what a stored wrap refers to.
func NewLocal(activeID string, activeKey string, retired map[string]string) (*Local, error) {
	if strings.TrimSpace(activeKey) == "" {
		return nil, ErrNoKey
	}

	box, err := crypto.NewFromString(activeKey)
	if err != nil {
		return nil, fmt.Errorf("kms: active key: %w", err)
	}

	keys := make(map[string]*crypto.Box, len(retired))
	for id, encoded := range retired {
		trimmedID := strings.TrimSpace(id)
		if trimmedID == "" {
			return nil, fmt.Errorf("kms: a retired key needs an id")
		}
		older, err := crypto.NewFromString(encoded)
		if err != nil {
			return nil, fmt.Errorf("kms: retired key %q: %w", trimmedID, err)
		}
		keys[trimmedID] = older
	}

	if strings.TrimSpace(activeID) == "" {
		return nil, fmt.Errorf("kms: the active key needs an id")
	}

	return &Local{active: *box, retired: keys, activeID: strings.TrimSpace(activeID)}, nil
}

// NewLocalFromConfig builds a manager from the configured key and retired list.
// A missing key is not an error: a deployment without one runs with integrations
// disabled, and the screen says so rather than storing a token in clear text.
func NewLocalFromConfig(masterKey, masterKeyID, retired string) (*Local, error) {
	if strings.TrimSpace(masterKey) == "" {
		return nil, nil
	}

	older, err := parseRetired(retired)
	if err != nil {
		return nil, err
	}

	id := strings.TrimSpace(masterKeyID)
	if id == "" {
		// A single-key deployment needs no ceremony: the id only has to be
		// stable, and the key's fingerprint is stable without being secret.
		box, err := crypto.NewFromString(masterKey)
		if err != nil {
			return nil, fmt.Errorf("kms: master key: %w", err)
		}
		id = "local-" + box.Fingerprint()
	}

	return NewLocal(id, masterKey, older)
}

// parseRetired reads the `id:key` pairs. A key that is itself base64 can contain
// no colon, so splitting on the first one is unambiguous.
func parseRetired(encoded string) (map[string]string, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return nil, nil
	}

	keys := map[string]string{}
	for _, pair := range strings.Split(trimmed, ",") {
		entry := strings.TrimSpace(pair)
		if entry == "" {
			continue
		}

		id, key, found := strings.Cut(entry, ":")
		if !found {
			return nil, fmt.Errorf("kms: retired key %q must be `id:key`", entry)
		}
		if strings.TrimSpace(id) == "" || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("kms: retired key %q must be `id:key`", entry)
		}
		keys[strings.TrimSpace(id)] = strings.TrimSpace(key)
	}

	if len(keys) == 0 {
		return nil, nil
	}
	return keys, nil
}

// Wrap seals a data key under the active key.
func (l *Local) Wrap(_ context.Context, workspaceID uuid.UUID, dataKey []byte) (Wrapped, error) {
	if l == nil {
		return Wrapped{}, ErrNoKey
	}
	if len(dataKey) != DataKeySize {
		return Wrapped{}, fmt.Errorf("kms: data key must be %d bytes (got %d)", DataKeySize, len(dataKey))
	}

	sealed, err := l.active.EncryptWith(string(dataKey), associated(workspaceID))
	if err != nil {
		return Wrapped{}, fmt.Errorf("kms: wrap data key: %w", err)
	}

	return Wrapped{KeyID: l.activeID, Bytes: sealed}, nil
}

// Unwrap opens a data key. A wrap made by a retired key opens too, which is what
// makes a rotation non-breaking.
func (l *Local) Unwrap(_ context.Context, workspaceID uuid.UUID, wrapped Wrapped) ([]byte, error) {
	if l == nil {
		return nil, ErrNoKey
	}

	box, ok := l.retired[wrapped.KeyID]
	if wrapped.KeyID == l.activeID {
		box, ok = &l.active, true
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, wrapped.KeyID)
	}

	// The workspace is bound into the wrap, so a stored value moved to another
	// workspace's row does not open as that workspace's key.
	plaintext, err := box.DecryptWith(wrapped.Bytes, associated(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("kms: unwrap data key: %w", err)
	}

	key := []byte(plaintext)
	if len(key) != DataKeySize {
		return nil, fmt.Errorf("kms: unwrap data key: %w", crypto.ErrInvalidCiphertext)
	}
	return key, nil
}

// ActiveKeyID is the id a new wrap records.
func (l *Local) ActiveKeyID() string {
	if l == nil {
		return ""
	}
	return l.activeID
}

// RetiredIDs lists the keys that only open, for diagnostics. It is sorted so a
// log line is stable across runs.
func (l *Local) RetiredIDs() []string {
	if l == nil {
		return nil
	}

	ids := make([]string, 0, len(l.retired))
	for id := range l.retired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// NewDataKey returns a fresh random data key.
func NewDataKey() ([]byte, error) {
	key := make([]byte, DataKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("kms: read data key: %w", err)
	}
	return key, nil
}

// associated is the data every wrap is bound to. Binding the workspace means a
// stored wrap cannot be moved to another workspace's row and opened there.
func associated(workspaceID uuid.UUID) []byte {
	return []byte("workspace-keys:" + workspaceID.String())
}
