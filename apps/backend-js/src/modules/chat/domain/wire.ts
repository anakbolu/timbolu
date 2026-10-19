/**
 * The wire shapes the API and the event stream share.
 *
 * Ported 1:1 from internal/modules/chat/domain/wire.go. This is the single
 * place the message shape is built, so the message a client receives in a
 * response and the one it receives on the stream cannot drift apart — which is
 * what once let a user's message be published with a zero agent id and render
 * as a Bolu's.
 */

import { isNil, type Attachment, type Block, type Message } from "./conversation.ts";
import type { AgentStatePayload, AttachmentPayload, MessagePayload } from "./event.ts";

/** Renders one message for the API and for the event stream. */
export function WireMessage(message: Message): MessagePayload {
  const attachments: AttachmentPayload[] = message.attachments.map(WireAttachment);

  const payload: MessagePayload = {
    message_id: message.id,
    conversation_id: message.conversationId,
    status: message.status,
    blocks: WireBlocks(message.blocks),
    attachments,
  };

  const agentId = WireID(message.authorAgentId);
  if (agentId) payload.agent_id = agentId;
  const userId = WireID(message.authorUserId);
  if (userId) payload.user_id = userId;
  const taskId = WireID(message.taskId);
  if (taskId) payload.task_id = taskId;
  if (message.finishReason) payload.finish_reason = message.finishReason;
  // Go formats the stored timestamp with RFC3339Nano, and only when it is set.
  if (message.createdAt instanceof Date && !Number.isNaN(message.createdAt.getTime())) {
    payload.created_at = message.createdAt.toISOString();
  }

  return payload;
}

/** Renders one attachment for a client. The storage key stays on the server. */
export function WireAttachment(attachment: Attachment): AttachmentPayload {
  return {
    id: attachment.id,
    filename: attachment.filename,
    content_type: attachment.contentType,
    byte_size: attachment.byteSize,
    url: `/api/attachments/${attachment.id}`,
  };
}

/**
 * Renders a message body. An empty body is an empty list rather than null, so a
 * client never has to handle both.
 */
export function WireBlocks(blocks: Block[]): unknown[] {
  const raw: unknown[] = [];
  for (const block of blocks) {
    if (block.body === undefined || block.body === null) {
      continue;
    }
    raw.push(block.body);
  }
  return raw;
}

/**
 * Renders an id, or the empty string when there is none.
 *
 * The empty string is what absence looks like on the wire: a zero uuid is an
 * array in Go, so `omitempty` would publish it rather than omit it.
 */
export function WireID(id: string): string {
  return isNil(id) ? "" : id;
}

/** Renders a Bolu's derived state for the wire. */
export function WireState(
  agentId: string,
  state: string,
  reason: string,
  taskId: string,
  draftId: string,
): AgentStatePayload {
  const payload: AgentStatePayload = { agent_id: WireID(agentId), state };
  if (reason) payload.reason = reason;
  const task = WireID(taskId);
  if (task) payload.task_id = task;
  const draft = WireID(draftId);
  if (draft) payload.draft_id = draft;
  return payload;
}
