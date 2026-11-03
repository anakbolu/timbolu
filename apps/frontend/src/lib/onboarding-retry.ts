import type { Agent, AgentTeam, ApiResult, OnboardResult } from "@/lib/api";

export type OnboardInput = {
  name: string;
  business_field: string;
  timezone: string;
};

export type CreateAgentInput = {
  team_id: string;
  name?: string;
  template_key?: string;
  copy_from?: string;
  persona?: string;
  role?: string;
  shape?: string;
  color?: string;
};

export type OnboardingRetryDeps = {
  onboard: (input: OnboardInput) => Promise<ApiResult<OnboardResult>>;
  listTeams: (workspaceID: string) => Promise<ApiResult<AgentTeam[]>>;
  createAgent: (workspaceID: string, input: CreateAgentInput) => Promise<ApiResult<Agent>>;
};

export type OnboardingRetryInput = {
  onboarded: OnboardResult | null;
  name: string;
  businessField: string;
  timezone: string;
  boluName: string;
  boluRole: string;
  shape: string;
  color: string;
  template?: string;
};

export type OnboardingRetryOutcome =
  | { kind: "done"; onboarded: OnboardResult }
  | { kind: "workspace_failed"; message: string }
  | { kind: "bolu_failed"; onboarded: OnboardResult; message: string };

const DEFAULT_BOLU_NAME = "Oren";
const DEFAULT_BOLU_TEMPLATE = "sales";

/**
 * finishOnboarding executes workspace onboarding and first-Bolu creation
 * with idempotent retry logic:
 * - If `onboarded` already exists, skip calling `onboard()`.
 * - If `onboard()` fails, return `workspace_failed`.
 * - Before creating Bolu, query existing teams in the workspace. If an agent
 *   with the same trimmed, case-insensitive name already exists, return `done`
 *   without calling `createAgent()`.
 * - If `createAgent()` fails, return `bolu_failed` preserving the workspace.
 * - Otherwise return `done`.
 */
export async function finishOnboarding(
  deps: OnboardingRetryDeps,
  input: OnboardingRetryInput,
): Promise<OnboardingRetryOutcome> {
  let currentOnboard = input.onboarded;
  if (!currentOnboard) {
    const workspace = await deps.onboard({
      name: input.name.trim(),
      business_field: input.businessField.trim(),
      timezone: input.timezone,
    });
    if (!workspace.ok) {
      return { kind: "workspace_failed", message: workspace.message };
    }
    currentOnboard = workspace.data;
  }

  const workspaceID = currentOnboard.workspace.id;
  const targetName = input.boluName.trim() || DEFAULT_BOLU_NAME;

  const existingTeams = await deps.listTeams(workspaceID);
  if (existingTeams.ok) {
    const alreadyExists = existingTeams.data.some((t) =>
      t.agents.some((a) => a.name.trim().toLowerCase() === targetName.toLowerCase()),
    );
    if (alreadyExists) {
      return { kind: "done", onboarded: currentOnboard };
    }
  }

  const team = currentOnboard.teams[0]?.id || (existingTeams.ok ? existingTeams.data[0]?.id : undefined);
  if (team) {
    const bolu = await deps.createAgent(workspaceID, {
      team_id: team,
      name: targetName,
      role: input.boluRole.trim(),
      shape: input.shape,
      color: input.color,
      template_key: input.template ?? DEFAULT_BOLU_TEMPLATE,
    });
    if (!bolu.ok) {
      return {
        kind: "bolu_failed",
        onboarded: currentOnboard,
        message: `Workspace tersimpan, tapi Bolu pertama gagal dibuat: ${bolu.message}`,
      };
    }
  }

  return { kind: "done", onboarded: currentOnboard };
}
