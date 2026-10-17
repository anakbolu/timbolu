/**
 * The erasure half of the retention module: removing a workspace's data on
 * request.
 *
 * Retention is a clock and erasure is a request, but they are the same policy
 * seen from two sides — how long this deployment keeps a customer's data, and
 * what it does when the customer asks for it back. Putting them in one module
 * is what keeps a single answer to "what do we hold about a workspace": the
 * same code that knows a step's payload has a window knows the attachments live
 * in two object-storage prefixes, and a second module would be a second list
 * that drifts.
 *
 * Three things about the design are worth stating rather than discovering
 * later:
 *
 * - **Everything that points at a workspace cascades from it.** Every table
 *   with a `workspace_id` is `REFERENCES workspaces (id) ON DELETE CASCADE`,
 *   which is what makes "delete the workspace" a deletion of the tenant's rows
 *   rather than a walk over twenty tables that a new migration can forget to
 *   extend.
 * - **Object storage is not a row and does not cascade.** Attachments and
 *   sandboxed content are bytes in a bucket, so they are removed first and
 *   deliberately: a bucket object nothing points at is a copy of a customer's
 *   file that no query will ever find again.
 * - **The evidence outlives the data.** The record names the workspace and the
 *   person who asked, as values rather than as references, because both have
 *   to be answerable after the rows are gone. That is the half of UU PDP that
 *   is easy to forget: the right to be answered.
 *
 * Ported 1:1 from internal/modules/retention/domain/erasure.go.
 */

/** What a lookup for a workspace's erasure returns when there is none. */
export const ErrErasureNotFound = new Error("retention: no erasure recorded");

/** A person asking for a workspace's data to be removed. */
export interface ErasureRequest {
  workspaceId: string;
  /**
   * Who asked. It is kept as a value on the record, because the person may no
   * longer be a member — or may no longer exist — by the time the record is
   * read.
   */
  requestedBy: string;
}

/** The outcome of one request. */
export interface Erasure {
  id: number;
  workspaceId: string;
  workspaceName: string;
  requestedBy: string;
  requestedByMail: string;
  requestedAt: Date;
  /** Null until the request completes, matching Go's zero `time.Time`. */
  completedAt: Date | null;
  /**
   * What was removed from object storage: the attachments and the sandboxed
   * content, which live in a bucket rather than in Postgres.
   */
  objects: number;
  /**
   * What the database cascaded from the workspace. It is reported rather than
   * checked against an expected total, because the number depends on how much
   * work the tenant had done — what matters is that the row exists and the
   * workspace does not.
   */
  rows: number;
  error: string;
}

/** What one erasure removed. */
export interface ErasureResult {
  objects: number;
  rows: number;
}

/**
 * Removes a workspace and everything that cascades from it.
 *
 * It is one method rather than a walk over each module's delete, and that is
 * the point: the cascade is a fact about the schema — every `workspace_id`
 * references `workspaces` — so the deletion is one statement rather than
 * twenty calls whose order somebody has to get right. A module that adds a
 * table is covered by the foreign key it declares; a module that added one
 * *without* the reference is a table the schema check is there to catch.
 */
export interface WorkspaceEraser {
  /** Deletes the workspace and reports how many rows went with it. */
  eraseWorkspace(workspaceId: string): Promise<number>;
  /**
   * Reads the name before it is deleted, so the record says "Toko Sinar"
   * rather than a uuid.
   */
  workspaceName(workspaceId: string): Promise<string>;
  /** Reads the requester's address, for the same reason. */
  requesterEmail(userId: string): Promise<string>;
}

/**
 * Removes every object one workspace owns.
 *
 * It is the module that owns the bucket path which implements this — the chat
 * module, which writes attachments and sandboxed content — because the
 * prefixes are its own knowledge. The job knows only that a workspace's bytes
 * have to go.
 */
export interface ObjectEraser {
  /** Removes everything under a workspace's prefixes and reports how many objects went. */
  eraseObjects(workspaceId: string): Promise<number>;
}

/**
 * The three ports one erasure needs, declared together because a deployment
 * wires them as a set: an erasure that had the record but not the object walk
 * would report a success it did not achieve, and one that had the walk but not
 * the deletion would leave the rows.
 */
export interface ErasurePolicy
  extends ErasureRepository,
    WorkspaceEraser,
    ObjectEraser {}

/** The storage for the evidence. */
export interface ErasureRepository {
  /** Opens a record and returns its id, before anything is removed. */
  start(
    request: ErasureRequest,
    workspaceName: string,
    requesterEmail: string,
  ): Promise<number>;
  /** Closes a record with what was removed, or with why it stopped. */
  finish(
    id: number,
    result: ErasureResult,
    failure: Error | null,
  ): Promise<void>;
  /** Returns the newest erasures, which is what an auditor reads. */
  list(limit: number): Promise<Erasure[]>;
  /** Returns one workspace's completed erasure, or `ErrErasureNotFound`. */
  find(workspaceId: string): Promise<Erasure>;
}

/**
 * The half of the module the erasure endpoints call: run a request, and read
 * back what one recorded.
 *
 * It is declared separately from `Service` so the container can install the
 * erasure routes only when the deployment wired the three ports they need — a
 * handler built from a service that would refuse every call is a screen that
 * exists and cannot work.
 */
export interface ErasureService {
  /** Removes a workspace's data on request. */
  erase(request: ErasureRequest): Promise<ErasureResult>;
  /** Answers whether a workspace's data was erased. */
  findErasure(workspaceId: string): Promise<Erasure>;
  /** Returns the newest erasures, which is what an auditor reads. */
  erasures(limit: number): Promise<Erasure[]>;
}
