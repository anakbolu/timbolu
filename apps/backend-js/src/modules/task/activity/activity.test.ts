/**
 * The runtime's half of the `agent.state` contract.
 *
 * A Bolu's state on screen is derived from the tasks and drafts that reference
 * it, and an open screen only hears about a change through `agent.state`. The
 * runtime used to publish `task.finished` and nothing else, so a run that ended
 * by asking the user a question left the header saying "sedang bekerja" until
 * the tab was reloaded (#315). These cases pin the event on the way out, and pin
 * that it carries the registry's derived answer rather than this task's status.
 */

import { describe, expect, test } from "bun:test";

import { deriveStatus, StoredActive } from "../../agent/domain/agent.ts";
import {
  DraftApproved,
  DraftPending,
  StatusQueued,
  StatusRunning,
  StatusSucceeded,
  StatusWaitingApproval,
  agentActive,
  type AgentDirectory,
  type AgentRef,
  type Draft,
  type DraftExecution,
  type DraftGate,
  type DraftRequest,
  type Event,
  type EventSink,
  type Finish,
  type Scope,
  type StepRepository,
  type Task,
  type TaskRepository,
} from "../domain/task.ts";
import type { CompleteRequest } from "../workflow/workflow.ts";
import { Activities, type Deps } from "./activity.ts";

const scope: Scope = { workspaceId: "ws-1", userId: null };

function task(overrides: Partial<Task> = {}): Task {
  return {
    id: "task-1",
    workspaceId: "ws-1",
    agentId: "agent-1",
    parentTaskId: "",
    conversationId: "conversation-1",
    trigger: "chat",
    title: "jawab pertanyaan",
    status: StatusRunning,
    replyMessageId: "message-1",
    workflowId: "",
    temporalRunId: "",
    depth: 0,
    summary: "",
    stoppedReason: "",
    waitingReason: "",
    waitingDraftId: "",
    stepCount: 0,
    inputTokens: 0,
    outputTokens: 0,
    costMicros: 0,
    heartbeatAt: null,
    startedAt: null,
    finishedAt: null,
    createdAt: new Date(0),
    updatedAt: new Date(0),
    ...overrides,
  };
}

/** An in-memory task store: the only rows are the ones a case seeds. */
class MemoryTasks implements TaskRepository {
  constructor(readonly rows: Task[]) {}

  async get(_ctx: AbortSignal | undefined, _scope: Scope, id: string): Promise<Task> {
    const row = this.rows.find((candidate) => candidate.id === id);
    if (!row) throw new Error(`no task ${id}`);
    return { ...row };
  }

  async finish(_ctx: AbortSignal | undefined, _scope: Scope, finish: Finish): Promise<Task> {
    const row = await this.get(undefined, scope, finish.taskId);
    row.status = finish.status;
    row.summary = finish.summary;
    row.stoppedReason = finish.stoppedReason;
    row.finishedAt = new Date(0);
    Object.assign(this.rows.find((candidate) => candidate.id === finish.taskId)!, row);
    return row;
  }

  async list(): Promise<Task[]> {
    return this.rows.map((row) => ({ ...row }));
  }

  async create(): Promise<Task> {
    throw new Error("not used by these cases");
  }

  async start(): Promise<Task> {
    throw new Error("not used by these cases");
  }

  async progress(): Promise<void> {
    throw new Error("not used by these cases");
  }

  async park(
    _ctx: AbortSignal | undefined,
    _scope: Scope,
    id: string,
    reason: string,
    draftId: string,
  ): Promise<Task> {
    const row = this.rows.find((candidate) => candidate.id === id)!;
    row.status = StatusWaitingApproval;
    row.waitingReason = reason;
    row.waitingDraftId = draftId;
    return { ...row };
  }

  async resume(): Promise<Task> {
    throw new Error("not used by these cases");
  }

  async cancel(): Promise<Task> {
    throw new Error("not used by these cases");
  }

  async children(): Promise<Task[]> {
    throw new Error("not used by these cases");
  }
}

/** A store whose park fails, which is the best-effort park in `CarryOutDraft`. */
class UnparkableTasks extends MemoryTasks {
  async park(): Promise<Task> {
    throw new Error("park is down");
  }
}

/** An approval gate with one draft, pending unless the case says it was held. */
class StubDrafts implements DraftGate {
  constructor(
    private readonly draftId: string,
    private readonly held = false,
  ) {}

  async requestDraft(
    _ctx: AbortSignal | undefined,
    _scope: Scope,
    request: DraftRequest,
  ): Promise<Draft> {
    return {
      id: this.draftId,
      title: request.title,
      status: DraftPending,
      note: "",
      auto: false,
      content: null,
    };
  }

  async carryOut(): Promise<DraftExecution> {
    return {
      status: DraftApproved,
      content: null,
      held: this.held,
      notBefore: null,
      reason: this.held ? "menunggu jam kerja" : "",
    };
  }
}

/** The steps of these cases are empty: the summary falls back to the count. */
class EmptySteps implements StepRepository {
  async append(): Promise<never> {
    throw new Error("not used by these cases");
  }

  async listSteps(): Promise<[]> {
    return [];
  }
}

/**
 * The registry as the runtime reads it.
 *
 * It derives the display status from the live rows of the store, which is the
 * same derivation the agent service runs — a Bolu is working only while one of
 * its tasks is in flight.
 */
class DerivedDirectory implements AgentDirectory {
  constructor(private readonly tasks: MemoryTasks) {}

  async agent(_ctx: AbortSignal | undefined, _scope: Scope, id: string): Promise<AgentRef> {
    // The repository counts a Bolu as working while one of its tasks is queued or
    // running, and waiting while one of its drafts is pending. These cases have no
    // drafts, so only the first half is modelled.
    const working = this.tasks.rows.some(
      (row) => row.agentId === id && (row.status === StatusQueued || row.status === StatusRunning),
    );

    return {
      ID: id,
      Name: "Bolu",
      Role: "",
      Persona: "",
      Tone: "",
      Stored: StoredActive,
      Display: deriveStatus(StoredActive, { working, waiting: false }),
      Tools: [],
      DefaultModel: {},
    };
  }

  active(ref: AgentRef): boolean {
    return agentActive(ref);
  }
}

/** Records every event the runtime publishes. */
class RecordingSink implements EventSink {
  readonly events: Event[] = [];

  async emit(_ctx: AbortSignal | undefined, event: Event): Promise<void> {
    this.events.push(event);
  }

  /** The states published for one Bolu, in order. */
  states(agentId: string): string[] {
    const out: string[] = [];
    for (const event of this.events) {
      if (event.type !== "agent.state" || event.actorAgentId !== agentId) continue;
      const payload = event.payload;
      if (payload && typeof payload === "object" && "state" in payload && typeof payload.state === "string") {
        out.push(payload.state);
      }
    }
    return out;
  }
}

function build(tasks: MemoryTasks, overrides: Partial<Deps> = {}): { activities: Activities; sink: RecordingSink } {
  const sink = new RecordingSink();
  const deps: Deps = {
    tasks,
    steps: new EmptySteps(),
    agents: new DerivedDirectory(tasks),
    events: sink,
    ...overrides,
  };
  return { activities: new Activities(deps), sink };
}

/** The completion a run reports when its answer is a question to the user. */
function completion(taskId: string): CompleteRequest {
  return {
    TaskID: taskId,
    WorkspaceID: "ws-1",
    Status: StatusSucceeded,
    Reason: "",
    InputTokens: 0,
    OutputTokens: 0,
    CostMicros: 0,
    StepCount: 1,
    Blocks: [{ Type: "text", Body: JSON.stringify({ markdown: "Mau saya lanjutkan besok pagi?" }) }],
  };
}

describe("CompleteTask", () => {
  test("publishes the derived state, not working, once the reply asks a question", async () => {
    const tasks = new MemoryTasks([task()]);
    const { activities, sink } = build(tasks);

    await activities.CompleteTask(completion("task-1"));

    const states = sink.states("agent-1");
    expect(states.length).toBeGreaterThan(0);
    expect(states[states.length - 1]).not.toBe("working");
    expect(states[states.length - 1]).toBe("idle");
  });

  test("carries the registry's answer, so another live task keeps the Bolu working", async () => {
    const tasks = new MemoryTasks([task(), task({ id: "task-2" })]);
    const { activities, sink } = build(tasks);

    await activities.CompleteTask(completion("task-1"));

    const states = sink.states("agent-1");
    expect(states[states.length - 1]).toBe("working");
  });

  test("publishes the task and agent ids on the event", async () => {
    const tasks = new MemoryTasks([task()]);
    const { activities, sink } = build(tasks);

    await activities.CompleteTask(completion("task-1"));

    const event = sink.events.find((candidate) => candidate.type === "agent.state");
    expect(event).toBeDefined();
    expect(event!.workspaceId).toBe("ws-1");
    expect(event!.conversationId).toBe("conversation-1");
    expect(event!.taskId).toBe("task-1");
    expect(event!.payload).toEqual({ agent_id: "agent-1", state: "idle", task_id: "task-1" });
  });

  test("a stream that throws never fails the task", async () => {
    const tasks = new MemoryTasks([task()]);
    const deps: Deps = {
      tasks,
      steps: new EmptySteps(),
      agents: new DerivedDirectory(tasks),
      events: {
        async emit(): Promise<void> {
          throw new Error("stream is down");
        },
      },
    };

    const outcome = await new Activities(deps).CompleteTask(completion("task-1"));

    expect(outcome.Status).toBe(StatusSucceeded);
    expect(tasks.rows[0]!.status).toBe(StatusSucceeded);
  });

  test("an unwired runtime still finishes the task", async () => {
    const tasks = new MemoryTasks([task()]);
    const activities = new Activities({ tasks, steps: new EmptySteps() });

    const outcome = await activities.CompleteTask(completion("task-1"));

    expect(outcome.Status).toBe(StatusSucceeded);
  });
});

describe("task.waiting_approval payload", () => {
  test("carries the draft id a task is parked on", async () => {
    const tasks = new MemoryTasks([task()]);
    const { activities, sink } = build(tasks, { drafts: new StubDrafts("draft-1") });

    await activities.RequestDraft({
      TaskID: "task-1",
      WorkspaceID: "ws-1",
      CallID: "call-1",
      Name: "gmail.send",
      Arguments: { subject: "Tagihan Maret" },
    });

    const event = sink.events.find((candidate) => candidate.type === "task.waiting_approval");
    expect(event).toBeDefined();
    expect(event!.payload).toEqual({
      task_id: "task-1",
      agent_id: "agent-1",
      status: StatusWaitingApproval,
      trigger: "chat",
      title: "jawab pertanyaan",
      waiting_draft_id: "draft-1",
    });
  });

  test("omits the field when the task waits without a draft", async () => {
    // The hold's park is best-effort: when it fails the task still reads as
    // waiting on the wire, and it names no draft because none was stored.
    const tasks = new UnparkableTasks([task()]);
    const { activities, sink } = build(tasks, { drafts: new StubDrafts("draft-1", true) });

    await activities.CarryOutDraft({ TaskID: "task-1", WorkspaceID: "ws-1", DraftID: "draft-1" });

    const event = sink.events.find((candidate) => candidate.type === "task.waiting_approval");
    expect(event).toBeDefined();
    const payload = event!.payload as Record<string, unknown>;
    expect("waiting_draft_id" in payload).toBe(false);
    expect(payload).toEqual({
      task_id: "task-1",
      agent_id: "agent-1",
      status: StatusRunning,
      trigger: "chat",
      title: "jawab pertanyaan",
    });
  });

  test("keeps every other event at five fields, even with a stale draft id", async () => {
    const tasks = new MemoryTasks([task({ status: StatusWaitingApproval, waitingDraftId: "draft-1" })]);
    const { activities, sink } = build(tasks);

    await activities.CompleteTask(completion("task-1"));

    const event = sink.events.find((candidate) => candidate.type === "task.finished");
    expect(event).toBeDefined();
    expect(event!.payload).toEqual({
      task_id: "task-1",
      agent_id: "agent-1",
      status: StatusSucceeded,
      trigger: "chat",
      title: "jawab pertanyaan",
    });
  });
});
