/**
 * Exposes the agent registry over HTTP. It talks to the domain service
 * interface only.
 *
 * Ported 1:1 from internal/modules/agent/handler/handler.go.
 */

import type { Hono } from "hono";
import { isUuid, scope as requestScope, type AppContext, type AppEnv } from "../../../platform/context.ts";
import type { Logger } from "../../../platform/logger.ts";
import { created, error, ok } from "../../../platform/response.ts";
import {
  ErrAgentHasRunningTask,
  ErrAgentLimitReached,
  ErrAgentNotFound,
  ErrIntegrationNotFound,
  ErrInvalidInput,
  ErrInvalidPermission,
  ErrInvalidStatus,
  ErrOrderIncomplete,
  ErrTeamNotFound,
  NilUUID,
  errorMessage,
  is,
  type Agent,
  type Arrangement,
  type Grant,
  type Scope,
  type Service,
  type Team,
  type Template,
  type Tool,
} from "../domain/agent.ts";

interface createAgentRequest {
  team_id: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[] | null;
  template_key: string;
  copy_from: string;
}

interface updateAgentRequest {
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[] | null;
  default_model: Record<string, unknown> | null;
}

interface setStatusRequest {
  status: string;
}

interface arrangeTeamRequest {
  team_id: string;
  agent_ids: string[];
}

interface arrangeRequest {
  teams: arrangeTeamRequest[];
}

interface setMemoryExtractionRequest {
  enabled: boolean | null;
}

interface setGrantRequest {
  integration_id: string;
  permission: string;
}

interface reasonPayload {
  kind: string;
  id: string;
  title?: string;
  status?: string;
  created_at: string;
}

interface agentPayload {
  id: string;
  team_id: string;
  team_name: string;
  team_kind: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  status: string;
  display_status: string;
  position: number;
  memory_extraction_enabled: boolean;
  reason?: reasonPayload;
  template_key?: string;
  tools: string[];
  default_model: Record<string, unknown>;
  created_at: string;
}

interface teamPayload {
  id: string;
  name: string;
  kind: string;
  agents: agentPayload[];
}

interface toolPayload {
  name: string;
  integration_app: string;
  label: string;
  description: string;
}

interface grantPayload {
  id: string;
  agent_id: string;
  integration_id: string;
  app: string;
  account_label: string;
  status: string;
  permission: string;
}

interface integrationPayload {
  id: string;
  app: string;
  account_label: string;
  status: string;
}

interface templatePayload {
  key: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[];
  integrations: string[];
}

/** Serves the registry endpoints. */
export class Handler {
  constructor(
    private readonly service: Service,
    private readonly log: Logger,
  ) {}

  /** Every Bolu of the active workspace with its derived status. */
  list = async (c: AppContext): Promise<Response> => {
    try {
      const agents = await this.service.list(scope(c));
      return ok(c, "ok", agents.map(toAgentPayload));
    } catch (err) {
      return this.fail(c, "list agents", err);
    }
  };

  /** The teams with their Bolu, which the team screen renders. */
  teams = async (c: AppContext): Promise<Response> => {
    try {
      const teams = await this.service.teams(scope(c));
      return ok(c, "ok", toTeamPayloads(teams));
    } catch (err) {
      return this.fail(c, "list teams", err);
    }
  };

  /**
   * Applies the sidebar's layout and answers with the teams as they now read.
   *
   * The answer is the stored arrangement rather than an acknowledgement,
   * because a drag is a guess about a list.
   */
  arrange = async (c: AppContext): Promise<Response> => {
    const body = await readBody<arrangeRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    const arrangement: Arrangement = { teams: [] };
    for (const team of body.teams ?? []) {
      if (!isUuid(team.team_id)) {
        return error(c, 400, "invalid team id", "invalid_request");
      }

      const agentIds: string[] = [];
      for (const raw of team.agent_ids ?? []) {
        if (!isUuid(raw)) {
          return error(c, 400, "invalid agent id", "invalid_request");
        }
        agentIds.push(raw);
      }

      arrangement.teams.push({ teamId: team.team_id, agents: agentIds });
    }

    try {
      const teams = await this.service.arrange(scope(c), arrangement);
      return ok(c, "urutan diperbarui", toTeamPayloads(teams));
    } catch (err) {
      return this.fail(c, "arrange agents", err);
    }
  };

  /** One Bolu. */
  get = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    try {
      return ok(c, "ok", toAgentPayload(await this.service.get(scope(c), id)));
    } catch (err) {
      return this.fail(c, "get agent", err);
    }
  };

  /** Adds a Bolu, from scratch, from a template, or by copying one. */
  create = async (c: AppContext): Promise<Response> => {
    const body = await readBody<createAgentRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    const teamId = parseOptionalId(body.team_id);
    if (teamId === null) return error(c, 400, "invalid team id", "invalid_request");
    const copyFrom = parseOptionalId(body.copy_from);
    if (copyFrom === null) return error(c, 400, "invalid copy_from", "invalid_request");

    try {
      const agent = await this.service.create(scope(c), {
        teamId,
        name: body.name ?? "",
        role: body.role ?? "",
        persona: body.persona ?? "",
        tone: body.tone ?? "",
        shape: body.shape ?? "",
        color: body.color ?? "",
        tools: body.tools ?? null,
        templateKey: body.template_key ?? "",
        copyFrom,
      });
      return created(c, "bolu created", toAgentPayload(agent));
    } catch (err) {
      return this.fail(c, "create agent", err);
    }
  };

  /** Edits a Bolu profile. */
  update = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    const body = await readBody<updateAgentRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    try {
      const agent = await this.service.update(scope(c), id, {
        name: body.name ?? "",
        role: body.role ?? "",
        persona: body.persona ?? "",
        tone: body.tone ?? "",
        shape: body.shape ?? "",
        color: body.color ?? "",
        tools: body.tools ?? null,
        defaultModel: body.default_model ?? null,
      });
      return ok(c, "bolu updated", toAgentPayload(agent));
    } catch (err) {
      return this.fail(c, "update agent", err);
    }
  };

  /** Flips the rest switch. */
  setStatus = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    const body = await readBody<setStatusRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    try {
      const agent = await this.service.setStatus(scope(c), id, body.status ?? "");
      return ok(c, "bolu status updated", toAgentPayload(agent));
    } catch (err) {
      return this.fail(c, "set agent status", err);
    }
  };

  /** Flips automatic memory extraction for one Bolu. */
  setMemoryExtraction = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    const body = await readBody<setMemoryExtractionRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");
    // A pointer in Go, so an absent field is a bad request rather than a silent
    // "turn it off".
    if (body.enabled === null || body.enabled === undefined) {
      return error(c, 400, "enabled is required", "invalid_request");
    }

    try {
      const agent = await this.service.setMemoryExtraction(scope(c), id, body.enabled);
      return ok(c, "memory extraction updated", toAgentPayload(agent));
    } catch (err) {
      return this.fail(c, "set memory extraction", err);
    }
  };

  /** Removes a Bolu from the registry. */
  delete = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    try {
      await this.service.delete(scope(c), id);
      return ok(c, "bolu removed");
    } catch (err) {
      return this.fail(c, "delete agent", err);
    }
  };

  /** The seeded Bolu profiles. */
  templates = async (c: AppContext): Promise<Response> => {
    try {
      const templates = await this.service.templates();
      return ok(c, "ok", templates.map(toTemplatePayload));
    } catch (err) {
      return this.fail(c, "list templates", err);
    }
  };

  /** The grants of one Bolu. */
  grants = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    try {
      const grants = await this.service.grants(scope(c), id);
      return ok(c, "ok", grants.map(toGrantPayload));
    } catch (err) {
      return this.fail(c, "list grants", err);
    }
  };

  /** Gives a Bolu access to an integration. */
  setGrant = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    const body = await readBody<setGrantRequest>(c);
    if (!body) return error(c, 400, "invalid request body", "invalid_request");

    if (!isUuid(body.integration_id)) {
      return error(c, 400, "invalid integration id", "invalid_request");
    }

    try {
      const grant = await this.service.setGrant(scope(c), id, body.integration_id, body.permission ?? "");
      return ok(c, "grant saved", toGrantPayload(grant));
    } catch (err) {
      return this.fail(c, "set grant", err);
    }
  };

  /** Revokes a grant. */
  deleteGrant = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }
    const integrationId = c.req.param("integrationID") ?? "";
    if (!isUuid(integrationId)) {
      return error(c, 400, "invalid integration id", "invalid_request");
    }

    try {
      await this.service.deleteGrant(scope(c), id, integrationId);
      return ok(c, "grant revoked");
    } catch (err) {
      return this.fail(c, "delete grant", err);
    }
  };

  /** The tool list a task may offer this Bolu. */
  allowedTools = async (c: AppContext): Promise<Response> => {
    const id = c.req.param("agentID") ?? "";
    if (!isUuid(id)) {
      return error(c, 400, "invalid agent id", "invalid_request");
    }

    try {
      const tools = await this.service.allowedTools(scope(c), id);
      return ok(c, "ok", tools.map(toToolPayload));
    } catch (err) {
      return this.fail(c, "allowed tools", err);
    }
  };

  /** Every known tool with its label. */
  toolCatalog = async (c: AppContext): Promise<Response> => {
    try {
      const tools = await this.service.toolCatalog();
      return ok(c, "ok", tools.map(toToolPayload));
    } catch (err) {
      return this.fail(c, "tool catalog", err);
    }
  };

  /** The workspace's connected applications. */
  integrations = async (c: AppContext): Promise<Response> => {
    try {
      const integrations = await this.service.integrations(scope(c));
      return ok(
        c,
        "ok",
        integrations.map(
          (integration): integrationPayload => ({
            id: integration.id,
            app: integration.app,
            account_label: integration.accountLabel,
            status: integration.status,
          }),
        ),
      );
    } catch (err) {
      return this.fail(c, "list integrations", err);
    }
  };

  private fail(c: AppContext, operation: string, err: unknown): Response {
    if (is(err, ErrAgentNotFound)) {
      return error(c, 404, "bolu not found", "agent_not_found");
    }
    if (is(err, ErrTeamNotFound)) {
      return error(c, 404, "team not found", "team_not_found");
    }
    if (is(err, ErrIntegrationNotFound)) {
      return error(c, 404, "integration not found", "integration_not_found");
    }
    if (is(err, ErrInvalidInput)) {
      return error(c, 400, errorMessage(err), "invalid_input");
    }
    if (is(err, ErrInvalidStatus)) {
      return error(c, 400, errorMessage(err), "invalid_status");
    }
    if (is(err, ErrInvalidPermission)) {
      return error(c, 400, errorMessage(err), "invalid_permission");
    }
    if (is(err, ErrOrderIncomplete)) {
      return error(c, 400, errorMessage(err), "order_incomplete");
    }
    if (is(err, ErrAgentHasRunningTask)) {
      return error(c, 409, errorMessage(err), "agent_busy");
    }
    if (is(err, ErrAgentLimitReached)) {
      return error(c, 409, errorMessage(err), "agent_limit_reached");
    }
    this.log.error({ operation, err }, "agent request failed");
    return error(c, 500, "internal server error", "internal_error");
  }
}

/** Registers the reads that describe the deployment rather than a workspace. */
export function catalogRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/agents/templates", h.templates);
  group.get("/agents/tool-catalog", h.toolCatalog);
}

/** Registers the endpoints every member of the workspace may use. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/agents", h.list);
  group.get("/agents/integrations", h.integrations);
  group.get("/teams", h.teams);
  group.get("/agents/:agentID", h.get);
  group.get("/agents/:agentID/grants", h.grants);
  group.get("/agents/:agentID/tools", h.allowedTools);
}

/** Registers the endpoints that change the registry. */
export function managerRoutes(group: Hono<AppEnv>, h: Handler): void {
  group.post("/agents", h.create);
  group.put("/agents/order", h.arrange);
  group.patch("/agents/:agentID", h.update);
  group.patch("/agents/:agentID/status", h.setStatus);
  group.patch("/agents/:agentID/memory-extraction", h.setMemoryExtraction);
  group.delete("/agents/:agentID", h.delete);
  group.put("/agents/:agentID/grants", h.setGrant);
  group.delete("/agents/:agentID/grants/:integrationID", h.deleteGrant);
}

/** The module scope from the identity the tenant middleware stored. */
function scope(c: AppContext): Scope {
  const s = requestScope(c);
  return { userId: s.userId ?? NilUUID, workspaceId: s.workspaceId ?? NilUUID };
}

async function readBody<T>(c: AppContext): Promise<T | null> {
  try {
    return (await c.req.json()) as T;
  } catch {
    return null;
  }
}

/** Parses an optional id, answering null when the value is not a uuid. */
function parseOptionalId(value: string | undefined): string | null {
  const trimmed = (value ?? "").trim();
  if (trimmed === "") return NilUUID;
  return isUuid(trimmed) ? trimmed : null;
}

function toAgentPayload(agent: Agent): agentPayload {
  const payload: agentPayload = {
    id: agent.id,
    team_id: agent.teamId,
    team_name: agent.teamName,
    team_kind: agent.teamKind,
    name: agent.name,
    role: agent.role,
    persona: agent.persona,
    tone: agent.tone,
    shape: agent.shape,
    color: agent.color,
    status: agent.status,
    display_status: agent.display,
    position: agent.position,
    memory_extraction_enabled: agent.memoryExtraction,
    tools: agent.tools ?? [],
    default_model: agent.defaultModel ?? {},
    created_at: agent.createdAt.toISOString(),
  };

  if (agent.templateKey) {
    payload.template_key = agent.templateKey;
  }

  if (agent.reason) {
    const reason: reasonPayload = {
      kind: agent.reason.kind,
      id: agent.reason.id,
      created_at: agent.reason.createdAt.toISOString(),
    };
    if (agent.reason.title) reason.title = agent.reason.title;
    if (agent.reason.status) reason.status = agent.reason.status;
    payload.reason = reason;
  }

  return payload;
}

function toTeamPayloads(teams: Team[]): teamPayload[] {
  return teams.map((team) => ({
    id: team.id,
    name: team.name,
    kind: team.kind,
    agents: team.agents.map(toAgentPayload),
  }));
}

function toGrantPayload(grant: Grant): grantPayload {
  return {
    id: grant.id,
    agent_id: grant.agentId,
    integration_id: grant.integrationId,
    app: grant.app,
    account_label: grant.accountLabel,
    status: grant.status,
    permission: grant.permission,
  };
}

function toToolPayload(tool: Tool): toolPayload {
  return {
    name: tool.name,
    integration_app: tool.integration,
    label: tool.label,
    description: tool.description,
  };
}

function toTemplatePayload(template: Template): templatePayload {
  return {
    key: template.key,
    name: template.name,
    role: template.role,
    persona: template.persona,
    tone: template.tone,
    shape: template.shape,
    color: template.color,
    tools: template.tools,
    integrations: template.integrations,
  };
}
