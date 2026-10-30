package repository

import (
	"time"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/whatsapp/repository/sqlcgen"
	"github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The conversions from the generated rows to the domain types.
//
// sqlc gives every query its own row type even when two queries select the same
// columns, so each table is read through one shared field set. The nullable
// columns are the interesting ones: a Bolu that has not been chosen, a draft a
// message did not come from, and the media a message does not carry are all
// "absent" in the domain rather than a zero value that reads as a real id.

// numberRow is the column set every number query returns.
type numberRow struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	IntegrationID *uuid.UUID
	Phone         string
	Label         string
	AgentID       *uuid.UUID
	Status        string
	StatusReason  string
	TosAcceptedAt *time.Time
	HourlyLimit   int32
	ConnectedAt   *time.Time
	LastSeenAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func toNumber(row numberRow) domain.Number {
	number := domain.Number{
		ID:            row.ID,
		WorkspaceID:   row.WorkspaceID,
		Phone:         row.Phone,
		Label:         row.Label,
		Status:        row.Status,
		StatusReason:  row.StatusReason,
		TOSAcceptedAt: row.TosAcceptedAt,
		HourlyLimit:   int(row.HourlyLimit),
		ConnectedAt:   row.ConnectedAt,
		LastSeenAt:    row.LastSeenAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	if row.IntegrationID != nil {
		number.IntegrationID = *row.IntegrationID
	}
	if row.AgentID != nil {
		number.AgentID = *row.AgentID
	}
	return number
}

func fromCreatedNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromGotNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromPhoneNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromIntegrationNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromListedNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromUpdatedNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromStatusNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

func fromTermsNumber(row sqlcgen.WhatsappNumber) domain.Number {
	return toNumber(numberRow(row))
}

// messageRow is the column set every message query returns.
type messageRow struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	NumberID    uuid.UUID
	WaMessageID string
	ChatJid     string
	SenderJid   string
	SenderName  string
	IsGroup     bool
	IsFromMe    bool
	Kind        string
	Text        string
	MediaKey    string
	MediaKind   string
	MediaSize   int64
	MediaStatus string
	MediaError  string
	ReceivedAt  time.Time
	TaskID      *uuid.UUID
	CreatedAt   time.Time
}

func toMessage(row messageRow) domain.Inbound {
	message := domain.Inbound{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		NumberID:    row.NumberID,
		WaMessageID: row.WaMessageID,
		ChatJID:     row.ChatJid,
		SenderJID:   row.SenderJid,
		SenderName:  row.SenderName,
		IsGroup:     row.IsGroup,
		IsFromMe:    row.IsFromMe,
		Kind:        row.Kind,
		Text:        row.Text,
		ReceivedAt:  row.ReceivedAt,
		CreatedAt:   row.CreatedAt,
		Media: domain.Media{
			Kind:   row.MediaKind,
			Size:   row.MediaSize,
			Status: row.MediaStatus,
			Key:    row.MediaKey,
			Error:  row.MediaError,
		},
	}
	if row.TaskID != nil {
		message.TaskID = *row.TaskID
	}
	return message
}

func fromRecordedMessage(row sqlcgen.WhatsappMessage) domain.Inbound {
	return toMessage(messageRow(row))
}

// fromGotMessage reads a message fetched by its own id or by WhatsApp's. Both
// queries select the same columns, so one reader serves both.
func fromGotMessage(row sqlcgen.WhatsappMessage) domain.Inbound {
	return toMessage(messageRow(row))
}

func fromListedMessage(row sqlcgen.WhatsappMessage) domain.Inbound {
	return toMessage(messageRow(row))
}

func fromPendingMedia(row sqlcgen.WhatsappMessage) domain.Inbound {
	return toMessage(messageRow(row))
}

// outboxRow is the column set every outbox query returns.
type outboxRow struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	NumberID    uuid.UUID
	ToJid       string
	Kind        string
	Text        string
	MediaKey    string
	DraftID     *uuid.UUID
	Status      string
	Attempts    int32
	LastError   string
	NotBefore   time.Time
	ClaimedAt   *time.Time
	WaMessageID string
	SentAt      *time.Time
	CreatedAt   time.Time
}

func toOutbox(row outboxRow) domain.Outbox {
	out := domain.Outbox{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		NumberID:    row.NumberID,
		ToJID:       row.ToJid,
		Kind:        row.Kind,
		Text:        row.Text,
		MediaKey:    row.MediaKey,
		Status:      row.Status,
		Attempts:    int(row.Attempts),
		LastError:   row.LastError,
		NotBefore:   row.NotBefore,
		ClaimedAt:   row.ClaimedAt,
		WaMessageID: row.WaMessageID,
		SentAt:      row.SentAt,
		CreatedAt:   row.CreatedAt,
	}
	if row.DraftID != nil {
		out.DraftID = *row.DraftID
	}
	return out
}

func fromEnqueued(row sqlcgen.WhatsappOutbox) domain.Outbox {
	return toOutbox(outboxRow(row))
}

func fromClaimed(row sqlcgen.WhatsappOutbox) domain.Outbox {
	return toOutbox(outboxRow(row))
}

func fromListedOutbox(row sqlcgen.WhatsappOutbox) domain.Outbox {
	return toOutbox(outboxRow(row))
}

// optionalID writes an id that may be absent. A zero uuid in the database would
// be a reference to a row nobody has, so absent is nil.
func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// optionalText writes a filter that may not narrow anything.
func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
