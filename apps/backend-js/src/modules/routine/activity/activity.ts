/**
 * The side-effecting half of a routine run: the one activity the worker
 * registers, which turns a tick into a recorded run.
 *
 * It exists as a thin layer rather than as a Temporal-aware service so the
 * workflow package stays free of the module's services (a replay must stay
 * deterministic) and the service stays free of the SDK (a unit test can call it
 * without a server).
 *
 * Ported 1:1 from internal/modules/routine/activity/activity.go.
 */

import { isUuid } from "../../../platform/context.ts";
import { nopLogger, type Logger } from "../../../platform/logger.ts";
import { ErrInvalidInput, ErrNotFound, is, wrap } from "../domain/routine.ts";
import type { RunInput, Service } from "../domain/service.ts";
import type { RunResult } from "../workflow/workflow.ts";

/** The activity dependencies. */
export interface Deps {
  routines?: Service | null;
  logger?: Logger | null;
}

/** The set the worker registers. */
export class Activities {
  private readonly routines: Service | null;
  private readonly log: Logger;

  constructor(deps: Deps) {
    this.routines = deps.routines ?? null;
    this.log = deps.logger ?? nopLogger;
  }

  /**
   * Performs one tick of one routine.
   *
   * It returns a failure only when nothing could be recorded at all: a tick
   * whose task could not be opened has already written a `failed` run with the
   * reason, so reporting an error as well would make Temporal retry a tick whose
   * outcome is on the record.
   */
  async RunRoutine(input: RunInput): Promise<RunResult> {
    if (!this.routines) {
      throw new Error("routine: the service is not configured");
    }

    const workspaceID = parseID(input.WorkspaceID, "workspace");
    const routineID = parseID(input.RoutineID, "routine");

    let run;
    try {
      run = await this.routines.runOnce({ workspaceId: workspaceID }, routineID);
    } catch (err) {
      // A routine that was deleted between the tick and this call, or one that
      // was paused, is a normal race rather than a failure: the schedule is
      // removed with the routine, but a tick already in flight still arrives.
      if (is(err, ErrNotFound) || is(err, ErrInvalidInput)) {
        this.log.info({ routine_id: routineID, err }, "a routine tick found nothing to run");
        return { RunID: "", Status: "", Summary: "" };
      }
      throw err;
    }

    this.log.info(
      { routine_id: routineID, run_id: run.id, status: run.status },
      "routine run recorded",
    );

    return { RunID: run.id, Status: run.status, Summary: run.summary };
  }
}

/** Reads an id that travelled through the workflow as a string. */
function parseID(value: string, field: string): string {
  if (!value) {
    throw wrap(ErrInvalidInput, `${field} id is required`);
  }
  if (!isUuid(value)) {
    throw wrap(ErrInvalidInput, `${field} id is not valid`);
  }
  return value;
}
