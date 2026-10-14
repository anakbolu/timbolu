"use client";

/**
 * The chat view: the thread, the composer, and the live state of the answer.
 *
 * A reply is rendered from what the server stored, not from tokens the client
 * assembled, so a message that is still arriving shows the text it has and the
 * final event replaces it with the canonical body.
 */

import { ArrowUp } from "lucide-react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { Bolu } from "@/components/bolu/bolu";
import { useWorkspace } from "@/components/dashboard/workspace-data";
import type { Agent, ChatConversation, ChatMessage } from "@/lib/api";
import { allBot, type CrewMember } from "@/lib/crew";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

import { Block } from "./blocks";
import { describeResult, isBusy, useChat } from "./use-chat";

const STATUS_LABEL: Record<string, string> = {
  connecting: "menyambung…",
  live: "tersambung",
  reconnecting: "menyambung ulang…",
  closed: "terputus",
};

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
  const { agents } = useWorkspace();
  const [draft, setDraft] = useState("");
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

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const text = draft.trim();
    if (!text || chat.sending) {
      return;
    }
    setDraft("");
    // Sending is a person looking for the answer, so the thread returns to its
    // newest message even if they had scrolled up to read something older.
    pin();
    await chat.send(text);
  }

  return (
    <div className="flex h-full min-h-0 flex-1 flex-col">
      <header className="flex flex-wrap items-center gap-3 border-b border-bolu-border bg-white px-6 py-3">
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

      <div className="flex min-h-0 flex-1 flex-col">
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
                className="mx-auto mb-3 block cursor-pointer rounded-full border border-bolu-border px-3 py-1 text-sm text-bolu-muted transition hover:bg-bolu-panel/60 disabled:opacity-50"
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
                />
              ))}

              {chat.responder ? (
                <div className="flex items-center gap-2 text-sm text-bolu-muted">
                  <span className="dots inline-flex gap-1">
                    <Dot />
                    <Dot delay="150ms" />
                    <Dot delay="300ms" />
                  </span>
                  {chat.responder.name} sedang menyiapkan jawaban
                </div>
              ) : null}

              {chat.routerNotice ? (
                <p className="text-xs text-bolu-muted">
                  Router memilih {chat.routerNotice.reason}
                  {chat.routerNotice.source === "model" ? " (model kecil)" : " (cadangan)"}.
                </p>
              ) : null}
            </div>
          </div>

          <form onSubmit={submit} className="bg-bolu-bg px-4 py-3">
            <div className="mx-auto flex max-w-[760px] items-center gap-2 rounded-full border border-bolu-border bg-white py-1.5 pl-4 pr-1.5">
              <input
                value={draft}
                onChange={(event) => setDraft(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" && !event.shiftKey) {
                    event.preventDefault();
                    event.currentTarget.form?.requestSubmit();
                  }
                }}
                placeholder={chat.active ? "Suruh atau tanya apa saja…" : "Pilih obrolan dulu"}
                disabled={!chat.active}
                className="min-h-9 min-w-0 flex-1 border-0 bg-transparent text-[15px] outline-none disabled:opacity-60"
              />
              <button
                type="submit"
                aria-label="Kirim"
                disabled={!chat.active || chat.sending || draft.trim() === ""}
                className="flex size-10 shrink-0 cursor-pointer items-center justify-center rounded-full border-0 bg-bolu-accent text-white disabled:opacity-50"
              >
                {chat.sending ? (
                  "…"
                ) : (
                  <ArrowUp size={18} strokeWidth={2.5} aria-hidden="true" />
                )}
              </button>
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

/** MessageRow renders one message, from the user or from a Bolu. */
function MessageRow({ message, name, face }: { message: ChatMessage; name: string; face: CrewMember | null }) {
  const fromAgent = Boolean(message.agent_id);
  const notice = describeResult(message);

  return (
    <article className={fromAgent ? "flex items-start gap-2.5" : "flex justify-end"}>
      {fromAgent && face ? (
        <Bolu member={face} expression="senang" size={36} className="mt-0.5 flex-none" />
      ) : null}

      <div className={fromAgent ? "min-w-0 max-w-[560px]" : "max-w-[78%]"}>
        <p className={fromAgent ? "mb-1 text-[13px] font-semibold text-bolu-body" : "mb-1 text-right text-[13px] font-semibold text-bolu-body"}>
          {fromAgent ? name : "Kamu"}
        </p>

        <div
          className={
            fromAgent
              ? "rounded-[6px_20px_20px_20px] border border-bolu-border bg-white px-4 py-2.5"
              : "rounded-[20px_20px_6px_20px] bg-bolu-ink px-4 py-2.5 text-[15px] text-white"
          }
        >
          {message.blocks.length === 0 && isBusy(message) ? (
            <span className="inline-flex gap-1 text-bolu-muted">
              <Dot />
              <Dot delay="150ms" />
              <Dot delay="300ms" />
            </span>
          ) : null}

          {message.blocks.map((block, index) => (
            <Block key={`${message.message_id}-${index}`} block={block} />
          ))}

          {notice ? (
            <p className="mt-2 rounded-md border border-bolu-flag/30 bg-bolu-flag/8 px-2.5 py-1.5 text-xs text-bolu-flag">
              {notice}
            </p>
          ) : null}

          {message.attachments.length > 0 ? (
            <ul className="mt-2 space-y-1">
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
    </article>
  );
}

function Dot({ delay = "0ms" }: { delay?: string }) {
  return (
    <span
      aria-hidden
      className="inline-block size-1.5 animate-bounce rounded-full bg-current"
      style={{ animationDelay: delay }}
    />
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
