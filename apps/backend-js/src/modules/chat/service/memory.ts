/**
 * The two things a conversation asks of the memory module.
 *
 * Both belong to the message rather than to the answer: a message may ask for
 * something to be remembered, and every message is a sign that a conversation is
 * still moving. Neither is the chat module's business, so both are ports.
 *
 * Ported 1:1 from internal/modules/chat/service/memory.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import {
  MessageComplete,
  isNil,
  newMessage,
  type Conversation,
  type Message,
} from "../domain/conversation.ts";
import { EventMessageNew } from "../domain/event.ts";
import type { Memory, Scope } from "../domain/ports.ts";
import type { SendResult } from "../domain/service.ts";
import { textBlock } from "./reply.ts";

/** What the memory helpers need. */
export interface MemoryDeps {
  memory: Memory;
  append: (scope: Scope, message: Message) => Promise<Message>;
  emitMessage: (scope: Scope, message: Message, conversationId: string, type: string) => Promise<void>;
  logger: Logger | null;
}

/**
 * Tells the memory module that this conversation moved.
 *
 * It runs on every message rather than once per thread, because the point is the
 * quiet period: each message pushes the extraction further out, so a thread is
 * read after it stops rather than while it is being written.
 */
export async function scheduleExtraction(
  deps: MemoryDeps,
  scope: Scope,
  conversation: Conversation,
  addressed: string,
): Promise<void> {
  const agentId = memoryBolu(conversation, addressed);

  try {
    await deps.memory.conversationActive(scope, conversation.id, agentId);
  } catch (err) {
    // A conversation that cannot be scheduled is simply never extracted
    // automatically. That is worth a warning and not worth failing the message
    // the user just sent.
    deps.logger?.warn(
      { conversation_id: conversation.id, err: (err as Error).message },
      "could not schedule memory extraction for this conversation",
    );
  }
}

/**
 * Decides which Bolu a conversation's extraction is attributed to.
 *
 * The message's own address wins, because that is who the fact is about. A
 * direct thread has exactly one Bolu, so it is that one. A group message nobody
 * addressed has no answer, and the extraction then writes only to the main
 * memory.
 */
function memoryBolu(conversation: Conversation, addressed: string): string {
  if (!isNil(addressed)) {
    return addressed;
  }
  for (const participant of conversation.participants) {
    if (!isNil(participant.agentId)) {
      return participant.agentId;
    }
  }
  return "";
}

/**
 * Stores what a message asked to be remembered and writes the answer.
 *
 * It reports whether it took the message over. When it did, the reply is already
 * in the thread and no task is opened: what the user asked for is a note in the
 * Bolu's memory, and the confirmation of that is a cheaper and more accurate
 * answer than a model call.
 */
export async function remember(
  deps: MemoryDeps,
  scope: Scope,
  conversation: Conversation,
  responderId: string,
  userMessage: Message,
  text: string,
  result: SendResult,
): Promise<boolean> {
  let outcome;
  try {
    outcome = await deps.memory.remember(scope, {
      text,
      agentId: responderId,
      messageId: userMessage.id,
    });
  } catch (err) {
    // The message is not lost: the ordinary reply runs, so a memory module that
    // is down degrades to a Bolu that answers like before.
    deps.logger?.warn(
      { conversation_id: conversation.id, err: (err as Error).message },
      "a memory request could not be handled; answering normally",
    );
    return false;
  }

  if (!outcome.handled) {
    return false;
  }

  const confirmation = await deps.append(
    scope,
    newMessage({
      workspaceId: scope.workspaceId,
      conversationId: conversation.id,
      authorAgentId: responderId,
      blocks: [textBlock(outcome.confirmation)],
      status: MessageComplete,
    }),
  );
  await deps.emitMessage(scope, confirmation, conversation.id, EventMessageNew);

  result.replyMessageIds = [confirmation.id];
  return true;
}
