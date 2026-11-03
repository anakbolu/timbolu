import assert from "node:assert/strict";
import test from "node:test";
import type { Agent, AgentTeam, ApiResult, OnboardResult } from "@/lib/api";
import {
  finishOnboarding,
  type OnboardingRetryDeps,
  type OnboardingRetryInput,
} from "./onboarding-retry";

function fakeOnboardResult(id = "ws-1"): OnboardResult {
  return {
    workspace: {
      id,
      name: "Usaha Saya",
      timezone: "Asia/Jakarta",
      business_field: "Retail",
      language: "id",
      role: "owner",
      work_hours_start: "09:00",
      work_hours_end: "17:00",
      work_days: [1, 2, 3, 4, 5],
      created_at: "2026-11-04T00:00:00Z",
    },
    teams: [
      {
        id: "team-1",
        name: "Tim Bolu",
        kind: "default",
      },
    ],
    created: true,
  };
}

function fakeAgent(teamId = "team-1", name = "Oren"): Agent {
  return {
    id: "agent-1",
    team_id: teamId,
    team_name: "Tim Bolu",
    team_kind: "default",
    name,
    role: "Sales assistant",
    persona: "Helpful",
    tone: "Ramah",
    shape: "bread",
    color: "#f59e0b",
    status: "idle",
    display_status: "idle",
    position: 0,
    tools: [],
    default_model: {},
    memory_extraction_enabled: false,
    created_at: "2026-11-04T00:00:00Z",
  };
}

function baseInput(overrides: Partial<OnboardingRetryInput> = {}): OnboardingRetryInput {
  return {
    onboarded: null,
    name: "Toko Sinar",
    businessField: "Retail",
    timezone: "Asia/Jakarta",
    boluName: "Oren",
    boluRole: "Sales",
    shape: "bread",
    color: "#f59e0b",
    template: "sales",
    ...overrides,
  };
}

test("percobaan pertama: onboard dipanggil 1x, createAgent dipanggil 1x", async () => {
  let onboardCalls = 0;
  let createAgentCalls = 0;
  let listTeamsCalls = 0;
  const deps: OnboardingRetryDeps = {
    onboard: async (): Promise<ApiResult<OnboardResult>> => {
      onboardCalls++;
      return { ok: true, status: 200, data: fakeOnboardResult() };
    },
    listTeams: async (): Promise<ApiResult<AgentTeam[]>> => {
      listTeamsCalls++;
      return { ok: true, status: 200, data: [] };
    },
    createAgent: async (): Promise<ApiResult<Agent>> => {
      createAgentCalls++;
      return { ok: true, status: 201, data: fakeAgent() };
    },
  };
  const outcome = await finishOnboarding(deps, baseInput());

  assert.equal(onboardCalls, 1);
  assert.equal(listTeamsCalls, 1);
  assert.equal(createAgentCalls, 1);
  assert.equal(outcome.kind, "done");
  if (outcome.kind === "done") {
    assert.equal(outcome.onboarded.workspace.id, "ws-1");
  }
});

test("createAgent gagal lalu diulang dengan onboarded terisi: onboard tetap 1x, createAgent 2x", async () => {
  let onboardCalls = 0;
  let createAgentCalls = 0;

  const deps: OnboardingRetryDeps = {
    onboard: async (): Promise<ApiResult<OnboardResult>> => {
      onboardCalls++;
      return { ok: true, status: 200, data: fakeOnboardResult() };
    },
    listTeams: async (): Promise<ApiResult<AgentTeam[]>> => {
      return { ok: true, status: 200, data: [] };
    },
    createAgent: async (): Promise<ApiResult<Agent>> => {
      createAgentCalls++;
      if (createAgentCalls === 1) {
        return { ok: false, status: 500, message: "backend timeout" };
      }
      return { ok: true, status: 201, data: fakeAgent() };
    },
  };

  // Run 1: first attempt, createAgent fails
  const firstOutcome = await finishOnboarding(deps, baseInput());
  assert.equal(firstOutcome.kind, "bolu_failed");
  assert.equal(onboardCalls, 1);
  assert.equal(createAgentCalls, 1);
  if (firstOutcome.kind !== "bolu_failed") {
    assert.fail("expected bolu_failed");
  }
  assert.match(firstOutcome.message, /backend timeout/);

  // Run 2: retry using the onboarded result from first run
  const retryOutcome = await finishOnboarding(
    deps,
    baseInput({ onboarded: firstOutcome.onboarded }),
  );

  assert.equal(retryOutcome.kind, "done");
  assert.equal(onboardCalls, 1, "onboard() tidak boleh dipanggil ulang");
  assert.equal(createAgentCalls, 2);
});

test("workspace sudah punya Bolu bernama sama: createAgent 0x, hasil done", async () => {
  let onboardCalls = 0;
  let createAgentCalls = 0;

  const existingTeam: AgentTeam = {
    id: "team-1",
    name: "Tim Bolu",
    kind: "default",
    agents: [fakeAgent("team-1", "  oReN  ")],
  };

  const deps: OnboardingRetryDeps = {
    onboard: async (): Promise<ApiResult<OnboardResult>> => {
      onboardCalls++;
      return { ok: true, status: 200, data: fakeOnboardResult() };
    },
    listTeams: async (): Promise<ApiResult<AgentTeam[]>> => {
      return { ok: true, status: 200, data: [existingTeam] };
    },
    createAgent: async (): Promise<ApiResult<Agent>> => {
      createAgentCalls++;
      return { ok: true, status: 201, data: fakeAgent() };
    },
  };

  const outcome = await finishOnboarding(
    deps,
    baseInput({ onboarded: fakeOnboardResult(), boluName: "oren" }),
  );

  assert.equal(outcome.kind, "done");
  assert.equal(onboardCalls, 0);
  assert.equal(createAgentCalls, 0, "createAgent tidak boleh dipanggil jika agent bernama sama sudah ada");
});

test("onboard gagal: workspace_failed, createAgent tidak dipanggil", async () => {
  let onboardCalls = 0;
  let createAgentCalls = 0;
  let listTeamsCalls = 0;

  const deps: OnboardingRetryDeps = {
    onboard: async (): Promise<ApiResult<OnboardResult>> => {
      onboardCalls++;
      return { ok: false, status: 400, message: "Nama workspace tidak valid" };
    },
    listTeams: async (): Promise<ApiResult<AgentTeam[]>> => {
      listTeamsCalls++;
      return { ok: true, status: 200, data: [] };
    },
    createAgent: async (): Promise<ApiResult<Agent>> => {
      createAgentCalls++;
      return { ok: true, status: 201, data: fakeAgent() };
    },
  };

  const outcome = await finishOnboarding(deps, baseInput());

  assert.equal(outcome.kind, "workspace_failed");
  if (outcome.kind === "workspace_failed") {
    assert.equal(outcome.message, "Nama workspace tidak valid");
  }
  assert.equal(onboardCalls, 1);
  assert.equal(listTeamsCalls, 0);
  assert.equal(createAgentCalls, 0);
});
