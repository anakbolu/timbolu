/**
 * The wire types of the Messages API.
 *
 * Ported from internal/platform/llm/anthropic/wire.go. Declared here so the
 * adapter stays a thin translation layer instead of pulling in an SDK. Every
 * field name is the provider's JSON tag exactly.
 */

import type { ChatRequest, FinishReason, Message, ToolCall, Usage } from "../registry.ts";
import { ErrInvalidRequest, ErrProviderUnavailable, ErrRateLimited, llmError } from "../registry.ts";
import type { Client } from "./client.ts";

export interface messagesResponse {
  model: string;
  content: contentBlock[];
  stop_reason: string;
  usage: wireUsage | null;
  error: wireError | null;
}

export interface contentBlock {
  type: string;
  text: string;
  id: string;
  name: string;
  input: unknown;
}

export interface wireUsage {
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
}

/**
 * Converts the provider's counts. Cached tokens are reported separately by this
 * API and are not part of input_tokens.
 */
export function wireUsageToUsage(u: wireUsage): Usage {
  return {
    inputTokens: u.input_tokens,
    outputTokens: u.output_tokens,
    cacheReadTokens: u.cache_read_input_tokens,
    cacheWriteTokens: u.cache_creation_input_tokens,
  };
}

export interface wireError {
  type: string;
  message: string;
}

/** Turns a provider error into the shared set. */
export function wireErrorToError(e: wireError): Error {
  let detail = e.message;
  if (e.type !== "") detail = e.type + ": " + e.message;

  switch (e.type) {
    case "rate_limit_error":
    case "overloaded_error":
      return llmError(ErrRateLimited, detail);
    case "invalid_request_error":
      return llmError(ErrInvalidRequest, detail);
    case "authentication_error":
    case "permission_error":
      return llmError(ErrProviderUnavailable, detail);
  }

  return llmError(ErrProviderUnavailable, detail);
}

/**
 * One event of the streaming API. Different event types use different fields,
 * so most of them are optional.
 */
export interface streamEvent {
  type: string;
  index: number;
  message: streamStart | null;
  content_block: contentBlock | null;
  delta: streamDelta | null;
  usage: wireUsage | null;
  error: wireError | null;
}

export interface streamStart {
  model: string;
  usage: wireUsage | null;
}

export interface streamDelta {
  type: string;
  text: string;
  partial_json: string;
  stop_reason: string;
}

/**
 * Builds the provider payload. This API takes streaming as a body field rather
 * than as a separate endpoint.
 */
export function encodeRequest(c: Client, req: ChatRequest, stream: boolean): Record<string, unknown> {
  const messages: Record<string, unknown>[] = [];
  for (const message of req.messages) {
    messages.push(encodeMessage(message));
  }

  const maxTokens = req.maxTokens > 0 ? req.maxTokens : c.configuredMaxTokens();

  const payload: Record<string, unknown> = {
    model: req.model !== "" ? req.model : c.model(),
    max_tokens: maxTokens,
    messages,
    // Streaming is a body field here, not a separate endpoint.
    stream,
  };

  // This API takes the system prompt as its own field, not as a message. A
  // retrieval block travels as a second block of that field, so the cache
  // breakpoint lands after the stable prefix rather than after the retrieved
  // text.
  if (req.system !== "" || req.retrieved !== "") {
    const blocks: Record<string, unknown>[] = [];

    if (req.system !== "") {
      const block: Record<string, unknown> = { type: "text", text: req.system };
      if (c.cachingPrompt()) {
        // A cache breakpoint on the stable prefix measurably cuts the cost of
        // every later turn.
        block.cache_control = { type: "ephemeral" };
      }
      blocks.push(block);
    }
    if (req.retrieved !== "") {
      blocks.push({ type: "text", text: req.retrieved });
    }

    payload.system = blocks;
  }

  if (req.temperature !== null) payload.temperature = req.temperature;
  if (req.stopSequences.length > 0) payload.stop_sequences = req.stopSequences;

  if (req.tools.length > 0) {
    payload.tools = req.tools.map((tool) => ({
      name: tool.name,
      description: tool.description,
      input_schema: tool.schema ?? { type: "object", properties: {} },
    }));

    const choice = toolChoiceFor(req);
    if (choice !== null) payload.tool_choice = choice;
  }

  return payload;
}

/** Projects one internal message onto the block shape. */
export function encodeMessage(message: Message): Record<string, unknown> {
  switch (message.role) {
    case "assistant": {
      const blocks: Record<string, unknown>[] = [];
      if (message.text !== "") blocks.push({ type: "text", text: message.text });
      for (const call of message.toolCalls) {
        blocks.push({
          type: "tool_use",
          id: call.id,
          name: call.name,
          input: call.arguments ?? {},
        });
      }
      return { role: "assistant", content: blocks };
    }

    case "tool": {
      // Tool answers are user-role messages carrying tool_result blocks.
      const blocks: Record<string, unknown>[] = [];
      for (const result of message.toolResults) {
        blocks.push({
          type: "tool_result",
          tool_use_id: result.callId,
          content: result.content === "" ? "(no output)" : result.content,
          is_error: result.isError,
        });
      }
      if (blocks.length === 0 && message.text !== "") {
        blocks.push({ type: "text", text: message.text });
      }
      return { role: "user", content: blocks };
    }
  }

  // User messages: text, or blocks when images are attached.
  if (message.images.length === 0) {
    return { role: message.role, content: message.text };
  }

  const blocks: Record<string, unknown>[] = [];
  if (message.text !== "") blocks.push({ type: "text", text: message.text });
  for (const image of message.images) {
    blocks.push({
      type: "image",
      source: { type: "base64", media_type: image.mediaType, data: image.data },
    });
  }
  return { role: "user", content: blocks };
}

/** Maps the internal choice onto this API's shape. */
export function toolChoiceFor(req: ChatRequest): unknown {
  if (req.toolName !== "") {
    return { type: "tool", name: req.toolName };
  }
  switch (req.toolChoice) {
    case "none":
      // There is no "none": omitting the tools is how this API disables them.
      return null;
    case "required":
      return { type: "any" };
    default:
      return { type: "auto" };
  }
}

/** Turns the response blocks into the internal shape. */
export function decodeContent(
  c: Client,
  payload: messagesResponse,
): { text: string; toolCalls: ToolCall[]; finishReason: FinishReason; rawFinishReason: string; model: string; usage: Usage | null } {
  const response = {
    text: "",
    toolCalls: [] as ToolCall[],
    finishReason: mapStopReason(payload.stop_reason),
    rawFinishReason: payload.stop_reason,
    model: payload.model !== "" ? payload.model : c.model(),
    usage: null as Usage | null,
  };

  let text = "";
  for (const block of payload.content ?? []) {
    switch (block.type) {
      case "text":
        text += block.text;
        break;
      case "tool_use":
        response.toolCalls.push({ id: block.id, name: block.name, arguments: block.input ?? {} });
        break;
    }
  }
  response.text = text;

  if (payload.usage !== null) response.usage = wireUsageToUsage(payload.usage);

  return response;
}

/** Normalises the provider's stop reason. */
export function mapStopReason(reason: string): FinishReason {
  switch (reason) {
    case "end_turn":
    case "stop_sequence":
    case "":
      return "stop";
    case "tool_use":
      return "tool_calls";
    case "max_tokens":
      return "length";
    case "refusal":
      return "content_filter";
    default:
      return "unknown";
  }
}
