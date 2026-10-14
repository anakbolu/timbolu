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
  ApiResult,
  ActivityEvent,
  ChatConversation,
  ChatMessage,
  MessageEventPayload,
  listMessages,
  openDirectConversation,
  sendMessage,
} from "@/lib/api";

/** The events the chat renders. Everything else is another view's business. */
const CHAT_EVENTS: Record<string, true> = {
  "message.new": true,
  "message.updated": true,
  "router.decision": true,
};

export type RouterNotice = {
  agentId: string;
  reason: string;
  source: string;
};

export type ChatState = {
  workspaceId: string | null;
  conversations: ChatConversation[];
  active: ChatConversation | null;
  messages: ChatMessage[];
  loading: boolean;
  sending: boolean;
  loadingOlder: boolean;
  hasMore: boolean;
  status: string;
  error: string | null;
  /** The Bolu that will answer the message being composed, once known. */
  responder: { agentId: string; name: string } | null;
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
  send: (text: string, agentId?: string) => Promise<void>;
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
  const [error, setError] = useState<string | null>(null);
  const [responder, setResponder] = useState<{ agentId: string; name: string } | null>(null);
  const [routerNotice, setRouterNotice] = useState<RouterNotice | null>(null);

  const cursor = useRef<string | undefined>(undefined);
  // The open conversation is read inside the event handler, which is registered
  // once: a ref keeps the handler from being torn down on every switch.
  const activeID = useRef<string | null>(null);

  useEffect(() => {
    activeID.current = active?.id ?? null;
  }, [active]);

  const refresh = useCallback(async () => {
    await reloadConversations();
  }, [reloadConversations]);

  const open = useCallback(
    (conversation: ChatConversation) => {
      setActive(conversation);
      setMessages([]);
      setRouterNotice(null);
      setResponder(null);
      cursor.current = undefined;
      setHasMore(false);

      if (!workspaceId) {
        return;
      }

      void (async () => {
        const result = await listMessages(workspaceId, conversation.id, undefined, 40);
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
      })();
    },
    [workspaceId],
  );

  const openByAgent = useCallback(
    async (agentId: string) => {
      if (!workspaceId) {
        return;
      }

      const result = await openDirectConversation(workspaceId, agentId);
      if (!result.ok) {
        setError(result.message);
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

    setLoadingOlder(true);
    const result = await listMessages(workspaceId, active.id, cursor.current, 40);
    setLoadingOlder(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    // Older messages go in front: the thread keeps reading oldest first.
    setMessages((previous) => [...[...result.data.messages].reverse(), ...previous]);
    cursor.current = result.data.next_cursor;
    setHasMore(result.data.has_more);
  }, [workspaceId, active, loadingOlder]);

  const send = useCallback(
    async (text: string, agentId?: string) => {
      if (!workspaceId || !active) {
        return;
      }

      setSending(true);
      const result = await sendMessage(workspaceId, active.id, {
        text,
        reply: true,
        agent_id: agentId,
      });
      setSending(false);

      if (!result.ok) {
        setError(result.message);
        return;
      }

      setError(null);
      setMessages((previous) => append(previous, result.data.message));

      if (result.data.responder) {
        setResponder({
          agentId: result.data.responder.agent_id ?? "",
          name: result.data.responder.name,
        });
      }

      // The reply placeholder is not added here: it arrives on the stream, which
      // is what makes a second tab show the same conversation.
      await reloadConversations();
    },
    [workspaceId, active, reloadConversations],
  );

  // The stream: the store's connection, filtered to this conversation.
  useEffect(() => {
    const unsubscribe = subscribe((event: ActivityEvent) => {
      if (CHAT_EVENTS[event.type] !== true) {
        return;
      }
      if (event.conversation_id && event.conversation_id !== activeID.current) {
        // Another thread: the list is refreshed, the open thread is left alone so
        // a reply cannot appear in the wrong conversation.
        void reloadConversations();
        return;
      }
      applyEvent(event, setMessages, setRouterNotice, setResponder);
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
    // The thread list arrives with the store, so "still loading" is exactly "the
    // store has not finished its first read".
    loading: !store.ready,
    sending,
    loadingOlder,
    hasMore,
    status: store.status,
    error: error ?? store.error,
    responder,
    routerNotice,
    open,
    openByAgent,
    openByID,
    send,
    loadOlder,
    refresh,
  };
}

/**
 * applyEvent folds one stream event into the thread.
 *
 * A message event carries the whole body, so a reply that is still arriving
 * replaces the version on screen rather than appending to it. That is what makes
 * a token arriving twice harmless and a client that joined late correct.
 */
function applyEvent(
  event: ActivityEvent,
  setMessages: React.Dispatch<React.SetStateAction<ChatMessage[]>>,
  setRouterNotice: React.Dispatch<React.SetStateAction<RouterNotice | null>>,
  setResponder: React.Dispatch<React.SetStateAction<{ agentId: string; name: string } | null>>,
) {
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

  setMessages((previous) => {
    const incoming: ChatMessage = {
      message_id: payload.message_id,
      conversation_id: payload.conversation_id,
      agent_id: payload.agent_id,
      user_id: payload.user_id,
      blocks: payload.blocks ?? [],
      attachments: [],
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
    next[index] = { ...previous[index], ...incoming, attachments: previous[index].attachments };
    return next;
  });

  if (payload.status === "complete" || payload.status === "failed" || payload.status === "partial") {
    setResponder(null);
  }
}

/** readString narrows one field of a streamed payload. */
function readString(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
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
