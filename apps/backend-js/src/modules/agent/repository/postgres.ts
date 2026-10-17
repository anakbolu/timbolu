/**
 * The agent registry's data access.
 *
 * Every method runs inside the caller's tenant scope, so Row Level Security
 * filters the statements. The seeded templates and the tool catalogue are
 * deployment-wide facts and are read without a tenant.
 *
 * Ported 1:1 from internal/modules/agent/repository/postgres.go, with
 * repository/sqlcgen/*.go folded in: each query keeps its name as a comment and
 * its SQL text unchanged.
 */

import type { Database, Tx } from "../../../platform/database/index.ts";
import {
  ErrAgentNotFound,
  ErrIntegrationNotFound,
  ErrTeamNotFound,
  type Activity,
  type Agent,
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
  unmarshalTools,
} from "../domain/agent.ts";

/** One joined agent row, the shape both read queries return. */
interface AgentRow {
  id: string;
  workspace_id: string;
  team_id: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  status: string;
  sort_order: number;
  memory_extraction_enabled: boolean;
  template_key: string;
  default_model: unknown;
  created_at: Date;
  updated_at: Date;
  tools: unknown;
  team_name: string;
  team_kind: string;
}

/** One row of `agent_templates`. */
interface TemplateRow {
  key: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: unknown;
  integrations: unknown;
  sort_order: number;
}

/** One row of `tool_catalog`. */
interface ToolRow {
  name: string;
  integration_app: string;
  label: string;
  description: string;
  needs_connection: boolean;
}

/** The JSONB model column arrives parsed, or as text when it was double-encoded. */
function readModel(raw: unknown): Record<string, unknown> {
  if (raw === null || raw === undefined) return {};
  let value: unknown = raw;
  if (typeof value === "string") {
    if (value.length === 0) return {};
    value = JSON.parse(value);
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) return {};
  return value as Record<string, unknown>;
}

function readStrings(raw: unknown): string[] {
  return unmarshalTools(raw);
}

function toAgent(row: AgentRow): Agent {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    teamId: row.team_id,
    teamName: row.team_name,
    teamKind: row.team_kind,
    name: row.name,
    role: row.role,
    persona: row.persona,
    tone: row.tone,
    shape: row.shape,
    color: row.color,
    position: row.sort_order,
    status: row.status,
    display: "",
    reason: null,
    memoryExtraction: row.memory_extraction_enabled,
    templateKey: row.template_key,
    tools: readStrings(row.tools),
    defaultModel: readModel(row.default_model),
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function toTool(row: ToolRow): Tool {
  return {
    name: row.name,
    integration: row.integration_app,
    label: row.label,
    description: row.description,
    needsConnection: row.needs_connection,
  };
}

/** True for the Postgres foreign-key violation. */
function isForeignKeyViolation(err: unknown): boolean {
  if (typeof err !== "object" || err === null || !("code" in err)) return false;
  return err.code === "23503";
}

export class PostgresRepository implements Repository {
  constructor(private readonly db: Database) {}

  /** Every live Bolu of the workspace. */
  async list(scope: Scope): Promise<Agent[]> {
    return this.read(scope, async (tx) => {
      // name: ListAgents
      const rows = await tx<AgentRow[]>`
        SELECT a.id, a.workspace_id, a.team_id, a.name, a.role, a.persona, a.tone, a.shape, a.color, a.status, a.template_key, a.default_model, a.created_at, a.updated_at, a.deleted_at, a.tools, a.memory_extraction_enabled, a.sort_order, t.name AS team_name, t.kind AS team_kind
        FROM agents a
        JOIN teams t ON t.id = a.team_id
        WHERE a.workspace_id = ${scope.workspaceId} AND a.deleted_at IS NULL
        ORDER BY t.name, a.sort_order, a.created_at
      `;
      return rows.map(toAgent);
    });
  }

  /** One live Bolu. */
  async get(scope: Scope, id: string): Promise<Agent> {
    return this.read(scope, async (tx) => {
      // name: GetAgent
      const rows = await tx<AgentRow[]>`
        SELECT a.id, a.workspace_id, a.team_id, a.name, a.role, a.persona, a.tone, a.shape, a.color, a.status, a.template_key, a.default_model, a.created_at, a.updated_at, a.deleted_at, a.tools, a.memory_extraction_enabled, a.sort_order, t.name AS team_name, t.kind AS team_kind
        FROM agents a
        JOIN teams t ON t.id = a.team_id
        WHERE a.id = ${id} AND a.workspace_id = ${scope.workspaceId} AND a.deleted_at IS NULL
      `;
      if (rows.length === 0) throw ErrAgentNotFound;
      return toAgent(rows[0]);
    });
  }

  /** Inserts a Bolu, then reads it back so the response carries its team name. */
  async create(scope: Scope, input: NewAgent): Promise<Agent> {
    const created = await this.write(scope, async (tx) => {
      // name: CreateAgent
      const rows = await tx<{ id: string }[]>`
        INSERT INTO agents (workspace_id, team_id, name, role, persona, tone, shape, color, tools, template_key, default_model, sort_order)
        VALUES (${scope.workspaceId}, ${input.teamId}, ${input.name}, ${input.role}, ${input.persona}, ${input.tone}, ${input.shape}, ${input.color}, ${tx.json((input.tools ?? []) as never)}, ${input.templateKey}, ${tx.json((input.defaultModel ?? {}) as never)},
                (SELECT COALESCE(max(sort_order) + 1, 0) FROM agents
                 WHERE workspace_id = ${scope.workspaceId} AND team_id = ${input.teamId} AND deleted_at IS NULL))
        RETURNING id
      `;
      return rows[0].id;
    }).catch((err: unknown) => {
      // A foreign-key failure here means the team is not in this workspace,
      // which RLS also refuses.
      if (isForeignKeyViolation(err)) throw ErrTeamNotFound;
      throw err;
    });

    return this.get(scope, created);
  }

  /** Edits a Bolu profile. */
  async update(scope: Scope, id: string, req: UpdateRequest): Promise<Agent> {
    await this.write(scope, async (tx) => {
      // name: UpdateAgent
      const rows = await tx<{ id: string }[]>`
        UPDATE agents
        SET name = ${req.name},
            role = ${req.role},
            persona = ${req.persona},
            tone = ${req.tone},
            shape = ${req.shape},
            color = ${req.color},
            tools = ${tx.json((req.tools ?? []) as never)},
            default_model = ${tx.json((req.defaultModel ?? {}) as never)},
            updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
        RETURNING id
      `;
      if (rows.length === 0) throw ErrAgentNotFound;
    });

    return this.get(scope, id);
  }

  /** Flips the switch that decides whether a quiet conversation is read into memory. */
  async setMemoryExtraction(scope: Scope, id: string, enabled: boolean): Promise<Agent> {
    await this.write(scope, async (tx) => {
      // name: SetMemoryExtraction
      const rows = await tx<{ id: string }[]>`
        UPDATE agents
        SET memory_extraction_enabled = ${enabled}, updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
        RETURNING id
      `;
      if (rows.length === 0) throw ErrAgentNotFound;
    });

    return this.get(scope, id);
  }

  /** Flips the rest switch. */
  async setStatus(scope: Scope, id: string, status: string): Promise<Agent> {
    await this.write(scope, async (tx) => {
      // name: SetAgentStatus
      const rows = await tx<{ id: string }[]>`
        UPDATE agents
        SET status = ${status}, updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
        RETURNING id
      `;
      if (rows.length === 0) throw ErrAgentNotFound;
    });

    return this.get(scope, id);
  }

  /** Marks a Bolu deleted. Its tasks, drafts, and messages stay. */
  async delete(scope: Scope, id: string): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: SoftDeleteAgent
      const rows = await tx`
        UPDATE agents
        SET deleted_at = now(), updated_at = now()
        WHERE id = ${id} AND workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
      `;
      if (rows.count === 0) throw ErrAgentNotFound;
    });
  }

  /**
   * Writes a whole arrangement in one transaction.
   *
   * A half-applied arrangement is worse than a refused one: a Bolu that moved
   * into its new team while the one it displaced kept the old position would
   * leave two rows claiming the same slot.
   */
  async setPlacements(scope: Scope, placements: Placement[]): Promise<void> {
    await this.write(scope, async (tx) => {
      for (const placement of placements) {
        // name: SetAgentPlacement
        const rows = await tx<{ id: string }[]>`
          UPDATE agents
          SET team_id = ${placement.teamId}, sort_order = ${placement.position}
          WHERE id = ${placement.agentId} AND workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
          RETURNING id
        `;
        if (rows.length === 0) {
          // The Bolu was deleted between the service's read and this write,
          // which is the one case the arrangement cannot cover.
          throw ErrAgentNotFound;
        }
      }
    });
  }

  /** The status inputs for every live Bolu at once. */
  async activity(scope: Scope): Promise<Map<string, Activity>> {
    return this.read(scope, async (tx) => {
      // name: AgentActivity
      const rows = await tx<{ id: string; working: boolean; waiting: boolean }[]>`
        SELECT a.id,
               EXISTS (
                   SELECT 1 FROM tasks t
                   WHERE t.agent_id = a.id AND t.status IN ('queued', 'running')
               ) AS working,
               EXISTS (
                   SELECT 1 FROM drafts d
                   WHERE d.agent_id = a.id AND d.status = 'pending'
               ) AS waiting
        FROM agents a
        WHERE a.workspace_id = ${scope.workspaceId} AND a.deleted_at IS NULL
      `;
      const activity = new Map<string, Activity>();
      for (const row of rows) {
        activity.set(row.id, { working: row.working, waiting: row.waiting });
      }
      return activity;
    });
  }

  /** The status inputs for one Bolu. */
  async activityFor(scope: Scope, id: string): Promise<Activity> {
    return this.read(scope, async (tx) => {
      // name: AgentActivityByID
      const rows = await tx<{ id: string; working: boolean; waiting: boolean }[]>`
        SELECT a.id,
               EXISTS (
                   SELECT 1 FROM tasks t
                   WHERE t.agent_id = a.id AND t.status IN ('queued', 'running')
               ) AS working,
               EXISTS (
                   SELECT 1 FROM drafts d
                   WHERE d.agent_id = a.id AND d.status = 'pending'
               ) AS waiting
        FROM agents a
        WHERE a.id = ${id} AND a.workspace_id = ${scope.workspaceId} AND a.deleted_at IS NULL
      `;
      if (rows.length === 0) throw ErrAgentNotFound;
      return { working: rows[0].working, waiting: rows[0].waiting };
    });
  }

  /** Explains a working or waiting status: the live task, else the pending draft. */
  async reasonFor(scope: Scope, id: string): Promise<Reason | null> {
    return this.read(scope, async (tx) => {
      // name: LatestTaskForAgent
      const tasks = await tx<{ id: string; title: string; status: string; created_at: Date }[]>`
        SELECT id, title, status, created_at
        FROM tasks
        WHERE agent_id = ${id} AND status IN ('queued', 'running')
        ORDER BY created_at DESC
        LIMIT 1
      `;
      if (tasks.length > 0) {
        const task = tasks[0];
        return { kind: "task", id: task.id, title: task.title, status: task.status, createdAt: task.created_at };
      }

      // name: LatestPendingDraftForAgent
      const drafts = await tx<{ id: string; action_kind: string; created_at: Date }[]>`
        SELECT id, action_kind, created_at
        FROM drafts
        WHERE agent_id = ${id} AND status = 'pending'
        ORDER BY created_at DESC
        LIMIT 1
      `;
      if (drafts.length > 0) {
        const draft = drafts[0];
        return { kind: "draft", id: draft.id, title: draft.action_kind, status: "pending", createdAt: draft.created_at };
      }

      return null;
    });
  }

  /** Counts the tasks in flight for one Bolu. */
  async runningTasks(scope: Scope, id: string): Promise<number> {
    return this.read(scope, async (tx) => {
      // name: CountRunningTasksForAgent
      const rows = await tx<{ count: number }[]>`
        SELECT count(*)::bigint FROM tasks
        WHERE agent_id = ${id} AND status IN ('queued', 'running')
      `;
      return rows[0].count;
    });
  }

  /** Counts the live Bolu of the workspace, which the plan limit uses. */
  async countLive(scope: Scope): Promise<number> {
    return this.read(scope, async (tx) => {
      // name: CountLiveAgents
      const rows = await tx<{ count: number }[]>`
        SELECT count(*)::bigint FROM agents
        WHERE workspace_id = ${scope.workspaceId} AND deleted_at IS NULL
      `;
      return rows[0].count;
    });
  }

  /** The teams of the workspace with their Bolu. */
  async teams(scope: Scope): Promise<Team[]> {
    const agents = await this.list(scope);

    return this.read(scope, async (tx) => {
      // name: ListWorkspaceTeams
      const rows = await tx<{ id: string; name: string; kind: string }[]>`
        SELECT id, workspace_id, name, kind, created_at, updated_at FROM teams WHERE workspace_id = ${scope.workspaceId} ORDER BY name
      `;

      const byTeam = new Map<string, Agent[]>();
      for (const agent of agents) {
        const bucket = byTeam.get(agent.teamId);
        if (bucket) bucket.push(agent);
        else byTeam.set(agent.teamId, [agent]);
      }

      return rows.map((row) => ({
        id: row.id,
        name: row.name,
        kind: row.kind,
        agents: byTeam.get(row.id) ?? [],
      }));
    });
  }

  /** The seeded profiles, in their display order. */
  async templates(): Promise<Template[]> {
    return this.readPublic(async (tx) => {
      // name: ListAgentTemplates
      const rows = await tx<TemplateRow[]>`
        SELECT id, key, name, role, persona, tone, shape, color, tools, integrations, default_model, sort_order, created_at FROM agent_templates ORDER BY sort_order
      `;
      return rows.map((row) => ({
        key: row.key,
        name: row.name,
        role: row.role,
        persona: row.persona,
        tone: row.tone,
        shape: row.shape,
        color: row.color,
        tools: readStrings(row.tools),
        integrations: readStrings(row.integrations),
        sortOrder: row.sort_order,
      }));
    });
  }

  /** One seeded template. */
  async template(key: string): Promise<Template> {
    return this.readPublic(async (tx) => {
      // name: GetAgentTemplate
      const rows = await tx<TemplateRow[]>`
        SELECT id, key, name, role, persona, tone, shape, color, tools, integrations, default_model, sort_order, created_at FROM agent_templates WHERE key = ${key}
      `;
      if (rows.length === 0) throw ErrAgentNotFound;
      const row = rows[0];
      return {
        key: row.key,
        name: row.name,
        role: row.role,
        persona: row.persona,
        tone: row.tone,
        shape: row.shape,
        color: row.color,
        tools: readStrings(row.tools),
        integrations: readStrings(row.integrations),
        sortOrder: row.sort_order,
      };
    });
  }

  /** Converts a seeded template into a new agent row. */
  async agentTemplate(key: string): Promise<NewAgent> {
    const template = await this.template(key);
    return {
      teamId: "",
      name: template.name,
      role: template.role,
      persona: template.persona,
      tone: template.tone,
      shape: template.shape,
      color: template.color,
      tools: template.tools,
      templateKey: template.key,
      defaultModel: null,
    };
  }

  /** The grants of one Bolu. */
  async grants(scope: Scope, agentId: string): Promise<Grant[]> {
    return this.read(scope, async (tx) => {
      // name: ListGrants
      const rows = await tx<
        {
          id: string;
          agent_id: string;
          integration_id: string;
          permission: string;
          integration_app: string;
          account_label: string;
          integration_status: string;
        }[]
      >`
        SELECT g.id, g.agent_id, g.integration_id, g.permission, g.created_at, g.updated_at,
               i.app AS integration_app, i.account_label, i.status AS integration_status
        FROM agent_grants g
        JOIN integrations i ON i.id = g.integration_id
        WHERE g.workspace_id = ${scope.workspaceId} AND g.agent_id = ${agentId}
        ORDER BY i.app
      `;
      return rows.map((row) => ({
        id: row.id,
        agentId: row.agent_id,
        integrationId: row.integration_id,
        app: row.integration_app,
        accountLabel: row.account_label,
        status: row.integration_status,
        permission: row.permission,
      }));
    });
  }

  /** Creates or updates a grant. */
  async setGrant(scope: Scope, agentId: string, integrationId: string, permission: string): Promise<Grant> {
    return this.write(scope, async (tx) => {
      // name: UpsertGrant
      let rows: { id: string; agent_id: string; integration_id: string; permission: string }[];
      try {
        rows = await tx<{ id: string; agent_id: string; integration_id: string; permission: string }[]>`
          INSERT INTO agent_grants (workspace_id, agent_id, integration_id, permission)
          VALUES (${scope.workspaceId}, ${agentId}, ${integrationId}, ${permission})
          ON CONFLICT (agent_id, integration_id)
          DO UPDATE SET permission = EXCLUDED.permission, updated_at = now()
          RETURNING id, workspace_id, agent_id, integration_id, permission, created_at, updated_at
        `;
      } catch (err) {
        if (isForeignKeyViolation(err)) throw ErrIntegrationNotFound;
        throw err;
      }

      const integration = await this.integrationIn(tx, scope, integrationId);
      const row = rows[0];
      return {
        id: row.id,
        agentId: row.agent_id,
        integrationId: row.integration_id,
        app: integration.app,
        accountLabel: integration.accountLabel,
        status: integration.status,
        permission: row.permission,
      };
    });
  }

  /** Revokes a grant, which immediately removes the matching tools. */
  async deleteGrant(scope: Scope, agentId: string, integrationId: string): Promise<void> {
    await this.write(scope, async (tx) => {
      // name: DeleteGrant
      const rows = await tx`
        DELETE FROM agent_grants
        WHERE workspace_id = ${scope.workspaceId} AND agent_id = ${agentId} AND integration_id = ${integrationId}
      `;
      if (rows.count === 0) throw ErrIntegrationNotFound;
    });
  }

  /**
   * The effective tool list: what the agent asks for, restricted to the
   * integrations it was actually granted.
   *
   * A read-only grant keeps only the `read`-labelled tools. A tool that belongs
   * to the workspace itself (`needs_connection = false`) needs no grant.
   */
  async allowedTools(scope: Scope, agentId: string): Promise<Tool[]> {
    return this.read(scope, async (tx) => {
      // name: AllowedToolsForAgent
      const rows = await tx<ToolRow[]>`
        SELECT tc.name, tc.integration_app, tc.label, tc.description, tc.needs_connection
        FROM agents a
        JOIN tool_catalog tc ON tc.name IN (SELECT jsonb_array_elements_text(a.tools))
        WHERE a.id = ${agentId} AND a.workspace_id = ${scope.workspaceId} AND a.deleted_at IS NULL
          AND (
            NOT tc.needs_connection
            OR EXISTS (
              SELECT 1
              FROM integrations i
              JOIN agent_grants g ON g.agent_id = a.id AND g.integration_id = i.id
              WHERE i.workspace_id = a.workspace_id
                AND i.app = tc.integration_app
                AND (g.permission = 'read_write' OR tc.label = 'read')
            )
          )
        ORDER BY tc.name
      `;
      return rows.map(toTool);
    });
  }

  /** Every known tool. */
  async toolCatalog(): Promise<Tool[]> {
    return this.readPublic(async (tx) => {
      // name: ListToolCatalog
      const rows = await tx<ToolRow[]>`
        SELECT name, integration_app, label, description, created_at, needs_connection FROM tool_catalog ORDER BY integration_app, name
      `;
      return rows.map(toTool);
    });
  }

  /** The workspace's connected applications. */
  async integrations(scope: Scope): Promise<Integration[]> {
    return this.read(scope, async (tx) => {
      // name: ListIntegrations
      const rows = await tx<{ id: string; app: string; account_label: string; status: string }[]>`
        SELECT id, workspace_id, app, account_label, status, scopes, access_token_encrypted, refresh_token_encrypted, token_expires_at, created_at, updated_at, status_reason, last_refreshed_at FROM integrations WHERE workspace_id = ${scope.workspaceId} ORDER BY app, account_label
      `;
      return rows.map((row) => ({
        id: row.id,
        app: row.app,
        accountLabel: row.account_label,
        status: row.status,
      }));
    });
  }

  /** Finds one integration inside an open transaction. */
  private async integrationIn(tx: Tx, scope: Scope, integrationId: string): Promise<Integration> {
    // name: ListIntegrations (filtered in memory, as the Go code does)
    const rows = await tx<{ id: string; app: string; account_label: string; status: string }[]>`
      SELECT id, workspace_id, app, account_label, status, scopes, access_token_encrypted, refresh_token_encrypted, token_expires_at, created_at, updated_at, status_reason, last_refreshed_at FROM integrations WHERE workspace_id = ${scope.workspaceId} ORDER BY app, account_label
    `;
    for (const row of rows) {
      if (row.id === integrationId) {
        return { id: row.id, app: row.app, accountLabel: row.account_label, status: row.status };
      }
    }
    throw ErrIntegrationNotFound;
  }

  private async read<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }

  private async write<T>(scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScope({ userId: scope.userId, workspaceId: scope.workspaceId }, fn);
  }

  /**
   * Runs a query that is not tenant scoped. It still opens a transaction,
   * because every statement here is written against one.
   */
  private async readPublic<T>(fn: (tx: Tx) => Promise<T>): Promise<T> {
    return this.db.inScopeRead({ userId: crypto.randomUUID() }, fn);
  }
}
