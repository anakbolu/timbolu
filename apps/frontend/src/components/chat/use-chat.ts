"use client";

/**
 * The chat state: which conversation is open, its messages, and the stream that
 * keeps it current.
 *
 * The rule the whole hook follows is that the server owns the message. A reply
 * is written by the backend whether or not a tab is open, so the client never
 * assembles one: it renders what the stream tells it, and a message it already
 * has is replaced by the version the server sends.
 *
 * The conversation list, the workspace and the connection all come from the
 * workspace store rather than from a second copy: two sockets in one tab would
 * deliver every frame twice and the last id they resume from would disagree.
 */

import { useCallback, useEffect, useRef, useState } from "react";

import { useWorkspace } from "@/components/dashboard/workspace-data";
import {
  ActivityEvent,
  AgentStatePayload,
  ApiError,
  ApiResult,
  Attachment,
  ChatConversation,
  ChatMessage,
  MessageEventPayload,
  listMessages,
  openDirectConversation,
  sendMessage,
} from "@/lib/api";
import { CHAT_EVENT_TYPES } from "@/lib/realtime";

// The events the chat renders are the list the stream subscribes to, so the
// fallback's listeners and this filter cannot drift apart.

export type RouterNotice = {
  agentId: string;
  reason: string;
  source: string;
};

/**
 * HistorySnapshot is the read the open thread was built from.
 *
 * The stream carries the same messages the history does, and after a reconnect
 * it carries them again from the backlog. A frame written before the history was
 * read is older than what the screen already shows, so applying it would put the
 * thread back to a state the server has since moved past — which is what makes a
 * slow replay overwrite a fresh page. The ids and the moment of the read are
 * kept together so those frames can be told apart from real ones.
 */
export type HistorySnapshot = {
  conversationId: string;
  /** When the server answered the read, as epoch milliseconds. */
  at: number;
  /** The messages that read returned. */
  ids: Set<string>;
};

export type ChatState = {
  workspaceId: string | null;
  conversations: ChatConversation[];
  active: ChatConversation | null;
  messages: ChatMessage[];
  /**
   * loading is true during the workspace store's initial read or while the
   * active thread's messages are being fetched from the server.
   */
  loading: boolean;
  sending: boolean;
  loadingOlder: boolean;
  hasMore: boolean;
  status: string;
  error: string | null;
  /**
   * Why opening the conversation a deep link names failed, when it did.
   *
   * It is kept apart from `error` because the API's sentence is not what the
   * screen may print: `internal server error` is a status label. The view maps
   * this through friendlyLoadFailure, which is also why the whole error is kept
   * rather than only its message — the status decides whether a retry is worth
   * offering.
   */
  openError: ApiError | null;
  /**
   * loaded reports whether the conversation list's first read has settled.
   *
   * A link whose id is missing from a list that has not loaded yet is not a
   * broken link, so a screen must wait for this before calling an id unknown.
   * The store's `ready` is set after the same first load, which includes the
   * conversation list, so it is that flag rather than a second read here.
   */
  loaded: boolean;
  /**
   * What the responding Bolu says it is doing, from its own state events.
   *
   * The agent is named because the stream is workspace-wide: another thread's
   * Bolu finishing must not rewrite this line.
   */
  activity: { agentId: string; text: string } | null;
  /** The Bolu that will answer the message being composed, once known. A group
   *  message can address more than one, so this is a list. */
  responders: { agentId: string; name: string }[];
  routerNotice: RouterNotice | null;
  open: (conversation: ChatConversation) => void;
  openByAgent: (agentId: string) => Promise<void>;
  /**
   * openByID opens a conversation the workspace already listed.
   *
   * A group is addressed by its own id rather than by a Bolu, and the sidebar
   * holds the same list, so opening one is a lookup rather than a call. It
   * reports whether the id was found, which is what lets a screen retry once
   * the store's first read lands.
   */
  openByID: (conversationId: string) => boolean;
  /**
   * send stores a message with the files it carries and answers with it.
   *
   * The attachments are already uploaded and named by id, so the message and its
   * files are written in one call: the reply sees them on the turn they were sent
   * with, and a file cannot end up hanging off a message that was stored without
   * it. A refused send answers with the API's failure, which is what tells the
   * composer which files to mark.
   */
  send: (text: string, attachmentIds?: string[], agentId?: string) => Promise<ApiResult<ChatMessage>>;
  loadOlder: () => Promise<void>;
  refresh: () => Promise<void>;
};

export function useChat(): ChatState {
  const store = useWorkspace();
  const workspaceId = store.workspaceID;
  // The store's object identity changes on every load, so the stable callbacks
  // are taken out of it: an effect depending on the store itself would resubscribe
  // on every event.
  const { reloadConversations, subscribe } = store;

  const [active, setActive] = useState<ChatConversation | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [sending, setSending] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [threadLoading, setThreadLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [openError, setOpenError] = useState<ApiError | null>(null);
  const [responders, setResponders] = useState<{ agentId: string; name: string }[]>([]);
  const [routerNotice, setRouterNotice] = useState<RouterNotice | null>(null);
  const [activity, setActivity] = useState<{ agentId: string; text: string } | null>(null);

  const cursor = useRef<string | undefined>(undefined);
  // The open conversation is read inside the event handler, which is registered
  // once: a ref keeps the handler from being torn down on every switch.
  const activeID = useRef<string | null>(null);
  // The history the open thread was built from, read by the stream handler that
  // outlives the render which created it.
  const snapshot = useRef<HistorySnapshot | null>(null);
  const openingConversationID = useRef<string | null>(null);
  const openingAgentID = useRef<string | null>(null);
  const refresh = useCallback(async () => {
    await reloadConversations();
  }, [reloadConversations]);

  const open = useCallback(
    (conversation: ChatConversation) => {
      setActive(conversation);
      activeID.current = conversation.id;
      setMessages([]);
      // A conversation is open now, so whatever deep link failed before is not
      // what the screen is about anymore, and errors or in-flight state from a
      // previous thread must not stick to the new one.
      setOpenError(null);
      setError(null);
      setRouterNotice(null);
      setResponders([]);
      setActivity(null);
      setLoadingOlder(false);
      setSending(false);
      cursor.current = undefined;
      snapshot.current = null;
      setHasMore(false);
      openingConversationID.current = conversation.id;
      openingAgentID.current = null;
      if (!workspaceId) {
        return;
      }

      // The moment the read starts: the fallback for when the server's own clock
      // is not in the response, and early enough that a frame written after it
      // counts as news rather than as replay.
      const startedAt = Date.now();
      setThreadLoading(true);
      void (async () => {
        try {
          const result = await listMessages(workspaceId, conversation.id, undefined, 40);
          if (openingConversationID.current !== conversation.id) {
            return;
          }
          if (!result.ok) {
            setError(result.message);
            return;
          }
          // The API returns newest first for the cursor; the thread reads oldest
          // first, so the page is reversed here rather than at every render.
          setMessages([...result.data.messages].reverse());
          cursor.current = result.data.next_cursor;
          setHasMore(result.data.has_more);
          setError(null);
          snapshot.current = {
            conversationId: conversation.id,
            at: result.serverDate ?? startedAt,
            ids: new Set(result.data.messages.map((message) => message.message_id)),
          };
        } finally {
          if (openingConversationID.current === conversation.id) {
            setThreadLoading(false);
          }
        }
      })();
    },
    [workspaceId],
  );

  const openByAgent = useCallback(
    async (agentId: string) => {
      if (!workspaceId) {
        return;
      }

      setOpenError(null);
      // Capture the active conversation and target agent before the round trip
      // so a late reply from an earlier click cannot replace a conversation
      // the user has already switched to.
      const targetBefore = openingConversationID.current;
      openingAgentID.current = agentId;
      const result = await openDirectConversation(workspaceId, agentId);
      if (openingAgentID.current !== agentId || openingConversationID.current !== targetBefore) {
        return;
      }
      if (!result.ok) {
        // The API's sentence is a status label (`internal server error`), so it
        // is kept whole for the view to translate rather than printed here.
        setOpenError(result);
        return;
      }
      open(result.data);
      await reloadConversations();
    },
    [workspaceId, open, reloadConversations],
  );

  const openByID = useCallback(
    (conversationId: string) => {
      const conversation = store.conversations.find((item) => item.id === conversationId);
      if (!conversation) {
        return false;
      }
      open(conversation);
      return true;
    },
    [store.conversations, open],
  );

  const loadOlder = useCallback(async () => {
    if (!workspaceId || !active || !cursor.current || loadingOlder) {
      return;
    }

    // Record the target conversation before the await so a page finishing after
    // the user switches threads is discarded without touching the new thread's
    // messages, cursor, hasMore, snapshot, or loadingOlder state.
    const target = active.id;
    const pageCursor = cursor.current;
    // The page is read before it is applied, so the moment the read started is
    // what the older rows are measured against.
    const startedAt = Date.now();
    setLoadingOlder(true);
    try {
      const result = await listMessages(workspaceId, target, pageCursor, 40);
      const applied = applyOlderPage({
        targetConversationID: target,
        activeConversationID: openingConversationID.current,
        startedAt,
        result,
        previousMessages: messages,
        previousCursor: cursor.current,
        previousHasMore: hasMore,
        previousSnapshot: snapshot.current,
      });
      if (applied.status === "discarded") {
        return;
      }
      if (applied.status === "error") {
        setError(applied.error);
        return;
      }
      if (!result.ok) {
        return;
      }

      // Apply the older rows against the latest messages state in case a stream
      // frame landed while the older page was in flight for the same thread.
      setMessages((previous) => [...[...result.data.messages].reverse(), ...previous]);
      cursor.current = applied.cursor;
      setHasMore(applied.hasMore);
      snapshot.current = applied.snapshot;
    } finally {
      if (openingConversationID.current === target) {
        setLoadingOlder(false);
      }
    }
  }, [workspaceId, active, loadingOlder, messages, hasMore]);

  const send = useCallback(
    async (text: string, attachmentIds?: string[], agentId?: string): Promise<ApiResult<ChatMessage>> => {
      if (!workspaceId || !active) {
        return { ok: false, status: 0, message: "Pilih obrolan dulu." };
      }

      // Record the target conversation before the await so a send completing after
      // a conversation switch does not append its message or responders to the new
      // thread or clear the new thread's sending state.
      const target = active.id;
      setSending(true);
      try {
        const result = await sendMessage(workspaceId, target, {
          text,
          reply: true,
          agent_id: agentId,
          attachment_ids: attachmentIds,
        });

        if (!result.ok) {
          // The reason is not put in the thread's banner: a refused send is about
          // the draft and the files beside it, and the composer says so where they
          // are, in the words the code calls for.
          return result;
        }

        if (openingConversationID.current === target) {
          setError(null);
          setMessages((previous) => append(previous, result.data.message));

          setResponders(
            (result.data.responders ?? []).map((responder) => ({
              agentId: responder.agent_id ?? "",
              name: responder.name,
            })),
          );
        }

        // The reply placeholder is not added here: it arrives on the stream, which
        // is what makes a second tab show the same conversation.
        await reloadConversations();
        return { ok: true, status: result.status, data: result.data.message };
      } finally {
        if (openingConversationID.current === target) {
          setSending(false);
        }
      }
    },
    [workspaceId, active, reloadConversations],
  );

  // The stream: the store's connection, filtered to this conversation.
  useEffect(() => {
    const unsubscribe = subscribe((event: ActivityEvent) => {
      if (!(CHAT_EVENT_TYPES as readonly string[]).includes(event.type)) {
        return;
      }
      const eventConversationID =
        event.conversation_id ?? readString((event.payload ?? {})["conversation_id"]);
      if (eventConversationID && eventConversationID !== activeID.current) {
        // Another thread: the list is refreshed, the open thread is left alone so
        // a reply cannot appear in the wrong conversation.
        void reloadConversations();
        return;
      }
      applyEvent(
        event,
        activeID.current,
        snapshot.current,
        setMessages,
        setRouterNotice,
        setResponders,
        setActivity,
      );
    });

    return unsubscribe;
  }, [subscribe, reloadConversations]);

  // The open thread is kept in step with the list the store owns: a conversation
  // renamed or a participant added elsewhere must not leave a stale header.
  const tracked = active
    ? store.conversations.find((conversation) => conversation.id === active.id) ?? active
    : null;

  return {
    workspaceId,
    conversations: store.conversations,
    active: tracked,
    messages,
    // The thread list arrives with the store, so "still loading" is true while
    // the store is loading or when a selected thread's messages are being fetched.
    loading: !store.ready || threadLoading,
    sending,
    loadingOlder,
    hasMore,
    status: store.status,
    error: error ?? store.error,
    openError,
    loaded: store.ready,
    responders,
    activity,
    routerNotice,
    open,
    openByAgent,
    openByID,
    send,
    loadOlder,
    refresh,
  };
}

export type OlderPageResult = {
  messages: ChatMessage[];
  next_cursor?: string;
  has_more: boolean;
};

export type ApplyOlderPageOutcome =
  | { status: "discarded" }
  | { status: "error"; error: string }
  | {
      status: "applied";
      messages: ChatMessage[];
      cursor: string | undefined;
      hasMore: boolean;
      snapshot: HistorySnapshot;
    };

/**
 * applyOlderPage decides whether a completed loadOlder read still belongs to the
 * active conversation and computes the updated thread state when it does.
 *
 * If the user switched conversations while listMessages was in flight
 * (`activeConversationID !== targetConversationID`), the older page is discarded
 * so messages, cursor, hasMore, and snapshot from the old thread never leak into
 * the newly opened thread (#617).
 */
export function applyOlderPage(input: {
  targetConversationID: string;
  activeConversationID: string | null;
  startedAt: number;
  result: ApiResult<OlderPageResult>;
  previousMessages: ChatMessage[];
  previousCursor: string | undefined;
  previousHasMore: boolean;
  previousSnapshot: HistorySnapshot | null;
}): ApplyOlderPageOutcome {
  if (input.activeConversationID !== input.targetConversationID) {
    return { status: "discarded" };
  }

  if (!input.result.ok) {
    return { status: "error", error: input.result.message };
  }

  // Older messages go in front: the thread keeps reading oldest first.
  const messages = [...[...input.result.data.messages].reverse(), ...input.previousMessages];
  const cursor = input.result.data.next_cursor;
  const hasMore = input.result.data.has_more;

  // These rows are history too, so a frame older than this read must not put the
  // thread back to what it said before. The open conversation's snapshot is
  // extended rather than replaced: the newer page is the later truth.
  const known = input.previousSnapshot;
  const ids =
    known && known.conversationId === input.targetConversationID
      ? new Set(known.ids)
      : new Set<string>();
  for (const message of input.result.data.messages) {
    ids.add(message.message_id);
  }
  const snapshot: HistorySnapshot = {
    conversationId: input.targetConversationID,
    at: input.result.serverDate ?? input.startedAt,
    ids,
  };

  return {
    status: "applied",
    messages,
    cursor,
    hasMore,
    snapshot,
  };
}

/**
 * applyEvent folds one stream event into the thread.
 *
 * A message event carries the whole body, so a reply that is still arriving
 * replaces the version on screen rather than appending to it. That is what makes
 * a token arriving twice harmless and a client that joined late correct. The
 * files ride along and are replaced the same way: the event is the server's
 * whole message, so a file it no longer holds is gone from the row.
 *
 * The two other events are what a client knows before the answer has any text:
 * which Bolu the router chose, and what that Bolu says it is doing.
 *
 * `snapshot` is the history the open thread was built from. A message event that
 * was written before that read is not news — it is the backlog a reconnect
 * replays — and applying it would put a message the server has since moved past
 * back on screen, so it is dropped. A message the read did not return is applied
 * as usual: it is one this client has not seen.
 */
function applyEvent(
  event: ActivityEvent,
  activeConversationID: string | null,
  snapshot: HistorySnapshot | null,
  setMessages: React.Dispatch<React.SetStateAction<ChatMessage[]>>,
  setRouterNotice: React.Dispatch<React.SetStateAction<RouterNotice | null>>,
  setResponders: React.Dispatch<React.SetStateAction<{ agentId: string; name: string }[]>>,
  setActivity: React.Dispatch<React.SetStateAction<{ agentId: string; text: string } | null>>,
) {
  if (event.type === "agent.state") {
    // The reason is the sentence the Bolu itself wrote for its state, and it is
    // the most specific thing the stream carries while an answer is prepared.
    // It is kept per Bolu rather than as one line, because the stream is the
    // workspace's: a state from another thread's Bolu must not be read as this
    // one's, and a Bolu going idle must not erase another one's sentence.
    const raw = (event.payload ?? {}) as Partial<AgentStatePayload>;
    const agentId = readString(raw.agent_id) ?? event.agent_id;
    if (!agentId) {
      return;
    }
    const doing =
      raw.state === "thinking" || raw.state === "working" ? readString(raw.reason) : undefined;
    setActivity((previous) => {
      if (doing) {
        return { agentId, text: doing };
      }
      return previous?.agentId === agentId ? null : previous;
    });
    return;
  }

  if (event.type === "router.decision") {
    // The payload arrives from the network, so each field it needs is checked
    // rather than asserted: a missing one leaves the notice unset instead of
    // rendering "undefined" next to a Bolu.
    const raw = event.payload;
    if (!raw) {
      return;
    }
    const agentId = readString(raw.agent_id) ?? event.agent_id;
    if (!agentId) {
      return;
    }
    setRouterNotice({
      agentId,
      reason: readString(raw.reason) ?? "",
      source: readString(raw.source) ?? "",
    });
    return;
  }

  const payload = event.payload as MessageEventPayload | undefined;
  if (!payload || typeof payload.message_id !== "string") {
    return;
  }
  if (!activeConversationID || (payload.conversation_id && payload.conversation_id !== activeConversationID)) {
    return;
  }

  if (snapshot && snapshot.ids.has(payload.message_id)) {
    const written = Date.parse(event.created_at);
    // An unparsable timestamp counts as news: dropping a real update over a field
    // this client cannot read is the worse of the two mistakes.
    if (Number.isFinite(written) && written < snapshot.at) {
      return;
    }
  }

  setMessages((previous) => {
    // The field is optional, so a payload written before it existed carries no
    // list at all. That is an event with nothing to say about files, and the
    // ones on screen stay; an empty array is the server saying there are none.
    const payloadAttachments = Array.isArray(payload.attachments)
      ? replaceAttachments(payload.attachments)
      : undefined;
    const incoming: ChatMessage = {
      message_id: payload.message_id,
      conversation_id: payload.conversation_id,
      agent_id: payload.agent_id,
      user_id: payload.user_id,
      blocks: payload.blocks ?? [],
      attachments: payloadAttachments ?? [],
      status: payload.status,
      finish_reason: payload.finish_reason,
      created_at: event.created_at,
    };

    const index = previous.findIndex((message) => message.message_id === incoming.message_id);
    if (index === -1) {
      return [...previous, incoming];
    }

    // The stored message is the whole message, so an update replaces it rather
    // than merging: a merge would keep a block the server removed.
    const next = [...previous];
    next[index] = {
      ...previous[index],
      ...incoming,
      // The event carries the files the server still holds, so it is the list
      // on screen rather than one to fold in: a file dropped for retention
      // disappears, because the latest event for the message is the whole
      // truth. An event without the field leaves the files already shown alone.
      attachments: payloadAttachments ?? previous[index].attachments,
    };
    return next;
  });

  if (payload.status === "complete" || payload.status === "failed" || payload.status === "partial") {
    // Only the Bolu whose message settled stops waiting: a group message can be
    // answered by more than one, and the other answers are still coming. A
    // payload that names no Bolu cannot be attributed, so the whole wait ends
    // rather than one row being held open by an empty id.
    setResponders((previous) =>
      payload.agent_id ? previous.filter((candidate) => candidate.agentId !== payload.agent_id) : [],
    );
    setActivity((previous) => (previous?.agentId === payload.agent_id ? null : previous));
  }
}

/** readString narrows one field of a streamed payload. */
function readString(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

/**
 * replaceAttachments takes the server's list as the whole truth.
 *
 * The order is the server's, so a file keeps the place the backend gave it, and
 * a repeated id collapses to one row: the same file is named twice when an
 * upload's own response and the event that followed it both carry it.
 */
function replaceAttachments(incoming: Attachment[]): Attachment[] {
  const seen = new Set<string>();
  const replaced: Attachment[] = [];
  for (const item of incoming) {
    if (seen.has(item.id)) {
      continue;
    }
    seen.add(item.id);
    replaced.push(item);
  }
  return replaced;
}

/** append adds a message once, by id, so a stream echo cannot duplicate it. */
function append(messages: ChatMessage[], message: ChatMessage): ChatMessage[] {
  if (messages.some((existing) => existing.message_id === message.message_id)) {
    return messages;
  }
  return [...messages, message];
}

/** isBusy reports whether a message is still arriving. */
export function isBusy(message: ChatMessage): boolean {
  return message.status === "streaming";
}

/** describeResult renders a non-complete message honestly. */
export function describeResult(message: ChatMessage): string | null {
  if (message.status === "partial") {
    return message.finish_reason
      ? `Jawaban terputus: ${message.finish_reason}`
      : "Jawaban terputus di tengah jalan.";
  }
  if (message.status === "failed") {
    return message.finish_reason ? `Gagal menjawab: ${message.finish_reason}` : "Gagal menjawab.";
  }
  return null;
}

/** unwrap reports an API failure, which the views turn into a line of text. */
export function unwrap<T>(result: ApiResult<T>): T | null {
  return result.ok ? result.data : null;
}
