/**
 * The approval data access: drafts, their revision trail, the policy screen,
 * and the audit trail.
 *
 * Every statement runs inside the caller's tenant scope, so Row Level Security
 * filters it even when a query forgets its workspace filter. The writes are
 * guarded by status in the statement itself: a second decision on a draft
 * changes nothing, which is what makes a duplicate signal harmless.
 *
 * Ported 1:1 from internal/modules/approval/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { JSONValue } from "postgres";
import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  DefaultListLimit,
  ErrAlreadyDecided,
  ErrDraftNotFound,
  ErrInvalidInput,
  ErrNotApproved,
  ErrPolicyNotFound,
  MaxListLimit,
  NilUUID,
  StatusRevise,
  isNil,
  isZeroScope,
  normaliseFilter,
  wrap,
  type AuditEntry,
  type AuditFilter,
  type AuditRecord,
  type Decision,
  type Draft,
  type Filter,
  type NewDraft,
  type Policy,
  type Repository as RepositoryPort,
  type Revision,
  type Scope,
} from "../domain/approval.ts";

/** One row of `drafts`. */
interface DraftRow {
  id: string;
  workspace_id: string;
  task_id: string;
  agent_id: string;
  action_kind: string;
  payload: unknown;
  status: string;
  revision: number;
  decided_by: string | null;
  decided_at: Date | null;
  decision_note: string;
  sent_at: Date | null;
  idempotency_key: string;
  created_at: Date;
  updated_at: Date;
  not_before: Date | null;
  blocked_reason: string;
  title: string;
  summary: string;
  card_message_id: string | null;
}

/** One row of `drafts` joined to the task that owns the thread. */
interface DraftWithConversationRow extends DraftRow {
  conversation_id: string | null;
}

/** One row of `draft_revisions`. */
interface DraftRevisionRow {
  id: string;
  workspace_id: string;
  draft_id: string;
  revision: number;
  note: string;
  payload: unknown;
  decided_by: string | null;
  created_at: Date;
}

/** One row of `approval_policies`. */
interface ApprovalPolicyRow {
  id: string;
  workspace_id: string;
  action_kind: string;
  mode: string;
  max_amount: number | null;
  updated_by: string | null;
  created_at: Date;
  updated_at: Date;
}

/** One row of `audit_logs`. */
interface AuditLogRow {
  id: number;
  workspace_id: string;
  actor_user_id: string | null;
  actor_agent_id: string | null;
  action: string;
  object_type: string;
  object_id: string;
  metadata: unknown;
  created_at: Date;
}

/** Postgres-backed implementation of the approval ports. */
export class Repository implements RepositoryPort {
  constructor(private readonly db: Database) {}

  // ----------------------------------------------------------------- drafts

  /**
   * Stores a proposal, returning the stored draft.
   *
   * The statement carries an ON CONFLICT on the idempotency key, so a retried
   * activity returns the draft it already opened rather than a second one for
   * the same action.
   */
  async createDraft(scope: Scope, draft: NewDraft): Promise<Draft> {
    return this.write(scope, async (tx) => {
      // name: CreateDraft
      const rows = await tx<DraftRow[]>`
        INSERT INTO drafts (
            workspace_id, task_id, agent_id, action_kind, payload,
            status, title, summary, idempotency_key
        )
        SELECT
            ${scope.workspaceId},
            t.id,
            t.agent_id,
            ${draft.actionKind},
            ${this.db.sql.json((draft.payload ?? {}) as JSONValue)},
            'pending',
            ${draft.title},
            ${draft.summary},
            ${draft.idempotencyKey}
        FROM tasks t
        WHERE t.id = ${draft.taskId}
          AND t.workspace_id = ${scope.workspaceId}
          AND t.agent_id = ${draft.agentId}
        ON CONFLICT (workspace_id, idempotency_key) DO UPDATE
            SET updated_at = drafts.updated_at
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        // The insert is a SELECT, so a task or an agent outside this workspace
        // comes back as no rows rather than as a constraint violation.
        throw wrap(ErrInvalidInput, "the task or the Bolu is not in this workspace");
      }
      return toDraft(row, draft.conversationId);
    });
  }

  /** Returns one draft. */
  async getDraft(scope: Scope, id: string): Promise<Draft> {
    return this.read(scope, async (tx) => {
      // name: GetDraft
      const rows = await tx<DraftWithConversationRow[]>`
        SELECT d.id, d.workspace_id, d.task_id, d.agent_id, d.action_kind, d.payload,
               d.status, d.revision, d.decided_by, d.decided_at, d.decision_note, d.sent_at,
               d.idempotency_key, d.created_at, d.updated_at, d.not_before, d.blocked_reason,
               d.title, d.summary, d.card_message_id, t.conversation_id AS conversation_id
        FROM drafts d
        JOIN tasks t ON t.id = d.task_id
        WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}
      `;
      const row = rows[0];
      if (!row) {
        throw ErrDraftNotFound;
      }
      return toDraft(row, row.conversation_id ?? NilUUID);
    });
  }

  /** Returns the workspace's drafts, newest first. */
  async listDrafts(scope: Scope, input: Filter): Promise<Draft[]> {
    const filter = normaliseFilter(input);
    return this.read(scope, async (tx) => {
      // name: ListDrafts
      const rows = await tx<DraftWithConversationRow[]>`
        SELECT d.id, d.workspace_id, d.task_id, d.agent_id, d.action_kind, d.payload,
               d.status, d.revision, d.decided_by, d.decided_at, d.decision_note, d.sent_at,
               d.idempotency_key, d.created_at, d.updated_at, d.not_before, d.blocked_reason,
               d.title, d.summary, d.card_message_id, t.conversation_id AS conversation_id
        FROM drafts d
        JOIN tasks t ON t.id = d.task_id
        WHERE d.workspace_id = ${scope.workspaceId}
          AND (${optionalText(filter.status)}::text IS NULL OR d.status = ${optionalText(filter.status)}::text)
          AND (${optionalId(filter.agentId)}::uuid IS NULL OR d.agent_id = ${optionalId(filter.agentId)}::uuid)
          AND (${optionalText(filter.actionKind)}::text IS NULL OR d.action_kind = ${optionalText(filter.actionKind)}::text)
          AND (${optionalId(filter.taskId)}::uuid IS NULL OR d.task_id = ${optionalId(filter.taskId)}::uuid)
        ORDER BY d.created_at DESC, d.id DESC
        LIMIT ${filter.limit}
      `;
      return rows.map((row) => toDraft(row, row.conversation_id ?? NilUUID));
    });
  }

  /**
   * Records a person's answer.
   *
   * The statement only matches a draft that is still pending, so a second
   * decision changes nothing: the caller gets ErrAlreadyDecided and the stored
   * answer stands.
   */
  async decide(scope: Scope, decision: Decision): Promise<Draft> {
    const bump = decision.status === StatusRevise ? 1 : 0;
    return this.write(scope, async (tx) => {
      // name: DecideDraft
      const rows = await tx<DraftRow[]>`
        UPDATE drafts
        SET status = ${decision.status},
            decided_by = ${optionalId(decision.decidedBy)},
            decided_at = now(),
            decision_note = ${decision.note},
            revision = revision + ${bump}::int,
            updated_at = now()
        WHERE id = ${decision.draftId}
          AND workspace_id = ${scope.workspaceId}
          AND status = 'pending'
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        throw await this.distinguish(tx, scope, decision.draftId, ErrAlreadyDecided);
      }
      return toDraft(row, NilUUID);
    });
  }

  /** Records that the action left. */
  async markSent(scope: Scope, id: string): Promise<Draft> {
    return this.write(scope, async (tx) => {
      // name: MarkDraftSent
      const rows = await tx<DraftRow[]>`
        UPDATE drafts
        SET status = 'sent',
            sent_at = now(),
            not_before = NULL,
            blocked_reason = '',
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND status = 'approved'
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        // The draft is not approved, or already sent: either way there is
        // nothing to send — unless it is not this workspace's draft at all.
        throw await this.distinguish(tx, scope, id, ErrNotApproved);
      }
      return toDraft(row, NilUUID);
    });
  }

  /** Records that an approved action is waiting for the working day. */
  async hold(scope: Scope, id: string, notBefore: Date, reason: string): Promise<Draft> {
    return this.write(scope, async (tx) => {
      // name: HoldDraft
      const rows = await tx<DraftRow[]>`
        UPDATE drafts
        SET not_before = ${notBefore},
            blocked_reason = ${reason},
            updated_at = now()
        WHERE id = ${id}
          AND workspace_id = ${scope.workspaceId}
          AND status = 'approved'
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        throw await this.distinguish(tx, scope, id, ErrNotApproved);
      }
      return toDraft(row, NilUUID);
    });
  }

  /** Records the message carrying the card. */
  async setCardMessage(scope: Scope, id: string, messageId: string): Promise<Draft> {
    return this.write(scope, async (tx) => {
      // name: SetDraftCard
      const rows = await tx<DraftRow[]>`
        UPDATE drafts
        SET card_message_id = ${optionalId(messageId)}, updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId}
        RETURNING *
      `;
      const row = rows[0];
      if (!row) {
        throw ErrDraftNotFound;
      }
      return toDraft(row, NilUUID);
    });
  }

  // ---------------------------------------------------------- revision trail

  /** Adds one turn to the trail. */
  async appendRevision(scope: Scope, revision: Revision): Promise<Revision> {
    return this.write(scope, async (tx) => {
      // name: AppendDraftRevision
      const rows = await this.translateWrite("append revision", () =>
        tx<DraftRevisionRow[]>`
          INSERT INTO draft_revisions (workspace_id, draft_id, revision, note, payload, decided_by)
          VALUES (${scope.workspaceId}, ${revision.draftId}, ${revision.revision},
                  ${revision.note}, ${this.db.sql.json((revision.payload ?? {}) as JSONValue)}, ${optionalId(revision.decidedBy)})
          RETURNING *
        `,
      );
      return toRevision(rows[0]);
    });
  }

  /** Returns a draft's trail, oldest first. */
  async revisions(scope: Scope, draftId: string): Promise<Revision[]> {
    return this.read(scope, async (tx) => {
      // name: ListDraftRevisions
      const rows = await tx<DraftRevisionRow[]>`
        SELECT * FROM draft_revisions
        WHERE workspace_id = ${scope.workspaceId} AND draft_id = ${draftId}
        ORDER BY revision, created_at
      `;
      return rows.map(toRevision);
    });
  }

  // -------------------------------------------------------------- policies

  /** Returns the workspace's stored policies. */
  async policies(scope: Scope): Promise<Policy[]> {
    return this.read(scope, async (tx) => {
      // name: ListPolicies
      const rows = await tx<ApprovalPolicyRow[]>`
        SELECT * FROM approval_policies
        WHERE workspace_id = ${scope.workspaceId}
        ORDER BY action_kind
      `;
      return rows.map(toPolicy);
    });
  }

  /** Returns one action kind's policy, or ErrPolicyNotFound. */
  async policy(scope: Scope, actionKind: string): Promise<Policy> {
    return this.read(scope, async (tx) => {
      // name: GetPolicy
      const rows = await tx<ApprovalPolicyRow[]>`
        SELECT * FROM approval_policies
        WHERE workspace_id = ${scope.workspaceId} AND action_kind = ${actionKind}
      `;
      const row = rows[0];
      if (!row) {
        throw ErrPolicyNotFound;
      }
      return toPolicy(row);
    });
  }

  /** Stores a decision about one action kind. */
  async setPolicy(scope: Scope, policy: Policy): Promise<Policy> {
    return this.write(scope, async (tx) => {
      // name: UpsertPolicy
      const rows = await this.translateWrite("set policy", () =>
        tx<ApprovalPolicyRow[]>`
          INSERT INTO approval_policies (workspace_id, action_kind, mode, max_amount, updated_by)
          VALUES (${scope.workspaceId}, ${policy.actionKind}, ${policy.mode},
                  ${policy.maxAmount}, ${optionalId(policy.updatedBy)})
          ON CONFLICT (workspace_id, action_kind) DO UPDATE
              SET mode = EXCLUDED.mode,
                  max_amount = EXCLUDED.max_amount,
                  updated_by = EXCLUDED.updated_by,
                  updated_at = now()
          RETURNING *
        `,
      );
      return toPolicy(rows[0]);
    });
  }

  /**
   * Gives a workspace the conservative rows it starts with.
   *
   * It is called inside the onboarding transaction, so a workspace never exists
   * without a policy for every action that reaches outside it. An existing
   * decision is left alone.
   */
  async seedPolicies(tx: Tx, workspaceId: string): Promise<void> {
    // name: SeedPolicies
    await tx`
      INSERT INTO approval_policies (workspace_id, action_kind, mode)
      SELECT ${workspaceId}, tc.name, 'always_draft'
      FROM tool_catalog tc
      WHERE tc.label = 'write_external'
      ON CONFLICT (workspace_id, action_kind) DO NOTHING
    `;
  }

  /** Lists the action kinds that reach outside Bolu. */
  async actionKinds(): Promise<string[]> {
    // tool_catalog is not tenant-scoped: it is the catalogue the product ships,
    // not something a workspace owns. The Go port still opens a scoped
    // transaction, with a random user id, so the read runs under the same pool.
    return this.db.inScopeRead({ userId: crypto.randomUUID() }, async (tx) => {
      // name: ListExternalActionKinds
      const rows = await tx<{ name: string }[]>`
        SELECT name FROM tool_catalog
        WHERE label = 'write_external'
        ORDER BY name
      `;
      return rows.map((row) => row.name);
    });
  }

  // ----------------------------------------------------------------- audit

  /** Appends one entry to the audit trail. */
  async recordAudit(scope: Scope, entry: AuditEntry): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: InsertAuditEntry
      await this.translateWrite("record audit", () =>
        tx`
          INSERT INTO audit_logs (
              workspace_id, actor_user_id, actor_agent_id, action, object_type, object_id, metadata
          )
          VALUES (${scope.workspaceId}, ${optionalId(entry.actorUserId)},
                  ${optionalId(entry.actorAgentId)}, ${entry.action}, ${entry.objectType},
                  ${entry.objectId}, ${this.db.sql.json((entry.metadata ?? {}) as JSONValue)})
        `,
      );
    });
  }

  /**
   * Returns the entries the filter names, newest first.
   *
   * Every clause is optional and they combine, so one statement answers the
   * three questions an incident asks. A value ending in a dot in `action` is a
   * prefix, which is what makes `grant.` read every grant change without a
   * caller enumerating the verbs.
   */
  async auditTrail(scope: Scope, filter: AuditFilter): Promise<AuditRecord[]> {
    // A limit nobody should be able to ask for is replaced rather than sent on.
    let limit = filter.limit;
    if (limit <= 0) {
      limit = DefaultListLimit;
    } else if (limit > MaxListLimit) {
      limit = MaxListLimit;
    }

    return this.read(scope, async (tx) => {
      const action = optionalText(filter.action);
      const objectType = optionalText(filter.objectType);
      const objectId = optionalText(filter.objectId);
      const actorAgentId = optionalId(filter.actorAgentId);
      const actorUserId = optionalId(filter.actorUserId);
      const since = filter.since;
      const until = filter.until;

      // name: ListAuditEntries
      const rows = await tx<AuditLogRow[]>`
        SELECT * FROM audit_logs
        WHERE workspace_id = ${scope.workspaceId}
          AND (${action}::text IS NULL
               OR (right(${action}::text, 1) = '.'
                   AND action LIKE ${action}::text || '%')
               OR action = ${action}::text)
          AND (${objectType}::text IS NULL OR object_type = ${objectType}::text)
          AND (${objectId}::text IS NULL OR object_id = ${objectId}::text)
          AND (${actorAgentId}::uuid IS NULL OR actor_agent_id = ${actorAgentId}::uuid)
          AND (${actorUserId}::uuid IS NULL OR actor_user_id = ${actorUserId}::uuid)
          AND (${since}::timestamptz IS NULL OR created_at >= ${since}::timestamptz)
          AND (${until}::timestamptz IS NULL OR created_at < ${until}::timestamptz)
        ORDER BY created_at DESC, id DESC
        LIMIT ${limit}
      `;
      return rows.map(toAuditRecord);
    });
  }

  // ------------------------------------------------------------- internals

  /**
   * Tells apart the two reasons a status-guarded write can match nothing.
   *
   * The statement only touches a row in the caller's workspace and in the
   * expected status, so no rows means either that the draft is not in that
   * state or that it is not in that workspace at all. Row Level Security makes
   * the second case invisible, and reporting it as "already decided" would tell
   * a caller that somebody else's draft is theirs.
   */
  private async distinguish(tx: Tx, scope: Scope, id: string, otherwise: Error): Promise<Error> {
    const rows = await tx<{ id: string }[]>`
      SELECT d.id FROM drafts d
      JOIN tasks t ON t.id = d.task_id
      WHERE d.id = ${id} AND d.workspace_id = ${scope.workspaceId}
    `;
    return rows[0] ? otherwise : ErrDraftNotFound;
  }

  /** Turns the constraint violations the runtime can hit into the module's errors. */
  private async translateWrite<T>(action: string, fn: () => PromiseLike<T>): Promise<T> {
    try {
      return await fn();
    } catch (err) {
      const code = (err as { code?: string } | null)?.code;
      if (code === "23503") {
        // A foreign key failure means the row referenced is not in this
        // workspace, which RLS also refuses.
        throw wrap(ErrInvalidInput, "a referenced row is not in this workspace", { cause: err });
      }
      if (code === "23514") {
        // A check constraint: an unknown mode, or a hold without a reason.
        throw wrap(ErrInvalidInput, "a value is outside the allowed set", { cause: err });
      }
      throw new Error(`approval: ${action}: ${err instanceof Error ? err.message : String(err)}`, {
        cause: err,
      });
    }
  }

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
    return this.db.inScopeRead({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    if (isZeroScope(scope)) {
      throw wrap(ErrInvalidInput, "workspace is required");
    }
    return this.db.inScope({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }
}

// -------------------------------------------------------------- conversions

/**
 * Renders a stored row.
 *
 * The conversation is passed in rather than read from the row: a draft reaches
 * its thread through the task that produced it, and the two reads that need it
 * join the task while a write already knows it.
 */
function toDraft(row: DraftRow, conversationId: string): Draft {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    taskId: row.task_id,
    agentId: row.agent_id,
    conversationId,
    actionKind: row.action_kind,
    title: row.title,
    payload: row.payload,
    status: row.status,
    revision: row.revision,
    decidedBy: row.decided_by ?? NilUUID,
    decidedAt: row.decided_at,
    decisionNote: row.decision_note,
    notBefore: row.not_before,
    blockedReason: row.blocked_reason,
    summary: row.summary,
    sentAt: row.sent_at,
    cardMessageId: row.card_message_id ?? NilUUID,
    idempotencyKey: row.idempotency_key,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function toRevision(row: DraftRevisionRow): Revision {
  return {
    id: row.id,
    draftId: row.draft_id,
    revision: row.revision,
    note: row.note,
    payload: row.payload,
    decidedBy: row.decided_by ?? NilUUID,
    createdAt: row.created_at,
  };
}

function toPolicy(row: ApprovalPolicyRow): Policy {
  return {
    actionKind: row.action_kind,
    mode: row.mode,
    maxAmount: row.max_amount,
    updatedBy: row.updated_by ?? NilUUID,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function toAuditRecord(row: AuditLogRow): AuditRecord {
  const metadata =
    row.metadata !== null && typeof row.metadata === "object" && !Array.isArray(row.metadata)
      ? (row.metadata as Record<string, unknown>)
      : {};
  return {
    id: row.id,
    actorUserId: row.actor_user_id ?? NilUUID,
    actorAgentId: row.actor_agent_id ?? NilUUID,
    action: row.action,
    objectType: row.object_type,
    objectId: row.object_id,
    metadata,
    createdAt: row.created_at,
  };
}

/** Renders an id for the driver, or null when the filter does not narrow by it. */
function optionalId(id: string | null | undefined): string | null {
  return isNil(id) ? null : (id as string);
}

/** Renders a text filter, or null when it does not narrow. */
function optionalText(value: string | null | undefined): string | null {
  return value ? value : null;
}

