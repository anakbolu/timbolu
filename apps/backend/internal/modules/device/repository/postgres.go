// Package repository implements the device registry with pgx and the sqlc-generated queries.
//
// Every statement runs inside the caller's scope. The policy on `device_tokens` is keyed on the user,
// so a statement that forgot its own user filter still cannot reach another account's address.
package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/repository/sqlcgen"
	"github.com/anakbolu/timbolu/packages/platform/database"
)

// Repository is the Postgres-backed implementation of the device ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Compile-time check that the repository is the interface the module asked for.
var _ domain.Repository = (*Repository)(nil)

// Register stores a token, or updates the row the same account already has for it.
func (r *Repository) Register(ctx context.Context, scope domain.Scope, request domain.RegisterRequest) (domain.Device, error) {
	if r == nil || r.pool == nil {
		return domain.Device{}, fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.UserID == uuid.Nil {
		return domain.Device{}, fmt.Errorf("%w: user is required", domain.ErrInvalidInput)
	}

	var device domain.Device

	err := r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID},
		func(tx pgx.Tx) error {
			row, err := sqlcgen.New(tx).RegisterDeviceToken(ctx, sqlcgen.RegisterDeviceTokenParams{
				UserID:      scope.UserID,
				WorkspaceID: optionalID(scope.WorkspaceID),
				Platform:    request.Platform,
				Token:       request.Token,
				AppVersion:  optionalString(request.AppVersion),
			})
			if err != nil {
				return fmt.Errorf("device: register: %w", err)
			}

			device = toDevice(row)
			return nil
		})
	if err != nil {
		return domain.Device{}, err
	}

	return device, nil
}

// Delete removes one of the caller's tokens.
//
// A statement that matched nothing is reported as ErrNotFound. That is the whole of "another
// account's token is invisible": the policy already hid the row, and this turns the empty result
// into the answer the handler gives.
func (r *Repository) Delete(ctx context.Context, scope domain.Scope, token string) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.UserID == uuid.Nil {
		return fmt.Errorf("%w: user is required", domain.ErrInvalidInput)
	}

	var deleted int64

	err := r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID},
		func(tx pgx.Tx) error {
			rows, err := sqlcgen.New(tx).DeleteDeviceToken(ctx, sqlcgen.DeleteDeviceTokenParams{
				UserID: scope.UserID,
				Token:  token,
			})
			if err != nil {
				return fmt.Errorf("device: delete: %w", err)
			}

			deleted = rows
			return nil
		})
	if err != nil {
		return err
	}
	if deleted == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// ----------------------------------------------------------------- helpers

// toDevice translates a stored row, where an absent workspace and an absent app version are nil.
func toDevice(row sqlcgen.DeviceToken) domain.Device {
	device := domain.Device{
		ID:         row.ID,
		UserID:     row.UserID,
		Platform:   row.Platform,
		Token:      row.Token,
		LastSeenAt: row.LastSeenAt,
		CreatedAt:  row.CreatedAt,
	}
	if row.WorkspaceID != nil {
		device.WorkspaceID = *row.WorkspaceID
	}
	if row.AppVersion != nil {
		device.AppVersion = *row.AppVersion
	}

	return device
}

// optionalID writes an id that may be absent. A zero uuid in the database would be a reference to a
// row nobody has, so absent is nil.
func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}

	return &id
}

// optionalString writes a value that may be absent. An empty app version is "the phone did not say"
// rather than a version called "".
func optionalString(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}
