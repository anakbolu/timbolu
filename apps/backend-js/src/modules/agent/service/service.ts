/**
 * The agent registry use cases: the Bolu profile CRUD, the derived status, the
 * teams, and the tool grants.
 *
 * Ported 1:1 from internal/modules/agent/service/service.go.
 */

import {
  DisplayWaiting,
  DisplayWorking,
  ErrAgentHasRunningTask,
  ErrAgentLimitReached,
  ErrAgentNotFound,
  ErrInvalidInput,
  ErrInvalidPermission,
  ErrInvalidStatus,
  ErrOrderIncomplete,
  NilUUID,
  PermissionRead,
  PermissionReadWrite,
  StoredActive,
  StoredResting,
  acceptsNewTasks as storedAcceptsNewTasks,
  deriveStatus,
  is,
  isZeroScope,
  wrap,
  type Agent,
  type Arrangement,
  type CreateRequest,
  type NewAgent,
  type PlanLimits,
  type Repository,
  type Scope,
  type Service as ServicePort,
  type Team,
  type Template,
  type Tool,
  type UpdateRequest,
} from "../domain/agent.ts";
import { BOLU_SHAPE_NAMES, DEFAULT_BOLU_SHAPE, isValidBoluShape } from "./shape.ts";

/** Bounds the persona field. A persona is injected into every prompt. */
export const PersonaMaxLength = 4000;

/** Bounds the display name. */
export const NameMaxLength = 60;

/**
 * The refusal for a body `shape` outside the 16 the character package draws.
 * Storing one would hand every renderer a name it cannot draw (#564).
 */
const SHAPE_NOT_KNOWN = `shape must be one of: ${BOLU_SHAPE_NAMES.join(", ")}`;

/** The service settings. */
export interface Config {
  /**
   * Caps the registry when the plan does not say otherwise. Zero means the
   * plan decides alone.
   */
  maxAgentsPerWorkspace: number;
}

/** The service dependencies. */
export interface ServiceDeps {
  repository: Repository;
  /** Reports the plan's Bolu allowance. Optional. */
  limits?: PlanLimits | null;
  config?: Partial<Config>;
}

/** The concrete service behind `ServicePort`. */
export class Service implements ServicePort {
  private readonly repo: Repository;
  private limits: PlanLimits | null;
  private readonly cfg: Config;

  constructor(deps: ServiceDeps) {
    this.repo = deps.repository;
    this.limits = deps.limits ?? null;
    this.cfg = { maxAgentsPerWorkspace: deps.config?.maxAgentsPerWorkspace ?? 0 };
  }

  /** Installs the plan's cap after construction, as the container does. */
  usePlanLimits(limits: PlanLimits): void {
    this.limits = limits;
  }

  /** Every live Bolu with its derived status. */
  async list(scope: Scope): Promise<Agent[]> {
    requireScope(scope);

    const agents = await this.repo.list(scope);
    const activity = await this.repo.activity(scope);

    for (const agent of agents) {
      agent.display = deriveStatus(agent.status, activity.get(agent.id));
    }

    return agents;
  }

  /** One Bolu with its derived status and the reason behind it. */
  async get(scope: Scope, id: string): Promise<Agent> {
    requireScope(scope);
    return this.decorate(scope, await this.repo.get(scope, id));
  }

  /**
   * Adds a Bolu. The profile comes from the request, from a template, or from
   * an existing Bolu, in that order of precedence.
   */
  async create(scope: Scope, req: CreateRequest): Promise<Agent> {
    requireScope(scope);

    const input = await this.buildNewAgent(scope, req);
    await this.checkLimit(scope);

    const created = await this.repo.create(scope, input);
    return this.decorate(scope, created);
  }

  /** Edits a Bolu profile. */
  async update(scope: Scope, id: string, req: UpdateRequest): Promise<Agent> {
    requireScope(scope);

    const name = (req.name ?? "").trim();
    if (name === "") {
      throw wrap(ErrInvalidInput, "name is required");
    }
    if (charLength(name) > NameMaxLength) {
      throw wrap(ErrInvalidInput, `name is longer than ${NameMaxLength} characters`);
    }

    if (req.persona !== undefined && req.persona !== null && charLength(req.persona) > PersonaMaxLength) {
      throw wrap(ErrInvalidInput, `persona is longer than ${PersonaMaxLength} characters`);
    }

    // An edit carries the shape it wants or nothing at all; a name the
    // character package cannot draw must not reach the row (#564).
    if (req.shape !== undefined && req.shape !== null) {
      const shape = req.shape.trim();
      if (shape !== "" && !isValidBoluShape(shape)) throw wrap(ErrInvalidInput, SHAPE_NOT_KNOWN);
    }

    // Single read-modify-write pattern: load the existing row once so any
    // field omitted by the patch keeps its stored profile value instead of
    // being wiped out with empty defaults (#568).
    const existing = await this.repo.get(scope, id);

    const shape =
      req.shape !== undefined && req.shape !== null ? req.shape.trim() : existing.shape;
    const role =
      req.role !== undefined && req.role !== null ? req.role.trim() : existing.role;
    const persona =
      req.persona !== undefined && req.persona !== null ? req.persona.trim() : existing.persona;
    const tone =
      req.tone !== undefined && req.tone !== null ? req.tone.trim() : existing.tone;
    const color =
      req.color !== undefined && req.color !== null ? req.color.trim() : existing.color;
    const tools =
      req.tools !== undefined && req.tools !== null ? req.tools : existing.tools;
    const defaultModel =
      req.defaultModel !== undefined && req.defaultModel !== null
        ? req.defaultModel
        : existing.defaultModel;

    const updated = await this.repo.update(scope, id, {
      name,
      role,
      persona,
      tone,
      shape,
      color,
      tools,
      defaultModel,
    });

    return this.decorate(scope, updated);
  }

  /** Flips the rest switch. */
  async setStatus(scope: Scope, id: string, status: string): Promise<Agent> {
    requireScope(scope);
    if (status !== StoredActive && status !== StoredResting) {
      throw wrap(ErrInvalidStatus, `"${status}"`);
    }

    return this.decorate(scope, await this.repo.setStatus(scope, id, status));
  }

  /** Flips automatic memory extraction for one Bolu. */
  async setMemoryExtraction(scope: Scope, id: string, enabled: boolean): Promise<Agent> {
    requireScope(scope);
    return this.decorate(scope, await this.repo.setMemoryExtraction(scope, id, enabled));
  }

  /**
   * Removes a Bolu from the registry. Refuses while the Bolu is still working,
   * because the task would be left without its agent.
   */
  async delete(scope: Scope, id: string): Promise<void> {
    requireScope(scope);

    const running = await this.repo.runningTasks(scope, id);
    if (running > 0) {
      throw wrap(ErrAgentHasRunningTask, `(${running} task berjalan)`);
    }

    await this.repo.delete(scope, id);
  }

  /**
   * Applies the sidebar's layout and answers with the teams as they now read.
   *
   * The requested layout is checked against the workspace's own registry before
   * anything is written, because a layout that forgets a Bolu and a layout that
   * names a foreign one are both destructive.
   */
  async arrange(scope: Scope, arrangement: Arrangement): Promise<Team[]> {
    requireScope(scope);

    const current = await this.repo.teams(scope);
    const placements = placementsFor(current, arrangement);

    await this.repo.setPlacements(scope, placements);
    return this.teams(scope);
  }

  /** The teams with their Bolu, each carrying its derived status. */
  async teams(scope: Scope): Promise<Team[]> {
    requireScope(scope);

    const teams = await this.repo.teams(scope);
    const activity = await this.repo.activity(scope);

    for (const team of teams) {
      for (const agent of team.agents) {
        agent.display = deriveStatus(agent.status, activity.get(agent.id));
      }
    }

    return teams;
  }

  /** The seeded Bolu profiles. */
  async templates(): Promise<Template[]> {
    return this.repo.templates();
  }

  /** The grants of one Bolu. */
  async grants(scope: Scope, agentId: string) {
    requireScope(scope);
    await this.repo.get(scope, agentId);
    return this.repo.grants(scope, agentId);
  }

  /**
   * Gives a Bolu access to an integration. Revoking a grant immediately removes
   * the matching tools from the next task's tool list.
   */
  async setGrant(scope: Scope, agentId: string, integrationId: string, permission: string) {
    requireScope(scope);
    if (permission !== PermissionRead && permission !== PermissionReadWrite) {
      throw wrap(ErrInvalidPermission, `"${permission}"`);
    }
    await this.repo.get(scope, agentId);

    return this.repo.setGrant(scope, agentId, integrationId, permission);
  }

  /** Revokes a grant. */
  async deleteGrant(scope: Scope, agentId: string, integrationId: string): Promise<void> {
    requireScope(scope);
    await this.repo.deleteGrant(scope, agentId, integrationId);
  }

  /** The tool list a task may offer this Bolu. */
  async allowedTools(scope: Scope, agentId: string): Promise<Tool[]> {
    requireScope(scope);
    await this.repo.get(scope, agentId);
    return this.repo.allowedTools(scope, agentId);
  }

  /** Every known tool with its label. */
  async toolCatalog(): Promise<Tool[]> {
    return this.repo.toolCatalog();
  }

  /** The workspace's connected applications. */
  async integrations(scope: Scope) {
    requireScope(scope);
    return this.repo.integrations(scope);
  }

  /** Reports whether a Bolu may be given work. */
  async acceptsNewTasks(scope: Scope, id: string): Promise<boolean> {
    const agent = await this.repo.get(scope, id);
    return storedAcceptsNewTasks(agent.status);
  }

  /** Fills in the derived status and the reason behind it. */
  private async decorate(scope: Scope, agent: Agent): Promise<Agent> {
    const activity = await this.repo.activityFor(scope, agent.id);

    agent.display = deriveStatus(agent.status, activity);
    if (agent.display === DisplayWorking || agent.display === DisplayWaiting) {
      agent.reason = await this.repo.reasonFor(scope, agent.id);
    }

    return agent;
  }

  /** Resolves the profile to insert. */
  private async buildNewAgent(scope: Scope, req: CreateRequest): Promise<NewAgent> {
    if (req.teamId === NilUUID || req.teamId === "") {
      throw wrap(ErrInvalidInput, "team is required");
    }

    let input: NewAgent = {
      teamId: "",
      name: "",
      role: "",
      persona: "",
      tone: "",
      shape: "",
      color: "",
      tools: null,
      templateKey: "",
      defaultModel: null,
    };

    if (req.copyFrom !== NilUUID && req.copyFrom !== "") {
      const source = await this.repo.get(scope, req.copyFrom);
      input = {
        teamId: "",
        name: `${source.name} (salinan)`,
        role: source.role,
        persona: source.persona,
        tone: source.tone,
        shape: source.shape,
        color: source.color,
        tools: source.tools,
        templateKey: "",
        defaultModel: source.defaultModel,
      };
    } else if (req.templateKey.trim() !== "") {
      try {
        input = await this.repo.agentTemplate(req.templateKey);
      } catch (err) {
        // A template key names one of the seeded profiles, so a key that names
        // none is a wrong body rather than a missing Bolu. The repository
        // reports the lookup miss with ErrAgentNotFound, which the handler
        // would otherwise answer as 404 agent_not_found and send the client
        // looking for an object that never existed.
        if (!is(err, ErrAgentNotFound)) throw err;
        throw wrap(ErrInvalidInput, `template "${req.templateKey}" is not one of the agent templates`);
      }
    }

    // Explicit request fields win over the copied profile.
    const name = req.name.trim();
    if (name !== "") {
      input.name = name;
    }
    if (input.name === "") {
      throw wrap(ErrInvalidInput, "name is required");
    }
    if (charLength(input.name) > NameMaxLength) {
      throw wrap(ErrInvalidInput, `name is longer than ${NameMaxLength} characters`);
    }
    if (req.role !== "") {
      input.role = req.role.trim();
    }
    if (req.persona !== "") {
      input.persona = req.persona.trim();
    }
    if (req.tone !== "") {
      input.tone = req.tone.trim();
    }
    if (req.shape !== "") {
      const shape = req.shape.trim();
      if (!isValidBoluShape(shape)) throw wrap(ErrInvalidInput, SHAPE_NOT_KNOWN);
      input.shape = shape;
    }
    if (req.color !== "") {
      input.color = req.color.trim();
    }
    if (req.tools !== null && req.tools !== undefined) {
      input.tools = req.tools;
    }
    if (input.shape === "") {
      input.shape = DEFAULT_BOLU_SHAPE;
    }

    if (charLength(input.persona) > PersonaMaxLength) {
      throw wrap(ErrInvalidInput, `persona is longer than ${PersonaMaxLength} characters`);
    }

    input.teamId = req.teamId;
    return input;
  }

  /**
   * Enforces the Bolu allowance of the plan. The plan wins when it reports a
   * limit; otherwise the configured cap applies, and with neither the registry
   * is unlimited.
   */
  private async checkLimit(scope: Scope): Promise<void> {
    let limit = this.cfg.maxAgentsPerWorkspace;
    if (this.limits) {
      const fromPlan = await this.limits.maxAgents(scope.workspaceId);
      if (fromPlan > 0) {
        limit = fromPlan;
      }
    }
    if (limit <= 0) {
      return;
    }

    const count = await this.repo.countLive(scope);
    if (count >= limit) {
      throw wrap(ErrAgentLimitReached, `(batas ${limit} Bolu)`);
    }
  }
}

/** Counts characters as Go's `len([]rune(s))` does. */
function charLength(value: string): number {
  return Array.from(value).length;
}

function requireScope(scope: Scope): void {
  if (isZeroScope(scope) || scope.workspaceId === NilUUID || !scope.workspaceId) {
    throw wrap(ErrInvalidInput, "a workspace scope is required");
  }
}

/**
 * Turns a requested layout into one position per Bolu.
 *
 * A layout that forgets a Bolu or names a foreign one is refused whole: the
 * rows it forgot would keep their old positions and the sidebar would
 * interleave two arrangements.
 */
function placementsFor(current: Team[], arrangement: Arrangement) {
  const memberTeam = new Map<string, string>();
  for (const team of current) {
    for (const agent of team.agents) {
      memberTeam.set(agent.id, team.id);
    }
  }

  const knownTeams = new Set(current.map((team) => team.id));
  const seenTeams = new Set<string>();
  const seenAgents = new Set<string>();

  const placements: { agentId: string; teamId: string; position: number }[] = [];
  for (const team of arrangement.teams) {
    if (!knownTeams.has(team.teamId)) {
      throw wrap(ErrOrderIncomplete, `team ${team.teamId} is not in this workspace`);
    }
    if (seenTeams.has(team.teamId)) {
      throw wrap(ErrOrderIncomplete, `team ${team.teamId} is listed twice`);
    }
    seenTeams.add(team.teamId);

    for (let position = 0; position < team.agents.length; position++) {
      const agentId = team.agents[position];
      if (!memberTeam.has(agentId)) {
        throw wrap(ErrOrderIncomplete, `bolu ${agentId} is not in this workspace`);
      }
      if (seenAgents.has(agentId)) {
        throw wrap(ErrOrderIncomplete, `bolu ${agentId} is listed twice`);
      }
      seenAgents.add(agentId);

      placements.push({ agentId, teamId: team.teamId, position });
    }
  }

  for (const teamId of knownTeams) {
    if (!seenTeams.has(teamId)) {
      throw wrap(ErrOrderIncomplete, `team ${teamId} is missing`);
    }
  }
  for (const agentId of memberTeam.keys()) {
    if (!seenAgents.has(agentId)) {
      throw wrap(ErrOrderIncomplete, `bolu ${agentId} is missing`);
    }
  }

  return placements;
}
