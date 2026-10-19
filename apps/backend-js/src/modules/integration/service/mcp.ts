/**
 * The MCP half: the servers a workspace added, the tools they contribute, and
 * running a call on one.
 *
 * Ported 1:1 from internal/modules/integration/service/mcp.go.
 */

import {
  ErrInvalidInput,
  ErrNoCipher,
  errorMessage,
  is,
  isNilId,
  wrap,
  type Scope,
} from "../domain/integration.ts";
import {
  ErrMCPUnavailable,
  type MCPServer,
  type MCPServerInput,
  type MCPTool,
} from "../domain/mcp.ts";
import { ErrUnknownTool, type Result, type ToolSpec } from "../domain/adapter.ts";
import type { IntegrationService } from "./service.ts";

/**
 * Registers a server a workspace wants to use, then asks it what it offers.
 *
 * The handshake happens here rather than lazily so a person who mistyped an
 * address finds out immediately, while they are still looking at the form.
 */
export async function addMCPServer(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  input: MCPServerInput,
): Promise<MCPServer> {
  validateMCPServerInput(input);
  if (!service.deps.mcp || !service.deps.mcpClients) {
    throw wrap(ErrInvalidInput, "MCP servers are not configured");
  }

  let sealed: Uint8Array | null = null;
  if (input.credential.trim() !== "") {
    if (!service.deps.cipher) {
      throw ErrNoCipher;
    }
    try {
      sealed = await service.deps.cipher.seal(ctx, scope.workspaceId, input.credential);
    } catch (err) {
      throw new Error(`integration: seal MCP credential: ${errorMessage(err)}`);
    }
  }

  const server = await service.deps.mcp.upsertMCPServer(scope, input, sealed);

  // A server that does not answer is stored anyway, with the reason: a person
  // behind a firewall should see the server listed as unreachable rather than
  // absent.
  try {
    await refreshMCPTools(service, ctx, scope, server, input.credential);
  } catch (err) {
    service.deps.logger.warn({ app: server.app, err }, "an MCP server was added but did not answer");
  }

  return service.deps.mcp.getMCPServer(scope, server.id);
}

/** The workspace's servers. */
export async function listMCPServers(
  service: IntegrationService,
  _ctx: unknown,
  scope: Scope,
): Promise<MCPServer[]> {
  if (!service.deps.mcp) {
    return [];
  }
  return service.deps.mcp.listMCPServers(scope);
}

/** Removes one. */
export async function removeMCPServer(
  service: IntegrationService,
  _ctx: unknown,
  scope: Scope,
  id: string,
): Promise<void> {
  if (!service.deps.mcp) {
    throw wrap(ErrInvalidInput, "MCP servers are not configured");
  }
  if (isNilId(id)) {
    throw wrap(ErrInvalidInput, "a server id is required");
  }
  return service.deps.mcp.deleteMCPServer(scope, id);
}

/** Asks one server what it offers and records the answer. */
export async function refreshMCPServer(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  id: string,
): Promise<MCPServer> {
  if (!service.deps.mcp || !service.deps.mcpClients) {
    throw wrap(ErrInvalidInput, "MCP servers are not configured");
  }

  const server = await service.deps.mcp.getMCPServer(scope, id);
  const credential = await mcpCredential(service, ctx, scope, id);

  try {
    await refreshMCPTools(service, ctx, scope, server, credential);
  } catch {
    // The failure is recorded on the row; the row is what the screen reads.
  }
  return service.deps.mcp.getMCPServer(scope, id);
}

/**
 * Lists the tools the workspace's servers contribute.
 *
 * It reads what each server last reported rather than reaching every server: a
 * task is waiting, and a server that is briefly down must not cost a Bolu its
 * tools.
 */
export async function mcpTools(
  service: IntegrationService,
  _ctx: unknown,
  scope: Scope,
): Promise<ToolSpec[]> {
  if (!service.deps.mcp) {
    return [];
  }

  const servers = await service.deps.mcp.listMCPServers(scope);

  const tools: ToolSpec[] = [];
  for (const server of servers) {
    if (!server.enabled) {
      continue;
    }

    const recorded = await service.deps.mcp.mcpServerTools(scope, server.id);
    for (const tool of recorded) {
      tools.push({
        name: tool.name,
        label: tool.label,
        description: tool.description,
        schema: new TextDecoder().decode(tool.schema),
      });
    }
  }
  return tools;
}

/**
 * Runs one tool on the server that offers it.
 *
 * A failure to reach the server is reported as retryable, because a server that
 * is down is usually down briefly. A server that answers with a failure is the
 * tool's own failure.
 */
export async function executeMCP(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  tool: string,
  args: string,
): Promise<Result> {
  const cut = tool.indexOf(".");
  const app = cut < 0 ? tool : tool.slice(0, cut);

  if (!service.deps.mcp || !service.deps.mcpClients) {
    throw new Error(`${ErrUnknownTool.message}: ${JSON.stringify(tool)}`);
  }

  const servers = await service.deps.mcp.listMCPServers(scope);

  let server: MCPServer | null = null;
  for (const candidate of servers) {
    if (candidate.app === app && candidate.enabled) {
      server = candidate;
      break;
    }
  }
  if (!server) {
    throw new Error(`${ErrUnknownTool.message}: ${JSON.stringify(tool)}`);
  }

  // The tool's own name on the server is the part after the app.
  const remote = tool.startsWith(`${app}.`) ? tool.slice(app.length + 1) : "";
  if (remote === "" || remote === tool) {
    throw new Error(`${ErrUnknownTool.message}: ${JSON.stringify(tool)}`);
  }

  const credential = await mcpCredential(service, ctx, scope, server.id);

  const client = await service.deps.mcpClients.client(server.endpoint, credential);

  try {
    const result = await client.call(ctx, remote, new TextEncoder().encode(args));
    return {
      content: new TextDecoder().decode(result.content),
      summary: result.summary,
    };
  } catch (err) {
    // The failure is recorded on the server row so the Integrations screen can
    // say why its tools stopped working.
    await recordMCPFailure(service, ctx, scope, server.id, err);
    if (is(err, ErrMCPUnavailable)) {
      throw wrap(ErrMCPUnavailable, `${server.app}: ${errorMessage(err)}`);
    }
    throw err;
  }
}

/** Asks a server what it offers and records the answer. */
export async function refreshMCPTools(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  server: MCPServer,
  credential: string,
): Promise<MCPTool[]> {
  if (!service.deps.mcp || !service.deps.mcpClients) {
    throw wrap(ErrInvalidInput, "MCP servers are not configured");
  }

  const client = await service.deps.mcpClients.client(server.endpoint, credential);

  try {
    await client.initialize(ctx);
  } catch (err) {
    await recordMCPFailure(service, ctx, scope, server.id, err);
    throw err;
  }

  let remote;
  try {
    remote = await client.tools(ctx);
  } catch (err) {
    await recordMCPFailure(service, ctx, scope, server.id, err);
    throw err;
  }

  const tools: MCPTool[] = remote.map((tool) => ({
    name: `${server.app}.${tool.name}`,
    label: labelForMCPTool(tool.readOnly),
    description: tool.description,
    schema: tool.schema,
    remoteName: tool.name,
  }));

  await service.deps.mcp.saveMCPTools(scope, server.id, tools);
  return tools;
}

/**
 * Decides how a server's tool is governed.
 *
 * The protocol's own hint is honoured when a server states one. A server that
 * says nothing is treated as reaching outside the workspace.
 */
function labelForMCPTool(readOnly: boolean): string {
  return readOnly ? "read" : "write_external";
}

/** Opens a server's stored credential. */
export async function mcpCredential(
  service: IntegrationService,
  ctx: unknown,
  scope: Scope,
  id: string,
): Promise<string> {
  if (!service.deps.mcp) {
    throw wrap(ErrInvalidInput, "MCP servers are not configured");
  }

  const sealed = await service.deps.mcp.mcpServerCredential(scope, id);
  if (!sealed || sealed.length === 0) {
    return "";
  }
  if (!service.deps.cipher) {
    throw ErrNoCipher;
  }

  try {
    return await service.deps.cipher.open(ctx, scope.workspaceId, sealed);
  } catch (err) {
    throw new Error(`integration: open MCP credential: ${errorMessage(err)}`);
  }
}

/** Notes why a server could not be reached, so a screen can say so. */
export async function recordMCPFailure(
  service: IntegrationService,
  _ctx: unknown,
  scope: Scope,
  id: string,
  err: unknown,
): Promise<void> {
  if (!service.deps.mcp) {
    return;
  }
  try {
    await service.deps.mcp.setMCPServerError(scope, id, errorMessage(err));
  } catch (recordErr) {
    service.deps.logger.warn({ server: id, err: recordErr }, "an MCP failure could not be recorded");
  }
}

/** Refuses what the schema or the protocol would. */
function validateMCPServerInput(input: MCPServerInput): void {
  const app = input.app.trim();
  if (app === "") {
    throw wrap(ErrInvalidInput, "an app key is required");
  }
  if (app.includes(".")) {
    throw wrap(ErrInvalidInput, "the app key must not contain a dot, because it prefixes tool names");
  }
  if (input.endpoint.trim() === "") {
    throw wrap(ErrInvalidInput, "an endpoint is required");
  }
}
