/**
 * The files a round's messages carry, read into the prompt.
 *
 * It is the activity's work rather than the workflow's because it touches object
 * storage and a PDF parser: the workflow keeps references, and the content enters
 * the prompt here, inside the call that is about to send it.
 *
 * Ported 1:1 from internal/modules/task/activity/attachments.go.
 */

import type { Message as LLMMessage } from "../../../platform/llm/registry.ts";
import * as attachtext from "../../../platform/attachtext/attachtext.ts";
import type { AttachmentReader, Scope, Task, Turn } from "../domain/task.ts";
import type { Logger } from "../../../platform/logger.ts";
import { taskLogger } from "./tracing.ts";

/** What a file's note says when this deployment has nothing that can read attachments. */
export const errNoAttachmentReader = new Error(
  "task: attachments cannot be read in this deployment",
);

/** What the attachment helpers need from the activity set. */
export interface AttachmentDeps {
  attachments?: AttachmentReader | null;
  attachmentLimits?: attachtext.Limits;
  logger?: Logger | null;
}

/**
 * Reads the files of one round's messages.
 *
 * The budget is spent newest first, and one renderer serves the whole round: what
 * the person just sent is what the task is about, so it gets the room.
 *
 * A file is read once. The same attachment can sit on two messages, and fetching
 * it twice buys the model the same bytes twice: the newest mention keeps the
 * content, and the older one only loses the file, not the text beside it.
 */
export async function attachmentTurns(
  deps: AttachmentDeps,
  ctx: AbortSignal | undefined,
  scope: Scope,
  task: Task,
  messages: Turn[],
): Promise<Array<attachtext.Turn | null>> {
  const turns: Array<attachtext.Turn | null> = messages.map(() => null);
  const renderer = new attachtext.Renderer(deps.attachmentLimits ?? {});
  const read = new Set<string>();

  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index];
    if (!message || message.Attachments.length === 0) {
      continue;
    }

    const turn = new attachtext.Turn();
    for (const ref of message.Attachments) {
      if (read.has(ref.ID)) {
        continue;
      }
      read.add(ref.ID);

      const part = renderer.render(await readAttachment(deps, ctx, scope, ref));
      if (part.err) {
        // A file that was meant to be read and was not is worth a line in the
        // log: the model only ever sees the note that stands in for it.
        taskLogger(deps, task).warn(
          { attachment: ref.Filename, err: part.err.message },
          "an attachment could not be read",
        );
      }
      turn.add(part);
    }
    if (turn.empty()) {
      continue;
    }
    turns[index] = turn;
  }

  return turns;
}

/**
 * Fetches one file's bytes, or reports why it could not.
 *
 * The size is checked before the object is fetched, so a file that is over the
 * bound costs a note rather than a download. A deployment with no reader names
 * every file and reads none.
 */
async function readAttachment(
  deps: AttachmentDeps,
  ctx: AbortSignal | undefined,
  scope: Scope,
  ref: { ID: string; Filename: string; ContentType: string; ByteSize: number },
): Promise<attachtext.File> {
  const file: attachtext.File = {
    filename: ref.Filename,
    contentType: ref.ContentType,
    byteSize: ref.ByteSize,
    content: new Uint8Array(),
    err: null,
  };

  if (!deps.attachments) {
    file.err = errNoAttachmentReader;
    return file;
  }
  if (attachtext.oversized(deps.attachmentLimits ?? {}, ref.ByteSize)) {
    file.err = attachtext.ErrTooLarge;
    return file;
  }

  try {
    const content = await deps.attachments.readAttachment(ctx, scope, ref.ID);
    file.content = content.content;
  } catch (err) {
    file.err = err instanceof Error ? err : new Error(String(err));
  }
  return file;
}

/** Reports whether the rendered round would send a picture. */
export function carriedImages(turns: Array<attachtext.Turn | null>): boolean {
  return turns.some((turn) => turn !== null && turn.images().length > 0);
}

/** Renders one turn's text with what its files contribute, and the pictures when the chain can see them. */
export function attachMessage(
  message: LLMMessage,
  attachments: attachtext.Turn | null,
  vision: boolean,
): LLMMessage {
  if (!attachments) return message;

  const text = attachments.text(vision);
  if (text !== "") {
    message.text = message.text === "" ? text : `${message.text}\n\n${text}`;
  }
  if (!vision) {
    return message;
  }

  for (const image of attachments.images()) {
    message.images.push({ mediaType: image.mediaType, data: image.data });
  }

  return message;
}

/**
 * Reports whether a rendered message contributes nothing to the request: no
 * text, no tool call, no tool answer, no picture.
 */
export function saysNothing(message: LLMMessage): boolean {
  return (
    message.text.trim() === "" &&
    message.toolCalls.length === 0 &&
    message.toolResults.length === 0 &&
    message.images.length === 0
  );
}
