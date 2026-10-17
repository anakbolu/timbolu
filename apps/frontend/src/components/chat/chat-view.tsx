"use client";

/**
 * The chat view: the thread, the composer, and the live state of the answer.
 *
 * A reply is rendered from what the server stored, not from tokens the client
 * assembled, so a message that is still arriving shows the text it has and the
 * final event replaces it with the canonical body.
 */

import { ArrowUp, Check, History, Paperclip, X } from "lucide-react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { useWorkspace } from "@/components/dashboard/workspace-data";
import { uploadStagedAttachment, type Agent, type ApiError, type ChatConversation, type ChatMessage } from "@/lib/api";
import { allBot, type CrewMember } from "@/lib/crew";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

import { Block } from "./blocks";
import { ConversationList } from "./conversation-list";
import { describeResult, isBusy, useChat } from "./use-chat";

const STATUS_LABEL: Record<string, string> = {
  connecting: "menyambung…",
  live: "tersambung",
  reconnecting: "menyambung ulang…",
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

const STATUS_CLASS: Record<string, string> = {
  connecting: "bg-bolu-panel text-bolu-muted",
  live: "bg-bolu-live/12 text-bolu-lunas",
  reconnecting: "bg-bolu-flag/12 text-bolu-flag",
  closed: "bg-bolu-belum/12 text-bolu-belum",
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

/** ChatView is the whole screen: one thread and its composer. */
export function ChatView({
  agentId,
  conversationId,
}: {
  agentId?: string;
  conversationId?: string;
}) {
  const chat = useChat();
  const { agents, user } = useWorkspace();
  const [draft, setDraft] = useState("");
  const [queued, setQueued] = useState<QueuedFile[]>([]);
  const [sendError, setSendError] = useState<string | null>(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const opened = useRef<string | null>(null);
  const picker = useRef<HTMLInputElement | null>(null);
  // The uploads in flight, by chip, so removing one drops its request too.
  const uploads = useRef(new Map<string, AbortController>());

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

  const title = useMemo(() => {
    if (!chat.active) {
      return "Pilih obrolan";
    }
    if (chat.active.title) {
      return chat.active.title;
    }
    const agent = chat.active.participants.find((participant) => participant.is_agent);
    return agent?.name ?? "Obrolan";
  }, [chat.active]);

  // The header shows the one Bolu a direct thread is with: its face, its role, and
  // what it is doing right now. A group has no single partner, so it names its
  // members instead of borrowing the first one's face.
  const partner = useMemo(() => {
    if (!chat.active || chat.active.kind !== "direct") return null;
    const participant = chat.active.participants.find((item) => item.is_agent);
    if (!participant) return null;
    return agents.find((agent) => agent.id === participant.agent_id) ?? null;
  }, [chat.active, agents]);

  /**
   * waiting are the lines that stand for the answers being prepared.
   *
   * A group message can be answered by more than one Bolu, so each one that is
   * still working gets its own line: one row for the whole group would hide the
   * second answer behind the first. The router may name the Bolu before its
   * message exists, and that message carries no text until the first block lands,
   * so both are the same wait and are drawn once. A message that already has
   * content is its own bubble, and its row is over.
   */
  const waiting = useMemo(() => {
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

  // The circle beside a user message carries the account's initial: the store has
  // no display name, and the email is what a person recognises as their own.
  const userInitial = user?.email ? user.email.charAt(0).toUpperCase() : "?";

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
    pin();

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
    <div className="flex h-full min-h-0 flex-1 min-[761px]:flex-row">
      <ConversationList
        conversations={chat.conversations}
        agents={agents}
        activeId={chat.active?.id}
        open={historyOpen}
        onClose={() => setHistoryOpen(false)}
        onNavigate={() => setHistoryOpen(false)}
      />

      <div className="flex min-h-0 min-w-0 flex-1 flex-col">
        <header className="flex flex-wrap items-center gap-3 border-b border-bolu-border bg-bolu-surface px-6 py-3 max-[760px]:px-3">
          {/* Below 760px the panel is a drawer, so the thread needs its way in. From
              761px up the list is always there and this button would be noise. */}
          <button
            type="button"
            aria-label="Riwayat obrolan"
            aria-expanded={historyOpen}
            onClick={() => setHistoryOpen((open) => !open)}
            className="hidden size-10 flex-none cursor-pointer items-center justify-center rounded-md border-0 bg-bolu-bg max-[760px]:flex"
          >
            <History size={19} strokeWidth={2.2} aria-hidden="true" />
          </button>
          {partner ? (
            <Bolu
              member={crewFor(partner)}
              expression={expressionOf(stateOf(partner))}
              color={partner.color || undefined}
              size={44}
              className="flex-none"
            />
          ) : null}
          <div className="min-w-0 flex-1">
            <h1 className="truncate font-display text-[22px] font-semibold">{title}</h1>
            <p className="truncate text-[14px] text-bolu-muted">
              {partner
                ? `${partner.role || "Bolu"} · ${partner.display_status === "working" ? "sedang bekerja" : partner.display_status === "waiting" ? "menunggu kamu" : "santai"}`
                : chat.active
                  ? chat.active.participants
                      .map((participant) => `${participant.name} · ${participant.role}`)
                      .join("  ·  ")
                  : "Pilih Bolu di kiri untuk mulai."}
            </p>
          </div>
          <span
            className={`shrink-0 rounded-full px-2.5 py-1 text-xs font-medium ${
              STATUS_CLASS[chat.status] ?? STATUS_CLASS.closed
            }`}
          >
            {STATUS_LABEL[chat.status] ?? chat.status}
          </span>
        </header>

        <section className="flex min-h-0 flex-1 flex-col">
          {chat.error ? (
            <p role="alert" className="mx-4 mt-3 rounded-md border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
              {chat.error}
            </p>
          ) : null}

          <div
            ref={thread}
            className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4 py-4"
          >
            {chat.hasMore ? (
              <button
                type="button"
                onClick={() => void chat.loadOlder()}
                disabled={chat.loadingOlder}
                className="mx-auto mb-3 block cursor-pointer rounded-full border border-bolu-border px-3 py-1 text-sm text-bolu-muted transition hover:bg-bolu-hover/60 disabled:opacity-50"
              >
                {chat.loadingOlder ? "Memuat…" : "Muat pesan lama"}
              </button>
            ) : null}

            {chat.messages.length === 0 && !chat.loading ? (
              <p className="mt-8 text-center text-[15px] text-bolu-muted">
                {chat.active
                  ? `Belum ada pesan dengan ${title}. Tulis sesuatu untuk mulai.`
                  : "Pilih Bolu di kiri untuk mulai."}
              </p>
            ) : null}

            <div ref={content} className="mx-auto mt-auto flex w-full max-w-[760px] flex-col gap-3">
              {chat.messages.map((message) => (
                <MessageRow
                  key={message.message_id}
                  message={message}
                  name={nameOf(chat.active, message)}
                  face={authorFace(chat.active, message, agents)}
                  initial={userInitial}
                />
              ))}

              {waiting.map((row) => (
                <div
                  key={row.key}
                  role="status"
                  aria-live="polite"
                  className="flex items-center gap-2.5 text-sm text-bolu-muted"
                >
                  <Bolu member={row.face} expression="mikir" size={22} className="flex-none" />
                  {/* The name is inside the pulsing text rather than a separate
                      line: one row is the whole state, and it is read once. */}
                  <span className="motion-safe:animate-soft-pulse">
                    {row.name} · {row.text}
                  </span>
                </div>
              ))}

              {chat.routerNotice ? (
                <p className="text-xs text-bolu-muted">
                  Router memilih {chat.routerNotice.reason}
                  {chat.routerNotice.source === "model" ? " (model kecil)" : " (cadangan)"}.
                </p>
              ) : null}
            </div>
          </div>

          <form onSubmit={submit} className="bg-bolu-bg px-4 py-3 max-[760px]:px-3">
            <div className="mx-auto flex max-w-[760px] flex-col gap-2">
              {hasFiles ? (
                <ul className="flex flex-wrap gap-1.5">
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
                        <span
                          role="status"
                          className="flex items-center gap-1.5"
                          aria-label={`Mengunggah ${item.file.name}`}
                        >
                          <span className="h-1 w-14 overflow-hidden rounded-full bg-bolu-track">
                            <span
                              className="block h-full rounded-full bg-bolu-accent"
                              style={{ width: `${Math.round(item.progress * 100)}%` }}
                            />
                          </span>
                          <span className="text-[11px] text-bolu-muted">
                            {Math.round(item.progress * 100)}%
                          </span>
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

              <div className="flex items-center gap-2 rounded-full border border-bolu-border bg-bolu-surface py-1.5 pl-1.5 pr-1.5">
                <input
                  ref={picker}
                  type="file"
                  multiple
                  hidden
                  onChange={(event) => queueFiles(event.target.files)}
                />
                <button
                  type="button"
                  aria-label="Lampirkan berkas"
                  disabled={!chat.active}
                  onClick={() => picker.current?.click()}
                  className="flex size-9 shrink-0 cursor-pointer items-center justify-center rounded-full border-0 bg-transparent text-bolu-muted hover:text-bolu-ink disabled:opacity-40"
                >
                  <Paperclip size={18} strokeWidth={2.2} aria-hidden="true" />
                </button>
                <input
                  value={draft}
                  onChange={(event) => setDraft(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" && !event.shiftKey) {
                      event.preventDefault();
                      event.currentTarget.form?.requestSubmit();
                    }
                  }}
                  placeholder={
                    uploaded.length > 0 && draft.trim() === ""
                      ? "Kirim lampiran saja, atau tulis pesan"
                      : chat.active
                        ? "Suruh atau tanya apa saja…"
                        : "Pilih obrolan dulu"
                  }
                  disabled={!chat.active}
                  className="min-h-9 min-w-0 flex-1 border-0 bg-transparent text-[15px] outline-none disabled:opacity-60"
                />
                <button
                  type="submit"
                  aria-label="Kirim"
                  disabled={!canSend}
                  className="flex size-10 shrink-0 cursor-pointer items-center justify-center rounded-full border-0 bg-bolu-accent text-bolu-solid-ink disabled:opacity-50"
                >
                  {chat.sending || uploading ? (
                    "…"
                  ) : (
                    <ArrowUp size={18} strokeWidth={2.5} aria-hidden="true" />
                  )}
                </button>
              </div>

              {/* A disabled control says nothing about why it is disabled, so the
                  composer says it: a refused send, or a file still uploading. */}
              {sendError ? (
                <p role="alert" className="px-3 text-[12px] text-bolu-belum">
                  {sendError}
                </p>
              ) : null}
              {failed ? (
                <p className="px-3 text-[12px] text-bolu-muted">
                  Hapus atau unggah ulang lampiran yang gagal untuk mengirim.
                </p>
              ) : null}
              {uploading ? (
                <p role="status" className="px-3 text-[12px] text-bolu-muted">
                  Menunggu lampiran selesai diunggah…
                </p>
              ) : null}
            </div>
          </form>
        </section>
      </div>
    </div>
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
function useStickToBottom(conversationId?: string) {
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
function nameOf(conversation: ChatConversation | null, message: ChatMessage): string {
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
function authorFace(
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

/**
 * MessageRow renders one message, from the user or from a Bolu.
 * A reply that is still arriving with nothing to show yet draws no bubble at
 * all: the waiting row above the composer's end of the thread is the one line
 * that says a Bolu is working, and a bubble would be a second one. The moment
 * the first block lands the bubble is the answer, and it streams into place.
 */
function MessageRow({
  message,
  name,
  face,
  initial,
}: {
  message: ChatMessage;
  name: string;
  face: CrewMember | null;
  /** The account's initial, drawn in the circle beside a user's message. */
  initial: string;
}) {
  const fromAgent = Boolean(message.agent_id);
  const notice = describeResult(message);
  // A message with nothing in it is not drawn: the server opens an empty message
  // for an upload that named none and publishes it before the file is stored,
  // and a reply that is still being prepared has no blocks yet either. The
  // waiting row stands for both, and an empty box with padding stands for
  // neither.
  const empty =
    message.blocks.length === 0 &&
    message.attachments.length === 0 &&
    notice === null &&
    !message.task_id;
  if (empty) {
    return null;
  }

  // What the attachment list hangs from inside the bubble. It is the first thing
  // in it for a message that is only files, and an `mt-2` there is a gap above
  // nothing.
  const stacked = message.blocks.length > 0 || notice !== null;

  return (
    <article className={fromAgent ? "flex items-start gap-2.5" : "flex justify-end gap-2.5"}>
      {fromAgent && face ? (
        <span className="mt-0.5 flex-none">
          <Bolu member={face} expression="senang" size={36} />
        </span>
      ) : null}

      <div className={fromAgent ? "min-w-0 max-w-[560px]" : "max-w-[78%]"}>
        <p className={fromAgent ? "mb-1 text-[13px] font-semibold text-bolu-body" : "mb-1 text-right text-[13px] font-semibold text-bolu-body"}>
          {fromAgent ? name : "Kamu"}
        </p>

        <div
          className={
            fromAgent
              ? "rounded-[6px_20px_20px_20px] border border-bolu-border bg-bolu-surface px-4 py-2.5"
              : "rounded-[20px_20px_6px_20px] bg-bolu-solid px-4 py-2.5 text-[15px] text-bolu-solid-ink"
          }
        >
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
                <li key={attachment.id}>
                  <a
                    href={attachment.url}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="text-sm underline"
                  >
                    {attachment.filename}
                  </a>
                  <span className="ml-2 text-xs text-bolu-muted">
                    {formatBytes(attachment.byte_size)}
                  </span>
                </li>
              ))}
            </ul>
          ) : null}
        </div>

        {/* A message that opened a task says so, and the way in is the task itself: the work behind
            an answer is the reason the answer can be trusted, and until this link existed the only
            way to see it was to guess from the dashboard. */}
        {message.task_id ? (
          <p className={fromAgent ? "mt-1 text-[13px]" : "mt-1 text-right text-[13px]"}>
            <Link
              href={`/tasks/${message.task_id}`}
              className="text-bolu-muted no-underline hover:underline"
            >
              Lihat tugasnya →
            </Link>
          </p>
        ) : null}
      </div>
      {/* mt-6 clears the name line above the bubble, so the circle sits beside the
          message rather than beside the label. */}
      {!fromAgent ? (
        <span className="mt-6 flex size-9 flex-none items-center justify-center rounded-full bg-bolu-accent text-[15px] font-bold text-bolu-solid-ink">
          {initial}
        </span>
      ) : null}
    </article>
  );
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
