"use client";

/**
 * The chat view: the conversation, the composer, the live state of the answer,
 * and the panel of work threads its Bolu opened.
 *
 * A reply is rendered from what the server stored, not from tokens the client
 * assembled, so a message that is still arriving shows the text it has and the
 * final event replaces it with the canonical body.
 */

import { ArrowRight, Check, FileText, MessageSquare, MessagesSquare, PanelRight, Paperclip, X } from "lucide-react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import type { BoluExpressionName } from "@/components/bolu/characters";
import { useWorkspace } from "@/components/dashboard/workspace-data";
import { useIsWide } from "@/hooks/use-wide";
import { uploadStagedAttachment, type Agent, type ApiError, type ChatConversation, type ChatMessage } from "@/lib/api";
import { allBot, type CrewMember } from "@/lib/crew";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

import { Block } from "./blocks";
import { ThreadPanel } from "./thread-panel";
import { describeResult, isBusy, useChat, type ChatState } from "./use-chat";

const STATUS_LABEL: Record<string, string> = {
  connecting: "menyambung…",
  live: "tersambung",
  reconnecting: "menyambung ulang…",
  // The stream is still being retried, but not on the fast path: say the
  // updates are late instead of claiming a connection that is not there.
  slow: "pembaruan tertunda",
  closed: "terputus",
};

/**
 * One file the composer is holding for the message being written.
 *
 * It is uploaded as soon as it is picked, so the send that names it is the same
 * call that stores the message: the file travels with the words rather than
 * after them, and the reply sees it on the turn it was sent with.
 */
type QueuedFile = {
  id: string;
  file: File;
  status: "uploading" | "uploaded" | "failed";
  /** How much of the file has left, from 0 to 1. */
  progress: number;
  /** What the API calls the stored file, once it holds it. */
  attachmentId?: string;
  /** Why an upload or a send was refused, in the composer's words. */
  error?: string;
};

/** What the composer says when the API refuses a send, by the reason it gave. */
const SEND_ERROR: Record<string, string> = {
  invalid_attachment_id: "Lampiran tidak valid. Hapus lalu unggah ulang.",
  attachment_not_found: "Lampiran sudah kedaluwarsa atau tidak ditemukan. Unggah ulang.",
  attachment_already_bound: "Lampiran ini sudah terkirim di pesan lain. Unggah ulang.",
  invalid_input: "Tulis pesan atau lampirkan file dulu.",
};

/** sendErrorText turns a refused send into the one line the composer shows. */
function sendErrorText(error: ApiError): string {
  return (error.code ? SEND_ERROR[error.code] : undefined) ?? error.message;
}

/**
 * blamedAttachments names the files a refused send is about.
 *
 * The API names the offending id only when the id itself could not be read. A
 * file that is gone or already bound comes back as a code with no id at all, and
 * then any of them could be the one, which is what an empty list means here.
 */
function blamedAttachments(error: ApiError): string[] {
  if (error.code !== "invalid_attachment_id") {
    return [];
  }
  const quoted = /"([^"]+)"/.exec(error.message);
  return quoted ? [quoted[1]] : [];
}

const STATUS_DOT: Record<string, string> = {
  connecting: "bg-bolu-rest",
  live: "bg-bolu-live",
  reconnecting: "bg-bolu-flag",
  slow: "bg-bolu-flag",
  closed: "bg-bolu-belum",
};

/**
 * ChatScreen opens the conversation the query names.
 *
 * The sidebar links to `?agent=<id>` for a Bolu and to `?conversation=<id>` for a
 * group, which is what makes a click open that thread: the id travels in the URL
 * rather than through shared state, so a link works from anywhere.
 */
export function ChatScreen() {
  const params = useSearchParams();
  return (
    <ChatView
      agentId={params.get("agent") ?? undefined}
      conversationId={params.get("conversation") ?? undefined}
    />
  );
}

/**
 * ChatView is the whole screen, laid out as the internal panel design: the
 * conversation in the middle and, beside it, the work threads its Bolu opened.
 *
 * A thread is a conversation of its own whose parent is this one, so the panel
 * reads the same store the sidebar does: a thread a Bolu opened a moment ago
 * appears here as soon as the stream says the workspace moved.
 */
export function ChatView({
  agentId,
  conversationId,
}: {
  agentId?: string;
  conversationId?: string;
}) {
  const chat = useChat();
  const { agents } = useWorkspace();
  const wide = useIsWide();
  const opened = useRef<string | null>(null);

  // The newest message is what a person came to read, so the thread is kept at the
  // bottom instead of the browser: on open, as a reply streams in, and when a block
  // renders late. See useStickToBottom.
  const { thread, content, pin } = useStickToBottom(chat.active?.id);

  // Opening by Bolu is what a click in the sidebar does. A link opened before the
  // store has the workspace cannot be answered yet, so it waits for the id rather
  // than marking itself opened and leaving the screen empty.
  useEffect(() => {
    if (!agentId || !chat.workspaceId || opened.current === agentId) {
      return;
    }
    opened.current = agentId;
    void (async () => {
      await chat.openByAgent(agentId);
    })();
  }, [agentId, chat]);

  // A group is addressed by its own id, and the store's list may not have arrived
  // yet, so an id that is not found is left for the next render rather than
  // marked as opened.
  useEffect(() => {
    if (!conversationId || opened.current === conversationId) {
      return;
    }
    if (!chat.openByID(conversationId)) {
      return;
    }
    opened.current = conversationId;
  }, [conversationId, chat]);

  const title = useMemo(() => titleOf(chat.active), [chat.active]);

  // The header shows the one Bolu a direct thread is with. A group has no single
  // partner, so it names its members instead of borrowing the first one's face.
  const partner = useMemo(() => {
    if (!chat.active || chat.active.kind === "group") return null;
    const participant = chat.active.participants.find((item) => item.is_agent);
    if (!participant) return null;
    return agents.find((agent) => agent.id === participant.agent_id) ?? null;
  }, [chat.active, agents]);

  // The face every Bolu message in this conversation wears: the one its state
  // poses, computed once so the header and the messages cannot disagree. A group
  // has no single partner, so its messages keep the neutral face.
  const partnerExpression = partner ? expressionOf(stateOf(partner)) : undefined;

  // The work threads opened from this conversation, newest first.
  const threads = useMemo(
    () =>
      chat.active
        ? chat.conversations
            .filter((conversation) => conversation.parent_conversation_id === chat.active?.id)
            .sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at))
        : [],
    [chat.active, chat.conversations],
  );

  /**
   * Which thread the panel shows. A person's own choice wins for the conversation
   * it was made in; until they make one, a wide screen shows the newest thread
   * beside the chat — which is the design's resting state — and a narrow one keeps
   * the chat to itself.
   */
  const [choice, setChoice] = useState<{ conversation: string; thread: string | null } | null>(null);
  const activeId = chat.active?.id ?? null;
  const shown =
    choice && choice.conversation === activeId
      ? choice.thread
      : wide
        ? (threads[0]?.id ?? null)
        : null;
  const selected = threads.find((item) => item.id === shown) ?? null;
  const panelOpen = shown !== null && (selected !== null || shown === LIST);
  const showThread = (id: string | null) => activeId && setChoice({ conversation: activeId, thread: id });

  /**
   * The timeline: every message, and a marker where each thread was opened, so the
   * place a piece of work left the chat is where a person finds the way into it.
   */
  const timeline = useMemo(() => {
    const items: ({ kind: "message"; at: number; message: ChatMessage } | { kind: "thread"; at: number; thread: ChatConversation })[] =
      chat.messages.map((message) => ({ kind: "message", at: Date.parse(message.created_at), message }));
    for (const item of threads) {
      items.push({ kind: "thread", at: Date.parse(item.created_at), thread: item });
    }
    // A message without a parsable time keeps its place: the server's order is the
    // truth, and only a thread is slotted in by its own moment.
    return items.sort((left, right) =>
      Number.isFinite(left.at) && Number.isFinite(right.at) ? left.at - right.at : 0,
    );
  }, [chat.messages, threads]);

  const waiting = useWaiting(chat, agents);

  return (
    <div className="flex h-full min-h-0 flex-1">
      <div className="relative z-10 flex min-h-0 min-w-0 flex-1 flex-col bg-bolu-surface shadow-[-4px_0_15px_-3px_rgba(0,0,0,0.03)]">
        <header className="flex h-14 flex-none items-center justify-between gap-3 border-b border-bolu-line px-6 max-[760px]:px-3">
          <div className="flex min-w-0 items-center gap-2">
            {partner ? (
              <Bolu
                member={crewFor(partner)}
                expression={partnerExpression}
                color={partner.color || undefined}
                size={24}
                className="flex-none"
              />
            ) : chat.active ? (
              <span className="flex size-6 flex-none items-center justify-center rounded-md bg-bolu-amber-soft text-bolu-ink">
                <MessageSquare size={13} strokeWidth={2} aria-hidden="true" />
              </span>
            ) : null}
            <div className="min-w-0">
              <h1 className="truncate text-[15px] font-semibold text-bolu-ink">{title}</h1>
              <p className="truncate text-[12px] text-bolu-muted">
                {partner
                  ? `${partner.role || "Bolu"} · ${partner.display_status === "working" ? "sedang bekerja" : partner.display_status === "waiting" ? "menunggu kamu" : "santai"}`
                  : chat.active
                    ? chat.active.participants
                        .filter((participant) => participant.is_agent)
                        .map((participant) => participant.name)
                        .join(", ")
                    : (
                        <>
                          <span className="max-[760px]:hidden">Pilih Bolu di kiri untuk mulai.</span>
                          <span className="hidden max-[760px]:inline">Buka menu ☰ untuk memilih Bolu atau obrolan.</span>
                        </>
                      )}
              </p>
            </div>
          </div>
          <div className="flex flex-none items-center gap-3 text-bolu-muted">
            <span
              className="flex items-center gap-1.5 text-[12px]"
              title={STATUS_LABEL[chat.status] ?? chat.status}
            >
              <span className={`size-2 rounded-full ${STATUS_DOT[chat.status] ?? STATUS_DOT.closed}`} />
              <span className="max-[760px]:sr-only">{STATUS_LABEL[chat.status] ?? chat.status}</span>
            </span>
            {chat.active ? (
              <button
                type="button"
                aria-label="Thread kerja"
                aria-expanded={panelOpen}
                onClick={() => showThread(panelOpen ? null : (threads[0]?.id ?? LIST))}
                className="relative flex size-8 cursor-pointer items-center justify-center rounded-md border-0 bg-transparent text-bolu-muted hover:bg-bolu-hover hover:text-bolu-ink"
              >
                <PanelRight size={18} strokeWidth={2} aria-hidden="true" />
                {threads.length > 0 ? (
                  <span className="absolute -right-0.5 -top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-bolu-amber px-1 text-[10px] font-bold text-bolu-on-tint">
                    {threads.length}
                  </span>
                ) : null}
              </button>
            ) : null}
          </div>
        </header>

        {chat.error ? (
          <p role="alert" className="mx-6 mt-3 rounded-md border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
            {chat.error}
          </p>
        ) : null}

        <div ref={thread} className="flex min-h-0 flex-1 flex-col overflow-y-auto px-6 py-6 max-[760px]:px-3">
          {chat.hasMore ? (
            <button
              type="button"
              onClick={() => void chat.loadOlder()}
              disabled={chat.loadingOlder}
              className="mx-auto mb-3 block cursor-pointer rounded-full border border-bolu-border bg-transparent px-3 py-1 text-sm text-bolu-muted transition hover:bg-bolu-hover/60 disabled:opacity-50"
            >
              {chat.loadingOlder ? "Memuat…" : "Muat pesan lama"}
            </button>
          ) : null}

          {chat.messages.length === 0 && !chat.loading ? (
            <p className="mt-8 text-center text-[15px] text-bolu-muted">
              {chat.active
                ? `Belum ada pesan dengan ${title}. Tulis sesuatu untuk mulai.`
                : (
                    <>
                      <span className="max-[760px]:hidden">Pilih Bolu di kiri untuk mulai.</span>
                      <span className="hidden max-[760px]:inline">Buka menu ☰ untuk memilih Bolu atau obrolan.</span>
                    </>
                  )}
            </p>
          ) : null}

          <div ref={content} className="mx-auto mt-auto flex w-full max-w-3xl flex-col gap-6">
            {timeline.map((item) =>
              item.kind === "message" ? (
                <MessageRow
                  key={item.message.message_id}
                  message={item.message}
                  name={nameOf(chat.active, item.message)}
                  face={authorFace(chat.active, item.message, agents)}
                  color={colorOf(item.message, agents)}
                  expression={partnerExpression}
                />
              ) : (
                <ThreadMarker
                  key={item.thread.id}
                  thread={item.thread}
                  open={selected?.id === item.thread.id}
                  onOpen={() => showThread(item.thread.id)}
                />
              ),
            )}

            {waiting.map((row) => (
              <TypingRow key={row.key} name={row.name} face={row.face} text={row.text} />
            ))}

            {chat.routerNotice ? (
              <p className="text-xs text-bolu-muted">
                Router memilih {chat.routerNotice.reason}
                {chat.routerNotice.source === "model" ? " (model kecil)" : " (cadangan)"}.
              </p>
            ) : null}
          </div>
        </div>

        <Composer
          chat={chat}
          onSent={pin}
          placeholder={chat.active ? "Tanya Bolu atau mulai sebuah tugas…" : "Pilih obrolan dulu"}
          footer="Bolu bisa keliru. Periksa lagi informasi yang penting."
        />
      </div>

      {panelOpen && chat.active ? (
        <ThreadPanel
          threads={threads}
          selected={selected}
          wide={wide}
          onSelect={(id) => showThread(id)}
          onClose={() => showThread(null)}
        />
      ) : null}
    </div>
  );
}

/** LIST is the panel showing every thread rather than one of them. */
const LIST = "__list__";

/** titleOf names a conversation for its header. */
function titleOf(conversation: ChatConversation | null): string {
  if (!conversation) {
    return "Pilih obrolan";
  }
  if (conversation.title) {
    return conversation.title;
  }
  const agent = conversation.participants.find((participant) => participant.is_agent);
  return agent?.name ?? "Obrolan";
}

/**
 * useWaiting is the lines that stand for the answers being prepared.
 *
 * A group message can be answered by more than one Bolu, so each one that is
 * still working gets its own line: one row for the whole group would hide the
 * second answer behind the first. The router may name the Bolu before its
 * message exists, and that message carries no text until the first block lands,
 * so both are the same wait and are drawn once. A message that already has
 * content is its own bubble, and its row is over.
 */
function useWaiting(chat: ChatState, agents: Agent[]) {
  return useMemo(() => {
    const rows: { key: string; name: string; face: CrewMember; text: string }[] = [];

    for (const message of chat.messages) {
      if (!isBusy(message) || message.blocks.length > 0 || message.attachments.length > 0) {
        continue;
      }
      rows.push({
        key: message.message_id,
        name: nameOf(chat.active, message),
        face: authorFace(chat.active, message, agents) ?? allBot("Oren"),
        text:
          chat.activity && chat.activity.agentId === message.agent_id
            ? chat.activity.text
            : "Sedang memikirkan…",
      });
    }

    if (rows.length > 0) {
      return rows;
    }

    // No placeholder has arrived yet, which is the moment between the request
    // being accepted and the stream delivering it. The Bolu the send response
    // named are what the wait is made of until then.
    for (const responder of chat.responders) {
      const agent = agents.find((candidate) => candidate.id === responder.agentId) ?? null;
      const participant = chat.active?.participants.find(
        (item) => item.agent_id === responder.agentId,
      );
      rows.push({
        key: responder.agentId || responder.name,
        name: agent?.name ?? participant?.name ?? responder.name ?? "Bolu",
        face: agent ? crewFor(agent) : allBot("Oren"),
        text:
          chat.activity && chat.activity.agentId === responder.agentId
            ? chat.activity.text
            : "Sedang memikirkan…",
      });
    }

    return rows;
  }, [agents, chat.active, chat.activity, chat.messages, chat.responders]);
}

/**
 * Composer is the input at the foot of a conversation, in the main chat and in a
 * thread alike: a card with the text, the files it carries, and the send.
 *
 * Files are uploaded as soon as they are picked, so the send that names them is
 * the same call that stores the message: the file travels with the words rather
 * than after them, and the reply sees it on the turn it was sent with.
 */
export function Composer({
  chat,
  onSent,
  placeholder,
  footer,
  compact = false,
}: {
  chat: ChatState;
  /** Called when a send starts, which is a person looking for the answer. */
  onSent?: () => void;
  placeholder: string;
  /** The line under the card, when the composer has one. */
  footer?: string;
  /** The thread panel's smaller composer. */
  compact?: boolean;
}) {
  const [draft, setDraft] = useState("");
  const [queued, setQueued] = useState<QueuedFile[]>([]);
  const [sendError, setSendError] = useState<string | null>(null);
  const picker = useRef<HTMLInputElement | null>(null);
  // The uploads in flight, by chip, so removing one drops its request too.
  const uploads = useRef(new Map<string, AbortController>());

  const hasFiles = queued.length > 0;
  const activeId = chat.active?.id ?? null;
  const uploading = queued.some((item) => item.status === "uploading");
  const failed = queued.some((item) => item.status === "failed");
  const uploaded = queued.filter((item) => item.status === "uploaded");
  // A staged file is uploaded before the message exists, so a send is allowed
  // with files and no words: the message is then the files themselves.
  const canSend =
    Boolean(chat.active) &&
    !chat.sending &&
    !uploading &&
    !failed &&
    (draft.trim() !== "" || uploaded.length > 0);

  // A chip belongs to the thread it was picked in: the API refuses a message in
  // another thread that names a file staged for this one, so a switch drops the
  // chips instead of letting the next send fail on all of them.
  const stagedFor = useRef<string | null>(null);
  useEffect(() => {
    if (stagedFor.current === activeId) {
      return;
    }
    stagedFor.current = activeId;
    for (const controller of uploads.current.values()) {
      controller.abort();
    }
    uploads.current.clear();
    setQueued((previous) => (previous.length === 0 ? previous : []));
    setSendError(null);
  }, [activeId]);

  function queueFiles(files: FileList | null) {
    if (!files || files.length === 0) {
      return;
    }
    // The list is copied here rather than inside the updater: a FileList read off
    // the picker is live, and clearing the picker below would empty it before the
    // updater runs.
    const picked = Array.from(files, (file) => ({
      // A key has to survive two files of the same name chosen in one go.
      id: `${file.name}-${file.size}-${file.lastModified}-${Math.random().toString(36).slice(2)}`,
      file,
      status: "uploading" as const,
      progress: 0,
    }));
    setQueued((previous) => [...previous, ...picked]);
    setSendError(null);
    // The picker is reset so choosing the same file again is still a change.
    if (picker.current) {
      picker.current.value = "";
    }
    for (const item of picked) {
      void upload(item.id, item.file);
    }
  }

  /**
   * upload stages one file and reports how far it has got.
   *
   * The request is its own, so one file failing leaves the others alone: the chip
   * that failed keeps its place with the reason and a retry, and the ones that
   * succeeded hold the id the send will carry.
   */
  async function upload(id: string, file: File) {
    const workspaceId = chat.workspaceId;
    const conversationId = chat.active?.id;
    // A retry answers the line the last send left behind: it was about this file.
    setSendError(null);
    setQueued((previous) =>
      previous.map((entry) =>
        entry.id === id ? { ...entry, status: "uploading", progress: 0, error: undefined } : entry,
      ),
    );
    if (!workspaceId || !conversationId) {
      // The composer only offers the picker with a thread open, so this is the
      // thread closing underneath an upload rather than a state to recover from.
      setQueued((previous) =>
        previous.map((entry) =>
          entry.id === id ? { ...entry, status: "failed", error: "Obrolan belum siap." } : entry,
        ),
      );
      return;
    }

    const controller = new AbortController();
    uploads.current.set(id, controller);
    const result = await uploadStagedAttachment(
      workspaceId,
      conversationId,
      file,
      (fraction) =>
        setQueued((previous) =>
          previous.map((entry) => (entry.id === id ? { ...entry, progress: fraction } : entry)),
        ),
      controller.signal,
    );
    uploads.current.delete(id);

    // An aborted upload is a chip that is gone: dropping it from state was the
    // abort, so there is nothing left to mark.
    if (!result.ok && result.code === "aborted") {
      return;
    }
    setQueued((previous) =>
      previous.map((entry) => {
        if (entry.id !== id) {
          return entry;
        }
        return result.ok
          ? { ...entry, status: "uploaded", progress: 1, attachmentId: result.data.attachment_id }
          : { ...entry, status: "failed", error: result.message };
      }),
    );
  }

  /** removeQueued drops one chip, and its upload with it. */
  function removeQueued(id: string) {
    uploads.current.get(id)?.abort();
    uploads.current.delete(id);
    setQueued((previous) => previous.filter((entry) => entry.id !== id));
    setSendError(null);
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const text = draft.trim();
    if (!canSend) {
      return;
    }

    // Sending is a person looking for the answer, so the thread returns to its
    // newest message even if they had scrolled up to read something older.
    onSent?.();

    const attachmentIds = uploaded.flatMap((item) => (item.attachmentId ? [item.attachmentId] : []));
    const result = await chat.send(text, attachmentIds);
    if (!result.ok) {
      // The draft and the chips stay: the files are still staged, so what is
      // wrong is one of them and the composer marks which.
      const message = sendErrorText(result);
      const blamed = blamedAttachments(result);
      setSendError(message);
      setQueued((previous) =>
        previous.map((entry) =>
          entry.status === "uploaded" && (blamed.length === 0 || blamed.includes(entry.attachmentId ?? ""))
            ? { ...entry, status: "failed", error: message }
            : entry,
        ),
      );
      return;
    }

    setSendError(null);
    setDraft("");
    setQueued([]);
  }

  return (
    <form
      onSubmit={submit}
      className={`flex-none bg-gradient-to-t from-bolu-surface via-bolu-surface to-transparent ${
        compact ? "p-3" : "p-4 max-[760px]:px-3"
      }`}
    >
      <div className={compact ? "flex flex-col gap-2" : "mx-auto flex max-w-4xl flex-col gap-2"}>
        <div className="flex flex-col rounded-xl border border-bolu-border bg-bolu-surface shadow-sm transition-shadow focus-within:border-bolu-amber focus-within:ring-1 focus-within:ring-bolu-amber">
          {hasFiles ? (
            <ul className="flex flex-wrap gap-1.5 px-3 pt-3">
              {queued.map((item) => (
                <li
                  key={item.id}
                  className={`flex items-center gap-1.5 rounded-full border py-1 pl-3 pr-1 text-[13px] ${
                    item.status === "failed"
                      ? "border-bolu-belum/40 bg-bolu-belum/8 text-bolu-belum"
                      : "border-bolu-border bg-bolu-surface text-bolu-body"
                  }`}
                >
                  <span className="max-w-[180px] truncate">{item.file.name}</span>
                  <span className="text-[11px] text-bolu-muted">{formatBytes(item.file.size)}</span>
                  {item.status === "uploading" ? (
                    <span role="status" className="flex items-center gap-1.5" aria-label={`Mengunggah ${item.file.name}`}>
                      <span className="h-1 w-14 overflow-hidden rounded-full bg-bolu-track">
                        <span
                          className="block h-full rounded-full bg-bolu-accent"
                          style={{ width: `${Math.round(item.progress * 100)}%` }}
                        />
                      </span>
                      <span className="text-[11px] text-bolu-muted">{Math.round(item.progress * 100)}%</span>
                    </span>
                  ) : null}
                  {item.status === "uploaded" ? (
                    <span className="flex items-center gap-0.5 text-[11px] text-bolu-lunas">
                      <Check size={13} strokeWidth={2.6} aria-hidden="true" />
                      siap
                    </span>
                  ) : null}
                  {/* The reason is on the chip rather than in one banner: two
                      files can fail for two different reasons. */}
                  {item.status === "failed" && item.error ? (
                    <span className="max-w-[200px] truncate text-[11px]">{item.error}</span>
                  ) : null}
                  {item.status === "failed" ? (
                    <button
                      type="button"
                      aria-label={`Coba lagi ${item.file.name}`}
                      onClick={() => void upload(item.id, item.file)}
                      className="shrink-0 cursor-pointer rounded-full border border-bolu-belum/40 bg-transparent px-2 py-0.5 text-[11px] font-medium text-bolu-belum"
                    >
                      Coba lagi
                    </button>
                  ) : null}
                  {/* A chip can be dropped at any time: the file belongs to no
                      message yet, so removing it is local — and it takes the
                      upload with it when one is still running. */}
                  <button
                    type="button"
                    aria-label={`Buang ${item.file.name}`}
                    onClick={() => removeQueued(item.id)}
                    className="flex size-6 shrink-0 cursor-pointer items-center justify-center rounded-full border-0 bg-transparent text-bolu-muted"
                  >
                    <X size={13} strokeWidth={2.5} aria-hidden="true" />
                  </button>
                </li>
              ))}
            </ul>
          ) : null}

          <textarea
            value={draft}
            rows={1}
            onChange={(event) => {
              setDraft(event.target.value);
              // The box grows with what is written, up to the cap its class sets.
              event.target.style.height = "";
              event.target.style.height = `${event.target.scrollHeight}px`;
            }}
            onKeyDown={(event) => {
              if (event.key === "Enter" && !event.shiftKey) {
                event.preventDefault();
                event.currentTarget.form?.requestSubmit();
              }
            }}
            placeholder={uploaded.length > 0 && draft.trim() === "" ? "Kirim lampiran saja, atau tulis pesan" : placeholder}
            disabled={!chat.active}
            className={`w-full resize-none border-0 bg-transparent text-bolu-ink outline-none placeholder:text-bolu-muted disabled:opacity-60 ${
              compact ? "max-h-24 px-3 py-2.5 text-[14px]" : "max-h-32 px-4 py-3 text-[15px]"
            }`}
          />

          <div className={`flex items-center justify-between ${compact ? "px-2 pb-2" : "px-3 pb-2 pt-1"}`}>
            <input ref={picker} type="file" multiple hidden onChange={(event) => queueFiles(event.target.files)} />
            <button
              type="button"
              aria-label="Lampirkan berkas"
              disabled={!chat.active}
              onClick={() => picker.current?.click()}
              className="flex size-8 cursor-pointer items-center justify-center rounded-md border-0 bg-transparent text-bolu-muted hover:bg-bolu-hover hover:text-bolu-ink disabled:opacity-40"
            >
              <Paperclip size={compact ? 16 : 18} strokeWidth={2} aria-hidden="true" />
            </button>
            <button
              type="submit"
              aria-label="Kirim"
              disabled={!canSend}
              className={`flex cursor-pointer items-center justify-center rounded-md border border-bolu-border bg-bolu-bubble text-bolu-body transition-colors hover:bg-bolu-amber-soft hover:text-bolu-ink disabled:cursor-default disabled:opacity-50 ${
                compact ? "size-7" : "size-8"
              }`}
            >
              {chat.sending || uploading ? "…" : <ArrowRight size={compact ? 14 : 16} strokeWidth={2} aria-hidden="true" />}
            </button>
          </div>
        </div>

        {/* A disabled control says nothing about why it is disabled, so the
            composer says it: a refused send, or a file still uploading. */}
        {sendError ? (
          <p role="alert" className="px-3 text-[12px] text-bolu-belum">
            {sendError}
          </p>
        ) : null}
        {failed ? (
          <p className="px-3 text-[12px] text-bolu-muted">Hapus atau unggah ulang lampiran yang gagal untuk mengirim.</p>
        ) : null}
        {uploading ? (
          <p role="status" className="px-3 text-[12px] text-bolu-muted">
            Menunggu lampiran selesai diunggah…
          </p>
        ) : null}
        {footer ? <p className="text-center text-[10px] text-bolu-muted">{footer}</p> : null}
      </div>
    </form>
  );
}

/**
 * ThreadMarker is where a work thread left the chat: a chip that opens it in the
 * panel. The dot is lit while the thread is the one the panel shows.
 */
function ThreadMarker({
  thread,
  open,
  onOpen,
}: {
  thread: ChatConversation;
  open: boolean;
  onOpen: () => void;
}) {
  return (
    <div className="flex justify-start pl-12 max-[760px]:pl-0">
      <button
        type="button"
        onClick={onOpen}
        aria-pressed={open}
        className="flex max-w-full cursor-pointer items-center gap-1.5 rounded-md border border-bolu-border bg-bolu-surface px-2.5 py-1.5 text-[12px] text-bolu-muted shadow-sm transition-colors hover:bg-bolu-hover/60 hover:text-bolu-ink"
      >
        <MessagesSquare size={14} strokeWidth={2} aria-hidden="true" />
        <span>Diteruskan ke thread</span>
        <span className={`size-1.5 flex-none rounded-full ${open ? "bg-bolu-amber" : "bg-bolu-accent"}`} />
        <span className="truncate font-medium text-bolu-link">{thread.title || "Thread"}</span>
      </button>
    </div>
  );
}

/** TypingRow is a Bolu preparing an answer: its thinking face and three dots. */
export function TypingRow({
  name,
  face,
  text,
  compact = false,
}: {
  name: string;
  face: CrewMember;
  text: string;
  compact?: boolean;
}) {
  return (
    <div role="status" aria-live="polite" className={`flex items-center ${compact ? "gap-3" : "gap-4"}`}>
      <Avatar face={face} expression="mikir" compact={compact} />
      <div className="flex items-center gap-2">
        <span className="inline-flex items-center gap-1 rounded-2xl rounded-bl-sm border border-bolu-border bg-bolu-bubble px-3 py-2">
          {[0, 1, 2].map((dot) => (
            <span
              key={dot}
              className="block size-1.5 rounded-full bg-bolu-muted motion-safe:animate-typing-dot"
              style={{ animationDelay: `${(dot - 2) * 0.16}s` }}
            />
          ))}
        </span>
        {/* The name and the activity are the whole state, read once. */}
        <span className="text-[12px] text-bolu-muted">
          {name} · {text}
        </span>
      </div>
    </div>
  );
}

/** Avatar is a Bolu's face in the round tile the panel design gives a speaker. */
export function Avatar({
  face,
  color,
  expression = "senang",
  compact = false,
}: {
  face: CrewMember;
  color?: string;
  expression?: BoluExpressionName;
  compact?: boolean;
}) {
  return (
    <span
      className={`flex flex-none items-center justify-center overflow-hidden rounded-full border border-bolu-border bg-bolu-amber-soft shadow-sm ${
        compact ? "mt-0.5 size-6" : "size-8"
      }`}
    >
      <Bolu member={face} color={color} expression={expression} size={compact ? 20 : 28} />
    </span>
  );
}

/**
 * useStickToBottom keeps a thread at its newest message.
 *
 * The scroll lives inside the thread rather than on the page, so the composer is
 * always where it was and a reply never has to be chased down the window. Height
 * is what is watched rather than the messages: a chart, a diagram, and a streamed
 * paragraph all arrive *after* the render that added them, and each one would
 * otherwise push the newest message below the fold.
 *
 * A reader who scrolled up is left where they are — the place they chose is not
 * taken from them — which is why the pin follows them only while they are near the
 * bottom.
 */
export function useStickToBottom(conversationId?: string) {
  const thread = useRef<HTMLDivElement | null>(null);
  const content = useRef<HTMLDivElement | null>(null);
  const sticky = useRef(true);
  // The position this hook last wrote, or null when it has not written since the
  // last scroll event. A scroll event is not always somebody scrolling — a thread
  // switch and a late-rendering block both move the position from here — so an
  // event that arrives at the position this hook just wrote is its own echo and is
  // not read as intent. Everything else is a person, and is.
  const wrote = useRef<number | null>(null);

  const pin = useCallback(() => {
    const box = thread.current;
    if (!box) {
      return;
    }
    sticky.current = true;
    wrote.current = box.scrollHeight - box.clientHeight;
    box.scrollTop = box.scrollHeight;
  }, []);

  // Opening a thread is a request for its newest message.
  useEffect(() => {
    pin();
  }, [conversationId, pin]);

  useEffect(() => {
    const box = thread.current;
    const inner = content.current;
    if (!box || !inner) {
      return;
    }

    const stick = () => {
      if (sticky.current) {
        wrote.current = box.scrollHeight - box.clientHeight;
        box.scrollTop = box.scrollHeight;
      }
    };
    // Both: the content grows when a block renders late, and the box shrinks when
    // the window does. Either one leaves the newest message below the fold.
    const observer = new ResizeObserver(stick);
    observer.observe(inner);
    observer.observe(box);

    // Scrolling up is a person reading history, and it is taken from the gesture:
    // a reply that is still arriving changes the height under a scroll that is
    // already in flight, and reading that scroll as intent would pull the reader
    // back down mid-sentence.
    const onWheel = (event: WheelEvent) => {
      if (event.deltaY < 0) {
        sticky.current = false;
      }
      // The gesture is the intent itself, so a write from before it can no longer
      // be mistaken for one: without this a reader who dragged the scrollbar back
      // to a position this hook once wrote could be read as its own echo.
      wrote.current = null;
    };

    // The nearest few pixels still count as the bottom: a person who scrolled with
    // a trackpad lands a little short of it, and that is not them reading history.
    const onScroll = () => {
      const expected = wrote.current;
      wrote.current = null;
      if (expected !== null && box.scrollTop === expected) {
        return;
      }
      sticky.current = box.scrollHeight - box.scrollTop - box.clientHeight < 64;
    };

    box.addEventListener("scroll", onScroll, { passive: true });
    box.addEventListener("wheel", onWheel, { passive: true });
    return () => {
      observer.disconnect();
      box.removeEventListener("scroll", onScroll);
      box.removeEventListener("wheel", onWheel);
    };
  }, []);

  return { thread, content, pin };
}

/**
 * nameOf is the display name of a message's author.
 *
 * The message carries the author id and the conversation carries the names, so
 * the name is read from the participants: a Bolu that answered in a group is
 * named by the same record the header lists, which is what keeps the two from
 * disagreeing.
 */
export function nameOf(conversation: ChatConversation | null, message: ChatMessage): string {
  if (!message.agent_id) {
    return "Kamu";
  }
  const participant = conversation?.participants.find(
    (candidate) => candidate.agent_id === message.agent_id,
  );
  return participant?.name ?? "Bolu";
}

/**
 * authorFace is the avatar of a message's author.
 *
 * The face comes from the registry row — the shape and colour the workspace gave
 * that Bolu — so a Bolu created here looks the same in the thread and the sidebar,
 */
export function authorFace(
  conversation: ChatConversation | null,
  message: ChatMessage,
  agents: Agent[],
): CrewMember | null {
  if (!message.agent_id) {
    return null;
  }
  const agent = agents.find((candidate) => candidate.id === message.agent_id);
  if (agent) {
    return crewFor(agent);
  }
  // A message from a Bolu that has since been deleted still names its author, so
  // the thread draws the crew member its name maps to rather than dropping the
  // author entirely.
  return allBot("Oren");
}

/** colorOf is the colour the workspace gave the Bolu that wrote a message. */
export function colorOf(message: ChatMessage, agents: Agent[]): string | undefined {
  if (!message.agent_id) return undefined;
  return agents.find((candidate) => candidate.id === message.agent_id)?.color || undefined;
}

/**
 * MessageRow renders one message, as the panel design draws it: a Bolu speaks
 * beside its face in plain text, and a person's message is a soft bubble on the
 * right.
 *
 * A reply that is still arriving with nothing to show yet draws nothing at all:
 * the typing row is the one line that says a Bolu is working, and a bubble would
 * be a second one. The moment the first block lands the row is the answer, and it
 * streams into place.
 */
export function MessageRow({
  message,
  name,
  face,
  color,
  expression,
  compact = false,
}: {
  message: ChatMessage;
  name: string;
  face: CrewMember | null;
  color?: string;
  /** The face the Bolu's current state poses; absent keeps the neutral face. */
  expression?: BoluExpressionName;
  /** The thread panel's smaller row. */
  compact?: boolean;
}) {
  const fromAgent = Boolean(message.agent_id);
  const notice = describeResult(message);
  // A message with nothing in it is not drawn: the server opens an empty message
  // for an upload that named none and publishes it before the file is stored,
  // and a reply that is still being prepared has no blocks yet either.
  const empty =
    message.blocks.length === 0 &&
    message.attachments.length === 0 &&
    notice === null &&
    !message.task_id;
  if (empty) {
    return null;
  }

  // What the attachment list hangs from. It is the first thing in the row for a
  // message that is only files, and a gap above it would be a gap above nothing.
  const stacked = message.blocks.length > 0 || notice !== null;
  const time = clockOf(message.created_at);

  const body = (
    <>
      {message.blocks.map((block, index) => (
        <Block key={`${message.message_id}-${index}`} block={block} />
      ))}

      {notice ? (
        <p className="mt-2 rounded-md border border-bolu-flag/30 bg-bolu-flag/8 px-2.5 py-1.5 text-xs text-bolu-flag">
          {notice}
        </p>
      ) : null}

      {message.attachments.length > 0 ? (
        <ul className={stacked ? "mt-2 space-y-1" : "space-y-1"}>
          {message.attachments.map((attachment) => (
            <li key={attachment.id} className="flex items-center gap-2">
              <FileText size={14} strokeWidth={2} aria-hidden="true" className="flex-none text-bolu-muted" />
              <a href={attachment.url} target="_blank" rel="noreferrer noopener" className="text-sm underline">
                {attachment.filename}
              </a>
              <span className="text-xs text-bolu-muted">{formatBytes(attachment.byte_size)}</span>
            </li>
          ))}
        </ul>
      ) : null}
    </>
  );

  if (!fromAgent) {
    return (
      <article className="ml-auto flex max-w-[85%] flex-col items-end gap-1">
        <span className="sr-only">Kamu</span>
        <div
          className={`inline-block rounded-2xl rounded-tr-sm bg-bolu-bubble text-bolu-ink shadow-sm ${
            compact ? "px-4 py-2.5 text-[14px]" : "px-5 py-3 text-[15px]"
          }`}
        >
          {body}
        </div>
        {message.task_id ? <TaskLink taskId={message.task_id} align="end" /> : null}
      </article>
    );
  }

  return (
    <article className={`group flex ${compact ? "gap-3" : "gap-4"}`}>
      <Avatar face={face ?? allBot("Oren")} color={color} expression={expression} compact={compact} />
      <div className={`min-w-0 flex-1 leading-relaxed text-bolu-body ${compact ? "text-[14px]" : "text-[15px]"}`}>
        <p className="sr-only">{name}</p>
        {body}
        <div className="mt-1.5 flex items-center gap-3 text-xs text-bolu-muted">
          {time ? <span>{time}</span> : null}
          {/* A message that opened a task says so, and the way in is the task itself: the work
              behind an answer is the reason the answer can be trusted. */}
          {message.task_id ? <TaskLink taskId={message.task_id} /> : null}
        </div>
      </div>
    </article>
  );
}

function TaskLink({ taskId, align = "start" }: { taskId: string; align?: "start" | "end" }) {
  return (
    <Link
      href={`/tasks/${taskId}`}
      className={`text-xs text-bolu-muted no-underline hover:underline ${align === "end" ? "text-right" : ""}`}
    >
      Lihat tugasnya →
    </Link>
  );
}

/** clockOf is a message's time of day, as the design stamps it under a reply. */
function clockOf(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString("id-ID", { hour: "2-digit", minute: "2-digit" });
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
