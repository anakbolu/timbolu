/**
 * The Bolu create path's rules, without a database.
 *
 * The insert and the template lookup are SQL's job, so these cases pin the half
 * the service owns: which profile reaches the repository, and which failures
 * come back as a wrong body rather than a missing object. The distinction
 * matters at the HTTP edge — the handler answers `ErrAgentNotFound` with 404
 * `agent_not_found`, which for an unknown `template_key` would send the client
 * hunting for a Bolu that never existed.
 */

import { describe, expect, test } from "bun:test";

import {
  ErrAgentNotFound,
  ErrInvalidInput,
  StoredActive,
  errorMessage,
  is,
  type Activity,
  type Agent,
  type CreateRequest,
  type Grant,
  type Integration,
  type NewAgent,
  type Placement,
  type Reason,
  type Repository,
  type Scope,
  type Team,
  type Template,
  type Tool,
  type UpdateRequest,
} from "../domain/agent.ts";
import { Service } from "./service.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const workspaceId = "11111111-1111-1111-1111-111111111111";
const teamId = "55555555-5555-5555-5555-555555555555";
const missingAgentId = "66666666-6666-6666-6666-666666666666";

const scope: Scope = { userId, workspaceId };

/** One seeded profile, as the migration writes it. */
const oren: Template = {
  key: "oren",
  name: "Oren",
  role: "Penjualan",
  persona: "Menangani pesanan dan menagih pelanggan dengan ramah.",
  tone: "hangat dan ringkas",
  shape: "kacang",
  color: "#F97316",
  tools: ["gmail.search", "gmail.create_draft", "gmail.send"],
  integrations: ["gmail", "whatsapp"],
  sortOrder: 1,
};

/** A body that asks for nothing but a name, so each case varies one field. */
const baseRequest: CreateRequest = {
  teamId,
  name: "Bolu Uji",
  role: "",
  persona: "",
  tone: "",
  shape: "",
  color: "",
  tools: null,
  templateKey: "",
  copyFrom: "",
};

/** Fails if `create` starts reaching for a method these cases do not model. */
function notUsed(method: string): never {
  throw new Error(`${method} is not part of the create path`);
}

/** Answers the create path, and records what it was asked for. */
class RecordingRepository implements Repository {
  readonly created: Array<{ scope: Scope; agent: NewAgent }> = [];
  readonly updated: Array<{ scope: Scope; id: string; req: UpdateRequest }> = [];
  readonly sources: string[] = [];
  readonly seeded: Record<string, Template> = { [oren.key]: oren };
  readonly agents: Record<string, Agent> = {};

  async get(_received: Scope, id: string): Promise<Agent> {
    this.sources.push(id);
    const found = this.agents[id];
    if (found) return found;
    throw ErrAgentNotFound;
  }

  async agentTemplate(key: string): Promise<NewAgent> {
    const template = this.seeded[key];
    if (!template) throw ErrAgentNotFound;

    // A fresh object each time: the service edits the profile it is handed,
    // and the template must survive those edits unchanged.
    return {
      teamId: "",
      name: template.name,
      role: template.role,
      persona: template.persona,
      tone: template.tone,
      shape: template.shape,
      color: template.color,
      tools: [...template.tools],
      templateKey: template.key,
      defaultModel: null,
    };
  }

  async create(received: Scope, agent: NewAgent): Promise<Agent> {
    this.created.push({ scope: received, agent });
    return {
      id: "77777777-7777-7777-7777-777777777777",
      workspaceId: received.workspaceId,
      teamId: agent.teamId,
      teamName: "Tim Bolu",
      teamKind: "system",
      name: agent.name,
      role: agent.role,
      persona: agent.persona,
      tone: agent.tone,
      shape: agent.shape,
      color: agent.color,
      position: 0,
      status: StoredActive,
      display: "",
      reason: null,
      memoryExtraction: true,
      templateKey: agent.templateKey,
      tools: agent.tools ?? [],
      defaultModel: agent.defaultModel ?? {},
      createdAt: new Date("2026-10-30T09:00:00Z"),
      updatedAt: new Date("2026-10-30T09:00:00Z"),
    };
  }

  async update(received: Scope, id: string, req: UpdateRequest): Promise<Agent> {
    this.updated.push({ scope: received, id, req });
    const existing = this.agents[id];
    return {
      id,
      workspaceId: received.workspaceId,
      teamId: existing?.teamId ?? teamId,
      teamName: existing?.teamName ?? "Tim Bolu",
      teamKind: existing?.teamKind ?? "system",
      name: req.name,
      role: req.role ?? "",
      persona: req.persona ?? "",
      tone: req.tone ?? "",
      shape: req.shape ?? "",
      color: req.color ?? "",
      position: existing?.position ?? 0,
      status: existing?.status ?? StoredActive,
      display: "",
      reason: null,
      memoryExtraction: existing?.memoryExtraction ?? true,
      templateKey: existing?.templateKey ?? "",
      tools: req.tools ?? [],
      defaultModel: req.defaultModel ?? {},
      createdAt: existing?.createdAt ?? new Date("2026-10-30T09:00:00Z"),
      updatedAt: new Date("2026-10-30T09:00:00Z"),
    };
  }

  async activityFor(): Promise<Activity> {
    return { working: false, waiting: false };
  }

  async reasonFor(): Promise<Reason | null> {
    return null;
  }

  list(): Promise<Agent[]> {
    return notUsed("list");
  }
  setStatus(): Promise<Agent> {
    return notUsed("setStatus");
  }
  setMemoryExtraction(): Promise<Agent> {
    return notUsed("setMemoryExtraction");
  }
  setPlacements(_scope: Scope, _placements: Placement[]): Promise<void> {
    return notUsed("setPlacements");
  }
  delete(): Promise<void> {
    return notUsed("delete");
  }
  activity(): Promise<Map<string, Activity>> {
    return notUsed("activity");
  }
  runningTasks(): Promise<number> {
    return notUsed("runningTasks");
  }
  countLive(): Promise<number> {
    return notUsed("countLive");
  }
  teams(): Promise<Team[]> {
    return notUsed("teams");
  }
  templates(): Promise<Template[]> {
    return notUsed("templates");
  }
  template(): Promise<Template> {
    return notUsed("template");
  }
  grants(): Promise<Grant[]> {
    return notUsed("grants");
  }
  setGrant(
    _scope: Scope,
    _agentId: string,
    _integrationId: string,
    _permission: string,
  ): Promise<Grant> {
    return notUsed("setGrant");
  }
  deleteGrant(): Promise<void> {
    return notUsed("deleteGrant");
  }
  allowedTools(): Promise<Tool[]> {
    return notUsed("allowedTools");
  }
  toolCatalog(): Promise<Tool[]> {
    return notUsed("toolCatalog");
  }
  integrations(): Promise<Integration[]> {
    return notUsed("integrations");
  }
}

function newService(): { repository: RecordingRepository; service: Service } {
  const repository = new RecordingRepository();
  return { repository, service: new Service({ repository }) };
}

describe("create", () => {
  test("answers an unknown template key as invalid input, not a missing Bolu", async () => {
    const { repository, service } = newService();

    const err = await service.create(scope, { ...baseRequest, templateKey: "sales" }).catch((thrown: unknown) => thrown);

    // A wrong body must not be reported as a missing object: the handler turns
    // ErrAgentNotFound into 404 agent_not_found, which blames the URL.
    expect(is(err, ErrInvalidInput)).toBe(true);
    expect(is(err, ErrAgentNotFound)).toBe(false);
    expect(errorMessage(err)).toContain("sales");
    expect(repository.created).toHaveLength(0);
  });

  test("starts the Bolu from a known template and keeps its key", async () => {
    const { repository, service } = newService();

    await service.create(scope, { ...baseRequest, name: "", templateKey: "oren" });

    expect(repository.created).toHaveLength(1);
    expect(repository.created[0]?.scope).toEqual(scope);
    expect(repository.created[0]?.agent).toEqual({
      teamId,
      name: oren.name,
      role: oren.role,
      persona: oren.persona,
      tone: oren.tone,
      shape: oren.shape,
      color: oren.color,
      tools: oren.tools,
      templateKey: oren.key,
      defaultModel: null,
    });
  });

  test("keeps a copy_from that names no Bolu as a missing Bolu", async () => {
    const { repository, service } = newService();

    const err = await service
      .create(scope, { ...baseRequest, copyFrom: missingAgentId })
      .catch((thrown: unknown) => thrown);

    // The source Bolu is a real object the client named, so its absence stays
    // 404 agent_not_found; only the template key is a body value.
    expect(is(err, ErrAgentNotFound)).toBe(true);
    expect(is(err, ErrInvalidInput)).toBe(false);
    expect(repository.sources).toEqual([missingAgentId]);
    expect(repository.created).toHaveLength(0);
  });

  test("gives a Bolu created without a shape the shape the characters default to", async () => {
    const { repository, service } = newService();

    await service.create(scope, baseRequest);

    // "circle" used to be stored here; no renderer draws it (#564).
    expect(repository.created).toHaveLength(1);
    expect(repository.created[0]?.agent.shape).toBe("kacang");
  });

  test("refuses a shape the character package cannot draw", async () => {
    const { repository, service } = newService();

    const err = await service
      .create(scope, { ...baseRequest, shape: "circle" })
      .catch((thrown: unknown) => thrown);

    expect(is(err, ErrInvalidInput)).toBe(true);
    expect(errorMessage(err)).toContain("kacang");
    expect(repository.created).toHaveLength(0);
  });

  test("stores any of the 16 shapes the client asks for", async () => {
    const { repository, service } = newService();

    await service.create(scope, { ...baseRequest, shape: "kucing" });

    expect(repository.created[0]?.agent.shape).toBe("kucing");
  });
});

/** An edit that asks for nothing but a name, so each case varies one field. */
const existingAgentId = "44444444-4444-4444-4444-444444444444";

const existingBolu: Agent = {
  id: existingAgentId,
  workspaceId,
  teamId,
  teamName: "Tim Bolu",
  teamKind: "system",
  name: "Bolu Asli",
  role: "Pemandu",
  persona: "Menjelaskan fitur dengan sabar.",
  tone: "tenang",
  shape: "hati",
  color: "#EC4899",
  position: 0,
  status: StoredActive,
  display: "",
  reason: null,
  memoryExtraction: true,
  templateKey: "",
  tools: ["gmail.search", "gmail.send"],
  defaultModel: { provider: "openai", model: "gpt-4o" },
  createdAt: new Date("2026-10-30T09:00:00Z"),
  updatedAt: new Date("2026-10-30T09:00:00Z"),
};

describe("update", () => {
  test("refuses an update without a name", async () => {
    const { service } = newService();

    const err = await service
      // @ts-expect-error test invalid caller input without name
      .update(scope, existingAgentId, { shape: "kucing" })
      .catch((thrown: unknown) => thrown);

    expect(is(err, ErrInvalidInput)).toBe(true);
    expect(errorMessage(err)).toContain("name is required");
  });

  test("refuses a shape the character package cannot draw", async () => {
    const { service } = newService();

    const err = await service
      .update(scope, missingAgentId, { name: "Bolu Uji", shape: "circle" })
      .catch((thrown: unknown) => thrown);

    // The body is wrong, so it is not reported as a missing Bolu.
    expect(is(err, ErrInvalidInput)).toBe(true);
    expect(is(err, ErrAgentNotFound)).toBe(false);
    expect(errorMessage(err)).toContain("kucing");
  });

  test("keeps stored profile fields when the patch only sends name", async () => {
    const { repository, service } = newService();
    repository.agents[existingAgentId] = { ...existingBolu };

    await service.update(scope, existingAgentId, { name: "Nama Baru" });

    expect(repository.updated).toHaveLength(1);
    const call = repository.updated[0];
    expect(call?.id).toBe(existingAgentId);
    expect(call?.req).toEqual({
      name: "Nama Baru",
      role: existingBolu.role,
      persona: existingBolu.persona,
      tone: existingBolu.tone,
      shape: "hati",
      color: existingBolu.color,
      tools: existingBolu.tools,
      defaultModel: existingBolu.defaultModel,
    });
  });

  test("forwards an explicit shape when provided", async () => {
    const { repository, service } = newService();
    repository.agents[existingAgentId] = { ...existingBolu };

    await service.update(scope, existingAgentId, { name: existingBolu.name, shape: "kucing" });

    expect(repository.updated).toHaveLength(1);
    expect(repository.updated[0]?.req.shape).toBe("kucing");
    expect(repository.updated[0]?.req.color).toBe(existingBolu.color);
  });

  test("overwrites default_model when sent explicitly as empty object", async () => {
    const { repository, service } = newService();
    repository.agents[existingAgentId] = { ...existingBolu };

    await service.update(scope, existingAgentId, {
      name: existingBolu.name,
      defaultModel: {},
    });

    expect(repository.updated).toHaveLength(1);
    expect(repository.updated[0]?.req.defaultModel).toEqual({});
  });
});