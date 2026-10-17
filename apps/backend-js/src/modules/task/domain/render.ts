/**
 * Turns a stored message body into the text a model reads.
 *
 * It lives in the domain because both the workflow and the activities need it,
 * and neither may depend on the other: the workflow is the deterministic half and
 * the activities are the side-effecting half, so a shared helper has to sit
 * below both.
 *
 * A block the model cannot read is summarised rather than dropped. A model that
 * forgot it already produced a chart would produce it again, and a second chart
 * costs a whole round.
 *
 * Ported 1:1 from internal/modules/task/domain/render.go.
 */

import { NilUUID, type Block } from "./task.ts";

/** Decodes one block body, or null when it is not a JSON document. */
function decodeBody(body: string): unknown {
  if (body === "") return null;
  try {
    return JSON.parse(body) as unknown;
  } catch {
    return null;
  }
}

/** Reads the `markdown` field of a text block, or "" when it is absent. */
function markdownOf(body: unknown): string {
  if (body && typeof body === "object" && "markdown" in body) {
    const value = body.markdown;
    if (typeof value === "string") return value;
  }
  return "";
}

/** Renders blocks as the text a model reads, summarising what it cannot. */
export function RenderBlocks(blocks: Block[]): string {
  const parts: string[] = [];

  for (const block of blocks) {
    switch (block.Type) {
      case "text":
        parts.push(markdownOf(decodeBody(block.Body)));
        break;
      case "chart":
        parts.push("[grafik]");
        break;
      case "mermaid":
        parts.push("[diagram]");
        break;
      case "html":
        parts.push("[konten interaktif]");
        break;
      case "table":
        parts.push("[tabel]");
        break;
      case "draft":
        parts.push("[draf menunggu persetujuan]");
        break;
    }
  }

  return parts.join("\n\n");
}

/**
 * Renders an id for a wire shape, or the empty string when there is none.
 *
 * Absence is an empty string rather than a zero uuid because encoding/json
 * cannot omit a `uuid.UUID` — it is an array — so a zero value would be
 * published and read as a real id.
 */
export function WireID(id: string | null | undefined): string {
  if (!id || id === NilUUID) return "";
  return id;
}
