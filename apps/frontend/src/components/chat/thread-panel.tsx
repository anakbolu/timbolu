"use client";

/**
 * The thread panel: the work threads a Bolu opened from the conversation on the
 * left, and the one a person is reading or steering.
 *
 * A thread is a conversation of its own — the Bolu's brief, the work it reports,
 * and whatever a person writes to steer it — so the panel runs a second chat over
 * the same store and stream as the main one. What a thread produced as files is
 * gathered into the bar at the top, which is where the panel design puts the
 * work's output.
 *
 * On a wide screen the panel is a column beside the chat; below that it is a
 * drawer over it, opened from the header and closed the same way.
 */

import { ChevronLeft, ChevronRight, FileText, MessagesSquare, X } from "lucide-react";
import { useEffect, useMemo } from "react";

import { useWorkspace } from "@/components/dashboard/workspace-data";
import type { Attachment, ChatConversation } from "@/lib/api";
import { expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";

import { Composer, MessageRow, TypingRow, authorFace, colorOf, nameOf, useStickToBottom } from "./chat-view";
import { isBusy, useChat } from "./use-chat";

/** ThreadPanel is the right-hand column: the list of threads, or one of them. */
export function ThreadPanel({
  threads,
  selected,
  wide,
  onSelect,
  onClose,
}: {
  /** The threads of the open conversation, newest first. */
  threads: ChatConversation[];
  /** The thread being read, or null for the list. */
  selected: ChatConversation | null;
  /** Whether the panel is a column (wide) or a drawer (narrow). */
  wide: boolean;
  onSelect: (id: string) => void;
  onClose: () => void;
}) {
  return (
    <>
      {wide ? null : <div onClick={onClose} className="fixed inset-0 z-[35] bg-bolu-scrim" />}
      <aside
        aria-label="Thread kerja"
        className={`flex flex-col border-l border-bolu-border bg-bolu-surface shadow-lg ${
          wide ? "relative z-20 w-[360px] flex-none" : "fixed inset-y-0 right-0 z-40 w-[min(360px,92vw)]"
        }`}
      >
        {selected ? (
          <ThreadView
            thread={selected}
            hasList={threads.length > 1}
            onBack={() => onSelect(LIST_ID)}
            onClose={onClose}
          />
        ) : (
          <ThreadList threads={threads} onSelect={onSelect} onClose={onClose} />
        )}
      </aside>
    </>
  );
}

/** LIST_ID asks the chat to show the list rather than one thread. */
const LIST_ID = "__list__";

/** ThreadList is every thread of the conversation, newest first. */
function ThreadList({
  threads,
  onSelect,
  onClose,
}: {
  threads: ChatConversation[];
  onSelect: (id: string) => void;
  onClose: () => void;
}) {
  return (
    <>
      <header className="flex h-14 flex-none items-center justify-between border-b border-bolu-line px-4">
        <span className="text-sm font-medium text-bolu-ink">Thread</span>
        <CloseButton onClose={onClose} />
      </header>
      {threads.length === 0 ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 px-6 text-center text-[13px] text-bolu-muted">
          <MessagesSquare size={22} strokeWidth={1.8} aria-hidden="true" />
          <p>
            Belum ada thread. Minta Bolu mengerjakan sesuatu yang butuh beberapa langkah, dan ia akan membuka
            thread kerja di sini.
          </p>
        </div>
      ) : (
        <ul className="flex flex-1 flex-col gap-0.5 overflow-y-auto p-2">
          {threads.map((thread) => (
            <li key={thread.id}>
              <button
                type="button"
                onClick={() => onSelect(thread.id)}
                className="flex w-full cursor-pointer items-center gap-2.5 rounded-md border-0 bg-transparent px-2.5 py-2 text-left hover:bg-bolu-hover/70"
              >
                <span className="size-1.5 flex-none rounded-full bg-bolu-accent" />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-[14px] font-medium text-bolu-ink">{thread.title || "Thread"}</span>
                  <span className="block text-[12px] text-bolu-muted">
                    {thread.message_count} pesan · {relativeAge(thread.last_activity_at)}
                  </span>
                </span>
                <ChevronRight size={14} strokeWidth={2} aria-hidden="true" className="text-bolu-muted" />
              </button>
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

/**
 * ThreadView is one thread: its title, the files it produced, its messages, and
 * the composer a person steers it with.
 */
function ThreadView({
  thread,
  hasList,
  onBack,
  onClose,
}: {
  thread: ChatConversation;
  hasList: boolean;
  onBack: () => void;
  onClose: () => void;
}) {
  const chat = useChat();
  const { agents } = useWorkspace();
  const { thread: box, content, pin } = useStickToBottom(chat.active?.id);
  const { openByID } = chat;

  // The panel opens the thread it was given, and opens again when it is given
  // another; the store may not list a brand-new thread on the first render, which
  // is why a miss is left for the next one.
  const openedId = chat.active?.id;
  useEffect(() => {
    if (openedId !== thread.id) {
      openByID(thread.id);
    }
  }, [openByID, openedId, thread.id]);

  // What the thread produced, newest last, as the design's bar of outputs.
  const files = useMemo(() => {
    const all: Attachment[] = [];
    for (const message of chat.messages) {
      all.push(...message.attachments);
    }
    return all;
  }, [chat.messages]);

  const typing = chat.messages.find(
    (message) => isBusy(message) && message.blocks.length === 0 && message.attachments.length === 0,
  );
  const typingFace = typing ? authorFace(chat.active, typing, agents) : null;

  // The face every Bolu message in the thread wears: the one its state poses.
  // A group has no single partner, so its messages keep the neutral face.
  const partnerParticipant =
    chat.active && chat.active.kind !== "group"
      ? chat.active.participants.find((item) => item.is_agent)
      : undefined;
  const partner = agents.find((agent) => agent.id === partnerParticipant?.agent_id) ?? null;
  const partnerExpression = partner ? expressionOf(stateOf(partner)) : undefined;

  return (
    <>
      <header className="flex h-14 flex-none items-center justify-between gap-2 border-b border-bolu-line px-4">
        <div className="flex min-w-0 items-center gap-2 text-sm">
          {hasList ? (
            <button
              type="button"
              onClick={onBack}
              className="flex cursor-pointer items-center gap-0.5 border-0 bg-transparent p-0 font-medium text-bolu-muted hover:text-bolu-ink"
            >
              <ChevronLeft size={14} strokeWidth={2} aria-hidden="true" />
              Thread
            </button>
          ) : (
            <span className="font-medium text-bolu-muted">Thread</span>
          )}
          <ChevronRight size={14} strokeWidth={2} aria-hidden="true" className="flex-none text-bolu-muted" />
          <span className="truncate font-medium text-bolu-ink" title={thread.title}>
            {thread.title || "Thread"}
          </span>
        </div>
        <CloseButton onClose={onClose} />
      </header>

      {files.length > 0 ? (
        <div className="flex flex-none gap-2 overflow-x-auto border-b border-bolu-line bg-bolu-bg p-3">
          {files.map((file) => (
            <a
              key={file.id}
              href={file.url}
              target="_blank"
              rel="noreferrer noopener"
              title={file.filename}
              className="group w-20 flex-none no-underline"
            >
              <span className="flex h-14 items-center justify-center rounded-md border border-bolu-border bg-bolu-surface p-1 shadow-sm group-hover:border-bolu-accent">
                <span className="flex size-full items-center justify-center rounded bg-bolu-panel text-bolu-muted">
                  <FileText size={18} strokeWidth={1.8} aria-hidden="true" />
                </span>
              </span>
              <span className="mt-1 block truncate text-center text-[10px] text-bolu-muted">{file.filename}</span>
            </a>
          ))}
        </div>
      ) : null}

      <div ref={box} className="flex min-h-0 flex-1 flex-col overflow-y-auto p-4">
        {chat.error ? (
          <p role="alert" className="mb-3 rounded-md border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
            {chat.error}
          </p>
        ) : null}
        <div ref={content} className="mt-auto flex flex-col gap-5">
          {chat.messages.map((message) => (
            <MessageRow
              key={message.message_id}
              message={message}
              name={nameOf(chat.active, message)}
              face={authorFace(chat.active, message, agents)}
              color={colorOf(message, agents)}
              expression={partnerExpression}
              compact
            />
          ))}
          {typing && typingFace ? (
            <TypingRow
              name={nameOf(chat.active, typing)}
              face={typingFace}
              text={chat.activity && chat.activity.agentId === typing.agent_id ? chat.activity.text : "Sedang bekerja…"}
              compact
            />
          ) : null}
        </div>
      </div>

      <Composer chat={chat} onSent={pin} placeholder="Arahkan thread ini…" compact />
    </>
  );
}

function CloseButton({ onClose }: { onClose: () => void }) {
  return (
    <button
      type="button"
      onClick={onClose}
      aria-label="Tutup panel thread"
      className="flex size-7 flex-none cursor-pointer items-center justify-center rounded border-0 bg-transparent text-bolu-muted hover:bg-bolu-hover hover:text-bolu-ink"
    >
      <X size={16} strokeWidth={2} aria-hidden="true" />
    </button>
  );
}
