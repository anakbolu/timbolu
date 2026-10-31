/**
 * The agent registry contracts: the Bolu profile, the derived display status,
 * the tool and grant shapes, and the repository and service interfaces.
 *
 * The display status is never stored. Only the rest switch is a column; the
 * rest is computed from the tasks and drafts that reference the agent, so the
 * registry cannot drift from the work.
 *
 * Ported 1:1 from internal/modules/agent/domain/agent.go.
 */

/** The agent's own switch. */
export const StoredActive = "active";
export const StoredResting = "resting";

/** What the UI shows. It is derived, never stored. */
export const DisplayWorking = "working";
export const DisplayWaiting = "waiting";
export const DisplayIdle = "idle";
export const DisplayResting = "resting";

/** Tool labels decide whether a call needs human approval. */
export const LabelRead = "read";
export const LabelWriteInternal = "write_internal";
export const LabelWriteExternal = "write_external";

/** Grant permissions. */
export const PermissionRead = "read";
export const PermissionReadWrite = "read_write";

/** The zero uuid, as Go's `uuid.Nil`. */
export const NilUUID = "00000000-0000-0000-0000-000000000000";

/** Errors returned by the service. */
export const ErrAgentNotFound = new Error("agent: bolu not found");
export const ErrTeamNotFound = new Error("agent: team not found");
export const ErrInvalidInput = new Error("agent: invalid input");
export const ErrInvalidStatus = new Error("agent: unknown status");
export const ErrInvalidPermission = new Error("agent: unknown permission");
export const ErrAgentHasRunningTask = new Error("agent: this bolu is still working on a task");
export const ErrAgentLimitReached = new Error("agent: the plan's bolu limit is reached");
export const ErrIntegrationNotFound = new Error("agent: integration not found");
/** The arrangement a client sent is not the workspace's registry. */
export const ErrOrderIncomplete = new Error("agent: the arrangement does not list every bolu exactly once");

/**
 * An error carrying the sentinel a caller switches on, so `is` reproduces Go's
 * `errors.Is` for the messages `fmt.Errorf("%w: ...")` produced.
 */
export class WrappedError extends Error {
  constructor(
    readonly sentinel: Error,
    message: string,
  ) {
    super(message);
    this.name = "WrappedError";
  }
}

/** Wraps a sentinel with a detail, rendering the message as Go's `%w: detail`. */
export function wrap(sentinel: Error, detail: string): WrappedError {
  return new WrappedError(sentinel, `${sentinel.message}: ${detail}`);
}

/** True when `err` is the sentinel or wraps it. */
export function is(err: unknown, sentinel: Error): boolean {
  let current: unknown = err;
  for (let guard = 0; guard < 32; guard++) {
    if (current === sentinel) return true;
    if (!(current instanceof Error)) return false;
    if (current instanceof WrappedError) {
      current = current.sentinel;
      continue;
    }
    current = "cause" in current ? current.cause : undefined;
  }
  return false;
}

/** The message of an unknown thrown value. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Why an agent is working or waiting. */
export interface Reason {
  /** task | draft */
  kind: string;
  id: string;
  title: string;
  status: string;
  createdAt: Date;
}

/** One Bolu. */
export interface Agent {
  id: string;
  workspaceId: string;
  teamId: string;
  teamName: string;
  teamKind: string;

  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;

  /** Where the Bolu sits in its team's list, counting from zero. */
  position: number;

  /** The stored switch (active or resting). */
  status: string;
  /** The derived status the UI shows. */
  display: string;
  /** Explains a working or waiting status. */
  reason: Reason | null;

  /** Whether a quiet conversation is read into memory automatically. */
  memoryExtraction: boolean;

  templateKey: string;
  tools: string[];
  defaultModel: Record<string, unknown>;

  createdAt: Date;
  updatedAt: Date;
}

/** Groups the Bolu of one workspace. */
export interface Team {
  id: string;
  name: string;
  kind: string;
  agents: Agent[];
}

/** One entry of the tool catalogue. */
export interface Tool {
  name: string;
  /** The integration app the tool belongs to. */
  integration: string;
  label: string;
  description: string;
  /** False for a tool that belongs to the workspace itself. */
  needsConnection: boolean;
}

/** Gives one agent access to one integration. */
export interface Grant {
  id: string;
  agentId: string;
  integrationId: string;
  app: string;
  accountLabel: string;
  status: string;
  permission: string;
}

/** A connected application of the workspace. */
export interface Integration {
  id: string;
  app: string;
  accountLabel: string;
  status: string;
}

/** The raw input of the derived status. */
export interface Activity {
  working: boolean;
  waiting: boolean;
}

/**
 * Turns the stored switch and the activity into the display status.
 *
 * The order matters: a resting Bolu is resting even if it still has a task
 * finishing up, and work outranks a pending draft.
 */
export function deriveStatus(stored: string, activity: Activity | undefined): string {
  if (stored === StoredResting) {
    return DisplayResting;
  }
  if (activity?.working) {
    return DisplayWorking;
  }
  if (activity?.waiting) {
    return DisplayWaiting;
  }
  return DisplayIdle;
}

/** True when the agent may be given new work. */
export function acceptsNewTasks(stored: string): boolean {
  return stored === StoredActive;
}

/** A new Bolu. `copyFrom` copies an existing profile; `templateKey` starts from a template. */
export interface CreateRequest {
  teamId: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[] | null;
  templateKey: string;
  copyFrom: string;
}

/** Edits a Bolu profile. */
export interface UpdateRequest {
  name: string;
  role?: string | null;
  persona?: string | null;
  tone?: string | null;
  shape?: string | null;
  color?: string | null;
  tools?: string[] | null;
  defaultModel?: Record<string, unknown> | null;
}

/** One team's row of the sidebar, in order. */
export interface TeamArrangement {
  teamId: string;
  /** The team's Bolu, first to last. Must name every live Bolu exactly once. */
  agents: string[];
}

/** The sidebar's layout: which team each Bolu is in, and in what order. */
export interface Arrangement {
  teams: TeamArrangement[];
}

/** One Bolu's resolved position. */
export interface Placement {
  agentId: string;
  teamId: string;
  position: number;
}

/** The row to insert. */
export interface NewAgent {
  teamId: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[] | null;
  templateKey: string;
  defaultModel: Record<string, unknown> | null;
}

/** A seeded Bolu profile. */
export interface Template {
  key: string;
  name: string;
  role: string;
  persona: string;
  tone: string;
  shape: string;
  color: string;
  tools: string[];
  integrations: string[];
  sortOrder: number;
}

/** The identity a query runs as. */
export interface Scope {
  userId: string;
  workspaceId: string;
}

/** True when the scope carries no identity. */
export function isZeroScope(scope: Scope): boolean {
  return scope.userId === NilUUID && scope.workspaceId === NilUUID;
}

/** Reports how many Bolu a workspace may have. */
export interface PlanLimits {
  maxAgents(workspaceId: string): Promise<number>;
}

/** The persistence contract. Every method runs inside the caller's tenant scope. */
export interface Repository {
  list(scope: Scope): Promise<Agent[]>;
  get(scope: Scope, id: string): Promise<Agent>;
  create(scope: Scope, agent: NewAgent): Promise<Agent>;
  update(scope: Scope, id: string, req: UpdateRequest): Promise<Agent>;
  setStatus(scope: Scope, id: string, status: string): Promise<Agent>;
  setMemoryExtraction(scope: Scope, id: string, enabled: boolean): Promise<Agent>;
  /** Writes a whole arrangement in one transaction. */
  setPlacements(scope: Scope, placements: Placement[]): Promise<void>;
  delete(scope: Scope, id: string): Promise<void>;

  activity(scope: Scope): Promise<Map<string, Activity>>;
  activityFor(scope: Scope, id: string): Promise<Activity>;
  reasonFor(scope: Scope, id: string): Promise<Reason | null>;
  runningTasks(scope: Scope, id: string): Promise<number>;
  countLive(scope: Scope): Promise<number>;

  teams(scope: Scope): Promise<Team[]>;
  templates(): Promise<Template[]>;
  template(key: string): Promise<Template>;
  agentTemplate(key: string): Promise<NewAgent>;

  grants(scope: Scope, agentId: string): Promise<Grant[]>;
  setGrant(scope: Scope, agentId: string, integrationId: string, permission: string): Promise<Grant>;
  deleteGrant(scope: Scope, agentId: string, integrationId: string): Promise<void>;
  allowedTools(scope: Scope, agentId: string): Promise<Tool[]>;
  toolCatalog(): Promise<Tool[]>;
  integrations(scope: Scope): Promise<Integration[]>;
}

/** The agent registry use case contract. */
export interface Service {
  list(scope: Scope): Promise<Agent[]>;
  get(scope: Scope, id: string): Promise<Agent>;
  create(scope: Scope, req: CreateRequest): Promise<Agent>;
  update(scope: Scope, id: string, req: UpdateRequest): Promise<Agent>;
  setStatus(scope: Scope, id: string, status: string): Promise<Agent>;
  setMemoryExtraction(scope: Scope, id: string, enabled: boolean): Promise<Agent>;
  delete(scope: Scope, id: string): Promise<void>;

  /** Applies the sidebar's layout and answers with the teams as they now read. */
  arrange(scope: Scope, arrangement: Arrangement): Promise<Team[]>;

  teams(scope: Scope): Promise<Team[]>;
  templates(): Promise<Template[]>;

  grants(scope: Scope, agentId: string): Promise<Grant[]>;
  setGrant(scope: Scope, agentId: string, integrationId: string, permission: string): Promise<Grant>;
  deleteGrant(scope: Scope, agentId: string, integrationId: string): Promise<void>;
  allowedTools(scope: Scope, agentId: string): Promise<Tool[]>;
  toolCatalog(): Promise<Tool[]>;
  integrations(scope: Scope): Promise<Integration[]>;

  /** Reports whether a Bolu may be given work. */
  acceptsNewTasks(scope: Scope, id: string): Promise<boolean>;
}

/** Renders a tool list for the JSONB column. */
export function marshalTools(tools: string[] | null): string {
  return JSON.stringify(tools ?? []);
}

/** Reads a tool list from the JSONB column. */
export function unmarshalTools(raw: unknown): string[] {
  if (raw === null || raw === undefined) return [];
  let value: unknown = raw;
  if (typeof value === "string") {
    if (value.length === 0) return [];
    value = JSON.parse(value);
  }
  if (!Array.isArray(value)) return [];
  return value.map((item) => String(item));
}
