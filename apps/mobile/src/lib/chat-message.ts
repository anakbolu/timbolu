import type { ChatMessage, MessageAttachment, MessageBlock, MessageStatus } from "./api";

const MESSAGE_STATUSES: Record<string, true> = {
  complete: true,
  streaming: true,
  partial: true,
  failed: true,
};

export function isMessageStatus(value: unknown): value is MessageStatus {
  return typeof value === "string" && MESSAGE_STATUSES[value] === true;
}

/**
 * Extract draft ID from a draft block, or undefined if not a draft block.
 */
export function draftIdOf(block: MessageBlock): string | undefined {
  if (block.type === "draft" && typeof block.draft_id === "string" && block.draft_id !== "") {
    return block.draft_id;
  }
  return undefined;
}

/**
 * Find all draft IDs present in a message's blocks.
 */
export function draftIdsOf(blocks: MessageBlock[]): string[] {
  const ids: string[] = [];
  for (const block of blocks) {
    const id = draftIdOf(block);
    if (id && !ids.includes(id)) {
      ids.push(id);
    }
  }
  return ids;
}

/**
 * The readable text of one message.
 *
 * A body is a list of typed blocks: `text` renders markdown, `draft` renders
 * "Draf menunggu persetujuan", and the other four types are counted rather than
 * dropped silently so a message never looks empty when it carried a table or a chart.
 */
export function messageText(blocks: MessageBlock[]): string {
  const parts: string[] = [];
  let others = 0;
  for (const block of blocks) {
    if (block.type === "text" && typeof block.markdown === "string") {
      parts.push(block.markdown);
    } else if (block.type === "draft") {
      parts.push("Draf menunggu persetujuan");
    } else {
      others += 1;
    }
  }
  if (others > 0) {
    parts.push(`(${others} bagian lain belum bisa ditampilkan di app ini)`);
  }
  return parts.join("\n\n").trim();
}

/**
 * Filter and map raw attachments payload into safe MessageAttachment records.
 * Non-array or malformed items are ignored or given safe defaults.
 */
function parseAttachments(raw: unknown): MessageAttachment[] {
  if (!Array.isArray(raw)) return [];

  const attachments: MessageAttachment[] = [];
  for (const item of raw) {
    if (typeof item !== "object" || item === null) continue;
    const entry = item as Record<string, unknown>;

    if (
      typeof entry.id !== "string" ||
      entry.id === "" ||
      typeof entry.filename !== "string" ||
      typeof entry.url !== "string"
    ) {
      continue;
    }

    attachments.push({
      id: entry.id,
      filename: entry.filename,
      content_type: typeof entry.content_type === "string" ? entry.content_type : "",
      byte_size:
        typeof entry.byte_size === "number" && Number.isFinite(entry.byte_size) && entry.byte_size >= 0
          ? entry.byte_size
          : 0,
      url: entry.url,
    });
  }

  return attachments;
}

/**
 * Reads the message an event carries, or null when it is not one this screen
 * can draw. The payload comes from the network, so every field it needs is
 * checked rather than asserted.
 */
export function readMessage(value: unknown): ChatMessage | null {
  if (typeof value !== "object" || value === null) return null;
  const raw = value as Record<string, unknown>;

  if (typeof raw.message_id !== "string" || raw.message_id === "") return null;
  if (typeof raw.conversation_id !== "string") return null;
  if (!isMessageStatus(raw.status)) return null;
  if (!Array.isArray(raw.blocks)) return null;

  const blocks = raw.blocks.filter((block): block is MessageBlock => {
    return typeof block === "object" && block !== null && "type" in block && typeof block.type === "string";
  });

  return {
    message_id: raw.message_id,
    conversation_id: raw.conversation_id,
    agent_id: typeof raw.agent_id === "string" && raw.agent_id !== "" ? raw.agent_id : undefined,
    user_id: typeof raw.user_id === "string" && raw.user_id !== "" ? raw.user_id : undefined,
    blocks,
    attachments: parseAttachments(raw.attachments),
    task_id: typeof raw.task_id === "string" && raw.task_id !== "" ? raw.task_id : undefined,
    status: raw.status,
    finish_reason: typeof raw.finish_reason === "string" ? raw.finish_reason : undefined,
    // A message without a timestamp would sort unpredictably; stamping it once,
    // here, keeps the thread's comparator a pure function of the messages.
    created_at:
      typeof raw.created_at === "string" && raw.created_at !== ""
        ? raw.created_at
        : new Date().toISOString(),
  };
}
