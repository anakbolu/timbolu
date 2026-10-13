// Package repository implements the WhatsApp data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a query forgets its workspace. The one exception is the
// fleet directory, which is named as such where it is read: the gateway has to
// discover which tenants have a number before it can open any workspace's scope,
// and that table holds a routing pair and nothing else.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of the WhatsApp ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// listLimit is the ceiling of a list read, so one request cannot pull every
// message a number ever received.
const listLimit = 200

// Create stores one number.
//
// A phone number is unique across the whole deployment rather than per
// workspace: the WhatsApp session belongs to the number, so a second tenant
// cannot have it. That conflict arrives as a unique violation rather than as a
// visible row — Row Level Security hides the other tenant's number — so it is
// translated into the error that says what actually happened.
func (r *Repository) Create(ctx context.Context, scope domain.Scope, number domain.Number) (domain.Number, error) {
	var stored domain.Number

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateWhatsAppNumber(ctx, sqlcgen.CreateWhatsAppNumberParams{
			WorkspaceID: scope.WorkspaceID,
			// A number that is being prepared has no connection yet: the connection
			// is created when the phone pairs, and the column is nullable for
			// exactly that gap.
			IntegrationID: optionalID(number.IntegrationID),
			Phone:         number.Phone,
			Label:         number.Label,
			AgentID:       optionalID(number.AgentID),
			Status:        number.Status,
			TosAcceptedAt: number.TOSAcceptedAt,
			HourlyLimit:   int32(number.HourlyLimit),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The insert is a SELECT, so a Bolu outside this workspace comes
				// back as no rows rather than as a constraint violation.
				return fmt.Errorf("%w: the bolu is not in this workspace", domain.ErrInvalidInput)
			}
			if isUnique(err, "whatsapp_numbers_phone_key") {
				return fmt.Errorf("%w: %s", domain.ErrAlreadyLinked, number.Phone)
			}
			return translate(err, "create number")
		}

		stored = fromCreatedNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return stored, nil
}

// Get returns one number.
func (r *Repository) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Number, error) {
	var number domain.Number

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppNumber(ctx, sqlcgen.GetWhatsAppNumberParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "get number")
		}

		number = fromGotNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return number, nil
}

// ByPhone returns the workspace's number with this phone, which is how a
// relink of a number this workspace already has finds its row.
func (r *Repository) ByPhone(ctx context.Context, scope domain.Scope, phone string) (domain.Number, error) {
	var number domain.Number

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppNumberByPhone(ctx, phone)
		if err != nil {
			return translate(err, "get number by phone")
		}

		number = fromPhoneNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return number, nil
}

// ByIntegration returns the number a connection speaks as.
//
// A number that is not linked yet has no connection, so a call that names one
// cannot match it: the answer is not-found rather than the row.
func (r *Repository) ByIntegration(ctx context.Context, scope domain.Scope, integrationID uuid.UUID) (domain.Number, error) {
	var number domain.Number

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppNumberByIntegration(ctx, sqlcgen.GetWhatsAppNumberByIntegrationParams{
			// The parameter is nullable because the column is, and the caller has
			// already refused a zero id: a call that names no connection cannot
			// match the row of a number nobody linked.
			IntegrationID: &integrationID,
			WorkspaceID:   scope.WorkspaceID,
		})
		if err != nil {
			return translate(err, "get number by integration")
		}

		number = fromIntegrationNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return number, nil
}

// List returns the workspace's numbers, newest first.
func (r *Repository) List(ctx context.Context, scope domain.Scope) ([]domain.Number, error) {
	numbers := []domain.Number{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListWhatsAppNumbers(ctx, sqlcgen.ListWhatsAppNumbersParams{
			WorkspaceID: scope.WorkspaceID,
			Limit:       listLimit,
		})
		if err != nil {
			return translate(err, "list numbers")
		}

		for _, row := range rows {
			numbers = append(numbers, fromListedNumber(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return numbers, nil
}

// Update writes the fields a person edits: the label, who answers, and the
// ceiling.
func (r *Repository) Update(ctx context.Context, scope domain.Scope, number domain.Number) (domain.Number, error) {
	var stored domain.Number

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateWhatsAppNumber(ctx, sqlcgen.UpdateWhatsAppNumberParams{
			ID:          number.ID,
			WorkspaceID: scope.WorkspaceID,
			Label:       number.Label,
			AgentID:     optionalID(number.AgentID),
			HourlyLimit: int32(number.HourlyLimit),
		})
		if err != nil {
			return translate(err, "update number")
		}

		stored = fromUpdatedNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return stored, nil
}

// SetStatus records a status and why.
//
// It is one statement rather than a read followed by a write because two
// observers can report at once — the transport's own event and the drain's
// heartbeat — and the later write is the one that is true.
func (r *Repository) SetStatus(ctx context.Context, scope domain.Scope, id uuid.UUID, status, reason string) (domain.Number, error) {
	var stored domain.Number

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.SetWhatsAppNumberStatus(ctx, sqlcgen.SetWhatsAppNumberStatusParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Status:      status,
			Reason:      reason,
		})
		if err != nil {
			return translate(err, "set number status")
		}

		stored = fromStatusNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return stored, nil
}

// Touch moves the number's last-seen forward.
func (r *Repository) Touch(ctx context.Context, scope domain.Scope, id uuid.UUID, at time.Time) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.TouchWhatsAppNumber(ctx, sqlcgen.TouchWhatsAppNumberParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			At:          &at,
		}); err != nil {
			return translate(err, "touch number")
		}
		return nil
	})
}

// AcceptTerms records that the workspace accepted the warning. It is idempotent:
// accepting twice keeps the first moment, because that is when the risk was
// taken.
func (r *Repository) AcceptTerms(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Number, error) {
	var stored domain.Number

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AcceptWhatsAppTerms(ctx, sqlcgen.AcceptWhatsAppTermsParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "accept terms")
		}

		stored = fromTermsNumber(row)
		return nil
	})
	if err != nil {
		return domain.Number{}, err
	}

	return stored, nil
}

// Delete removes one number and, by cascade, its session and its queue.
func (r *Repository) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.DeleteWhatsAppNumber(ctx, sqlcgen.DeleteWhatsAppNumberParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "delete number")
		}
		if rows == 0 {
			return fmt.Errorf("%w: delete number", domain.ErrNotFound)
		}
		return nil
	})
}

// Publish records that this number should be served by the fleet.
func (r *Repository) Publish(ctx context.Context, scope domain.Scope, numberID uuid.UUID, enabled bool) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if err := queries.PublishWhatsAppDirectory(ctx, sqlcgen.PublishWhatsAppDirectoryParams{
			NumberID:    numberID,
			WorkspaceID: scope.WorkspaceID,
			Enabled:     enabled,
		}); err != nil {
			return translate(err, "publish number")
		}
		return nil
	})
}

// Serving returns the numbers the fleet is expected to serve.
//
// It is the one read outside a tenant scope, and the migration says why: a
// gateway has to discover which tenants have a number before it can open any
// workspace's scope at all. What it returns is a routing pair, not data.
func (r *Repository) Serving(ctx context.Context) ([]domain.Assignment, error) {
	if r == nil || r.pool == nil {
		return nil, fmt.Errorf("%w: database is not configured", domain.ErrNotConfigured)
	}

	rows, err := sqlcgen.New(r.pool.PgxPool()).ServingWhatsAppNumbers(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: serving numbers: %w", err)
	}

	assignments := make([]domain.Assignment, 0, len(rows))
	for _, row := range rows {
		assignments = append(assignments, domain.Assignment{NumberID: row.NumberID, WorkspaceID: row.WorkspaceID})
	}
	return assignments, nil
}

// Save stores the sealed device state.
func (r *Repository) Save(ctx context.Context, scope domain.Scope, session domain.Session) error {
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

// Load returns the sealed device state of one number.
func (r *Repository) Load(ctx context.Context, scope domain.Scope, numberID uuid.UUID) (domain.Session, error) {
	var session domain.Session

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppSession(ctx, sqlcgen.GetWhatsAppSessionParams{
			NumberID:    numberID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return translate(err, "load session")
		}

		session = fromGotSession(row)
		return nil
	})
	if err != nil {
		return domain.Session{}, err
	}

	return session, nil
}

// ForgetSession forgets a device.
func (r *Repository) ForgetSession(ctx context.Context, scope domain.Scope, numberID uuid.UUID) error {
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

// Record stores one inbound message and reports whether it was new.
//
// A duplicate collides on the dedupe key and comes back as no rows, which is the
// answer the caller acts on: the message was already handled, so no second task
// is opened for it.
func (r *Repository) Record(ctx context.Context, scope domain.Scope, message domain.Inbound) (domain.Inbound, bool, error) {
	var stored domain.Inbound
	created := false

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.RecordWhatsAppMessage(ctx, sqlcgen.RecordWhatsAppMessageParams{
			WorkspaceID: scope.WorkspaceID,
			NumberID:    message.NumberID,
			WaMessageID: message.WaMessageID,
			ChatJid:     message.ChatJID,
			SenderJid:   message.SenderJID,
			SenderName:  message.SenderName,
			IsGroup:     message.IsGroup,
			IsFromMe:    message.IsFromMe,
			Kind:        message.Kind,
			Text:        message.Text,
			MediaKey:    message.Media.Key,
			MediaKind:   message.Media.Kind,
			MediaSize:   message.Media.Size,
			MediaStatus: message.Media.Status,
			MediaError:  message.Media.Error,
			ReceivedAt:  message.ReceivedAt,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The message is already recorded, which is what a redelivery
				// looks like. The stored row is read back so the caller answers
				// with the id it already has.
				existing, readErr := queries.GetWhatsAppMessageByWaID(ctx, sqlcgen.GetWhatsAppMessageByWaIDParams{
					WorkspaceID: scope.WorkspaceID,
					WaMessageID: message.WaMessageID,
				})
				if readErr != nil {
					return translate(readErr, "get message by wa id")
				}
				stored = fromGotMessage(existing)
				return nil
			}
			return translate(err, "record message")
		}

		stored = fromRecordedMessage(row)
		created = true
		return nil
	})
	if err != nil {
		return domain.Inbound{}, false, err
	}

	return stored, created, nil
}

// MessageByID returns one message.
func (r *Repository) MessageByID(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Inbound, error) {
	var message domain.Inbound

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppMessage(ctx, sqlcgen.GetWhatsAppMessageParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			return translate(err, "get message")
		}

		message = fromGotMessage(row)
		return nil
	})
	if err != nil {
		return domain.Inbound{}, err
	}

	return message, nil
}

// Messages returns what the customers wrote, newest first.
func (r *Repository) Messages(ctx context.Context, scope domain.Scope, filter domain.MessageFilter) ([]domain.Inbound, error) {
	messages := []domain.Inbound{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListWhatsAppMessages(ctx, sqlcgen.ListWhatsAppMessagesParams{
			WorkspaceID: scope.WorkspaceID,
			NumberID:    optionalID(filter.NumberID),
			ChatJid:     optionalText(filter.ChatJID),
			Limit:       limitOr(filter.Limit),
		})
		if err != nil {
			return translate(err, "list messages")
		}

		for _, row := range rows {
			messages = append(messages, fromListedMessage(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return messages, nil
}

// ByWaID returns one message by WhatsApp's own id.
func (r *Repository) ByWaID(ctx context.Context, scope domain.Scope, waMessageID string) (domain.Inbound, error) {
	var message domain.Inbound

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetWhatsAppMessageByWaID(ctx, sqlcgen.GetWhatsAppMessageByWaIDParams{
			WorkspaceID: scope.WorkspaceID,
			WaMessageID: waMessageID,
		})
		if err != nil {
			return translate(err, "get message")
		}

		message = fromGotMessage(row)
		return nil
	})
	if err != nil {
		return domain.Inbound{}, err
	}

	return message, nil
}

// PendingMedia returns the messages whose bytes have not been fetched yet.
func (r *Repository) PendingMedia(ctx context.Context, scope domain.Scope, limit int) ([]domain.Inbound, error) {
	messages := []domain.Inbound{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListWhatsAppMediaPending(ctx, sqlcgen.ListWhatsAppMediaPendingParams{
			WorkspaceID: scope.WorkspaceID,
			Limit:       limitOr(limit),
		})
		if err != nil {
			return translate(err, "list pending media")
		}

		for _, row := range rows {
			messages = append(messages, fromPendingMedia(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return messages, nil
}

// SetMedia records where a message's bytes were stored, or that they could not
// be.
func (r *Repository) SetMedia(ctx context.Context, scope domain.Scope, id uuid.UUID, media domain.Media) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.SetWhatsAppMessageMedia(ctx, sqlcgen.SetWhatsAppMessageMediaParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			MediaKey:    media.Key,
			MediaKind:   media.Kind,
			MediaSize:   media.Size,
			MediaStatus: media.Status,
			MediaError:  media.Error,
		}); err != nil {
			return translate(err, "set message media")
		}
		return nil
	})
}

// AttachTask records the task a message opened.
//
// A message that is not there — or is another workspace's — is refused rather than
// reported as linked. The statement updates nothing in that case, and answering
// success would leave a screen showing a message with no task while the caller
// believes it wrote one.
func (r *Repository) AttachTask(ctx context.Context, scope domain.Scope, id, taskID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.AttachWhatsAppMessageTask(ctx, sqlcgen.AttachWhatsAppMessageTaskParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			TaskID:      optionalID(taskID),
		})
		if err != nil {
			return translate(err, "attach message task")
		}
		if rows == 0 {
			return fmt.Errorf("%w: attach message task", domain.ErrNotFound)
		}
		return nil
	})
}

// Enqueue records one message to send.
func (r *Repository) Enqueue(ctx context.Context, scope domain.Scope, item domain.Outbox) (domain.Outbox, error) {
	var stored domain.Outbox

	notBefore := item.NotBefore
	if notBefore.IsZero() {
		notBefore = time.Now()
	}

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.EnqueueWhatsAppMessage(ctx, sqlcgen.EnqueueWhatsAppMessageParams{
			WorkspaceID: scope.WorkspaceID,
			NumberID:    item.NumberID,
			ToJid:       item.ToJID,
			Kind:        item.Kind,
			Text:        item.Text,
			MediaKey:    item.MediaKey,
			DraftID:     optionalID(item.DraftID),
			NotBefore:   notBefore,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the number is not in this workspace", domain.ErrInvalidInput)
			}
			return translate(err, "enqueue message")
		}

		stored = fromEnqueued(row)
		return nil
	})
	if err != nil {
		return domain.Outbox{}, err
	}

	return stored, nil
}

// Due claims the messages of one number that are ready to send.
func (r *Repository) Due(ctx context.Context, scope domain.Scope, numberID uuid.UUID, limit int) ([]domain.Outbox, error) {
	items := []domain.Outbox{}

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ClaimWhatsAppOutbox(ctx, sqlcgen.ClaimWhatsAppOutboxParams{
			WorkspaceID: scope.WorkspaceID,
			NumberID:    numberID,
			Limit:       limitOr(limit),
		})
		if err != nil {
			return translate(err, "claim outbox")
		}

		for _, row := range rows {
			items = append(items, fromClaimed(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return items, nil
}

// MarkSent records that a message left.
func (r *Repository) MarkSent(ctx context.Context, scope domain.Scope, id uuid.UUID, waMessageID string, at time.Time) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.MarkWhatsAppOutboxSent(ctx, sqlcgen.MarkWhatsAppOutboxSentParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			WaMessageID: waMessageID,
			SentAt:      &at,
		}); err != nil {
			return translate(err, "mark sent")
		}
		return nil
	})
}

// MarkHeld returns a message to the queue with a later time, which is what the
// hourly ceiling does: it paces a message, it does not refuse it.
func (r *Repository) MarkHeld(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string, at time.Time) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.MarkWhatsAppOutboxHeld(ctx, sqlcgen.MarkWhatsAppOutboxHeldParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Reason:      reason,
			NotBefore:   at,
		}); err != nil {
			return translate(err, "mark held")
		}
		return nil
	})
}

// MarkFailed parks a message that will not be retried, so it stays visible.
func (r *Repository) MarkFailed(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string, attempts int) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.MarkWhatsAppOutboxFailed(ctx, sqlcgen.MarkWhatsAppOutboxFailedParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Reason:      reason,
			Attempts:    int32(attempts),
		}); err != nil {
			return translate(err, "mark failed")
		}
		return nil
	})
}

// SentSince counts what one number sent since a moment, which is how the hourly
// ceiling is measured.
func (r *Repository) SentSince(ctx context.Context, scope domain.Scope, numberID uuid.UUID, since time.Time) (int, error) {
	var count int

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		total, err := queries.CountWhatsAppOutboxSentSince(ctx, sqlcgen.CountWhatsAppOutboxSentSinceParams{
			NumberID:    numberID,
			WorkspaceID: scope.WorkspaceID,
			SentAt:      &since,
		})
		if err != nil {
			return translate(err, "count sent")
		}

		count = int(total)
		return nil
	})
	if err != nil {
		return 0, err
	}

	return count, nil
}

// Outbox returns what the workspace asked one number to send, newest first.
func (r *Repository) Outbox(ctx context.Context, scope domain.Scope, filter domain.OutboxFilter) ([]domain.Outbox, error) {
	items := []domain.Outbox{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListWhatsAppOutbox(ctx, sqlcgen.ListWhatsAppOutboxParams{
			WorkspaceID: scope.WorkspaceID,
			NumberID:    optionalID(filter.NumberID),
			Status:      optionalText(filter.Status),
			Limit:       limitOr(filter.Limit),
		})
		if err != nil {
			return translate(err, "list outbox")
		}

		for _, row := range rows {
			items = append(items, fromListedOutbox(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return items, nil
}

// Reclaim returns messages a dead gateway left mid-send to the queue.
func (r *Repository) Reclaim(ctx context.Context, scope domain.Scope, olderThan time.Time) (int, error) {
	var reclaimed int

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ReclaimWhatsAppOutbox(ctx, sqlcgen.ReclaimWhatsAppOutboxParams{
			WorkspaceID: scope.WorkspaceID,
			OlderThan:   &olderThan,
			NotBefore:   time.Now(),
		})
		if err != nil {
			return translate(err, "reclaim outbox")
		}

		reclaimed = int(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}

	return reclaimed, nil
}

// read runs fn in a read-only transaction scoped to the tenant.
func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrNotConfigured)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// write runs fn in a transaction scoped to the tenant.
func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrNotConfigured)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// translate turns a driver error into the module's own.
func translate(err error, operation string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, operation)
	}
	return fmt.Errorf("whatsapp: %s: %w", operation, err)
}

// isUnique reports whether an error is a unique violation on one index.
func isUnique(err error, index string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == index
}

// limitOr applies the module's list ceiling.
func limitOr(value int) int32 {
	if value <= 0 || value > listLimit {
		return listLimit
	}
	return int32(value)
}
