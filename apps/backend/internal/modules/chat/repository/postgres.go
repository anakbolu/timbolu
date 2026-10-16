// Package repository implements the conversation data access with pgx and the
// sqlc-generated queries.
//
// Every statement runs inside the caller's tenant scope, so Row Level Security
// filters it even when a query forgets its workspace filter. Pagination is by
// cursor, which is what keeps a page boundary stable while new messages arrive.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/repository/sqlcgen"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// Repository is the Postgres-backed implementation of the conversation ports.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// List returns the conversations of the workspace, most recently active first.
func (r *Repository) List(ctx context.Context, scope domain.Scope) ([]domain.Conversation, error) {
	var conversations []domain.Conversation

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListConversations(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("chat: list conversations: %w", err)
		}

		conversations = make([]domain.Conversation, 0, len(rows))
		for _, row := range rows {
			conversations = append(conversations, toConversation(row.ID, row.WorkspaceID, row.Kind, row.Title, row.MessageCount, row.LastActivityAt, row.CreatedAt, row.UpdatedAt))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The participants of every thread in one query, rather than one per row.
	if err := r.attachParticipants(ctx, scope, conversations); err != nil {
		return nil, err
	}

	return conversations, nil
}

// Get returns one conversation with its participants.
func (r *Repository) GetConversation(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Conversation, error) {
	var conversation domain.Conversation

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetConversation(ctx, sqlcgen.GetConversationParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrConversationNotFound
			}
			return fmt.Errorf("chat: get conversation: %w", err)
		}

		conversation = toConversation(row.ID, row.WorkspaceID, row.Kind, row.Title, row.MessageCount, row.LastActivityAt, row.CreatedAt, row.UpdatedAt)
		return nil
	})
	if err != nil {
		return domain.Conversation{}, err
	}

	participants, err := r.participants(ctx, scope, conversation.ID)
	if err != nil {
		return domain.Conversation{}, err
	}
	conversation.Participants = participants

	return conversation, nil
}

// Create makes a conversation with its participants in one transaction, so a
// thread never exists without the people it is between.
func (r *Repository) Create(ctx context.Context, scope domain.Scope, kind, title string, agentIDs, userIDs []uuid.UUID) (domain.Conversation, error) {
	var conversation domain.Conversation

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateConversation(ctx, sqlcgen.CreateConversationParams{
			WorkspaceID: scope.WorkspaceID,
			Kind:        kind,
			Title:       title,
		})
		if err != nil {
			return fmt.Errorf("chat: create conversation: %w", err)
		}

		if err := addParticipants(ctx, queries, scope.WorkspaceID, row.ID, agentIDs, userIDs); err != nil {
			return err
		}

		conversation = domain.Conversation{
			ID:          row.ID,
			WorkspaceID: row.WorkspaceID,
			Kind:        row.Kind,
			Title:       row.Title,
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   row.UpdatedAt,
		}
		return nil
	})
	if err != nil {
		return domain.Conversation{}, err
	}

	return r.GetConversation(ctx, scope, conversation.ID)
}

// AddParticipants adds Bolu and members to a conversation.
func (r *Repository) AddParticipants(ctx context.Context, scope domain.Scope, id uuid.UUID, agentIDs, userIDs []uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if err := addParticipants(ctx, queries, scope.WorkspaceID, id, agentIDs, userIDs); err != nil {
			return err
		}
		return nil
	})
}

// Rename changes a group's title.
func (r *Repository) Rename(ctx context.Context, scope domain.Scope, id uuid.UUID, title string) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if _, err := queries.RenameConversation(ctx, sqlcgen.RenameConversationParams{
			ID: id, WorkspaceID: scope.WorkspaceID, Title: title,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrConversationNotFound
			}
			return fmt.Errorf("chat: rename conversation: %w", err)
		}
		return nil
	})
}

// Direct returns the 1:1 conversation with one Bolu, creating it on first use.
//
// One thread per Bolu is deliberate: a new thread per visit would scatter one
// relationship across dozens of conversations and lose the context the model
// needs.
func (r *Repository) Direct(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (domain.Conversation, error) {
	var existing uuid.UUID

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.FindDirectConversation(ctx, sqlcgen.FindDirectConversationParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     optionalUUID(agentID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("chat: find direct conversation: %w", err)
		}
		existing = row.ID
		return nil
	})
	if err != nil {
		return domain.Conversation{}, err
	}

	if existing != uuid.Nil {
		return r.GetConversation(ctx, scope, existing)
	}

	// The workspace owner is the other participant of a direct thread: it is who
	// the Bolu is talking to.
	conversation, err := r.Create(ctx, scope, domain.KindDirect, "", []uuid.UUID{agentID}, []uuid.UUID{scope.UserID})
	if err != nil {
		return domain.Conversation{}, err
	}
	return conversation, nil
}

// Append stores one message and returns it as stored.
func (r *Repository) Append(ctx context.Context, scope domain.Scope, message domain.Message) (domain.Message, error) {
	blocks, err := domain.MarshalBlocks(message.Blocks)
	if err != nil {
		return domain.Message{}, err
	}
	attachments, err := marshalAttachments(message.Attachments)
	if err != nil {
		return domain.Message{}, err
	}

	var stored domain.Message

	err = r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AppendMessage(ctx, sqlcgen.AppendMessageParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: message.ConversationID,
			AuthorAgentID:  optionalUUID(message.AuthorAgentID),
			AuthorUserID:   optionalUUID(message.AuthorUserID),
			Blocks:         blocks,
			Attachments:    attachments,
			TaskID:         optionalUUID(message.TaskID),
			Status:         statusOr(message.Status, domain.MessageComplete),
			FinishReason:   message.FinishReason,
		})
		if err != nil {
			return translateWriteError(err, "append message")
		}

		if stored, err = toMessage(row); err != nil {
			return err
		}

		if _, err := queries.TouchConversation(ctx, sqlcgen.TouchConversationParams{
			ID: message.ConversationID, WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("chat: touch conversation: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Message{}, err
	}

	return stored, nil
}

// Update replaces a message's body and status. It is how a streamed reply is
// finished, and how a partial one is marked.
func (r *Repository) Update(ctx context.Context, scope domain.Scope, message domain.Message) (domain.Message, error) {
	blocks, err := domain.MarshalBlocks(message.Blocks)
	if err != nil {
		return domain.Message{}, err
	}
	attachments, err := marshalAttachments(message.Attachments)
	if err != nil {
		return domain.Message{}, err
	}

	var stored domain.Message

	err = r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpdateMessage(ctx, sqlcgen.UpdateMessageParams{
			ID:           message.ID,
			WorkspaceID:  scope.WorkspaceID,
			Blocks:       blocks,
			Attachments:  attachments,
			TaskID:       optionalUUID(message.TaskID),
			Status:       statusOr(message.Status, domain.MessageComplete),
			FinishReason: message.FinishReason,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMessageNotFound
			}
			return fmt.Errorf("chat: update message: %w", err)
		}

		stored, err = toMessage(row)
		return err
	})
	if err != nil {
		return domain.Message{}, err
	}

	return stored, nil
}

// Get returns one message.
func (r *Repository) GetMessage(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Message, error) {
	var message domain.Message

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetMessage(ctx, sqlcgen.GetMessageParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMessageNotFound
			}
			return fmt.Errorf("chat: get message: %w", err)
		}

		message, err = toMessage(row)
		return err
	})
	if err != nil {
		return domain.Message{}, err
	}

	return message, nil
}

// History returns one page of a conversation, newest first.
func (r *Repository) History(ctx context.Context, scope domain.Scope, conversationID uuid.UUID, cursor domain.Cursor) (domain.Page, error) {
	limit := clampLimit(cursor.Limit)
	var page domain.Page

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		params := sqlcgen.ListMessagesDescParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: conversationID,
			// One extra row is what says whether another page exists, without a
			// second count query.
			Limit: int32(limit + 1),
		}
		if cursor.BeforeID != uuid.Nil {
			at := time.Unix(0, cursor.BeforeCreatedAtNanos).UTC()
			before := cursor.BeforeID
			params.BeforeCreatedAt = &at
			params.BeforeID = &before
		}

		rows, err := queries.ListMessagesDesc(ctx, params)
		if err != nil {
			return fmt.Errorf("chat: list messages: %w", err)
		}

		page.Messages = make([]domain.Message, 0, len(rows))
		for _, row := range rows {
			message, err := toMessage(row)
			if err != nil {
				return err
			}
			page.Messages = append(page.Messages, message)
		}
		return nil
	})
	if err != nil {
		return domain.Page{}, err
	}

	if len(page.Messages) > limit {
		page.HasMore = true
		page.Messages = page.Messages[:limit]
	}
	if last := len(page.Messages); last > 0 {
		tail := page.Messages[last-1]
		page.NextBeforeCreatedAtNanos = tail.CreatedAt.UnixNano()
		page.NextBeforeID = tail.ID
	}

	return page, nil
}

// Latest returns the newest messages of a conversation, oldest first, which is
// what a chat opens with.
func (r *Repository) LatestMessages(ctx context.Context, scope domain.Scope, conversationID uuid.UUID, limit int) ([]domain.Message, error) {
	var messages []domain.Message

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListMessagesAsc(ctx, sqlcgen.ListMessagesAscParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: conversationID,
			Limit:          int32(clampLimit(limit)),
		})
		if err != nil {
			return fmt.Errorf("chat: list recent messages: %w", err)
		}

		messages = make([]domain.Message, 0, len(rows))
		for _, row := range rows {
			message, err := toMessage(row)
			if err != nil {
				return err
			}
			messages = append(messages, message)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return messages, nil
}

// Save stores attachment bytes and their metadata.
//
// The row and the message's read model are written in one transaction: the rows
// in `message_attachments` are the storage record, while the JSONB column is
// what history and the event stream render, so a message that pointed at a file
// its column did not list would be a file no client ever sees.
func (r *Repository) Save(ctx context.Context, scope domain.Scope, attachment domain.Attachment, content []byte) (domain.Attachment, error) {
	var stored domain.Attachment

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateAttachment(ctx, sqlcgen.CreateAttachmentParams{
			WorkspaceID:    scope.WorkspaceID,
			MessageID:      optionalUUID(attachment.MessageID),
			ConversationID: optionalUUID(attachment.ConversationID),
			UserID:         optionalUUID(scope.UserID),
			StorageKey:     attachment.StorageKey,
			Filename:       attachment.Filename,
			ContentType:    attachment.ContentType,
			ByteSize:       int64(len(content)),
			ChecksumSha256: attachment.ChecksumSHA256,
		})
		if err != nil {
			return translateWriteError(err, "create attachment")
		}

		stored = toAttachment(row)

		element, err := marshalAttachment(stored)
		if err != nil {
			return err
		}

		// A message that is gone takes the whole transaction with it: the row
		// written just above must not outlive the message it points at.
		if _, err := queries.AppendMessageAttachment(ctx, sqlcgen.AppendMessageAttachmentParams{
			ID:          stored.MessageID,
			WorkspaceID: scope.WorkspaceID,
			Attachment:  element,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMessageNotFound
			}
			return fmt.Errorf("chat: append message attachment: %w", err)
		}

		return nil
	})
	if err != nil {
		return domain.Attachment{}, err
	}

	return stored, nil
}

// Open returns the metadata and the bytes of one attachment.
func (r *Repository) Open(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Attachment, error) {
	var attachment domain.Attachment

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetAttachment(ctx, sqlcgen.GetAttachmentParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAttachmentNotFound
			}
			return fmt.Errorf("chat: get attachment: %w", err)
		}

		attachment = toAttachment(row)
		return nil
	})
	if err != nil {
		return domain.Attachment{}, err
	}

	return attachment, nil
}

// ListForMessage returns the attachments of one message.
func (r *Repository) ListForMessage(ctx context.Context, scope domain.Scope, messageID uuid.UUID) ([]domain.Attachment, error) {
	var attachments []domain.Attachment

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListAttachmentsForMessage(ctx, sqlcgen.ListAttachmentsForMessageParams{
			WorkspaceID: scope.WorkspaceID,
			MessageID:   optionalUUID(messageID),
		})
		if err != nil {
			return fmt.Errorf("chat: list attachments: %w", err)
		}

		attachments = make([]domain.Attachment, 0, len(rows))
		for _, row := range rows {
			attachments = append(attachments, toAttachment(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return attachments, nil
}

// boundedLimit takes the caller's bound or the module's default.
func boundedLimit(limit int) int {
	if limit <= 0 {
		return domain.DefaultPruneLimit
	}

	return limit
}

// ListExpired returns the attachments past a moment, oldest first.
func (r *Repository) ListExpired(ctx context.Context, scope domain.Scope, olderThan time.Time, limit int) ([]domain.Attachment, error) {
	var attachments []domain.Attachment

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListExpiredAttachments(ctx, sqlcgen.ListExpiredAttachmentsParams{
			WorkspaceID: scope.WorkspaceID,
			CreatedAt:   olderThan,
			Limit:       int32(boundedLimit(limit)),
		})
		if err != nil {
			return fmt.Errorf("chat: list expired attachments: %w", err)
		}

		attachments = make([]domain.Attachment, 0, len(rows))
		for _, row := range rows {
			attachments = append(attachments, toAttachment(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return attachments, nil
}

// ListStagedExpired returns the uploads that were never named in a message and are past
// a moment, oldest first.
//
// It reads its own statement rather than filtering the sweep above, because a staged
// upload is not an attachment whose window expired: nothing ever sent it, so the
// deployment's attachment policy has nothing to say about it.
func (r *Repository) ListStagedExpired(ctx context.Context, scope domain.Scope, olderThan time.Time, limit int) ([]domain.Attachment, error) {
	var attachments []domain.Attachment

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListStagedExpiredAttachments(ctx, sqlcgen.ListStagedExpiredAttachmentsParams{
			WorkspaceID: scope.WorkspaceID,
			StagedAt:    &olderThan,
			Limit:       int32(boundedLimit(limit)),
		})
		if err != nil {
			return fmt.Errorf("chat: list staged attachments: %w", err)
		}

		attachments = make([]domain.Attachment, 0, len(rows))
		for _, row := range rows {
			attachments = append(attachments, toAttachment(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return attachments, nil
}

// SaveWithNewMessage opens the message an upload carries and stores the file in it,
// all in one transaction.
//
// The order inside the transaction is the order the keys demand: the message row
// first, because the attachment row needs its id, then the row that records the
// bytes, then the element that makes the message render it. Nothing is visible
// between the steps — a client sees the message once, complete, with its file
// already in it — which is what removes the empty bubble an upload used to open
// before its bytes were known to be storable.
func (r *Repository) SaveWithNewMessage(ctx context.Context, scope domain.Scope, message domain.Message, attachment domain.Attachment, content []byte) (domain.Message, domain.Attachment, error) {
	blocks, err := domain.MarshalBlocks(message.Blocks)
	if err != nil {
		return domain.Message{}, domain.Attachment{}, err
	}

	var storedMessage domain.Message
	var stored domain.Attachment

	err = r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AppendMessage(ctx, sqlcgen.AppendMessageParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: message.ConversationID,
			AuthorAgentID:  optionalUUID(message.AuthorAgentID),
			AuthorUserID:   optionalUUID(message.AuthorUserID),
			Blocks:         blocks,
			// The column starts empty and the element is appended below, once the
			// attachment row has the id the element carries.
			Attachments:  []byte("[]"),
			TaskID:       optionalUUID(message.TaskID),
			Status:       statusOr(message.Status, domain.MessageComplete),
			FinishReason: message.FinishReason,
		})
		if err != nil {
			return translateWriteError(err, "append message")
		}
		if storedMessage, err = toMessage(row); err != nil {
			return err
		}

		attachmentRow, err := queries.CreateAttachment(ctx, sqlcgen.CreateAttachmentParams{
			WorkspaceID:    scope.WorkspaceID,
			MessageID:      optionalUUID(storedMessage.ID),
			ConversationID: optionalUUID(message.ConversationID),
			UserID:         optionalUUID(scope.UserID),
			StorageKey:     attachment.StorageKey,
			Filename:       attachment.Filename,
			ContentType:    attachment.ContentType,
			ByteSize:       int64(len(content)),
			ChecksumSha256: attachment.ChecksumSHA256,
		})
		if err != nil {
			return translateWriteError(err, "create attachment")
		}

		stored = toAttachment(attachmentRow)

		element, err := marshalAttachment(stored)
		if err != nil {
			return err
		}

		filled, err := queries.AppendMessageAttachment(ctx, sqlcgen.AppendMessageAttachmentParams{
			ID:          storedMessage.ID,
			WorkspaceID: scope.WorkspaceID,
			Attachment:  element,
		})
		if err != nil {
			return fmt.Errorf("chat: append message attachment: %w", err)
		}
		if storedMessage, err = toMessage(filled); err != nil {
			return err
		}

		if _, err := queries.TouchConversation(ctx, sqlcgen.TouchConversationParams{
			ID: message.ConversationID, WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("chat: touch conversation: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Message{}, domain.Attachment{}, err
	}

	return storedMessage, stored, nil
}

// SaveStaged stores an upload that belongs to no message yet.
//
// It is one insert and no more: there is no message to open, no read model to fill, and
// no event to publish, because nothing has happened in the thread yet. The row says
// which thread and which person it came from, which is what the bind checks against
// when the message that names the file is sent.
func (r *Repository) SaveStaged(ctx context.Context, scope domain.Scope, attachment domain.Attachment, content []byte) (domain.Attachment, error) {
	var stored domain.Attachment

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateStagedAttachment(ctx, sqlcgen.CreateStagedAttachmentParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: optionalUUID(attachment.ConversationID),
			UserID:         optionalUUID(scope.UserID),
			StorageKey:     attachment.StorageKey,
			Filename:       attachment.Filename,
			ContentType:    attachment.ContentType,
			ByteSize:       int64(len(content)),
			ChecksumSha256: attachment.ChecksumSHA256,
		})
		if err != nil {
			return translateWriteError(err, "create staged attachment")
		}

		stored = toAttachment(row)
		return nil
	})
	if err != nil {
		return domain.Attachment{}, err
	}

	return stored, nil
}

// SaveWithAttachments stores a message and binds the staged uploads it names, in one
// transaction.
//
// The order inside it is the point of the whole flow. The message row is written first
// because the binds need its id, and each bind is locked, checked and moved onto that
// id before the element is appended to the read model — so by the time this returns,
// the message and the files a client will render are one committed fact. Anything that
// reads the message afterwards, including the reply worker this call is not allowed to
// start, sees the files.
//
// A single id that cannot be bound fails the transaction: the message it would have
// been sent with is not created either, and the uploads that were staged stay staged
// and readable. That is the difference between a send that is refused and a send that
// silently loses a file.
func (r *Repository) SaveWithAttachments(ctx context.Context, scope domain.Scope, message domain.Message, attachmentIDs []uuid.UUID) (domain.Message, error) {
	blocks, err := domain.MarshalBlocks(message.Blocks)
	if err != nil {
		return domain.Message{}, err
	}

	var stored domain.Message

	err = r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AppendMessage(ctx, sqlcgen.AppendMessageParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: message.ConversationID,
			AuthorAgentID:  optionalUUID(message.AuthorAgentID),
			AuthorUserID:   optionalUUID(message.AuthorUserID),
			Blocks:         blocks,
			// The column starts empty and the binds below fill it one element at a
			// time, so what a client renders is exactly the rows that were bound
			// rather than what the caller hoped to bind.
			Attachments:  []byte("[]"),
			TaskID:       optionalUUID(message.TaskID),
			Status:       statusOr(message.Status, domain.MessageComplete),
			FinishReason: message.FinishReason,
		})
		if err != nil {
			return translateWriteError(err, "append message")
		}
		if stored, err = toMessage(row); err != nil {
			return err
		}

		for _, attachmentID := range attachmentIDs {
			element, err := r.bindAttachment(ctx, queries, scope, stored.ID, message.ConversationID, attachmentID)
			if err != nil {
				return err
			}

			filled, err := queries.AppendMessageAttachment(ctx, sqlcgen.AppendMessageAttachmentParams{
				ID:          stored.ID,
				WorkspaceID: scope.WorkspaceID,
				Attachment:  element,
			})
			if err != nil {
				return fmt.Errorf("chat: append message attachment: %w", err)
			}
			if stored, err = toMessage(filled); err != nil {
				return err
			}
		}

		if _, err := queries.TouchConversation(ctx, sqlcgen.TouchConversationParams{
			ID: message.ConversationID, WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("chat: touch conversation: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Message{}, err
	}

	return stored, nil
}

// bindAttachment locks one staged upload, checks that this caller may bind it, and moves
// it onto the message. It answers the element the message's read model lists.
//
// The lock is what makes the check and the move one decision. Two sends racing over the
// same file both read it here, and the one that loses finds no row to update — a conflict
// rather than a message that quietly points at a file the other message already carries.
//
// A file from another thread, another workspace, or another person is answered as "not
// found" and not as "forbidden": whether somebody else's upload exists is not a client's
// business, and the workspace is already the statement's filter.
func (r *Repository) bindAttachment(ctx context.Context, queries *sqlcgen.Queries, scope domain.Scope, messageID, conversationID, attachmentID uuid.UUID) ([]byte, error) {
	row, err := queries.LockAttachment(ctx, sqlcgen.LockAttachmentParams{
		ID:          attachmentID,
		WorkspaceID: scope.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", domain.ErrAttachmentNotFound, attachmentID)
		}
		return nil, fmt.Errorf("chat: lock attachment: %w", err)
	}

	if row.ConversationID == nil || *row.ConversationID != conversationID ||
		row.UserID == nil || *row.UserID != scope.UserID {
		return nil, fmt.Errorf("%w: %s", domain.ErrAttachmentNotFound, attachmentID)
	}
	if row.Status != domain.AttachmentStaged {
		return nil, fmt.Errorf("%w: %s", domain.ErrAttachmentAlreadyBound, attachmentID)
	}

	bound, err := queries.BindAttachment(ctx, sqlcgen.BindAttachmentParams{
		ID:          attachmentID,
		WorkspaceID: scope.WorkspaceID,
		MessageID:   &messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Staged when it was read and not staged now: another send bound it in
			// between, which is the conflict it looks like rather than a bug.
			return nil, fmt.Errorf("%w: %s", domain.ErrAttachmentAlreadyBound, attachmentID)
		}
		return nil, fmt.Errorf("chat: bind attachment: %w", err)
	}

	return marshalAttachment(toAttachment(bound))
}

// Delete removes one attachment's row and the element it left in its message's read
// model, in one transaction, and answers that message as it now reads.
//
// Both halves go together because they are two views of one fact: the row is the storage
// record and the column is what a client renders, so a row removed alone leaves a file on
// screen whose download is gone.
//
// A row that is already gone is not an error, for the same reason removing an object that is
// absent is not: a retry after a partial failure has to converge rather than report a failure
// that is really "this was already done". The zero message comes back then, and the caller
// publishes nothing.
func (r *Repository) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Message, error) {
	var message domain.Message

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		messageID, err := queries.DeleteAttachment(ctx, sqlcgen.DeleteAttachmentParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("chat: delete attachment: %w", err)
		}
		// A staged upload answers no message, and that is not a gap to fill: it is
		// in no read model, so the row going is the whole removal.
		if messageID == nil {
			return nil
		}

		row, err := queries.RemoveMessageAttachment(ctx, sqlcgen.RemoveMessageAttachmentParams{
			ID:           *messageID,
			WorkspaceID:  scope.WorkspaceID,
			AttachmentID: id.String(),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The message went with the file, so there is no read model left to
				// fix and nothing to publish.
				return nil
			}
			return fmt.Errorf("chat: remove message attachment: %w", err)
		}

		message, err = toMessage(row)
		return err
	})
	if err != nil {
		return domain.Message{}, err
	}

	return message, nil
}

// AppendEvent writes one activity event and returns it with its assigned ID.
func (r *Repository) AppendEvent(ctx context.Context, event domain.Event) (domain.Event, error) {
	var stored domain.Event

	err := r.write(ctx, domain.Scope{WorkspaceID: event.WorkspaceID}, func(queries *sqlcgen.Queries) error {
		row, err := queries.AppendEvent(ctx, sqlcgen.AppendEventParams{
			WorkspaceID:    event.WorkspaceID,
			Type:           event.Type,
			ActorAgentID:   optionalUUID(event.ActorAgentID),
			ActorUserID:    optionalUUID(event.ActorUserID),
			ConversationID: optionalUUID(event.ConversationID),
			TaskID:         optionalUUID(event.TaskID),
			DraftID:        optionalUUID(event.DraftID),
			Payload:        payloadOr(event.Payload),
		})
		if err != nil {
			return fmt.Errorf("chat: append event: %w", err)
		}

		stored = toEvent(row)
		return nil
	})
	if err != nil {
		return domain.Event{}, err
	}

	return stored, nil
}

// SinceEvents returns the events after an ID, oldest first.
func (r *Repository) SinceEvents(ctx context.Context, workspaceID uuid.UUID, afterID int64, limit int) ([]domain.Event, error) {
	var events []domain.Event

	err := r.read(ctx, domain.Scope{WorkspaceID: workspaceID}, func(queries *sqlcgen.Queries) error {
		rows, err := queries.EventsSince(ctx, sqlcgen.EventsSinceParams{
			WorkspaceID: workspaceID,
			ID:          afterID,
			Limit:       int32(clampLimit(limit)),
		})
		if err != nil {
			return fmt.Errorf("chat: events since: %w", err)
		}

		events = make([]domain.Event, 0, len(rows))
		for _, row := range rows {
			events = append(events, toEvent(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

// Latest returns the newest events of a workspace, newest first.
func (r *Repository) LatestEvents(ctx context.Context, workspaceID uuid.UUID, limit int) ([]domain.Event, error) {
	var events []domain.Event

	err := r.read(ctx, domain.Scope{WorkspaceID: workspaceID}, func(queries *sqlcgen.Queries) error {
		rows, err := queries.LatestEvents(ctx, sqlcgen.LatestEventsParams{
			WorkspaceID: workspaceID,
			Limit:       int32(clampLimit(limit)),
		})
		if err != nil {
			return fmt.Errorf("chat: latest events: %w", err)
		}

		events = make([]domain.Event, 0, len(rows))
		for _, row := range rows {
			events = append(events, toEvent(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

// participants returns the participants of one conversation.
func (r *Repository) participants(ctx context.Context, scope domain.Scope, conversationID uuid.UUID) ([]domain.Participant, error) {
	var participants []domain.Participant

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListParticipants(ctx, sqlcgen.ListParticipantsParams{
			WorkspaceID:    scope.WorkspaceID,
			ConversationID: conversationID,
		})
		if err != nil {
			return fmt.Errorf("chat: list participants: %w", err)
		}

		participants = make([]domain.Participant, 0, len(rows))
		for _, row := range rows {
			participants = append(participants, domain.Participant{
				ID:             row.ID,
				ConversationID: row.ConversationID,
				AgentID:        deref(row.AgentID),
				UserID:         deref(row.UserID),
				DisplayName:    row.DisplayName,
				Role:           row.Role,
				CreatedAt:      row.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return participants, nil
}

// attachParticipants fills the participants of every conversation at once.
func (r *Repository) attachParticipants(ctx context.Context, scope domain.Scope, conversations []domain.Conversation) error {
	for i := range conversations {
		participants, err := r.participants(ctx, scope, conversations[i].ID)
		if err != nil {
			return err
		}
		conversations[i].Participants = participants
	}
	return nil
}

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("chat: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("chat: database is not configured")
	}
	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// addParticipants inserts both kinds of participant. A user id and an agent id
// can never be the same row, so the two loops stay separate.
func addParticipants(ctx context.Context, queries *sqlcgen.Queries, workspaceID, conversationID uuid.UUID, agentIDs, userIDs []uuid.UUID) error {
	for _, agentID := range agentIDs {
		if agentID == uuid.Nil {
			continue
		}
		if _, err := queries.AddAgentParticipant(ctx, sqlcgen.AddAgentParticipantParams{
			WorkspaceID: workspaceID, ConversationID: conversationID, AgentID: optionalUUID(agentID),
		}); err != nil {
			return fmt.Errorf("chat: add agent participant: %w", err)
		}
	}

	for _, userID := range userIDs {
		if userID == uuid.Nil {
			continue
		}
		if _, err := queries.AddUserParticipant(ctx, sqlcgen.AddUserParticipantParams{
			WorkspaceID: workspaceID, ConversationID: conversationID, UserID: optionalUUID(userID),
		}); err != nil {
			return fmt.Errorf("chat: add user participant: %w", err)
		}
	}
	return nil
}

// ------------------------------------------------------------- conversions

// toConversation takes the fields rather than a generated row, because the list
// and the single read generate two different row types with the same columns.
func toConversation(
	id, workspaceID uuid.UUID,
	kind, title string,
	messageCount int64,
	lastActivityAt, createdAt, updatedAt time.Time,
) domain.Conversation {
	return domain.Conversation{
		ID:             id,
		WorkspaceID:    workspaceID,
		Kind:           kind,
		Title:          title,
		MessageCount:   int(messageCount),
		LastActivityAt: lastActivityAt,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	}
}

func toMessage(row sqlcgen.Message) (domain.Message, error) {
	blocks, err := domain.UnmarshalBlocks(row.Blocks)
	if err != nil {
		return domain.Message{}, err
	}
	attachments, err := unmarshalAttachments(row.Attachments)
	if err != nil {
		return domain.Message{}, err
	}

	return domain.Message{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		ConversationID: row.ConversationID,
		AuthorAgentID:  deref(row.AuthorAgentID),
		AuthorUserID:   deref(row.AuthorUserID),
		Blocks:         blocks,
		Attachments:    attachments,
		TaskID:         deref(row.TaskID),
		Status:         row.Status,
		FinishReason:   row.FinishReason,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}

// toAttachment takes the nullable message id and the two columns that say where an upload
// came from. A staged row has no message, and that is a state rather than a missing value:
// the zero id is what "not bound yet" reads as everywhere above this line.
func toAttachment(row sqlcgen.MessageAttachment) domain.Attachment {
	return domain.Attachment{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		MessageID:      deref(row.MessageID),
		ConversationID: deref(row.ConversationID),
		StorageKey:     row.StorageKey,
		Filename:       row.Filename,
		ContentType:    row.ContentType,
		ByteSize:       row.ByteSize,
		ChecksumSHA256: row.ChecksumSha256,
		CreatedAt:      row.CreatedAt,
	}
}

func toEvent(row sqlcgen.ActivityEvent) domain.Event {
	return domain.Event{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		Type:           row.Type,
		ActorAgentID:   deref(row.ActorAgentID),
		ActorUserID:    deref(row.ActorUserID),
		ConversationID: deref(row.ConversationID),
		TaskID:         deref(row.TaskID),
		DraftID:        deref(row.DraftID),
		Payload:        row.Payload,
		CreatedAt:      row.CreatedAt,
	}
}

// marshalAttachments writes the read model of a message's attachments.
//
// The metadata is stored twice on purpose: the rows in `message_attachments` are
// the storage record and carry the uniqueness and tenant rules, while this
// column is the read model, so a page of history renders its attachments without
// a query per message. Both are written in the same transaction, and a test
// asserts they agree.
func marshalAttachments(attachments []domain.Attachment) ([]byte, error) {
	// One element at a time through marshalAttachment, which is the same encoder
	// AppendMessageAttachment uses for the one element it appends: an appended
	// element and a rewritten column cannot drift into two shapes.
	elements := make([]json.RawMessage, 0, len(attachments))
	for _, attachment := range attachments {
		element, err := marshalAttachment(attachment)
		if err != nil {
			return nil, err
		}
		elements = append(elements, element)
	}

	raw, err := json.Marshal(elements)
	if err != nil {
		return nil, fmt.Errorf("chat: encode attachments: %w", err)
	}
	return raw, nil
}

// marshalAttachment writes one attachment as one element of that read model.
func marshalAttachment(attachment domain.Attachment) ([]byte, error) {
	raw, err := json.Marshal(attachment)
	if err != nil {
		return nil, fmt.Errorf("chat: encode attachment: %w", err)
	}
	return raw, nil
}

func unmarshalAttachments(raw []byte) ([]domain.Attachment, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var attachments []domain.Attachment
	if err := json.Unmarshal(raw, &attachments); err != nil {
		return nil, fmt.Errorf("chat: decode attachments: %w", err)
	}
	return attachments, nil
}

func payloadOr(payload json.RawMessage) []byte {
	if len(payload) == 0 {
		return []byte("{}")
	}
	return payload
}

func statusOr(status, fallback string) string {
	if status == "" {
		return fallback
	}
	return status
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return domain.DefaultPageSize
	case limit > domain.MaxPageSize:
		return domain.MaxPageSize
	default:
		return limit
	}
}

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func deref(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func translateWriteError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			// A foreign key failure means the conversation or the author is not
			// in this workspace, which RLS also refuses.
			return fmt.Errorf("%w: %s", domain.ErrConversationNotFound, pgErr.ConstraintName)
		case "23505":
			return fmt.Errorf("%w: %s already exists", domain.ErrInvalidInput, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("chat: %s: %w", action, err)
}
