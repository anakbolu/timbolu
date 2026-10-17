/**
 * The erasure half of the service: removing a workspace's data on request.
 *
 * Ported 1:1 from internal/modules/retention/service/erasure.go. TypeScript
 * cannot spread one class across files, so these are functions over the
 * service, the way the billing module splits `quota.ts` and `payment.ts`; the
 * class methods in `service.ts` delegate here.
 */

import {
  ErrInvalidInput,
  ErrNoRepository,
  NilUUID,
  WrappedError,
  wrap,
} from "../domain/retention.ts";
import {
  ErrErasureNotFound,
  type Erasure,
  type ErasureRequest,
  type ErasureResult,
} from "../domain/erasure.ts";
import type { Service } from "./service.ts";

/**
 * Removes a workspace's data on request.
 *
 * The order is the design and it is the opposite of the retention pass's, for
 * a reason worth stating: **the objects go first and the rows second**. If the
 * workspace row goes first, every attachment row cascades with it and the
 * storage keys are gone — the bytes are then unreachable by any query, in a
 * bucket nothing points at, and the request has silently failed to erase the
 * copy of the customer's file that matters most. So the bytes are removed while
 * the keys that name them still exist.
 *
 * A failure at either step keeps the record with its reason, and the request is
 * *not* retried automatically: an erasure that half-succeeded is a state a
 * person has to look at, and a background retry would be the deployment
 * deciding on its own to try again. What the record gives is the one thing
 * needed to finish the job by hand — which prefix and which workspace.
 */
export async function erase(s: Service, request: ErasureRequest): Promise<ErasureResult> {
  const result: ErasureResult = { objects: 0, rows: 0 };

  if (!s.deps.erasures) throw ErrNoRepository;
  if (!request.workspaceId || request.workspaceId === NilUUID) {
    throw wrap(ErrInvalidInput, "workspace is required");
  }
  if (!s.deps.workspaces || !s.deps.objects) {
    // Without both, an erasure would delete half of what the workspace held and
    // report success. Refusing is the only honest answer: the caller is told
    // the deployment cannot complete the request rather than being told it did.
    throw wrap(ErrInvalidInput, "the erasure path is not configured");
  }

  // The names are read while the rows that hold them are still there: an
  // erasure record that says a uuid was deleted cannot be shown to the person
  // who asked for their data back.
  let name: string;
  try {
    name = await s.deps.workspaces.workspaceName(request.workspaceId);
  } catch (err) {
    throw new WrappedError(
      asError(err),
      `retention: read the workspace name: ${errorMessage(err)}`,
    );
  }

  let email = "";
  try {
    email = await s.deps.workspaces.requesterEmail(request.requestedBy);
  } catch (err) {
    // An address that cannot be read is not a reason to refuse the erasure:
    // the person's right is to have their data removed, and the record can say
    // who asked by id.
    s.warn(
      "could not read the requester's address for the erasure record",
      request.workspaceId,
      asError(err),
    );
  }

  const recordID = await s.deps.erasures.start(request, name, email);

  // Objects first, while the keys that name them still exist.
  try {
    result.objects = await s.deps.objects.eraseObjects(request.workspaceId);
  } catch (err) {
    await finishErasure(s, recordID, result, asError(err));
    throw err;
  }

  try {
    result.rows = await s.deps.workspaces.eraseWorkspace(request.workspaceId);
  } catch (err) {
    await finishErasure(s, recordID, result, asError(err));
    throw err;
  }

  await finishErasure(s, recordID, result, null);

  return result;
}

/** Returns the newest erasures, which is what an auditor reads. */
export async function erasures(s: Service, limit: number): Promise<Erasure[]> {
  if (!s.deps.erasures) throw ErrNoRepository;
  return s.deps.erasures.list(limit);
}

/** Answers whether one workspace's data was erased, which is the question its owner has. */
export async function findErasure(s: Service, workspaceId: string): Promise<Erasure> {
  if (!s.deps.erasures) throw ErrNoRepository;
  return s.deps.erasures.find(workspaceId);
}

/** Closes the record, and reports a failure it could not write. */
export async function finishErasure(
  s: Service,
  id: number,
  result: ErasureResult,
  failure: Error | null,
): Promise<void> {
  try {
    await s.deps.erasures?.finish(id, result, failure);
  } catch (err) {
    s.warn("could not close the erasure record", null, asError(err));
  }
}

/** Re-exported so the service file does not import the sentinel twice. */
export { ErrErasureNotFound };

/** Coerces anything thrown into an `Error`. */
function asError(err: unknown): Error {
  return err instanceof Error ? err : new Error(String(err));
}

/** The message a wrapped error prints, matching Go's `%w`. */
function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
