/**
 * The handoff tool, the titles a draft and a task take, and the way a tool's
 * answer is bounded.
 *
 * Ported 1:1 from internal/modules/task/activity/helpers.go.
 */

import type { Tool } from "../domain/task.ts";
import { LabelRead, ToolHandoff } from "../domain/task.ts";
import type { ToolRequest } from "../workflow/workflow.ts";

/**
 * The tool that asks another Bolu to do part of the work.
 *
 * It is part of the runtime rather than an integration, so it is offered
 * alongside the granted tools and the workflow performs it itself.
 */
export function HandoffTool(): Tool {
  return {
    Name: ToolHandoff,
    Integration: "",
    Description:
      "Minta Bolu lain mengerjakan bagian pekerjaan ini, lalu pakai hasilnya. " +
      "Pakai hanya bila pekerjaannya memang bukan keahlianmu.",
    Schema: {
      type: "object",
      required: ["agent", "instructions"],
      properties: {
        agent: { type: "string", description: "Nama Bolu, mis. Ijo" },
        instructions: {
          type: "string",
          description: "Apa yang perlu dikerjakan dan data apa yang dibutuhkan",
        },
      },
    },
    Label: LabelRead,
  };
}

/** Names a draft from the arguments the tool was called with. */
export function draftTitle(request: ToolRequest): string {
  const payload = request.Arguments;
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    return request.Name;
  }

  for (const key of ["title", "subject", "summary"]) {
    const value = stringField(payload, key);
    if (value.trim() !== "") {
      return value.trim();
    }
  }
  return request.Name;
}

/**
 * Reads one string field of a JSON object, or "" when it is absent or not a
 * string. Tool arguments are genuinely dynamic, so the read is narrowed rather
 * than asserted.
 */
export function stringField(payload: object, key: string): string {
  if (!(key in payload)) return "";
  const value: unknown = Reflect.get(payload, key);
  return typeof value === "string" ? value : "";
}

/** Takes the first line of a text, bounded, which is what a task title and a summary are. */
export function firstLine(text: string): string {
  let trimmed = text.trim();
  if (trimmed === "") {
    return "";
  }
  const newline = trimmed.indexOf("\n");
  if (newline >= 0) {
    trimmed = trimmed.slice(0, newline);
  }

  const runes = Array.from(trimmed.trim());
  if (runes.length <= 120) {
    return runes.join("");
  }
  return `${runes.slice(0, 119).join("")}…`;
}

/** What a tool's answer becomes when it is longer than the limit. */
interface Truncated {
  truncated: boolean;
  bytes: number;
  preview: string;
}

/**
 * Cuts a tool's answer at the limit.
 *
 * It is a document rather than a fragment on purpose: a model handed the first N
 * bytes of JSON would have to guess what was cut, and the guess is what costs the
 * round. Saying so in the same shape is what lets it react.
 */
export function truncate(content: unknown, limit: number): unknown {
  const encoded = JSON.stringify(content ?? {});
  const bytes = new TextEncoder().encode(encoded);
  if (bytes.length <= limit) {
    return content;
  }

  // The preview is decoded from the first `limit` bytes with a lossy decoder, so
  // a cut in the middle of a multi-byte rune still produces a valid document.
  const preview = new TextDecoder("utf-8", { fatal: false }).decode(bytes.subarray(0, limit));
  const document: Truncated = { truncated: true, bytes: bytes.length, preview };
  return document;
}
