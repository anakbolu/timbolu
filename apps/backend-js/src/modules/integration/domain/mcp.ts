/**
 * The MCP half of the adapter contract.
 *
 * An application that publishes an MCP server needs no adapter of its own: the
 * server already says what it offers. What this deployment adds is the
 * governance around it — which workspace added which server, which Bolu may use
 * it, and how a tool the server describes becomes a tool this product can
 * decide about.
 *
 * Ported 1:1 from internal/modules/integration/domain/mcp.go.
 */

import type { Scope } from "./integration.ts";

/** The workspace has no such server. */
export const ErrMCPServerNotFound = new Error("integration: MCP server not found");
/** The server could not be reached. */
export const ErrMCPUnavailable = new Error("integration: the MCP server is unavailable");

/** One server a workspace added. */
export interface MCPServer {
  id: string;
  app: string;
  label: string;
  endpoint: string;
  enabled: boolean;
  /** How many tools the server last reported. */
  toolCount: number;
  /** The last thing that went wrong talking to it. */
  lastError: string;
  createdAt: Date;
  updatedAt: Date;
}

/** What a person supplies to add one. */
export interface MCPServerInput {
  app: string;
  label: string;
  endpoint: string;
  /** Sent as a bearer token. Empty means the server is open. */
  credential: string;
  enabled: boolean;
}

/** One tool a server reported, in this product's terms. */
export interface MCPTool {
  /** The qualified name the model calls: `app.tool`. */
  name: string;
  label: string;
  description: string;
  schema: Uint8Array;
  /** What the server calls it, which is what a call must use. */
  remoteName: string;
}

/** Stores the servers a workspace added. */
export interface MCPRepository {
  listMCPServers(scope: Scope): Promise<MCPServer[]>;
  getMCPServer(scope: Scope, id: string): Promise<MCPServer>;
  upsertMCPServer(scope: Scope, input: MCPServerInput, sealedCredential: Uint8Array | null): Promise<MCPServer>;
  saveMCPTools(scope: Scope, id: string, tools: MCPTool[]): Promise<void>;
  mcpServerTools(scope: Scope, id: string): Promise<MCPTool[]>;
  setMCPServerError(scope: Scope, id: string, reason: string): Promise<void>;
  deleteMCPServer(scope: Scope, id: string): Promise<void>;
  mcpServerCredential(scope: Scope, id: string): Promise<Uint8Array>;
}

/** One conversation with a server. */
export interface MCPClient {
  initialize(ctx: unknown): Promise<void>;
  tools(ctx: unknown): Promise<MCPRemoteTool[]>;
  call(ctx: unknown, remoteName: string, args: Uint8Array): Promise<MCPCallResult>;
}

/** A tool as the server describes it. */
export interface MCPRemoteTool {
  name: string;
  description: string;
  schema: Uint8Array;
  /** A server that says nothing is treated as though it changes something. */
  readOnly: boolean;
}

/** What a call answered. */
export interface MCPCallResult {
  content: Uint8Array;
  summary: string;
}

/** Builds a client for one server, given the opened credential. */
export interface MCPClientFactory {
  client(endpoint: string, credential: string): Promise<MCPClient>;
}
