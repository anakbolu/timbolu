// Package repository is the gateway's own data access: the sealed device state it
// reads and writes, the fleet directory it follows, and the workspace key that
// seals a blob.
//
// It is deliberately narrower than the backend's WhatsApp repository. The gateway
// owns the socket, so it owns the session — and it must not be able to reach the
// numbers, the messages, or the outbox, which are the product's. The two
// repositories share the schema and nothing else.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/anakbolu/timbolu/apps/whatsapp-gw/internal/repository/sqlcgen"
	"github.com/anakbolu/timbolu/packages/platform/database"
	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// Repository reads and writes what the gateway owns.
type Repository struct {
	pool *database.Pool
}

// New builds the repository. A nil pool leaves every method reporting that the
// database is not configured rather than panicking.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Serving returns the numbers the fleet is expected to serve.
//
// It is the one read outside a tenant scope, and the migration says why: a gateway
// has to discover which tenants have a number before it can open any workspace's
// scope at all. What it returns is a routing pair, not data.
func (r *Repository) Serving(ctx context.Context) ([]whatsappdomain.Assignment, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", whatsappdomain.ErrNotConfigured)
	}

	rows, err := sqlcgen.New(r.pool.PgxPool()).ServingWhatsAppNumbers(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: serving numbers: %w", err)
	}

	assignments := make([]whatsappdomain.Assignment, 0, len(rows))
	for _, row := range rows {
		assignments = append(assignments, whatsappdomain.Assignment{NumberID: row.NumberID, WorkspaceID: row.WorkspaceID})
	}

	return assignments, nil
}

// Load returns the sealed device state of one number.
func (r *Repository) Load(ctx context.Context, scope whatsappdomain.Scope, numberID uuid.UUID) (whatsappdomain.Session, error) {
	var session whatsappdomain.Session

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppSession(ctx, sqlcgen.GetWhatsAppSessionParams{
			NumberID:    numberID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return translate(err, "load session")
		}

		session = whatsappdomain.Session{
			NumberID:    row.NumberID,
			WorkspaceID: row.WorkspaceID,
			Sealed:      row.SealedDevice,
			JID:         row.Jid,
			UpdatedAt:   row.UpdatedAt,
		}
		return nil
	})
	if err != nil {
		return whatsappdomain.Session{}, err
	}

	return session, nil
}

// Save stores the sealed device state.
func (r *Repository) Save(ctx context.Context, scope whatsappdomain.Scope, session whatsappdomain.Session) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.UpsertWhatsAppSession(ctx, sqlcgen.UpsertWhatsAppSessionParams{
			NumberID:     session.NumberID,
			WorkspaceID:  session.WorkspaceID,
			SealedDevice: session.Sealed,
			Jid:          session.JID,
		}); err != nil {
			return translate(err, "save session")
		}
		return nil
	})
}

// ForgetSession forgets a device.
func (r *Repository) ForgetSession(ctx context.Context, scope whatsappdomain.Scope, numberID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.DeleteWhatsAppSession(ctx, sqlcgen.DeleteWhatsAppSessionParams{
			NumberID:    numberID,
			WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			return translate(err, "delete session")
		}
		return nil
	})
}

// KeyStore reads and creates the wrapped data key of one workspace.
//
// It is the key ring's storage, not the key ring: the wrap itself is the
// key-management key's business, and this only holds what it produced.
type KeyStore struct {
	pool *database.Pool
}

// NewKeyStore builds the key store.
func NewKeyStore(pool *database.Pool) *KeyStore { return &KeyStore{pool: pool} }

// WrappedKey is a workspace's data key as it is stored.
type WrappedKey struct {
	KeyID string
	Bytes []byte
}

// Key returns the workspace's wrapped data key, or ErrNotFound when it has none.
func (k *KeyStore) Key(ctx context.Context, workspaceID uuid.UUID) (WrappedKey, error) {
	var out WrappedKey

	err := k.read(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWorkspaceKey(ctx, workspaceID)
		if err != nil {
			return translate(err, "get workspace key")
		}

		out = WrappedKey{KeyID: row.KmsKeyID, Bytes: row.WrappedDataKey}
		return nil
	})
	if err != nil {
		return WrappedKey{}, err
	}

	return out, nil
}

// SaveKey records the workspace's wrapped data key, keeping one that already
// exists.
//
// Keeping it is the point: a key minted here and a key minted by the hub are the
// same key only if the first writer wins, and replacing a wrap would make every
// credential sealed under it unreadable. So this is an insert that does nothing on
// conflict, and the caller re-reads to learn which wrap it now holds.
func (k *KeyStore) SaveKey(ctx context.Context, workspaceID uuid.UUID, key WrappedKey) error {
	return k.write(ctx, workspaceID, func(queries *sqlcgen.Queries) error {
		if _, err := queries.MintWorkspaceKey(ctx, sqlcgen.MintWorkspaceKeyParams{
			WorkspaceID:    workspaceID,
			WrappedDataKey: key.Bytes,
			KmsKeyID:       key.KeyID,
		}); err != nil {
			return translate(err, "mint workspace key")
		}
		return nil
	})
}

// read runs fn in a read-only transaction scoped to the workspace.
func (r *Repository) read(ctx context.Context, scope whatsappdomain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", whatsappdomain.ErrNotConfigured)
	}

	return r.pool.WithWorkspaceRead(ctx, scope.WorkspaceID, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// write runs fn in a transaction scoped to the workspace.
func (r *Repository) write(ctx context.Context, scope whatsappdomain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", whatsappdomain.ErrNotConfigured)
	}

	return r.pool.WithWorkspace(ctx, scope.WorkspaceID, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// read runs fn in a read-only transaction scoped to the workspace.
func (k *KeyStore) read(ctx context.Context, workspaceID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if k == nil || k.pool == nil {
		return fmt.Errorf("%w: database is not configured", whatsappdomain.ErrNotConfigured)
	}

	return k.pool.WithWorkspaceRead(ctx, workspaceID, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// write runs fn in a transaction scoped to the workspace.
func (k *KeyStore) write(ctx context.Context, workspaceID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if k == nil || k.pool == nil {
		return fmt.Errorf("%w: database is not configured", whatsappdomain.ErrNotConfigured)
	}

	return k.pool.WithWorkspace(ctx, workspaceID, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// translate turns the driver's own errors into the module's.
func translate(err error, operation string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", whatsappdomain.ErrNotFound, operation)
	}

	return fmt.Errorf("whatsapp: %s: %w", operation, err)
}
