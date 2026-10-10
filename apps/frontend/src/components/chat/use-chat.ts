"use client";

/**
 * The chat state: conversations, one open thread, and the event stream that
 * keeps it current.
 *
 * The rule the whole hook follows is that the server owns the message. A reply
 * is written by the backend whether or not a tab is open, so the client never
 * assembles one: it renders what the stream tells it, and a message it already
 * has is replaced by the version the server sends.
 */

import { useCallback, useEffect, useRef, useState } from "react";

import {
  ApiResult,
  ActivityEvent,
  ChatConversation,
  ChatMessage,
  MessageEventPayload,
  ensureActiveWorkspace,
  listConversations,
  listMessages,
  openDirectConversation,
  refreshSession,
  sendMessage,
} from "@/lib/api";
import { connect, type RealtimeConnection, type RealtimeStatus } from "@/lib/realtime";

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
  status: RealtimeStatus;
  error: string | null;
  /** The Bolu that will answer the message being composed, once known. */
  responder: { agentId: string; name: string } | null;
  routerNotice: RouterNotice | null;
  open: (conversation: ChatConversation) => void;
  openByAgent: (agentId: string) => Promise<void>;
  send: (text: string, agentId?: string) => Promise<void>;
  loadOlder: () => Promise<void>;
  refresh: () => Promise<void>;
};

export function useChat(): ChatState {
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);
  const [conversations, setConversations] = useState<ChatConversation[]>([]);
  const [active, setActive] = useState<ChatConversation | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [loading, setLoading] = useState(true);
  const [sending, setSending] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [status, setStatus] = useState<RealtimeStatus>("connecting");
  const [error, setError] = useState<string | null>(null);
  const [responder, setResponder] = useState<{ agentId: string; name: string } | null>(null);
  const [routerNotice, setRouterNotice] = useState<RouterNotice | null>(null);

  const cursor = useRef<string | undefined>(undefined);
  const connection = useRef<RealtimeConnection | null>(null);
  // The open conversation is read inside the event handler, which is registered
  // once: a ref keeps the handler from being torn down on every switch.
  const activeID = useRef<string | null>(null);

  // The ref follows the open conversation from an effect, not from the render
  // body: writing a ref while rendering is what the rule forbids, and the event
  // handler only reads it after a commit anyway.
  useEffect(() => {
    activeID.current = active?.id ?? null;
  }, [active]);

  const refresh = useCallback(async () => {
    // The workspace is resolved before it is read: an account that owns one but
    // has none stored — a second device, a private window — would otherwise be
    // told to pick a workspace it has no way to pick.
    const workspace = await ensureActiveWorkspace();
    setWorkspaceId(workspace);
    if (!workspace) {
      setLoading(false);
      setError("Pilih workspace dulu.");
      return;
    }

    const session = await refreshSession();
    if (!session.ok) {
      setLoading(false);
      setError("Sesi berakhir. Masuk lagi.");
      return;
    }

    const result = await listConversations(workspace);
    setLoading(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setConversations(result.data);
    setError(null);
  }, []);

  useEffect(() => {
    void (async () => {
      await refresh();
    })();
  }, [refresh]);

  const open = useCallback((conversation: ChatConversation) => {
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
  }, [workspaceId]);

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
      setConversations((previous) => {
        const others = previous.filter((item) => item.id !== result.data.id);
        return [result.data, ...others];
      });
    },
    [workspaceId, open],
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
        setResponder({ agentId: result.data.responder.agent_id ?? "", name: result.data.responder.name });
      }

      // The reply placeholder is not added here: it arrives on the stream, which
      // is what makes a second tab show the same conversation.
      setConversations((previous) =>
        previous.map((item) =>
          item.id === active.id
            ? { ...item, message_count: item.message_count + 1, last_activity_at: result.data.message.created_at }
            : item,
        ),
      );
    },
    [workspaceId, active],
  );

  // The stream: one connection per tab, opened once per workspace.
  useEffect(() => {
    if (!workspaceId) {
      return;
    }

    const handle = connect(
      workspaceId,
      {
        onEvent: (event) => {
          if (CHAT_EVENTS[event.type] !== true) {
            return;
          }
          if (event.conversation_id && event.conversation_id !== activeID.current) {
            // Another thread: the list is refreshed, the open thread is left
            // alone so a reply cannot appear in the wrong conversation.
            void refresh();
            return;
          }
          applyEvent(event, setMessages, setRouterNotice, setResponder);
        },
        onStatus: setStatus,
      },
      0,
    );

    connection.current = handle;

    return () => {
      handle.close();
      connection.current = null;
    };
  }, [workspaceId, refresh]);

  return {
    workspaceId,
    conversations,
    active,
    messages,
    loading,
    sending,
    loadingOlder,
    hasMore,
    status,
    error,
    responder,
    routerNotice,
    open,
    openByAgent,
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
