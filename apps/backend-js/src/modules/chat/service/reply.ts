/**
 * Sending a message, and producing the answer to it.
 *
 * `send` stores the user's message and starts the reply, then returns: the
 * answer is produced in the background and delivered on the event stream, which
 * is what makes closing the tab harmless. The tokens it costs are already paid
 * for, so the reply is written whether or not anyone is watching.
 *
 * Ported 1:1 from internal/modules/chat/service/reply.go.
 */

import {
  ChunkBlock,
  ChunkDone,
  ChunkError,
  ChunkNotice,
  ChunkText,
  EventMessageNew,
  EventMessageUpdated,
  StateIdle,
  StateThinking,
  StateWorking,
  type StreamChunk,
} from "../domain/event.ts";
import {
  BlockText,
  ErrAgentNotInConversation,
  ErrInvalidInput,
  ErrNoResponder,
  KindDirect,
  MessageComplete,
  MessageFailed,
  MessagePartial,
  MessageStreaming,
  WrappedError,
  decodeOrNull,
  isNil,
  newMessage,
  type Attachment,
  type Block,
  type Conversation,
  type Message,
} from "../domain/conversation.ts";
import {
  ErrNoBudget,
  TriggerChat,
  type AgentRef,
  type RouterDecision,
  type Scope,
} from "../domain/ports.ts";
import { ValidateBlock } from "../domain/block.ts";
import type { SendRequest, SendResult } from "../domain/service.ts";
import { TextMaxRunes, dedupe, type Service } from "./service.ts";
import { mentioned } from "./mention.ts";

/** How much of a conversation is given to the model. */
const historyLimit = 30;

/** One Bolu that will answer, and how it was chosen. */
interface ResponderPick {
  agent: AgentRef;
  /** Only a routed pick carries one. */
  decision: RouterDecision;
  routed: boolean;
}

/**
 * Stores the user's message and starts the reply.
 *
 * It returns as soon as the message is stored. The answer arrives on the event
 * stream, so closing the tab does not lose it.
 */
export async function sendMessage(
  service: Service,
  scope: Scope,
  req: SendRequest,
): Promise<SendResult> {
  const text = req.text.trim();
  // A message opened for an upload is allowed to carry no text: the file is the
  // message, and the client sends it before the words.
  if (text === "" && req.attachmentIds.length === 0 && !req.forAttachment) {
    throw new WrappedError(ErrInvalidInput, "write something first");
  }
  if (Array.from(text).length > TextMaxRunes) {
    throw new WrappedError(ErrInvalidInput, `the message is longer than ${TextMaxRunes} characters`);
  }

  const conversation = await service.deps.conversations.getConversation(scope, req.conversationId);

  const attachments = await resolveAttachments(service, scope, req.attachmentIds);

  const blocks: Block[] = [];
  if (text !== "") {
    blocks.push(textBlock(text));
  }

  const userMessage = await service.deps.messages.append(
    scope,
    newMessage({
      workspaceId: scope.workspaceId,
      conversationId: conversation.id,
      authorUserId: scope.userId,
      blocks,
      attachments,
      status: MessageComplete,
    }),
  );

  await service.emitMessage(scope, userMessage, conversation.id, EventMessageNew);

  const result: SendResult = {
    message: userMessage,
    replyMessageIds: [],
    responders: [],
    routed: false,
  };

  if (service.deps.memory !== null) {
    // The conversation moved, so any extraction waiting on it starts over. This
    // runs before the reply decision on purpose: a message that does not ask for
    // an answer is still a message.
    await service.scheduleExtraction(scope, conversation, req.agentId);
  }

  // A message opened for an attachment is a container, not a question: it never
  // starts a reply, whatever the caller asked for.
  if (!req.reply || req.forAttachment) {
    return result;
  }

  const picks = await pickResponders(service, scope, conversation, userMessage, req.agentId, text);

  if (service.deps.memory !== null) {
    // A message that asks for something to be remembered is answered here
    // instead of by the model: the module that stores it is the module that
    // names what it stored. It also costs no tokens.
    const handled = await service.remember(
      scope,
      conversation,
      picks[0].agent.id,
      userMessage,
      text,
      result,
    );
    if (handled) {
      // One Bolu wrote the confirmation, so one Bolu is what answered.
      result.responders = [picks[0].agent];
      return result;
    }
  }

  // Every picked Bolu answers: a group message that names two of them is a
  // request to two of them.
  result.responders = picks.map((pick) => pick.agent);
  // Naming a Bolu is not the router's doing; only the router's own pick reports
  // itself.
  result.routed = picks[0].routed;

  for (const pick of picks) {
    // The placeholder is written before the answer starts, so a client can show
    // the Bolu thinking and follow that one message. It also means a crash
    // mid-answer leaves a visible partial rather than nothing.
    const placeholder = await service.deps.messages.append(
      scope,
      newMessage({
        workspaceId: scope.workspaceId,
        conversationId: conversation.id,
        authorAgentId: pick.agent.id,
        blocks: [],
        status: MessageStreaming,
      }),
    );
    result.replyMessageIds.push(placeholder.id);

    await service.emitMessage(scope, placeholder, conversation.id, EventMessageNew);
    if (pick.routed) {
      await service.emitRouterDecision(scope, conversation, userMessage, pick.decision);
    }

    // The durable runtime owns the answer when it is configured. A task survives
    // the request, a restart, and an approval that takes hours; the in-process
    // reply below does not, which is why it is the fallback rather than the
    // default.
    if (service.deps.tasks !== null) {
      await dispatchTask(service, scope, conversation, pick.agent, userMessage, placeholder, text, result);
      continue;
    }

    // The reply runs on its own: the request that started it ends as soon as the
    // message is stored, and the answer must not end with it. The promise is
    // deliberately not awaited.
    void service.runReply(scope, conversation, pick.agent, userMessage, placeholder).catch((err) => {
      service.deps.logger?.error(
        { message_id: placeholder.id, err: (err as Error).message },
        "a reply could not be produced",
      );
    });
  }

  return result;
}

/**
 * Hands the message to the durable runtime and records the task on the
 * placeholder, so a client can follow the task from the message it answers.
 *
 * A dispatch that fails marks the placeholder failed rather than leaving it
 * streaming forever.
 */
async function dispatchTask(
  service: Service,
  scope: Scope,
  conversation: Conversation,
  responder: AgentRef,
  userMessage: Message,
  placeholder: Message,
  prompt: string,
  result: SendResult,
): Promise<void> {
  if (service.deps.tasks === null) return;

  let taskId: string;
  try {
    taskId = await service.deps.tasks.start(scope, {
      conversationId: conversation.id,
      messageId: placeholder.id,
      agentId: responder.id,
      trigger: TriggerChat,
      title: prompt,
    });
  } catch (err) {
    await finishReply(
      service,
      scope,
      conversation,
      responder,
      placeholder,
      null,
      MessageFailed,
      (err as Error).message,
    );

    // A refusal a person has to act on is passed through as it is: what fixes it
    // is money rather than another attempt, and the sentence the package wrote
    // already says which.
    if (err === ErrNoBudget) {
      throw err;
    }
    throw new Error(`chat: open task: ${(err as Error).message}`);
  }

  placeholder.taskId = taskId;
  try {
    placeholder = await service.deps.messages.update(scope, placeholder);
    result.message = userMessage;
  } catch {
    // The task is opened; a failed write is reported by the next read.
  }

  await service.emitMessage(scope, placeholder, conversation.id, EventMessageUpdated);
  await service.emitAgentState(scope, responder.id, StateWorking, {
    agent_id: responder.id,
    state: StateWorking,
    reason: "mengerjakan tugas",
    task_id: taskId,
  });
}

/** Loads the metadata of the files named in a message. */
async function resolveAttachments(
  service: Service,
  scope: Scope,
  ids: string[],
): Promise<Attachment[]> {
  if (ids.length === 0) {
    return [];
  }

  const attachments: Attachment[] = [];
  for (const id of dedupe(ids)) {
    attachments.push(await service.deps.attachments.open(scope, id));
  }
  return attachments;
}

/**
 * Decides which Bolu answer.
 *
 * A pinned Bolu wins. A direct thread answers with the one Bolu it is with. A
 * group message that names Bolu goes to the ones it names, and each of them
 * answers. A group message that names nobody goes to the router, which picks
 * exactly one.
 */
async function pickResponders(
  service: Service,
  scope: Scope,
  conversation: Conversation,
  message: Message,
  pinned: string,
  text: string,
): Promise<ResponderPick[]> {
  const agents = await agentsOf(service, scope, conversation);
  if (agents.length === 0) {
    throw ErrNoResponder;
  }

  if (!isNil(pinned)) {
    for (const agent of agents) {
      if (agent.id === pinned) {
        return [
          {
            agent,
            decision: {
              agentId: pinned,
              reason: "diminta langsung",
              source: "fallback",
              candidates: 0,
            },
            routed: false,
          },
        ];
      }
    }
    throw ErrAgentNotInConversation;
  }

  if (conversation.kind === KindDirect) {
    const agent = agents[0];
    if (!service.deps.agents.active(agent)) {
      // A resting Bolu is left alone rather than woken: the switch is the
      // user's, and a reply would contradict it.
      throw new WrappedError(ErrNoResponder, `${agent.name} sedang istirahat`);
    }
    return [
      {
        agent,
        decision: { agentId: agent.id, reason: "obrolan langsung", source: "fallback", candidates: 1 },
        routed: false,
      },
    ];
  }

  // The names in the message come first: a person who types "@Biru" has said who
  // they want, and a router that second-guessed them would be answering a
  // question nobody asked.
  const named = mentioned(agents, text);
  if (named.length > 0) {
    return named.map((agent) => ({
      agent,
      decision: { agentId: agent.id, reason: "", source: "", candidates: 0 },
      routed: false,
    }));
  }

  if (service.deps.router === null) {
    throw ErrNoResponder;
  }

  const decision = await service.deps.router.pick(scope, conversation, message);
  for (const agent of agents) {
    if (agent.id === decision.agentId) {
      return [{ agent, decision, routed: true }];
    }
  }

  throw ErrNoResponder;
}

/** The Bolu of a conversation, in the order the participants are listed. */
async function agentsOf(
  service: Service,
  scope: Scope,
  conversation: Conversation,
): Promise<AgentRef[]> {
  const ids = conversation.participants
    .filter((participant) => !isNil(participant.agentId))
    .map((participant) => participant.agentId);
  if (ids.length === 0) {
    return [];
  }
  return service.deps.agents.agents(scope, ids);
}

/**
 * Produces one answer and keeps the stored message in step with it.
 *
 * The message is the record, so it is updated as the answer grows and finished
 * in every outcome: complete, partial, or failed. A stream that dies therefore
 * leaves the tokens it produced behind, marked for what they are.
 */
export async function runReply(
  service: Service,
  scope: Scope,
  conversation: Conversation,
  agent: AgentRef,
  prompt: Message,
  placeholder: Message,
): Promise<void> {
  let history: Message[];
  try {
    history = await service.deps.messages.latestMessages(scope, conversation.id, historyLimit);
  } catch (err) {
    await finishReply(
      service,
      scope,
      conversation,
      agent,
      placeholder,
      null,
      MessageFailed,
      (err as Error).message,
    );
    return;
  }

  // The placeholder is part of the history it must not be part of: a model given
  // its own empty answer would continue it instead of answering.
  history = history.filter((message) => message.id !== placeholder.id);

  await service.emitAgentState(scope, agent.id, StateThinking, {
    agent_id: agent.id,
    state: StateThinking,
    reason: "membaca pesanmu",
    task_id: placeholder.taskId,
  });

  let events: AsyncIterable<StreamChunk>;
  try {
    events = await service.deps.responder.reply(scope, {
      conversation,
      agent,
      message: prompt,
      history,
      repairBudget: service.settings.repairBudget,
    });
  } catch (err) {
    await finishReply(
      service,
      scope,
      conversation,
      agent,
      placeholder,
      null,
      MessageFailed,
      (err as Error).message,
    );
    return;
  }

  const collector = new ReplyCollector(service.clock);

  for await (const chunk of events) {
    switch (chunk.kind) {
      case ChunkText:
        collector.appendText(chunk.text ?? "");
        await flushIfDue(service, scope, conversation, placeholder, collector);
        break;

      case ChunkNotice:
        collector.appendNotice(chunk.text ?? "");
        break;

      case ChunkBlock: {
        const block = decodeBlock(chunk.block);
        if (block === null) {
          continue;
        }
        collector.appendBlock(block);
        await flushIfDue(service, scope, conversation, placeholder, collector, true);
        break;
      }

      case ChunkError: {
        const reason = chunk.error || "penyedia gagal menjawab";
        const status = collector.empty() ? MessageFailed : MessagePartial;
        collector.finish(status, reason);
        await finishReply(service, scope, conversation, agent, placeholder, collector, status, reason);
        return;
      }

      case ChunkDone:
        // The answer ended; the code below finishes it so a provider that sends
        // Done and then closes does not finish twice.
        break;
    }
  }

  let status = MessageComplete;
  let reason = collector.reason;
  if (collector.error !== null) {
    status = collector.empty() ? MessageFailed : MessagePartial;
    reason = collector.error;
  }
  collector.finish(status, reason);

  await finishReply(service, scope, conversation, agent, placeholder, collector, status, reason);
}

/**
 * Writes and publishes the growing message on a timer, so a token costs neither
 * a database write nor a broadcast.
 */
async function flushIfDue(
  service: Service,
  scope: Scope,
  conversation: Conversation,
  placeholder: Message,
  collector: ReplyCollector,
  force = false,
): Promise<void> {
  if (!force && service.clock().getTime() - collector.flushAt.getTime() < service.settings.streamFlush) {
    return;
  }

  collector.flushAt = service.clock();
  await updateMessage(service, scope, conversation, placeholder, collector, MessageStreaming, "");
}

/** Writes the final state of the answer and tells the clients. */
async function finishReply(
  service: Service,
  scope: Scope,
  conversation: Conversation,
  agent: AgentRef,
  placeholder: Message,
  collector: ReplyCollector | null,
  status: string,
  reason: string,
): Promise<void> {
  await updateMessage(service, scope, conversation, placeholder, collector, status, reason);

  await service.emitAgentState(scope, agent.id, StateIdle, {
    agent_id: agent.id,
    state: StateIdle,
    reason: status === MessageComplete ? "menunggu pesan berikutnya" : reason,
  });
}

/** Stores the current body and publishes it. */
async function updateMessage(
  service: Service,
  scope: Scope,
  conversation: Conversation,
  placeholder: Message,
  collector: ReplyCollector | null,
  status: string,
  reason: string,
): Promise<Message> {
  let stored: Message;
  try {
    stored = await service.deps.messages.update(scope, {
      ...placeholder,
      blocks: collector?.blocks ?? [],
      status,
      finishReason: reason,
      taskId: placeholder.taskId,
    });
  } catch (err) {
    service.deps.logger?.error(
      { message_id: placeholder.id, err: (err as Error).message },
      "could not store the reply",
    );
    return placeholder;
  }

  // An update, never a new message: the placeholder already exists on the
  // client, and announcing it again would show it twice.
  await service.emitMessage(scope, stored, conversation.id, EventMessageUpdated);
  return stored;
}

/**
 * Accumulates one answer.
 *
 * It is the only writer of the message body while the answer arrives, so the
 * stored body and the published one cannot disagree: both come from here.
 */
export class ReplyCollector {
  blocks: Block[] = [];
  reason = "";
  error: string | null = null;
  flushAt: Date;

  private lastText = "";

  constructor(clock: () => Date) {
    this.flushAt = clock();
  }

  appendText(text: string): void {
    if (text === "") {
      return;
    }
    this.lastText += text;
    this.replaceText();
  }

  appendNotice(notice: string): void {
    if (notice === "") {
      return;
    }
    // A notice is shown as text so a client that renders only text still says
    // what is happening; the prefix marks it as a status rather than an answer.
    this.lastText += `\n\n_${notice}_`;
    this.replaceText();
  }

  appendBlock(block: Block): void {
    // A block ends the text that came before it, so the text block is kept and
    // the next text starts a new one. That keeps the order the model produced.
    this.blocks.push(block);
    this.lastText = "";
  }

  /** Keeps the trailing text block in step with what arrived. */
  private replaceText(): void {
    if (this.lastText === "") {
      return;
    }

    const block = textBlock(this.lastText);
    const tail = this.blocks[this.blocks.length - 1];
    if (tail && tail.type === BlockText) {
      this.blocks[this.blocks.length - 1] = block;
      return;
    }
    this.blocks.push(block);
  }

  finish(status: string, reason: string): void {
    this.reason = reason;
    if (status === MessageFailed || status === MessagePartial) {
      this.error = reason;
    }
  }

  /** True when the answer produced nothing but blank text. */
  empty(): boolean {
    for (const block of this.blocks) {
      if (block.type !== BlockText) {
        return false;
      }
      const document = decodeOrNull(block.body);
      const markdown = document !== null && typeof document.markdown === "string" ? document.markdown : "";
      if (markdown.trim() !== "") {
        return false;
      }
    }
    return true;
  }
}

/**
 * Builds a text block, which is the one block type the service itself produces.
 * It is validated like any other, so a body the validator would refuse never
 * reaches a row.
 */
export function textBlock(markdown: string): Block {
  const block: Block = { type: BlockText, body: { type: BlockText, markdown } };
  ValidateBlock(block);
  return block;
}

/** Reads a block a responder produced, so it can be validated before storage. */
export function decodeBlock(raw: unknown): Block | null {
  const document = decodeOrNull(raw);
  if (document === null) {
    return null;
  }
  const type = typeof document.type === "string" ? document.type : "";
  return { type, body: document };
}
