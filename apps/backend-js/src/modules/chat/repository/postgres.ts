/**
 * The conversation data access.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a query forgets its workspace filter. Pagination is by
 * cursor, which is what keeps a page boundary stable while new messages arrive.
 *
 * Ported 1:1 from internal/modules/chat/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  ErrAttachmentNotFound,
  ErrConversationNotFound,
  ErrInvalidInput,
  ErrMessageNotFound,
  KindDirect,
  MessageComplete,
  NilUUID,
  WrappedError,
  isNil,
  marshalBlocks,
  unmarshalBlocks,
  type Attachment,
  type Conversation,
  type Message,
  type Participant,
} from "../domain/conversation.ts";
import type { Event } from "../domain/event.ts";
import {
  DefaultPageSize,
  DefaultPruneLimit,
  MaxPageSize,
  type Cursor,
  type Page,
  type Scope,
} from "../domain/ports.ts";

/** One conversation row, as the list and single reads project it. */
interface ConversationRow {
  id: string;
  workspace_id: string;
  kind: string;
  title: string;
  created_at: Date;
  updated_at: Date;
  message_count: number;
  last_activity_at: Date;
}

/** One row of `messages`. */
interface MessageRow {
  id: string;
  workspace_id: string;
  conversation_id: string;
  author_agent_id: string | null;
  author_user_id: string | null;
  blocks: unknown;
  attachments: unknown;
  task_id: string | null;
  created_at: Date;
  status: string;
  finish_reason: string;
  updated_at: Date;
}

/** One row of `message_attachments`. */
interface AttachmentRow {
  id: string;
  workspace_id: string;
  message_id: string;
  storage_key: string;
  filename: string;
  content_type: string;
  byte_size: number;
  checksum_sha256: string;
  created_at: Date;
}

/** One row of `conversation_participants`, joined to its identity. */
interface ParticipantRow {
  id: string;
  conversation_id: string;
  agent_id: string | null;
  user_id: string | null;
  created_at: Date;
  display_name: string;
  role: string;
}

/** One row of `activity_events`. */
interface EventRow {
  id: number;
  workspace_id: string;
  type: string;
  actor_agent_id: string | null;
  actor_user_id: string | null;
  conversation_id: string | null;
  task_id: string | null;
  draft_id: string | null;
  payload: unknown;
  created_at: Date;
}

/**
 * One attachment as it is stored inside a message's read-model column.
 *
 * The Go struct carries no json tags, so the keys are its field names verbatim;
 * `RemoveMessageAttachment` matches `element->>'ID'`, which is the same key.
 */
interface AttachmentElement {
  ID: string;
  WorkspaceID: string;
  MessageID: string;
  StorageKey: string;
  Filename: string;
  ContentType: string;
  ByteSize: number;
  ChecksumSHA256: string;
  CreatedAt: string;
}

const MESSAGE_COLUMNS = `id, workspace_id, conversation_id, author_agent_id, author_user_id,
  blocks, attachments, task_id, created_at, status, finish_reason, updated_at`;

const CONVERSATION_COLUMNS = `c.id, c.workspace_id, c.kind, c.title, c.created_at, c.updated_at,
  (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id)::bigint AS message_count,
  COALESCE(
      (SELECT max(m.created_at) FROM messages m WHERE m.conversation_id = c.id),
      c.created_at
  )::timestamptz AS last_activity_at`;

export class PostgresRepository {
  constructor(private readonly db: Database) {}

  // ----------------------------------------------------------- conversations

  /** The conversations of the workspace, most recently active first. */
  async list(scope: Scope): Promise<Conversation[]> {
    const conversations = await this.read(scope, async (tx) => {
      // name: ListConversations
      const rows = await tx<ConversationRow[]>`
        SELECT ${tx.unsafe(CONVERSATION_COLUMNS)}
        FROM conversations c
        WHERE c.workspace_id = ${scope.workspaceId}
        ORDER BY last_activity_at DESC
      `;
      return rows.map(toConversation);
    });

    // The participants of every thread, one query per thread, as the Go
    // repository does: the list is a screen's worth of rows, not a scan.
    for (const conversation of conversations) {
      conversation.participants = await this.participants(scope, conversation.id);
    }
    return conversations;
  }

  /** One conversation with its participants. */
  async getConversation(scope: Scope, id: string): Promise<Conversation> {
    const conversation = await this.read(scope, async (tx) => {
      // name: GetConversation
      const rows = await tx<ConversationRow[]>`
        SELECT ${tx.unsafe(CONVERSATION_COLUMNS)}
        FROM conversations c
        WHERE c.id = ${id} AND c.workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) {
        throw ErrConversationNotFound;
      }
      return toConversation(rows[0]);
    });

    conversation.participants = await this.participants(scope, conversation.id);
    return conversation;
  }

  /** Makes a conversation with its participants in one transaction. */
  async create(
    scope: Scope,
    kind: string,
    title: string,
    agentIds: string[],
    userIds: string[],
  ): Promise<Conversation> {
    const id = await this.write(scope, async (tx) => {
      // name: CreateConversation
      const rows = await tx<{ id: string }[]>`
        INSERT INTO conversations (workspace_id, kind, title)
        VALUES (${scope.workspaceId}, ${kind}, ${title})
        RETURNING id
      `;
      await addParticipants(tx, scope.workspaceId, rows[0].id, agentIds, userIds);
      return rows[0].id;
    });

    return this.getConversation(scope, id);
  }

  /** Adds Bolu and members to a conversation. */
  async addParticipants(
    scope: Scope,
    id: string,
    agentIds: string[],
    userIds: string[],
  ): Promise<void> {
    await this.write(scope, async (tx) => {
      await addParticipants(tx, scope.workspaceId, id, agentIds, userIds);
    });
  }

  /** Changes a group's title. */
  async rename(scope: Scope, id: string, title: string): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: RenameConversation
      const rows = await tx<{ id: string }[]>`
        UPDATE conversations
        SET title = ${title}, updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING id
      `;
      if (rows.length === 0) {
        throw ErrConversationNotFound;
      }
    });
  }

  /**
   * The 1:1 conversation with one Bolu, creating it on first use.
   *
   * One thread per Bolu is deliberate: a new thread per visit would scatter one
   * relationship across dozens of conversations and lose the context the model
   * needs.
   */
  async direct(scope: Scope, agentId: string): Promise<Conversation> {
    const existing = await this.read(scope, async (tx) => {
      // name: FindDirectConversation
      const rows = await tx<{ id: string }[]>`
        SELECT c.id
        FROM conversations c
        JOIN conversation_participants p
            ON p.conversation_id = c.id AND p.agent_id = ${agentId}
        WHERE c.workspace_id = ${scope.workspaceId}
          AND c.kind = 'direct'
          AND (SELECT count(*) FROM conversation_participants q WHERE q.conversation_id = c.id) = 2
        LIMIT 1
      `;
      return rows[0]?.id ?? "";
    });

    if (existing !== "") {
      return this.getConversation(scope, existing);
    }

    // The workspace owner is the other participant of a direct thread: it is
    // who the Bolu is talking to.
    return this.create(scope, KindDirect, "", [agentId], [scope.userId]);
  }

  /** The participants of one conversation. */
  private async participants(scope: Scope, conversationId: string): Promise<Participant[]> {
    return this.read(scope, async (tx) => {
      // name: ListParticipants
      const rows = await tx<ParticipantRow[]>`
        SELECT p.id, p.conversation_id, p.agent_id, p.user_id, p.created_at,
               COALESCE(a.name, u.email) AS display_name,
               COALESCE(a.role, 'member') AS role
        FROM conversation_participants p
        LEFT JOIN agents a ON a.id = p.agent_id
        LEFT JOIN users u ON u.id = p.user_id
        WHERE p.workspace_id = ${scope.workspaceId}::uuid
          AND p.conversation_id = ${conversationId}::uuid
        ORDER BY (p.agent_id IS NULL), display_name
      `;
      return rows.map(
        (row): Participant => ({
          id: row.id,
          conversationId: row.conversation_id,
          agentId: row.agent_id ?? "",
          userId: row.user_id ?? "",
          displayName: row.display_name,
          role: row.role,
          createdAt: row.created_at,
        }),
      );
    });
  }

  // ---------------------------------------------------------------- messages

  /** Stores one message and returns it as stored. */
  async append(scope: Scope, message: Message): Promise<Message> {
    return this.write(scope, async (tx) => {
      // tx.json, not JSON.stringify: a stringified value passed to ::jsonb is
      // stored as a JSON *string*, which the Go worker and the frontend cannot
      // read back as an array.
      const blocks = tx.json(marshalBlocks(message.blocks) as any);
      const attachments = tx.json(marshalAttachments(message.attachments) as any);
      // name: AppendMessage
      const rows = await this.translate("append message", () =>
        tx<MessageRow[]>`
          INSERT INTO messages (
              workspace_id, conversation_id, author_agent_id, author_user_id,
              blocks, attachments, task_id, status, finish_reason
          )
          VALUES (
              ${scope.workspaceId}, ${message.conversationId}, ${optionalId(message.authorAgentId)},
              ${optionalId(message.authorUserId)}, ${blocks}::jsonb, ${attachments}::jsonb,
              ${optionalId(message.taskId)}, ${message.status || MessageComplete}, ${message.finishReason}
          )
          RETURNING ${tx.unsafe(MESSAGE_COLUMNS)}
        `,
      );

      // name: TouchConversation
      await tx`
        UPDATE conversations
        SET updated_at = now()
        WHERE id = ${message.conversationId} AND workspace_id = ${scope.workspaceId}
      `;

      return toMessage(rows[0]);
    });
  }

  /** Replaces a message's body and status. */
  async update(scope: Scope, message: Message): Promise<Message> {
    return this.write(scope, async (tx) => {
      const blocks = tx.json(marshalBlocks(message.blocks) as any);
      const attachments = tx.json(marshalAttachments(message.attachments) as any);
      // name: UpdateMessage
      const rows = await tx<MessageRow[]>`
        UPDATE messages
        SET blocks = ${blocks}::jsonb,
            attachments = ${attachments}::jsonb,
            task_id = COALESCE(${optionalId(message.taskId)}, task_id),
            status = ${message.status || MessageComplete},
            finish_reason = ${message.finishReason}
        WHERE id = ${message.id} AND workspace_id = ${scope.workspaceId}
        RETURNING ${tx.unsafe(MESSAGE_COLUMNS)}
      `;
      if (rows.length === 0) {
        throw ErrMessageNotFound;
      }
      return toMessage(rows[0]);
    });
  }

  /** One message. */
  async getMessage(scope: Scope, id: string): Promise<Message> {
    return this.read(scope, async (tx) => {
      // name: GetMessage
      const rows = await tx<MessageRow[]>`
        SELECT ${tx.unsafe(MESSAGE_COLUMNS)}
        FROM messages
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) {
        throw ErrMessageNotFound;
      }
      return toMessage(rows[0]);
    });
  }

  /**
   * One page of a conversation, newest first.
   *
   * One extra row is what says whether another page exists, without a second
   * count query.
   */
  async history(scope: Scope, conversationId: string, cursor: Cursor): Promise<Page> {
    const limit = clampLimit(cursor.limit);
    const beforeAt = isNil(cursor.beforeId) ? null : new Date(cursor.beforeCreatedAtNanos / 1_000_000);
    const beforeId = isNil(cursor.beforeId) ? null : cursor.beforeId;

    const messages = await this.read(scope, async (tx) => {
      // name: ListMessagesDesc
      const rows = await tx<MessageRow[]>`
        SELECT ${tx.unsafe(MESSAGE_COLUMNS)}
        FROM messages
        WHERE workspace_id = ${scope.workspaceId}
          AND conversation_id = ${conversationId}
          AND (
              ${beforeAt}::timestamptz IS NULL
              OR (created_at, id) < (${beforeAt}::timestamptz, ${beforeId}::uuid)
          )
        ORDER BY created_at DESC, id DESC
        LIMIT ${limit + 1}
      `;
      return rows.map(toMessage);
    });

    const page: Page = {
      messages,
      nextBeforeCreatedAtNanos: 0,
      nextBeforeId: "",
      hasMore: false,
    };

    if (messages.length > limit) {
      page.hasMore = true;
      page.messages = messages.slice(0, limit);
    }
    const tail = page.messages[page.messages.length - 1];
    if (tail) {
      page.nextBeforeCreatedAtNanos = tail.createdAt.getTime() * 1_000_000;
      page.nextBeforeId = tail.id;
    }
    return page;
  }

  /** The newest messages of a conversation, oldest first. */
  async latestMessages(scope: Scope, conversationId: string, limit: number): Promise<Message[]> {
    return this.read(scope, async (tx) => {
      // name: ListMessagesAsc
      const rows = await tx<MessageRow[]>`
        SELECT ${tx.unsafe(MESSAGE_COLUMNS)} FROM (
            SELECT ${tx.unsafe(MESSAGE_COLUMNS)} FROM messages
            WHERE workspace_id = ${scope.workspaceId} AND conversation_id = ${conversationId}
            ORDER BY created_at DESC, id DESC
            LIMIT ${clampLimit(limit)}
        ) recent
        ORDER BY recent.created_at, recent.id
      `;
      return rows.map(toMessage);
    });
  }

  // ------------------------------------------------------------- attachments

  /**
   * Stores attachment bytes and their metadata.
   *
   * The row and the message's read model are written in one transaction: the
   * rows in `message_attachments` are the storage record, while the JSONB
   * column is what history and the event stream render.
   */
  async save(scope: Scope, attachment: Attachment, content: Uint8Array): Promise<Attachment> {
    return this.write(scope, async (tx) => {
      const stored = await this.createAttachment(tx, scope, attachment, content);

      // name: AppendMessageAttachment
      const rows = await tx<MessageRow[]>`
        UPDATE messages
        SET attachments = COALESCE(attachments, '[]'::jsonb) || jsonb_build_array(${tx.json(elementOf(stored) as any)})
        WHERE id = ${stored.messageId} AND workspace_id = ${scope.workspaceId}
        RETURNING ${tx.unsafe(MESSAGE_COLUMNS)}
      `;
      if (rows.length === 0) {
        throw ErrMessageNotFound;
      }
      return stored;
    });
  }

  /**
   * Opens the message an upload carries and stores the file in it, all in one
   * transaction.
   *
   * The order inside the transaction is the order the keys demand: the message
   * row first, because the attachment row needs its id, then the row that
   * records the bytes, then the element that makes the message render it.
   */
  async saveWithNewMessage(
    scope: Scope,
    message: Message,
    attachment: Attachment,
    content: Uint8Array,
  ): Promise<{ message: Message; attachment: Attachment }> {
    return this.write(scope, async (tx) => {
      const blocks = tx.json(marshalBlocks(message.blocks) as any);
      // name: AppendMessage
      const created = await this.translate("append message", () =>
        tx<MessageRow[]>`
          INSERT INTO messages (
              workspace_id, conversation_id, author_agent_id, author_user_id,
              blocks, attachments, task_id, status, finish_reason
          )
          VALUES (
              ${scope.workspaceId}, ${message.conversationId}, ${optionalId(message.authorAgentId)},
              ${optionalId(message.authorUserId)}, ${blocks}::jsonb, '[]'::jsonb,
              ${optionalId(message.taskId)}, ${message.status || MessageComplete}, ${message.finishReason}
          )
          RETURNING ${tx.unsafe(MESSAGE_COLUMNS)}
        `,
      );

      const stored = await this.createAttachment(
        tx,
        scope,
        { ...attachment, messageId: created[0].id },
        content,
      );

      // name: AppendMessageAttachment
      const filled = await tx<MessageRow[]>`
        UPDATE messages
        SET attachments = COALESCE(attachments, '[]'::jsonb) || jsonb_build_array(${tx.json(elementOf(stored) as any)})
        WHERE id = ${created[0].id} AND workspace_id = ${scope.workspaceId}
        RETURNING ${tx.unsafe(MESSAGE_COLUMNS)}
      `;

      // name: TouchConversation
      await tx`
        UPDATE conversations
        SET updated_at = now()
        WHERE id = ${message.conversationId} AND workspace_id = ${scope.workspaceId}
      `;

      return { message: toMessage(filled[0]), attachment: stored };
    });
  }

  /** One attachment's metadata. */
  async open(scope: Scope, id: string): Promise<Attachment> {
    return this.read(scope, async (tx) => {
      // name: GetAttachment
      const rows = await tx<AttachmentRow[]>`
        SELECT id, workspace_id, message_id, storage_key, filename, content_type, byte_size, checksum_sha256, created_at
        FROM message_attachments
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) {
        throw ErrAttachmentNotFound;
      }
      return toAttachment(rows[0]);
    });
  }

  /** The attachments of one message. */
  async listForMessage(scope: Scope, messageId: string): Promise<Attachment[]> {
    return this.read(scope, async (tx) => {
      // name: ListAttachmentsForMessage
      const rows = await tx<AttachmentRow[]>`
        SELECT id, workspace_id, message_id, storage_key, filename, content_type, byte_size, checksum_sha256, created_at
        FROM message_attachments
        WHERE workspace_id = ${scope.workspaceId} AND message_id = ${messageId}
        ORDER BY created_at
      `;
      return rows.map(toAttachment);
    });
  }

  /** The attachments past a moment, oldest first. */
  async listExpired(scope: Scope, olderThan: Date, limit: number): Promise<Attachment[]> {
    return this.read(scope, async (tx) => {
      // name: ListExpiredAttachments
      const rows = await tx<AttachmentRow[]>`
        SELECT id, workspace_id, message_id, storage_key, filename, content_type, byte_size, checksum_sha256, created_at
        FROM message_attachments
        WHERE workspace_id = ${scope.workspaceId} AND created_at < ${olderThan}
        ORDER BY created_at
        LIMIT ${limit <= 0 ? DefaultPruneLimit : limit}
      `;
      return rows.map(toAttachment);
    });
  }

  /**
   * Removes one attachment's row and the element it left in its message's read
   * model, in one transaction, and answers that message as it now reads.
   *
   * A row that is already gone is not an error: a retry after a partial failure
   * has to converge rather than report a failure that is really "this was
   * already done".
   */
  async delete(scope: Scope, id: string): Promise<Message> {
    return this.write(scope, async (tx) => {
      // name: DeleteAttachment
      const deleted = await tx<{ message_id: string }[]>`
        DELETE FROM message_attachments
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING message_id
      `;
      if (deleted.length === 0) {
        return zeroMessage();
      }

      // name: RemoveMessageAttachment
      const rows = await tx<MessageRow[]>`
        UPDATE messages
        SET attachments = COALESCE((
                SELECT jsonb_agg(element ORDER BY ordinality)
                FROM jsonb_array_elements(COALESCE(attachments, '[]'::jsonb)) WITH ORDINALITY AS element_at(element, ordinality)
                WHERE element_at.element->>'ID' <> ${id}::text
            ), '[]'::jsonb)
        WHERE id = ${deleted[0].message_id} AND workspace_id = ${scope.workspaceId}
        RETURNING ${tx.unsafe(MESSAGE_COLUMNS)}
      `;
      if (rows.length === 0) {
        // The message went with the file, so there is no read model left to fix
        // and nothing to publish.
        return zeroMessage();
      }
      return toMessage(rows[0]);
    });
  }

  /** Inserts one attachment row. */
  private async createAttachment(
    tx: Tx,
    scope: Scope,
    attachment: Attachment,
    content: Uint8Array,
  ): Promise<Attachment> {
    // name: CreateAttachment
    const rows = await this.translate("create attachment", () =>
      tx<AttachmentRow[]>`
        INSERT INTO message_attachments (
            workspace_id, message_id, storage_key, filename, content_type, byte_size, checksum_sha256
        )
        VALUES (
            ${scope.workspaceId}, ${attachment.messageId}, ${attachment.storageKey},
            ${attachment.filename}, ${attachment.contentType}, ${content.length},
            ${attachment.checksumSha256}
        )
        RETURNING id, workspace_id, message_id, storage_key, filename, content_type, byte_size, checksum_sha256, created_at
      `,
    );
    return toAttachment(rows[0]);
  }

  // ------------------------------------------------------------------ events

  /** Writes one activity event and returns it with its assigned id. */
  async appendEvent(event: Event): Promise<Event> {
    const payload = event.payload === undefined || event.payload === null ? {} : event.payload;

    return this.write({ userId: "", workspaceId: event.workspaceId }, async (tx) => {
      // name: AppendEvent
      const rows = await tx<EventRow[]>`
        INSERT INTO activity_events (
            workspace_id, type, actor_agent_id, actor_user_id,
            conversation_id, task_id, draft_id, payload
        )
        VALUES (
            ${event.workspaceId}, ${event.type}, ${optionalId(event.actorAgentId)},
            ${optionalId(event.actorUserId)}, ${optionalId(event.conversationId)},
            ${optionalId(event.taskId)}, ${optionalId(event.draftId)},
            ${tx.json(payload as any)}
        )
        RETURNING id, workspace_id, type, actor_agent_id, actor_user_id, conversation_id, task_id, draft_id, payload, created_at
      `;
      return toEvent(rows[0]);
    });
  }

  /** The events after an id, oldest first. */
  async sinceEvents(workspaceId: string, afterId: number, limit: number): Promise<Event[]> {
    return this.read({ userId: "", workspaceId }, async (tx) => {
      // name: EventsSince
      const rows = await tx<EventRow[]>`
        SELECT id, workspace_id, type, actor_agent_id, actor_user_id, conversation_id, task_id, draft_id, payload, created_at
        FROM activity_events
        WHERE workspace_id = ${workspaceId} AND id > ${afterId}
        ORDER BY id
        LIMIT ${clampLimit(limit)}
      `;
      return rows.map(toEvent);
    });
  }

  /** The newest events of a workspace, newest first. */
  async latestEvents(workspaceId: string, limit: number): Promise<Event[]> {
    return this.read({ userId: "", workspaceId }, async (tx) => {
      // name: LatestEvents
      const rows = await tx<EventRow[]>`
        SELECT id, workspace_id, type, actor_agent_id, actor_user_id, conversation_id, task_id, draft_id, payload, created_at
        FROM activity_events
        WHERE workspace_id = ${workspaceId}
        ORDER BY id DESC
        LIMIT ${clampLimit(limit)}
      `;
      return rows.map(toEvent);
    });
  }

  // ------------------------------------------------------------------ scoped

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead(
      { userId: isNil(scope.userId) ? null : scope.userId, workspaceId: scope.workspaceId },
      fn,
    );
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope(
      { userId: isNil(scope.userId) ? null : scope.userId, workspaceId: scope.workspaceId },
      fn,
    );
  }

  /**
   * Runs a write and translates the constraint failures the module answers for:
   * a foreign key means the conversation or the author is not in this workspace,
   * and a unique violation is a duplicate participant.
   */
  private async translate<T>(action: string, fn: () => Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (err) {
      // postgres.js reports a constraint failure as a PostgresError whose code
      // is the SQLSTATE. Narrowed rather than cast, so a thrown non-object is
      // not read for fields it does not have.
      let code = "";
      let constraint = "";
      if (typeof err === "object" && err !== null) {
        if ("code" in err && typeof err.code === "string") code = err.code;
        if ("constraint_name" in err && typeof err.constraint_name === "string") {
          constraint = err.constraint_name;
        }
      }
      if (code === "23503") {
        throw new WrappedError(ErrConversationNotFound, constraint);
      }
      if (code === "23505") {
        throw new WrappedError(ErrInvalidInput, `${constraint} already exists`);
      }
      throw new Error(`chat: ${action}: ${(err as Error).message}`);
    }
  }
}

// ------------------------------------------------------------- the queries

/**
 * Inserts both kinds of participant. A user id and an agent id can never be the
 * same row, so the two loops stay separate.
 */
async function addParticipants(
  tx: Tx,
  workspaceId: string,
  conversationId: string,
  agentIds: string[],
  userIds: string[],
): Promise<void> {
  for (const agentId of agentIds) {
    if (isNil(agentId)) {
      continue;
    }
    // name: AddAgentParticipant
    await tx`
      INSERT INTO conversation_participants (workspace_id, conversation_id, agent_id)
      VALUES (${workspaceId}, ${conversationId}, ${agentId})
      ON CONFLICT DO NOTHING
    `;
  }

  for (const userId of userIds) {
    if (isNil(userId)) {
      continue;
    }
    // name: AddUserParticipant
    await tx`
      INSERT INTO conversation_participants (workspace_id, conversation_id, user_id)
      VALUES (${workspaceId}, ${conversationId}, ${userId})
      ON CONFLICT DO NOTHING
    `;
  }
}

// ------------------------------------------------------------- conversions

function toConversation(row: ConversationRow): Conversation {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    kind: row.kind,
    title: row.title,
    participants: [],
    messageCount: Number(row.message_count),
    lastActivityAt: row.last_activity_at,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function toMessage(row: MessageRow): Message {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    conversationId: row.conversation_id,
    authorAgentId: row.author_agent_id ?? "",
    authorUserId: row.author_user_id ?? "",
    blocks: unmarshalBlocks(row.blocks),
    attachments: unmarshalAttachments(row.attachments),
    taskId: row.task_id ?? "",
    status: row.status,
    finishReason: row.finish_reason,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function toAttachment(row: AttachmentRow): Attachment {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    messageId: row.message_id,
    storageKey: row.storage_key,
    filename: row.filename,
    contentType: row.content_type,
    byteSize: Number(row.byte_size),
    checksumSha256: row.checksum_sha256,
    createdAt: row.created_at,
  };
}

function toEvent(row: EventRow): Event {
  return {
    id: Number(row.id),
    workspaceId: row.workspace_id,
    type: row.type,
    actorAgentId: row.actor_agent_id ?? "",
    actorUserId: row.actor_user_id ?? "",
    conversationId: row.conversation_id ?? "",
    taskId: row.task_id ?? "",
    draftId: row.draft_id ?? "",
    payload: row.payload,
    createdAt: row.created_at,
  };
}

/** The zero message a delete of an already-gone row answers with. */
function zeroMessage(): Message {
  return {
    id: NilUUID,
    workspaceId: "",
    conversationId: "",
    authorAgentId: "",
    authorUserId: "",
    blocks: [],
    attachments: [],
    taskId: "",
    status: "",
    finishReason: "",
    createdAt: new Date(0),
    updatedAt: new Date(0),
  };
}

/** One attachment as the element a message's read model carries. */
function elementOf(attachment: Attachment): AttachmentElement {
  return {
    ID: attachment.id,
    WorkspaceID: attachment.workspaceId,
    MessageID: attachment.messageId,
    StorageKey: attachment.storageKey,
    Filename: attachment.filename,
    ContentType: attachment.contentType,
    ByteSize: attachment.byteSize,
    ChecksumSHA256: attachment.checksumSha256,
    CreatedAt: attachment.createdAt.toISOString(),
  };
}

/**
 * Writes the read model of a message's attachments.
 *
 * The metadata is stored twice on purpose: the rows in `message_attachments`
 * are the storage record, while this column is the read model, so a page of
 * history renders its attachments without a query per message.
 */
function marshalAttachments(attachments: Attachment[]): AttachmentElement[] {
  return attachments.map(elementOf);
}

/** Reads the read model back. */
function unmarshalAttachments(raw: unknown): Attachment[] {
  if (raw === null || raw === undefined) {
    return [];
  }

  let elements: unknown[];
  if (typeof raw === "string") {
    try {
      elements = JSON.parse(raw) as unknown[];
    } catch (err) {
      throw new Error(`chat: decode attachments: ${(err as Error).message}`);
    }
  } else if (Array.isArray(raw)) {
    elements = raw;
  } else {
    throw new Error("chat: decode attachments: attachments column is not an array");
  }

  return elements.map((element) => {
    const value: Record<string, unknown> =
      typeof element === "object" && element !== null ? (element as Record<string, unknown>) : {};
    const text = (key: string): string => (typeof value[key] === "string" ? (value[key] as string) : "");
    return {
      id: text("ID"),
      workspaceId: text("WorkspaceID"),
      messageId: text("MessageID"),
      storageKey: text("StorageKey"),
      filename: text("Filename"),
      contentType: text("ContentType"),
      byteSize: typeof value.ByteSize === "number" ? value.ByteSize : 0,
      checksumSha256: text("ChecksumSHA256"),
      createdAt: text("CreatedAt") ? new Date(text("CreatedAt")) : new Date(0),
    };
  });
}

/** An id as a nullable parameter, since Postgres has no zero uuid. */
function optionalId(id: string): string | null {
  return isNil(id) ? null : id;
}

/** Clamps a page size to the module's bounds. */
function clampLimit(limit: number): number {
  if (limit <= 0) {
    return DefaultPageSize;
  }
  if (limit > MaxPageSize) {
    return MaxPageSize;
  }
  return limit;
}
