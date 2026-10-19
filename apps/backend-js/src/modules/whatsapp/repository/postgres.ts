/**
 * The WhatsApp data access with postgres.js.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a query forgets its workspace. The one exception is the
 * fleet directory, which is named as such where it is read: the gateway has to
 * discover which tenants have a number before it can open any workspace's
 * scope, and that table holds a routing pair and nothing else.
 *
 * Ported 1:1 from internal/modules/whatsapp/repository/postgres.go, links.go,
 * and the folded sqlcgen queries (repository/sqlcgen/*.go). Each query keeps its
 * name as a comment and its SQL text unchanged.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  ErrAlreadyLinked,
  ErrInvalidInput,
  ErrNotFound,
  ErrNotConfigured,
  NilUUID,
  scopeIsZero,
  wrap,
  type Inbound,
  type Media,
  type MessageFilter,
  type Number,
  type Outbox,
  type OutboxFilter,
  type Scope,
  type Store,
} from "../domain/whatsapp.ts";
import {
  optionalID,
  optionalText,
  toMessage,
  toNumber,
  toOutbox,
  type MessageRow,
  type NumberRow,
  type OutboxRow,
} from "./convert.ts";
import { markOutboxRetry, setNumberIntegration, setNumberPhone } from "./links.ts";

/** The ceiling of a list read, so one request cannot pull every message ever. */
const listLimit = 200;

/** Postgres' unique violation. */
const uniqueViolation = "23505";

/** Reads a driver error's constraint name, when it has one. */
function constraintOf(err: unknown): string {
  const candidate = err as { constraint_name?: string; constraint?: string } | null;
  return candidate?.constraint_name ?? candidate?.constraint ?? "";
}

/** Reads a driver error's SQLSTATE, when it has one. */
function sqlState(err: unknown): string {
  return (err as { code?: string } | null)?.code ?? "";
}

/**
 * Turns a driver error into the module's own. A statement that returned nothing
 * is not-found; anything else keeps its own words behind the operation name.
 */
function translate(err: unknown, operation: string): Error {
  if (err === null || err === undefined) {
    return new Error(`whatsapp: ${operation}`);
  }
  if (err instanceof Error && /no rows/i.test(err.message)) {
    return wrap(ErrNotFound, operation);
  }
  const detail = err instanceof Error ? err.message : String(err);
  return new Error(`whatsapp: ${operation}: ${detail}`);
}

/** Applies the module's list ceiling. */
function limitOr(value: number): number {
  if (value <= 0 || value > listLimit) {
    return listLimit;
  }
  return value;
}

export class PostgresRepository implements Store {
  constructor(private readonly db: Database) {}

  // ---------------------------------------------------------------- numbers

  /**
   * name: CreateWhatsAppNumber
   *
   * A phone number is unique across the whole deployment rather than per
   * workspace: the WhatsApp session belongs to the number, so a second tenant
   * cannot have it. That conflict arrives as a unique violation rather than as
   * a visible row — Row Level Security hides the other tenant's number — so it
   * is translated into the error that says what actually happened.
   */
  async create(scope: Scope, number: Number): Promise<Number> {
    return this.write(scope, async (tx) => {
      let rows: NumberRow[];
      try {
        rows = await tx<NumberRow[]>`
          INSERT INTO whatsapp_numbers (
              workspace_id, integration_id, phone, label, agent_id, status, tos_accepted_at, hourly_limit
          )
          SELECT
              ${scope.workspaceId},
              ${optionalID(number.integrationID)}::uuid,
              ${number.phone},
              ${number.label},
              ${number.agentID},
              ${number.status},
              ${number.tosAcceptedAt},
              ${number.hourlyLimit}
          WHERE EXISTS (
              SELECT 1 FROM agents
              WHERE agents.id = ${number.agentID}
                AND agents.workspace_id = ${scope.workspaceId}
          )
          RETURNING *
        `;
      } catch (err) {
        if (sqlState(err) === uniqueViolation && constraintOf(err) === "whatsapp_numbers_phone_key") {
          throw wrap(ErrAlreadyLinked, number.phone);
        }
        throw translate(err, "create number");
      }

      if (rows.length === 0) {
        // The insert is a SELECT, so a Bolu outside this workspace comes back
        // as no rows rather than as a constraint violation.
        throw wrap(ErrInvalidInput, "the bolu is not in this workspace");
      }
      return toNumber(rows[0]);
    });
  }

  /** name: GetWhatsAppNumber */
  async get(scope: Scope, id: string): Promise<Number> {
    return this.read(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        SELECT * FROM whatsapp_numbers
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "get number");
      return toNumber(rows[0]);
    });
  }

  /** name: GetWhatsAppNumberByPhone */
  async byPhone(scope: Scope, phone: string): Promise<Number> {
    return this.read(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        SELECT * FROM whatsapp_numbers
        WHERE phone = ${phone}
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "get number by phone");
      return toNumber(rows[0]);
    });
  }

  /**
   * name: GetWhatsAppNumberByIntegration
   *
   * A number that is not linked yet has no connection, so a call that names one
   * cannot match it: the answer is not-found rather than the row.
   */
  async byIntegration(scope: Scope, integrationID: string): Promise<Number> {
    return this.read(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        SELECT * FROM whatsapp_numbers
        WHERE integration_id = ${integrationID} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "get number by integration");
      return toNumber(rows[0]);
    });
  }

  /** name: ListWhatsAppNumbers */
  async list(scope: Scope): Promise<Number[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        SELECT * FROM whatsapp_numbers
        WHERE workspace_id = ${scope.workspaceId}
        ORDER BY created_at DESC
        LIMIT ${listLimit}
      `;
      return rows.map(toNumber);
    });
  }

  /** name: UpdateWhatsAppNumber */
  async update(scope: Scope, number: Number): Promise<Number> {
    return this.write(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        UPDATE whatsapp_numbers
        SET label = ${number.label},
            agent_id = ${optionalID(number.agentID)}::uuid,
            hourly_limit = ${number.hourlyLimit},
            updated_at = now()
        WHERE id = ${number.id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "update number");
      return toNumber(rows[0]);
    });
  }

  /**
   * name: SetWhatsAppNumberStatus
   *
   * One statement rather than a read followed by a write because two observers
   * can report at once, and the later write is the one that is true.
   */
  async setStatus(scope: Scope, id: string, status: string, reason: string): Promise<Number> {
    return this.write(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        UPDATE whatsapp_numbers
        SET status = ${status},
            status_reason = ${reason},
            connected_at = CASE WHEN ${status} = 'connected' THEN now() ELSE connected_at END,
            last_seen_at = CASE
                WHEN ${status} = 'connected' THEN GREATEST(COALESCE(last_seen_at, to_timestamp(0)), now())
                ELSE last_seen_at
            END,
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "set number status");
      return toNumber(rows[0]);
    });
  }

  /** name: TouchWhatsAppNumber — moves last-seen forward only. */
  async touch(scope: Scope, id: string, at: Date): Promise<void> {
    await this.write(scope, async (tx) => {
      const rows = await tx`
        UPDATE whatsapp_numbers
        SET last_seen_at = GREATEST(COALESCE(last_seen_at, to_timestamp(0)), ${at}),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.count === 0) throw wrap(ErrNotFound, "touch number");
    });
  }

  /** name: SetWhatsAppNumberPhone — the statement lives in links.ts. */
  async setPhone(scope: Scope, id: string, phone: string): Promise<void> {
    await this.write(scope, (tx) => setNumberPhone(tx, scope, id, phone));
  }

  /** name: AcceptWhatsAppTerms — idempotent; keeps the first moment. */
  async acceptTerms(scope: Scope, id: string): Promise<Number> {
    return this.write(scope, async (tx) => {
      const rows = await tx<NumberRow[]>`
        UPDATE whatsapp_numbers
        SET tos_accepted_at = COALESCE(tos_accepted_at, now()),
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "accept terms");
      return toNumber(rows[0]);
    });
  }

  /** name: DeleteWhatsAppNumber */
  async delete(scope: Scope, id: string): Promise<void> {
    await this.write(scope, async (tx) => {
      const rows = await tx`
        DELETE FROM whatsapp_numbers
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.count === 0) throw wrap(ErrNotFound, "delete number");
    });
  }

  // -------------------------------------------------------------- directory

  /** name: PublishWhatsAppDirectory */
  async publish(scope: Scope, numberID: string, enabled: boolean): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        INSERT INTO whatsapp_directory (number_id, workspace_id, enabled)
        VALUES (${numberID}, ${scope.workspaceId}, ${enabled})
        ON CONFLICT (number_id) DO UPDATE
        SET workspace_id = EXCLUDED.workspace_id,
            enabled = EXCLUDED.enabled,
            updated_at = now()
      `;
    });
  }

  // --------------------------------------------------------------- sessions

  /**
   * name: ForgetWhatsAppSession
   *
   * The product's own half of the session table: the gateway reads and writes
   * the device blob, and this ends one.
   */
  async forgetSession(scope: Scope, numberID: string): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        DELETE FROM whatsapp_sessions
        WHERE number_id = ${numberID} AND workspace_id = ${scope.workspaceId}
      `;
    });
  }

  // ---------------------------------------------------------------- inbound

  /**
   * name: RecordWhatsAppMessage
   *
   * A redelivery collides on the dedupe key and returns no rows, which is how
   * the caller learns the message was already recorded.
   */
  async record(scope: Scope, message: Inbound): Promise<{ message: Inbound; created: boolean }> {
    return this.write(scope, async (tx) => {
      const rows = await tx<MessageRow[]>`
        INSERT INTO whatsapp_messages (
            workspace_id, number_id, wa_message_id, chat_jid, sender_jid, sender_name,
            is_group, is_from_me, kind, text, media_key, media_kind, media_size,
            media_status, media_error, received_at
        ) VALUES (
            ${scope.workspaceId}, ${message.numberID}, ${message.waMessageID},
            ${message.chatJID}, ${message.senderJID}, ${message.senderName},
            ${message.isGroup}, ${message.isFromMe}, ${message.kind}, ${message.text},
            ${message.media.key}, ${message.media.kind}, ${message.media.size},
            ${message.media.status}, ${message.media.error}, ${message.receivedAt}
        )
        ON CONFLICT (workspace_id, wa_message_id) DO NOTHING
        RETURNING *
      `;

      if (rows.length > 0) {
        return { message: toMessage(rows[0]), created: true };
      }

      // The message is already recorded, which is what a redelivery looks like.
      // The stored row is read back so the caller answers with the id it has.
      const existing = await tx<MessageRow[]>`
        SELECT * FROM whatsapp_messages
        WHERE workspace_id = ${scope.workspaceId} AND wa_message_id = ${message.waMessageID}
      `;
      if (existing.length === 0) throw wrap(ErrNotFound, "get message by wa id");
      return { message: toMessage(existing[0]), created: false };
    });
  }

  /** name: GetWhatsAppMessage */
  async messageByID(scope: Scope, id: string): Promise<Inbound> {
    return this.read(scope, async (tx) => {
      const rows = await tx<MessageRow[]>`
        SELECT * FROM whatsapp_messages
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "get message");
      return toMessage(rows[0]);
    });
  }

  /** name: ListWhatsAppMessages */
  async messages(scope: Scope, filter: MessageFilter): Promise<Inbound[]> {
    return this.read(scope, async (tx) => {
      const numberID = optionalID(filter.numberID);
      const chatJID = optionalText(filter.chatJID);
      const rows = await tx<MessageRow[]>`
        SELECT * FROM whatsapp_messages
        WHERE workspace_id = ${scope.workspaceId}
          AND (${numberID}::uuid IS NULL OR number_id = ${numberID}::uuid)
          AND (${chatJID}::text IS NULL OR chat_jid = ${chatJID}::text)
        ORDER BY received_at DESC, id DESC
        LIMIT ${limitOr(filter.limit)}
      `;
      return rows.map(toMessage);
    });
  }

  /** name: GetWhatsAppMessageByWaID */
  async byWaID(scope: Scope, waMessageID: string): Promise<Inbound> {
    return this.read(scope, async (tx) => {
      const rows = await tx<MessageRow[]>`
        SELECT * FROM whatsapp_messages
        WHERE workspace_id = ${scope.workspaceId} AND wa_message_id = ${waMessageID}
      `;
      if (rows.length === 0) throw wrap(ErrNotFound, "get message");
      return toMessage(rows[0]);
    });
  }

  /** name: ListWhatsAppMediaPending */
  async pendingMedia(scope: Scope, limit: number): Promise<Inbound[]> {
    return this.read(scope, async (tx) => {
      const rows = await tx<MessageRow[]>`
        SELECT * FROM whatsapp_messages
        WHERE workspace_id = ${scope.workspaceId} AND media_status = 'pending'
        ORDER BY received_at
        LIMIT ${limitOr(limit)}
      `;
      return rows.map(toMessage);
    });
  }

  /** name: SetWhatsAppMessageMedia */
  async setMedia(scope: Scope, id: string, media: Media): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE whatsapp_messages
        SET media_key = ${media.key},
            media_kind = ${media.kind},
            media_size = ${media.size},
            media_status = ${media.status},
            media_error = ${media.error}
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
    });
  }

  /**
   * name: AttachWhatsAppMessageTask
   *
   * A message that is not there — or is another workspace's — is refused rather
   * than reported as linked.
   */
  async attachTask(scope: Scope, id: string, taskID: string): Promise<void> {
    await this.write(scope, async (tx) => {
      const rows = await tx`
        UPDATE whatsapp_messages
        SET task_id = ${optionalID(taskID)}::uuid
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
      if (rows.count === 0) throw wrap(ErrNotFound, "attach message task");
    });
  }

  // ----------------------------------------------------------------- outbox

  /** name: EnqueueWhatsAppMessage */
  async enqueue(scope: Scope, item: Outbox): Promise<Outbox> {
    const notBefore = item.notBefore.getTime() === 0 ? new Date() : item.notBefore;

    return this.write(scope, async (tx) => {
      const rows = await tx<OutboxRow[]>`
        INSERT INTO whatsapp_outbox (
            workspace_id, number_id, to_jid, kind, text, media_key, draft_id, not_before
        ) VALUES (
            ${scope.workspaceId}, ${item.numberID}, ${item.toJID},
            ${item.kind}, ${item.text}, ${item.mediaKey},
            ${optionalID(item.draftID)}::uuid, ${notBefore}
        )
        RETURNING *
      `;
      if (rows.length === 0) {
        throw wrap(ErrInvalidInput, "the number is not in this workspace");
      }
      return toOutbox(rows[0]);
    });
  }

  /**
   * name: ClaimWhatsAppOutbox
   *
   * The read and the claim are one statement on purpose: two workers of one
   * fleet cannot both send it, and SKIP LOCKED means a second reader skips rows
   * a first one is claiming rather than waiting for them.
   */
  async due(scope: Scope, numberID: string, limit: number): Promise<Outbox[]> {
    return this.write(scope, async (tx) => {
      const rows = await tx<OutboxRow[]>`
        UPDATE whatsapp_outbox SET status = 'sending', claimed_at = now(), attempts = attempts + 1
        WHERE id IN (
            SELECT pending.id FROM whatsapp_outbox AS pending
            WHERE pending.workspace_id = ${scope.workspaceId}
              AND pending.number_id = ${numberID}
              AND pending.status = 'queued'
              AND pending.not_before <= now()
            ORDER BY pending.not_before, pending.id
            LIMIT ${limitOr(limit)}
            FOR UPDATE SKIP LOCKED
        )
        RETURNING *
      `;
      return rows.map(toOutbox);
    });
  }

  /** name: MarkWhatsAppOutboxSent */
  async markSent(scope: Scope, id: string, waMessageID: string, at: Date): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE whatsapp_outbox
        SET status = 'sent', wa_message_id = ${waMessageID}, sent_at = ${at},
            last_error = ''
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
    });
  }

  /**
   * name: MarkWhatsAppOutboxHeld
   *
   * Held rather than failed: the ceiling is a pace, so the message keeps its
   * place in the queue and its attempt is given back.
   */
  async markHeld(scope: Scope, id: string, reason: string, at: Date): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE whatsapp_outbox
        SET status = 'queued', not_before = ${at}, last_error = ${reason},
            attempts = GREATEST(attempts - 1, 0)
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
    });
  }

  /**
   * name: MarkWhatsAppOutboxRetry
   *
   * The attempt count is kept, unlike a hold: a transient failure that keeps
   * happening is eventually parked, and a pace is not a failure.
   */
  async markRetry(scope: Scope, id: string, reason: string, at: Date): Promise<void> {
    await this.write(scope, (tx) => markOutboxRetry(tx, scope, id, reason, at));
  }

  /** name: MarkWhatsAppOutboxFailed */
  async markFailed(scope: Scope, id: string, reason: string, attempts: number): Promise<void> {
    await this.write(scope, async (tx) => {
      await tx`
        UPDATE whatsapp_outbox
        SET status = 'failed', last_error = ${reason}, attempts = ${attempts}
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
      `;
    });
  }

  /** name: CountWhatsAppOutboxSentSince — counted from what actually left. */
  async sentSince(scope: Scope, numberID: string, since: Date): Promise<number> {
    return this.read(scope, async (tx) => {
      const rows = await tx<{ count: number }[]>`
        SELECT count(*) FROM whatsapp_outbox
        WHERE number_id = ${numberID} AND workspace_id = ${scope.workspaceId}
          AND status = 'sent' AND sent_at >= ${since}
      `;
      return Number(rows[0]?.count ?? 0);
    });
  }

  /** name: ListWhatsAppOutbox */
  async outbox(scope: Scope, filter: OutboxFilter): Promise<Outbox[]> {
    return this.read(scope, async (tx) => {
      const numberID = optionalID(filter.numberID);
      const status = optionalText(filter.status);
      const rows = await tx<OutboxRow[]>`
        SELECT * FROM whatsapp_outbox
        WHERE workspace_id = ${scope.workspaceId}
          AND (${numberID}::uuid IS NULL OR number_id = ${numberID}::uuid)
          AND (${status}::text IS NULL OR status = ${status}::text)
        ORDER BY created_at DESC, id DESC
        LIMIT ${limitOr(filter.limit)}
      `;
      return rows.map(toOutbox);
    });
  }

  /**
   * name: ReclaimWhatsAppOutbox
   *
   * A gateway killed mid-send leaves messages in `sending`. They go back to the
   * queue: the send either happened or it did not, and a duplicate is worse than
   * a delay, so the attempt is spent and the message waits.
   */
  async reclaim(scope: Scope, olderThan: Date): Promise<number> {
    return this.write(scope, async (tx) => {
      const rows = await tx`
        UPDATE whatsapp_outbox
        SET status = 'queued', not_before = now(), claimed_at = NULL,
            last_error = 'gateway terputus saat mengirim'
        WHERE workspace_id = ${scope.workspaceId}
          AND status = 'sending'
          AND claimed_at < ${olderThan}
      `;
      return rows.count;
    });
  }

  // ------------------------------------------------------------- link writes

  /** name: SetWhatsAppNumberIntegration — the statement lives in links.ts. */
  async setIntegration(scope: Scope, id: string, integrationID: string): Promise<void> {
    await this.write(scope, (tx) => setNumberIntegration(tx, scope, id, integrationID));
  }

  // ---------------------------------------------------------------- helpers

  /** Runs `fn` in a read-only transaction scoped to the tenant. */
  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (!this.db) throw wrap(ErrNotConfigured, "database is not configured");
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return this.db.inScopeRead(
      { userId: scope.userId ?? null, workspaceId: scope.workspaceId },
      fn,
    );
  }

  /** Runs `fn` in a transaction scoped to the tenant. */
  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (!this.db) throw wrap(ErrNotConfigured, "database is not configured");
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return this.db.inScope(
      { userId: scope.userId ?? null, workspaceId: scope.workspaceId },
      fn,
    );
  }
}

/** Re-exported so a caller that only wants the id sentinel does not import two files. */
export { NilUUID };
