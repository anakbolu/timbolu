/**
 * How a conversation's stored messages become the request the model reads.
 *
 * A message is a list of blocks, and its files contribute text (and, for
 * pictures, images). The two are kept apart until the chain is known, because a
 * model that cannot see pictures reads the same turn with every picture replaced
 * by the line that says so.
 *
 * The budget is spent newest first, and one renderer serves the whole request:
 * what a person just sent is what they are asking about, so it is the file that
 * gets the room, and an older thread full of documents degrades into the notes
 * that name them.
 *
 * Ported 1:1 from internal/modules/chat/service/attachment_context.go.
 */

import * as attachtext from "../../../platform/attachtext/attachtext.ts";
import type { Logger } from "../../../platform/logger.ts";
import {
  RoleAssistant,
  RoleUser,
  type Image,
  type Message as LLMMessage,
  type Role,
} from "../../llm/domain/provider.ts";
import {
  ErrStorageUnavailable,
  messageFromAgent,
  type Attachment,
  type Block,
  type Message,
} from "../domain/conversation.ts";
import type { ObjectStore, Scope } from "../domain/ports.ts";

/** One stored message as the model will read it. */
export interface ConversationTurn {
  role: Role;
  text: string;
  /** Null for a message without files, which is the common case. */
  attachments: attachtext.Turn | null;
}

/** What the turn builder needs to fetch and bound a message's files. */
export interface TurnDeps {
  storage: ObjectStore | null;
  limits: attachtext.Limits;
  logger: Logger;
}

/** Renders the turn for a model that can, or cannot, see pictures. */
export function turnMessage(turn: ConversationTurn, vision: boolean): LLMMessage {
  const message: LLMMessage = { role: turn.role, text: turn.text, images: [], toolCalls: [], toolResults: [] };

  const attachments = turn.attachments?.text(vision) ?? "";
  if (attachments !== "") {
    message.text = message.text.trim() === "" ? attachments : `${message.text}\n\n${attachments}`;
  }
  if (vision && turn.attachments) {
    for (const image of turn.attachments.images()) {
      message.images.push({ mediaType: image.mediaType, data: image.data });
    }
  }

  return message;
}

/**
 * Reports whether the turn says nothing at all, which is the only case a message
 * is left out of the request. A message that is nothing but files is not empty:
 * its files are what it says.
 */
export function turnEmpty(turn: ConversationTurn): boolean {
  return turn.text.trim() === "" && (turn.attachments === null || turn.attachments.empty());
}

/**
 * Maps stored messages onto the model's message format, files included.
 *
 * A file is rendered once: uploading a file and then linking it to a message
 * leaves the same attachment on two messages, and the model reading it twice
 * pays for it twice. The newest mention is the one that keeps the content, and
 * the older one only loses the file, not the text beside it.
 */
export async function conversationTurns(
  deps: TurnDeps,
  scope: Scope,
  history: Message[],
): Promise<ConversationTurn[]> {
  const turns: ConversationTurn[] = history.map((message) => ({
    role: messageFromAgent(message) ? RoleAssistant : RoleUser,
    text: renderBlocksForModel(message.blocks),
    attachments: null,
  }));

  const renderer = new attachtext.Renderer(deps.limits);
  const read = new Set<string>();

  for (let index = history.length - 1; index >= 0; index--) {
    const message = history[index];
    if (message.attachments.length === 0) {
      continue;
    }

    const turn = new attachtext.Turn();
    for (const attachment of message.attachments) {
      if (read.has(attachment.id)) {
        continue;
      }
      read.add(attachment.id);

      const part = renderer.render(await readAttachment(deps, scope, attachment));
      if (part.err) {
        deps.logger.warn(
          { attachment: attachment.filename, workspace_id: scope.workspaceId, err: part.err.message },
          "an attachment could not be read",
        );
      }
      turn.add(part);
    }
    turns[index].attachments = turn;
  }

  return turns;
}

/** Renders the turns as the request the model is sent. */
export function turnMessages(turns: ConversationTurn[], vision: boolean): LLMMessage[] {
  const messages: LLMMessage[] = [];
  for (const turn of turns) {
    if (turnEmpty(turn)) {
      continue;
    }
    messages.push(turnMessage(turn, vision));
  }
  return messages;
}

/** Reports whether the request would carry a picture. */
export function carriesImages(turns: ConversationTurn[]): boolean {
  return turns.some((turn) => (turn.attachments?.images().length ?? 0) > 0);
}

/**
 * Fetches one file's bytes, or reports why it could not.
 *
 * The size is checked before the object is fetched, so a file that is over the
 * bound costs a note rather than a download.
 */
async function readAttachment(
  deps: TurnDeps,
  scope: Scope,
  attachment: Attachment,
): Promise<attachtext.File> {
  const file: attachtext.File = {
    filename: attachment.filename,
    contentType: attachment.contentType,
    byteSize: attachment.byteSize,
    content: new Uint8Array(0),
    err: null,
  };

  if (deps.storage === null) {
    // A deployment without object storage can still answer: the file is named,
    // and the model is told it could not be read.
    file.err = ErrStorageUnavailable;
    return file;
  }
  if (attachtext.oversized(deps.limits, attachment.byteSize)) {
    file.err = attachtext.ErrTooLarge;
    return file;
  }

  try {
    file.content = await deps.storage.get(attachment.storageKey);
  } catch (err) {
    file.err = err instanceof Error ? err : new Error(String(err));
  }
  return file;
}

/**
 * Turns a stored body into the text the model reads.
 *
 * Every block becomes one line naming what it is, because the model is given
 * the conversation as prose: a chart's data is not re-read, and a sandboxed
 * document is named rather than inlined.
 */
export function renderBlocksForModel(blocks: Block[]): string {
  const parts: string[] = [];

  for (const block of blocks) {
    const body = objectOf(block.body);
    if (body === null) {
      continue;
    }

    switch (block.type) {
      case "text": {
        const markdown = stringOf(body.markdown);
        if (markdown !== null) {
          parts.push(markdown);
        }
        break;
      }

      case "chart": {
        const spec = objectOf(body.spec);
        const kind = spec ? stringOf(spec.kind) : null;
        const title = stringOf(body.title) ?? "";
        const specTitle = spec ? (stringOf(spec.title) ?? "") : "";
        parts.push(`[grafik ${kind ?? ""}: ${firstNonEmpty(title, specTitle)}]`);
        break;
      }

      case "mermaid": {
        parts.push(`[diagram ${stringOf(body.diagram) ?? ""}: ${stringOf(body.title) ?? ""}]`);
        break;
      }

      case "html": {
        parts.push(`[konten interaktif: ${stringOf(body.title) ?? ""}]`);
        break;
      }

      case "table": {
        const columns = stringArrayOf(body.columns);
        const rows = Array.isArray(body.rows) ? body.rows : [];
        parts.push(`[tabel ${columns.length} kolom, ${rows.length} baris: ${columns.join(" | ")}]`);
        break;
      }

      case "draft": {
        parts.push(`[draf menunggu persetujuan: ${stringOf(body.title) ?? ""}]`);
        break;
      }
    }
  }

  return parts.join("\n\n");
}

/** A JSON object, or null. */
function objectOf(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

/** A string field, or null when it is absent or another type. */
function stringOf(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

/** A string array field, or an empty array. */
function stringArrayOf(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

/** The first value that is not blank. */
function firstNonEmpty(...values: string[]): string {
  for (const value of values) {
    if (value.trim() !== "") {
      return value;
    }
  }
  return "";
}

export type { Image };
