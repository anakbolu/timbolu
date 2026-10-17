/**
 * The attention list: the work that needs a person, with the link that diagnoses
 * it.
 *
 * It is one endpoint rather than three, because it is one question — "what is
 * waiting on me?" — and a screen that asked for the stuck, the parked, and the
 * failed separately would show a list whose sections disagreed about their own
 * paging. What the three have in common is exactly what the screen needs: none
 * of them is going to move without somebody doing something.
 *
 * The Temporal link is built here rather than in the browser. The deployment is
 * the only thing that knows which Temporal UI it has — a compose port, a Cloud
 * host, or none at all — so the URL is a server fact.
 *
 * Ported 1:1 from internal/modules/task/handler/attention.go.
 */

import type { AppContext } from "../../../platform/context.ts";
import { okWithMeta } from "../../../platform/response.ts";
import type { Task } from "../domain/task.ts";
import {
  HealthStuck,
  HealthWaiting,
  StatusFailed,
  taskHealth,
} from "../domain/task.ts";
import { Payload, type PayloadShape } from "../service/wire.ts";
import type { Handler } from "./handler.ts";

/** One task that needs a person. */
export interface AttentionPayload extends PayloadShape {
  /** Why it is on the list: stuck, waiting, or failed. */
  attention: string;
  /** Opens the task's workflow in this deployment's Temporal UI. Empty when none is named. */
  temporal_url?: string;
}

/** The three reasons a task is on the list. */
export const AttentionStuck = "stuck";
export const AttentionWaiting = "waiting";
export const AttentionFailed = "failed";

/**
 * Returns the tasks a person has to do something about.
 *
 * The order is the order of urgency: a task that stopped, then one that is
 * waiting on a decision, then one a worker died under.
 */
export async function attention(h: Handler, c: AppContext): Promise<Response> {
  const limit = queryInt(c.req.query("limit"));

  let tasks: Task[];
  try {
    tasks = await h.service.history(undefined, h.scope(c), {
      // The three states are read in one call each and merged, because `status`
      // is one value and the answer to this question is a set of them. Reading
      // each separately also means the limit applies to each kind, so a
      // workspace with fifty failures still shows its parked tasks.
      status: StatusFailed,
      limit,
      agentId: "",
      parentTaskId: "",
      live: false,
    });
  } catch (err) {
    throw h.fail("list failed tasks", err);
  }

  const payload: AttentionPayload[] = tasks.map((task) => renderAttention(h, task, AttentionFailed));

  let live: Task[];
  try {
    live = await h.service.history(undefined, h.scope(c), {
      status: "",
      live: true,
      limit,
      agentId: "",
      parentTaskId: "",
    });
  } catch (err) {
    throw h.fail("list live tasks", err);
  }

  // A live task is on the list only when it is not going to move on its own:
  // one that is running is doing its job, and listing it would bury the ones
  // that are not.
  const waiting: AttentionPayload[] = [];
  for (const task of live) {
    switch (taskHealth(task, h.now())) {
      case HealthWaiting:
        waiting.push(renderAttention(h, task, AttentionWaiting));
        break;
      case HealthStuck:
        payload.push(renderAttention(h, task, AttentionStuck));
        break;
    }
  }

  // The parked tasks come after the failed ones: a person who has an approval to
  // make and a failure to look at should make the decision first, because a task
  // that is waiting is holding work that is otherwise done.
  payload.push(...waiting);

  return okWithMeta(c, "ok", payload, { count: payload.length });
}

/** Renders one task for the list. */
function renderAttention(h: Handler, task: Task, reason: string): AttentionPayload {
  const out: AttentionPayload = {
    ...Payload(task, h.now()),
    attention: reason,
  };
  const url = temporalUrl(h, task);
  if (url !== "") out.temporal_url = url;
  return out;
}

/**
 * Builds the link that opens a task's workflow.
 *
 * The path is the self-hosted UI's own, which Temporal Cloud serves at the same
 * path under its own host — so one format covers both, and the only deployment
 * fact is the base. A task with no workflow never reached the engine and gets no
 * link.
 */
function temporalUrl(h: Handler, task: Task): string {
  const base = h.temporalUI.trim().replace(/\/+$/, "");
  if (base === "" || task.workflowId === "") {
    return "";
  }

  let path = `/namespaces/${pathEscape(h.temporalNamespace)}/workflows/${pathEscape(task.workflowId)}`;
  if (task.temporalRunId !== "") {
    // The run is part of the link: a workflow id is not unique over time, so a
    // URL without it opens whichever run the UI picks rather than the one that
    // happened.
    path += `/${pathEscape(task.temporalRunId)}`;
  }

  return base + path;
}

/**
 * Go's `url.PathEscape`: the unreserved marks, the sub-delims `$&+:=@`, and
 * alphanumerics survive; `/ ; , ?` and everything else are percent-encoded.
 */
function pathEscape(value: string): string {
  let out = "";
  for (const byte of new TextEncoder().encode(value)) {
    if (isPathSegmentByte(byte)) {
      out += String.fromCharCode(byte);
    } else {
      out += `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return out;
}

function isPathSegmentByte(byte: number): boolean {
  if ((byte >= 0x30 && byte <= 0x39) || (byte >= 0x41 && byte <= 0x5a) || (byte >= 0x61 && byte <= 0x7a)) {
    return true;
  }
  switch (byte) {
    case 0x2d: // -
    case 0x5f: // _
    case 0x2e: // .
    case 0x7e: // ~
    case 0x24: // $
    case 0x26: // &
    case 0x2b: // +
    case 0x3a: // :
    case 0x3d: // =
    case 0x40: // @
      return true;
    default:
      return false;
  }
}

/** Fiber's `c.QueryInt("limit", 0)`: an absent or unparsable value is zero. */
function queryInt(value: string | undefined): number {
  if (value === undefined || value.trim() === "") return 0;
  const parsed = Number(value);
  return Number.isInteger(parsed) ? parsed : 0;
}
