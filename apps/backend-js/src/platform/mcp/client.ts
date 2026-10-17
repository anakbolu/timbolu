/**
 * Speaks the Model Context Protocol to a server somebody else runs.
 *
 * Ported from internal/platform/mcp/client.go. The transport is JSON-RPC over
 * HTTP: one POST per request, the server answering either a result or an error.
 * It needs no long-lived connection to stay correct, which matters because a
 * task may be the first thing to talk to a server in days.
 *
 * The `@modelcontextprotocol/sdk` is installed, but its transports own
 * sessions and connections that this client deliberately does not have; the
 * protocol surface used here is small enough to keep on `fetch`, which also
 * preserves the Go client's exact error classification.
 */

/** The revision this client speaks. */
export const PROTOCOL_VERSION = "2025-06-18";

/**
 * Bounds what is read from a server, so a broken or hostile one cannot exhaust
 * memory. A tool result may be large; this is generous enough for one.
 */
const MAX_BODY = 8 << 20;

/** The failure classes a caller branches on. */
export type MCPErrorCode = "server_unavailable" | "tool_failed" | "unauthorized";

/** Every failure this client raises carries one of the classes above. */
export class MCPError extends Error {
  constructor(
    readonly code: MCPErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "MCPError";
  }
}

/** The server could not be reached or answered nonsense. Retryable. */
export const ErrServerUnavailable = new MCPError(
  "server_unavailable",
  "mcp: the server is unavailable",
);
/** The server answered, and the call failed. The tool's own failure. */
export const ErrToolFailed = new MCPError("tool_failed", "mcp: the tool failed");
/** The server refused the credential. */
export const ErrUnauthorized = new MCPError("unauthorized", "mcp: the server refused the credential");

/** Wraps a sentinel with detail, the way `%w: %s` does in Go. */
export function mcpError(sentinel: MCPError, detail: string): MCPError {
  return new MCPError(sentinel.code, detail ? `${sentinel.message}: ${detail}` : sentinel.message);
}

/** `errors.Is` for the MCP error set. */
export function isMCPError(err: unknown, sentinel: MCPError): boolean {
  return err instanceof MCPError && err.code === sentinel.code;
}

/** What a client needs. */
export interface Config {
  /** The server's HTTP address. */
  endpoint: string;
  /**
   * Sent as a bearer token. Empty means the server is open, which is what a
   * server on a private network looks like.
   */
  credential?: string;
  /**
   * Bounds one request, in milliseconds. A server that does not answer within
   * it is treated as unavailable rather than waited on, because a task is
   * blocked while this runs.
   */
  timeout?: number;
  /** Overrides the transport, which is how a test points the client at a stub. */
  fetch?: typeof fetch;
}

/** One thing a server offers. */
export interface Tool {
  name: string;
  description: string;
  inputSchema: unknown;
  /**
   * The server's own hints about what the tool does. They are hints, not
   * promises: a server is free to say nothing, and this client treats silence
   * as the unsafe answer.
   */
  annotations?: {
    readOnlyHint?: boolean | null;
    title?: string;
  };
}

/**
 * Reports whether the server says this tool changes nothing.
 *
 * A server that says nothing is treated as though it changes something: the
 * product's rule is that a person holds the door to anything that leaves, and
 * guessing "probably fine" from silence is how that rule stops being true.
 */
export function readOnly(tool: Tool): boolean {
  return tool.annotations?.readOnlyHint === true;
}

/** One block of a tool's answer. */
export interface Content {
  type: string;
  text: string;
  /** Carries an image or an embedded resource. */
  data: string;
  mimeType: string;
}

/** What a tool answered. */
export interface CallResult {
  /** The server's own content blocks, kept whole. */
  content: Content[];
  /** The server reported the call as failed while still answering. */
  isError: boolean;
}

/** Joins the text blocks, which is what a model reads. */
export function callResultText(result: CallResult): string {
  const parts: string[] = [];
  for (const block of result.content) {
    if (block.type === "text" && block.text !== "") parts.push(block.text);
  }
  return parts.join("\n");
}

/** One JSON-RPC call. */
interface RPCRequest {
  jsonrpc: string;
  id: number;
  method: string;
  params?: unknown;
}

/** A server's own failure. */
interface RPCError {
  code: number;
  message: string;
  data?: unknown;
}

/** One JSON-RPC answer. */
interface RPCResponse {
  jsonrpc: string;
  id: number;
  result?: unknown;
  error?: RPCError | null;
}

/** Talks to one server. */
export class Client {
  private readonly endpoint: string;
  private readonly credential: string;
  private readonly timeout: number;
  private readonly http: typeof fetch;

  constructor(cfg: Config) {
    const endpoint = cfg.endpoint.trim();
    if (endpoint === "") {
      throw new Error("mcp: an endpoint is required");
    }
    this.endpoint = endpoint;
    this.credential = cfg.credential ?? "";
    this.timeout = cfg.timeout !== undefined && cfg.timeout > 0 ? cfg.timeout : 30_000;
    this.http = cfg.fetch ?? fetch;
  }

  /**
   * Performs the protocol handshake and returns what the server calls itself.
   *
   * Separate from the tool listing because a server may refuse the handshake
   * for a reason worth showing a person — a version it does not speak, a
   * credential it does not accept — and that is different from an empty tool
   * list.
   */
  async initialize(ctx: AbortSignal | undefined): Promise<string> {
    const result = await this.request(ctx, "initialize", {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: {},
      clientInfo: { name: "bolu", version: "1" },
    });

    const serverInfo = asRecord(result)?.["serverInfo"];
    const name = asRecord(serverInfo)?.["name"];
    if (typeof name !== "string" || name === "") {
      return "mcp";
    }
    return name;
  }

  /** Lists what the server offers. */
  async tools(ctx: AbortSignal | undefined): Promise<Tool[]> {
    const result = await this.request(ctx, "tools/list", {});
    const record = asRecord(result);
    const tools = record?.["tools"];
    if (!Array.isArray(tools)) {
      return [];
    }
    return tools.map((tool) => decodeTool(tool));
  }

  /** Runs one tool. */
  async call(
    ctx: AbortSignal | undefined,
    name: string,
    args: unknown,
  ): Promise<CallResult> {
    const parsed = decodeArguments(args);
    const result = await this.request(ctx, "tools/call", { name, arguments: parsed });
    const record = asRecord(result) ?? {};
    const content = Array.isArray(record["content"])
      ? (record["content"] as unknown[]).map(decodeContent)
      : [];
    const answer: CallResult = { content, isError: record["isError"] === true };

    // The protocol allows a call to answer with an error while still returning
    // a result. Reporting it as a failure keeps a tool that could not do its
    // job from reading as though it did.
    if (answer.isError) {
      throw mcpError(ErrToolFailed, callResultText(answer));
    }
    return answer;
  }

  // -------------------------------------------------------------- transport

  /** Performs one request and returns its decoded result. */
  private async request(
    ctx: AbortSignal | undefined,
    method: string,
    params: unknown,
  ): Promise<unknown> {
    const document = JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method,
      params,
    } satisfies RPCRequest);

    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      // The protocol asks a client to accept both, because a server may answer
      // with either depending on how it is deployed.
      Accept: "application/json, text/event-stream",
    };
    if (this.credential !== "") {
      headers.Authorization = "Bearer " + this.credential;
    }

    let response: Response;
    try {
      response = await this.http(this.endpoint, {
        method: "POST",
        headers,
        body: document,
        signal: ctx
          ? AbortSignal.any([ctx, AbortSignal.timeout(this.timeout)])
          : AbortSignal.timeout(this.timeout),
      });
    } catch (err) {
      // A transport failure is retryable: a server that is down is usually down
      // briefly, and a task should wait rather than fail.
      throw mcpError(ErrServerUnavailable, `${method}: ${errorText(err)}`);
    }

    if (response.status === 401 || response.status === 403) {
      await response.body?.cancel().catch(() => undefined);
      throw new MCPError(
        ErrUnauthorized.code,
        `${ErrUnauthorized.message} (status ${response.status})`,
      );
    }

    let body: string;
    try {
      body = await readLimited(response, MAX_BODY);
    } catch (err) {
      throw mcpError(ErrServerUnavailable, `read ${method} answer: ${errorText(err)}`);
    }

    if (response.status >= 400) {
      // A server that answers 5xx is down; one that answers 4xx has refused the
      // request itself, which is not a reason to retry it unchanged.
      if (response.status >= 500) {
        throw mcpError(ErrServerUnavailable, `${method} answered ${response.status}`);
      }
      throw new Error(`mcp: ${method} answered ${response.status}: ${truncate(body, 200)}`);
    }

    // A server may answer with a server-sent event stream instead of a body.
    const contentType = response.headers.get("content-type") ?? "";
    const payload = contentType.includes("text/event-stream") ? firstEventData(body) : body;

    let decoded: RPCResponse;
    try {
      decoded = JSON.parse(payload) as RPCResponse;
    } catch (err) {
      throw mcpError(
        ErrServerUnavailable,
        `${method} answered something that is not JSON-RPC: ${errorText(err)}`,
      );
    }

    if (decoded.error) {
      const serverError = decoded.error;
      if (!serverError.message) {
        throw new Error(`mcp: server error ${serverError.code}`);
      }
      throw new Error(`mcp: server error ${serverError.code}: ${serverError.message}`);
    }
    if (decoded.result === undefined) {
      throw mcpError(ErrServerUnavailable, `${method} answered with no result`);
    }
    return decoded.result;
  }
}

/** Reads at most `limit` bytes of a response, as `io.LimitReader` does. */
async function readLimited(response: Response, limit: number): Promise<string> {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let out = "";
  let total = 0;
  try {
    for (;;) {
      const step = await reader.read();
      if (step.done) break;
      const size = step.value.byteLength;
      total += size;
      if (total > limit) {
        out += decoder.decode(step.value.subarray(0, size - (total - limit)), { stream: true });
        break;
      }
      out += decoder.decode(step.value, { stream: true });
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  return out + decoder.decode();
}

/** Reads the payload of the first `data:` line of an event stream. */
function firstEventData(body: string): string {
  for (const line of body.split("\n")) {
    const trimmed = line.trim();
    if (trimmed.startsWith("data:")) {
      return trimmed.slice("data:".length).trim();
    }
  }
  return body;
}

/** Bounds what an error message carries, so a server's body stays out of logs. */
function truncate(value: string, limit: number): string {
  if (value.length <= limit) return value;
  return value.slice(0, limit) + "…";
}

/** Mirrors `json.Unmarshal(arguments, &map)`: absent means `{}`, non-objects fail. */
function decodeArguments(args: unknown): Record<string, unknown> {
  if (args === null || args === undefined) return {};
  let value = args;
  if (typeof args === "string") {
    if (args === "") return {};
    try {
      value = JSON.parse(args);
    } catch (err) {
      throw new Error(`mcp: the arguments are not an object: ${errorText(err)}`);
    }
  }
  const record = asRecord(value);
  if (record === null) {
    throw new Error("mcp: the arguments are not an object: expected a JSON object");
  }
  return record;
}

function decodeTool(value: unknown): Tool {
  const record = asRecord(value) ?? {};
  const annotations = asRecord(record["annotations"]);
  return {
    name: typeof record["name"] === "string" ? record["name"] : "",
    description: typeof record["description"] === "string" ? record["description"] : "",
    inputSchema: record["inputSchema"] ?? null,
    annotations: {
      readOnlyHint:
        typeof annotations?.["readOnlyHint"] === "boolean"
          ? (annotations["readOnlyHint"] as boolean)
          : null,
      title: typeof annotations?.["title"] === "string" ? (annotations["title"] as string) : "",
    },
  };
}

function decodeContent(value: unknown): Content {
  const record = asRecord(value) ?? {};
  return {
    type: typeof record["type"] === "string" ? record["type"] : "",
    text: typeof record["text"] === "string" ? record["text"] : "",
    data: typeof record["data"] === "string" ? record["data"] : "",
    mimeType: typeof record["mimeType"] === "string" ? record["mimeType"] : "",
  };
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
