package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/repository/sqlcgen"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// The two statements the link flow and the retry path need, kept apart from the
// main file because each is written once in a number's life rather than on a
// path that runs per message.

// SetIntegration records the connection the hub created for this number.
func (r *Repository) SetIntegration(ctx context.Context, scope domain.Scope, id, integrationID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.SetWhatsAppNumberIntegration(ctx, sqlcgen.SetWhatsAppNumberIntegrationParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			// The parameter is nullable because the column is. Clearing it is not a
			// case this path has: a link that finished always names its connection.
			IntegrationID: &integrationID,
		})
		if err != nil {
			return translate(err, "set integration")
		}
		if rows == 0 {
			return fmt.Errorf("%w: set integration", domain.ErrNotFound)
		}
		return nil
	})
}

// SetPhone records the number WhatsApp reported when the phone paired.
func (r *Repository) SetPhone(ctx context.Context, scope domain.Scope, id uuid.UUID, phone string) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.SetWhatsAppNumberPhone(ctx, sqlcgen.SetWhatsAppNumberPhoneParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Phone:       phone,
		})
		if err != nil {
			return translate(err, "set phone")
		}
		if rows == 0 {
			return fmt.Errorf("%w: set phone", domain.ErrNotFound)
		}
		return nil
	})
}

// MarkRetry puts a message back in the queue after a failed attempt.
func (r *Repository) MarkRetry(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string, at time.Time) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.MarkWhatsAppOutboxRetry(ctx, sqlcgen.MarkWhatsAppOutboxRetryParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Reason:      reason,
			NotBefore:   at,
		}); err != nil {
			return translate(err, "mark retry")
		}
		return nil
	})
}
