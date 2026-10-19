/**
 * The wire types of the OpenAI-compatible chat completions format.
 *
 * Ported from internal/platform/llm/openai/wire.go. They are declared here
 * rather than pulled from a client SDK, so the adapter stays a thin, testable
 * translation layer. Every field name is the provider's JSON tag exactly.
 */

import type { ChatRequest, FinishReason, Message, ToolCall, Usage } from "../registry.ts";
import { ErrInvalidRequest, ErrProviderUnavailable, ErrRateLimited, llmError } from "../registry.ts";
import type { Client } from "./client.ts";

export interface chatCompletion {
  model: string;
  choices: choice[];
  usage: wireUsage | null;
  error: wireError | null;
}

export interface choice {
  index: number;
  message: message;
  finish_reason: string;
}

export interface message {
  role: string;
  content: string;
  tool_calls: wireToolCall[] | null;
  tool_call_id: string;
}

export interface wireToolCall {
  index: number;
  id: string;
  type: string;
  function: {
    name: string;
    arguments: string;
  };
}

export interface streamChunk {
  model: string;
  choices: streamDelta[];
  usage: wireUsage | null;
  error: wireError | null;
}

/** A tool-call fragment inside a delta. The call is identified by index. */
export type streamToolCall = wireToolCall;

export interface streamDelta {
  index: number;
  delta: message;
  finish_reason: string;
}

export interface wireUsage {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  prompt_tokens_details: { cached_tokens: number } | null;
}

/**
 * Converts the provider's counts. The cached tokens are a subset of the prompt
 * tokens, so they are reported separately and not added again.
 */
export function wireUsageToUsage(u: wireUsage): Usage {
  return {
    inputTokens: u.prompt_tokens,
    outputTokens: u.completion_tokens,
    cacheReadTokens: u.prompt_tokens_details ? u.prompt_tokens_details.cached_tokens : 0,
    cacheWriteTokens: 0,
  };
}

export interface wireError {
  message: string;
  type: string;
  code: string;
}

/**
 * Turns a provider error body into the shared error set, so the gateway can
 * decide whether to fall back.
 */
export function wireErrorToError(e: wireError): Error {
  let detail = e.message;
  if (e.code !== "") detail = e.code + ": " + detail;

  switch (e.code) {
    case "rate_limit_exceeded":
      return llmError(ErrRateLimited, detail);
    case "context_length_exceeded":
    case "invalid_request_error":
      return llmError(ErrInvalidRequest, detail);
  }
  switch (e.type) {
    case "invalid_request_error":
      return llmError(ErrInvalidRequest, detail);
    case "rate_limit_error":
      return llmError(ErrRateLimited, detail);
  }

  return llmError(ErrProviderUnavailable, detail);
}

/** Builds the provider payload from the internal request. */
export function encodeRequest(c: Client, req: ChatRequest, stream: boolean): Record<string, unknown> {
  const messages: Record<string, unknown>[] = [];

  // OpenAI carries the system prompt as the first message. The retrieved block
  // is appended to it rather than sent as a second system message: some
  // compatible gateways accept only one, and the order — instructions first,
  // then data — is what matters for the model.
  let system = req.system;
  if (req.retrieved !== "") {
    if (system !== "") system += "\n\n";
    system += req.retrieved;
  }
  if (system !== "") {
    messages.push({ role: "system", content: system });
  }

  for (const message of req.messages) {
    messages.push(encodeMessage(message));
  }

  const payload: Record<string, unknown> = {
    model: req.model !== "" ? req.model : c.model(),
    messages,
    stream,
  };

  const maxTokens = req.maxTokens > 0 ? req.maxTokens : c.configuredMaxTokens();
  if (maxTokens > 0) payload.max_tokens = maxTokens;
  if (req.temperature !== null) payload.temperature = req.temperature;
  if (req.stopSequences.length > 0) payload.stop = req.stopSequences;

  if (req.tools.length > 0) {
    payload.tools = req.tools.map((tool) => ({
      type: "function",
      function: {
        name: tool.name,
        description: tool.description,
        parameters: tool.schema ?? { type: "object", properties: {} },
      },
    }));

    const choice = toolChoiceFor(req);
    if (choice !== null) payload.tool_choice = choice;
  }

  // Some gateways return token counts in the final streamed chunk only when
  // asked.
  if (stream) {
    payload.stream_options = { include_usage: true };
  }

  return payload;
}

/** Projects one internal message onto the provider shape. */
export function encodeMessage(message: Message): Record<string, unknown> {
  const encoded: Record<string, unknown> = { role: message.role };

  switch (message.role) {
    case "tool": {
      // A tool result is one message per call in this format.
      if (message.toolResults.length === 0) {
        encoded.content = message.text;
        return encoded;
      }
      const result = message.toolResults[0];
      encoded.tool_call_id = result.callId;
      encoded.content = result.content;
      return encoded;
    }

    case "assistant": {
      if (message.toolCalls.length > 0) {
        encoded.tool_calls = message.toolCalls.map((call) => ({
          id: call.id,
          type: "function",
          function: {
            name: call.name,
            arguments: call.arguments === null || call.arguments === undefined ? "{}" : JSON.stringify(call.arguments),
          },
        }));
      }
      if (message.text !== "") encoded.content = message.text;
      return encoded;
    }
  }

  // User and system: text, or the multimodal parts array when images are
  // attached.
  if (message.images.length === 0) {
    encoded.content = message.text;
    return encoded;
  }

  const parts: Record<string, unknown>[] = [];
  if (message.text !== "") parts.push({ type: "text", text: message.text });
  for (const image of message.images) {
    parts.push({
      type: "image_url",
      image_url: { url: "data:" + image.mediaType + ";base64," + image.data },
    });
  }
  encoded.content = parts;
  return encoded;
}

/** Maps the internal choice onto the provider's. */
export function toolChoiceFor(req: ChatRequest): unknown {
  if (req.toolName !== "") {
    return { type: "function", function: { name: req.toolName } };
  }
  switch (req.toolChoice) {
    case "none":
      return "none";
    case "required":
      return "required";
    default:
      return "auto";
  }
}

/** Turns one provider choice into the internal response. */
export function decodeChoice(
  c: Client,
  payload: chatCompletion,
): {
  text: string;
  toolCalls: ToolCall[];
  finishReason: FinishReason;
  rawFinishReason: string;
  model: string;
  usage: Usage | null;
} {
  const picked = payload.choices[0];

  const response = {
    text: picked.message.content,
    toolCalls: [] as ToolCall[],
    finishReason: mapFinishReason(picked.finish_reason),
    rawFinishReason: picked.finish_reason,
    model: payload.model !== "" ? payload.model : c.model(),
    usage: null as Usage | null,
  };

  for (const call of picked.message.tool_calls ?? []) {
    let args: unknown = {};
    if (call.function.arguments !== "") {
      try {
        args = JSON.parse(call.function.arguments);
      } catch {
        args = call.function.arguments;
      }
    }
    response.toolCalls.push({ id: call.id, name: call.function.name, arguments: args });
  }

  if (payload.usage !== null) response.usage = wireUsageToUsage(payload.usage);

  return response;
}

/** Normalises the provider's reason. */
export function mapFinishReason(reason: string): FinishReason {
  switch (reason) {
    case "stop":
      return "stop";
    case "tool_calls":
    case "function_call":
      return "tool_calls";
    case "length":
      return "length";
    case "content_filter":
      return "content_filter";
    case "":
      return "stop";
    default:
      return "unknown";
  }
}
