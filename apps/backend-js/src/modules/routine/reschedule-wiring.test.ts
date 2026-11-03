import { describe, expect, test } from "bun:test";

import type { AppContext } from "../../platform/context.ts";
import { nopLogger } from "../../platform/logger.ts";
import {
  KindDaily,
  makeRoutine,
  type Routine,
  type Scope as RoutineScope,
} from "./domain/routine.ts";
import type {
  Engine,
  Repository as RoutineRepository,
} from "./domain/service.ts";
import { RoutineService } from "./service/service.ts";
import type {
  Scope as WorkspaceScope,
  Service as WorkspaceService,
  UpdateRequest,
  Workspace,
} from "../workspace/domain/workspace.ts";
import { Handler as WorkspaceHandler } from "../workspace/handler/handler.ts";
import { wireClockChange } from "../../container/container.ts";

/** Fake engine that records calls to register. */
class FakeEngine implements Engine {
  readonly registered: Array<{ routine: Routine; location: string | null }> = [];

  async register(routine: Routine, location: string | null): Promise<string> {
    this.registered.push({ routine, location });
    return `sched-${routine.id}`;
  }

  async setPaused(_scheduleID: string, _paused: boolean, _reason: string): Promise<void> {}
  async remove(_scheduleID: string): Promise<void> {}
}

/** Fake routine repository returning enabled routines. */
class FakeRoutineRepository implements Partial<RoutineRepository> {
  constructor(private readonly activeRoutines: Routine[]) {}

  async listEnabled(_scope: RoutineScope): Promise<Routine[]> {
    return this.activeRoutines;
  }

  async setScheduleID(_scope: RoutineScope, _id: string, _scheduleID: string): Promise<void> {}
}

/** Fake workspace service for Handler tests. */
class FakeWorkspaceService implements Partial<WorkspaceService> {
  constructor(public currentWorkspace: Workspace) {}

  async current(_scope: WorkspaceScope): Promise<Workspace> {
    return this.currentWorkspace;
  }

  async update(_scope: WorkspaceScope, _role: string, req: UpdateRequest): Promise<Workspace> {
    this.currentWorkspace = {
      ...this.currentWorkspace,
      name: req.name || this.currentWorkspace.name,
      businessField: req.businessField || this.currentWorkspace.businessField,
      timezone: req.timezone || this.currentWorkspace.timezone,
      language: req.language || this.currentWorkspace.language,
      workHoursStart: req.workHoursStart || this.currentWorkspace.workHoursStart,
      workHoursEnd: req.workHoursEnd || this.currentWorkspace.workHoursEnd,
      workDays: req.workDays ?? this.currentWorkspace.workDays,
    };
    return this.currentWorkspace;
  }
}

function makeContext(body: unknown, vars: Record<string, unknown> = {}): AppContext {
  return {
    req: {
      json: async () => body,
      header: (_name?: string) => undefined,
      query: (_name?: string) => undefined,
      param: (_name?: string) => undefined,
    },
    get: (key: string) => vars[key],
    json: (b: unknown, s?: number) => new Response(JSON.stringify(b), { status: s }),
  } as unknown as AppContext;
}

describe("Routine reschedule wiring", () => {
  test("routineService.reschedule invokes engine.register for all active routines", async () => {
    const r1 = makeRoutine({
      id: "r1",
      workspaceID: "w1",
      agentID: "a1",
      name: "Rutin 1",
      kind: KindDaily,
      minute: 0,
      hourStart: 9,
      hourEnd: 9,
      dayOfWeek: 0,
      triggerSource: "",
      instruction: "Laporan harian pagi",
      enabled: true,
      pausedReason: "",
      scheduleID: "s1",
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });

    const r2 = makeRoutine({
      id: "r2",
      workspaceID: "w1",
      agentID: "a1",
      name: "Rutin 2",
      kind: KindDaily,
      minute: 30,
      hourStart: 17,
      hourEnd: 17,
      dayOfWeek: 0,
      triggerSource: "",
      instruction: "Laporan harian sore",
      enabled: true,
      pausedReason: "",
      scheduleID: "s2",
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });

    const engine = new FakeEngine();
    const repo = new FakeRoutineRepository([r1, r2]);
    const routineService = new RoutineService({
      repository: repo as unknown as RoutineRepository,
      engine,
    });

    await routineService.reschedule({ workspaceId: "w1" });

    expect(engine.registered).toHaveLength(2);
    expect(engine.registered[0].routine.id).toBe("r1");
    expect(engine.registered[1].routine.id).toBe("r2");
  });

  test("WorkspaceHandler calls afterClockChange on timezone change and ignores rename only", async () => {
    const initialWorkspace: Workspace = {
      id: "w1",
      name: "Toko Bolu",
      businessField: "retail",
      timezone: "Asia/Jakarta",
      language: "id",
      workHoursStart: "09:00",
      workHoursEnd: "17:00",
      workDays: [1, 2, 3, 4, 5],
      role: "owner",
      createdAt: new Date(0),
    };

    const fakeService = new FakeWorkspaceService(initialWorkspace);
    const handler = new WorkspaceHandler(fakeService as unknown as WorkspaceService, nopLogger);

    const clockChanges: string[] = [];
    handler.withClockChange((workspaceId: string) => {
      clockChanges.push(workspaceId);
    });

    const contextVars = {
      scope: { workspaceId: "w1", userId: "u1" },
      role: "owner",
    };

    // 1. Rename only: clock should NOT move
    const renameRes = await handler.update(
      makeContext(
        {
          name: "Toko Bolu Baru",
          business_field: "retail",
          timezone: "Asia/Jakarta",
          language: "id",
          work_hours_start: "09:00",
          work_hours_end: "17:00",
          work_days: [1, 2, 3, 4, 5],
        },
        contextVars,
      ),
    );
    expect(renameRes.status).toBe(200);
    expect(clockChanges).toHaveLength(0);

    // 2. Timezone change: clock MOVED, callback MUST be called
    const tzRes = await handler.update(
      makeContext(
        {
          name: "Toko Bolu Baru",
          business_field: "retail",
          timezone: "Asia/Makassar",
          language: "id",
          work_hours_start: "09:00",
          work_hours_end: "17:00",
          work_days: [1, 2, 3, 4, 5],
        },
        contextVars,
      ),
    );
    expect(tzRes.status).toBe(200);
    expect(clockChanges).toEqual(["w1"]);
  });

  test("wireClockChange connects workspace clock change to routine rescheduling", async () => {
    const r1 = makeRoutine({
      id: "r1",
      workspaceID: "w1",
      agentID: "a1",
      name: "Rutin 1",
      kind: KindDaily,
      minute: 0,
      hourStart: 9,
      hourEnd: 9,
      dayOfWeek: 0,
      triggerSource: "",
      instruction: "Laporan harian pagi",
      enabled: true,
      pausedReason: "",
      scheduleID: "s1",
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });

    const r2 = makeRoutine({
      id: "r2",
      workspaceID: "w1",
      agentID: "a1",
      name: "Rutin 2",
      kind: KindDaily,
      minute: 30,
      hourStart: 17,
      hourEnd: 17,
      dayOfWeek: 0,
      triggerSource: "",
      instruction: "Laporan harian sore",
      enabled: true,
      pausedReason: "",
      scheduleID: "s2",
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });

    const engine = new FakeEngine();
    const repository = new FakeRoutineRepository([r1, r2]);
    const routineService = new RoutineService({
      repository: repository as unknown as RoutineRepository,
      engine,
    });

    const initialWorkspace: Workspace = {
      id: "w1",
      name: "Toko Bolu",
      businessField: "retail",
      timezone: "Asia/Jakarta",
      language: "id",
      workHoursStart: "09:00",
      workHoursEnd: "17:00",
      workDays: [1, 2, 3, 4, 5],
      role: "owner",
      createdAt: new Date(0),
    };

    const fakeService = new FakeWorkspaceService(initialWorkspace);
    const handler = new WorkspaceHandler(fakeService as unknown as WorkspaceService, nopLogger);

    wireClockChange(handler, routineService);

    const contextVars = {
      scope: { workspaceId: "w1", userId: "u1" },
      role: "owner",
    };

    const res = await handler.update(
      makeContext(
        {
          name: "Toko Bolu",
          business_field: "retail",
          timezone: "Asia/Makassar",
          language: "id",
          work_hours_start: "09:00",
          work_hours_end: "17:00",
          work_days: [1, 2, 3, 4, 5],
        },
        contextVars,
      ),
    );
    expect(res.status).toBe(200);

    expect(engine.registered).toHaveLength(2);
    expect(engine.registered[0].routine.id).toBe("r1");
    expect(engine.registered[1].routine.id).toBe("r2");
  });
});
