"use client";

/**
 * The chat view: the thread, the composer, and the live state of the answer.
 *
 * A reply is rendered from what the server stored, not from tokens the client
 * assembled, so a message that is still arriving shows the text it has and the
 * final event replaces it with the canonical body.
 */

import { useEffect, useMemo, useRef, useState } from "react";

import { BotSvg } from "@/components/bolu/bot-svg";
import type { ChatConversation, ChatMessage } from "@/lib/api";
import { ALL_BOTS, allBot, bot, withFace, type BotShape } from "@/lib/crew";

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

/** ChatView is the whole screen: thread list, one thread, and the composer. */
export function ChatView({ agentId }: { agentId?: string }) {
  const chat = useChat();
  const [draft, setDraft] = useState("");
  const opened = useRef<string | null>(null);

  // Opening by Bolu is what a click in the sidebar or the office does.
  useEffect(() => {
    if (!agentId || opened.current === agentId) {
      return;
    }
    opened.current = agentId;
    void (async () => {
      await chat.openByAgent(agentId);
    })();
  }, [agentId, chat]);

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

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const text = draft.trim();
    if (!text || chat.sending) {
      return;
    }
    setDraft("");
    await chat.send(text);
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex flex-wrap items-center gap-3 border-b border-bolu-border bg-bolu-surface px-6 py-3">
        <div className="min-w-0 flex-1">
          <h1 className="truncate font-display text-lg font-semibold">{title}</h1>
          <p className="truncate text-sm text-bolu-muted">
            {chat.active
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

      <div className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[260px_1fr]">
        <aside className="hidden min-h-0 overflow-y-auto border-r border-bolu-border bg-bolu-surface px-3 py-3 lg:block">
          <p className="px-1 pb-2 text-xs uppercase tracking-wide text-bolu-muted">Obrolan</p>
          {chat.conversations.length === 0 ? (
            <p className="px-1 text-sm text-bolu-muted">Belum ada obrolan.</p>
          ) : (
            <ul className="space-y-1">
              {chat.conversations.map((conversation) => (
                <li key={conversation.id}>
                  <button
                    type="button"
                    onClick={() => chat.open(conversation)}
                    className={`flex w-full items-center gap-2 rounded-xl px-2.5 py-2 text-left text-sm transition hover:bg-bolu-panel/60 ${
                      chat.active?.id === conversation.id ? "bg-bolu-panel/70" : ""
                    }`}
                  >
                    <span className="min-w-0 flex-1 truncate">
                      {conversation.title ||
                        conversation.participants.find((participant) => participant.is_agent)?.name ||
                        "Obrolan"}
                    </span>
                    <span className="shrink-0 text-xs text-bolu-muted">{conversation.message_count}</span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </aside>

        <section className="flex min-h-0 flex-col">
          {chat.error ? (
            <p role="alert" className="mx-4 mt-3 rounded-xl border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
              {chat.error}
            </p>
          ) : null}

          <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
            {chat.hasMore ? (
              <button
                type="button"
                onClick={() => void chat.loadOlder()}
                disabled={chat.loadingOlder}
                className="mx-auto mb-3 block rounded-full border border-bolu-border px-3 py-1 text-sm text-bolu-muted transition hover:bg-bolu-panel/60 disabled:opacity-50"
              >
                {chat.loadingOlder ? "Memuat…" : "Muat pesan lama"}
              </button>
            ) : null}

            {chat.messages.length === 0 && !chat.loading ? (
              <p className="mt-8 text-center text-sm text-bolu-muted">
                Belum ada pesan. Tulis sesuatu untuk mulai.
              </p>
            ) : null}

            <div className="mx-auto flex max-w-[760px] flex-col gap-3">
              {chat.messages.map((message) => (
                <MessageRow
                  key={message.message_id}
                  message={message}
                  name={nameOf(chat.active, message)}
                  face={faceOf(chat.active, message)}
                />
              ))}

              {chat.responder ? (
                <div className="flex items-center gap-2 text-sm text-bolu-muted">
                  <span className="inline-flex gap-1">
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

          <form onSubmit={submit} className="border-t border-bolu-border bg-bolu-surface px-4 py-3">
            <div className="mx-auto flex max-w-[760px] items-end gap-2">
              <textarea
                value={draft}
                onChange={(event) => setDraft(event.target.value)}
                onKeyDown={(event) => {
                  // Enter sends, Shift+Enter makes a new line: the shape everyone
                  // expects from a chat.
                  if (event.key === "Enter" && !event.shiftKey) {
                    event.preventDefault();
                    event.currentTarget.form?.requestSubmit();
                  }
                }}
                rows={1}
                placeholder={chat.active ? "Tulis pesan…" : "Pilih obrolan dulu"}
                disabled={!chat.active}
                className="max-h-40 min-h-11 flex-1 resize-y rounded-xl border border-bolu-border bg-bolu-surface px-3 py-2.5 text-[15px] disabled:opacity-60"
              />
              <button
                type="submit"
                disabled={!chat.active || chat.sending || draft.trim() === ""}
                className="h-11 shrink-0 rounded-xl bg-bolu-accent px-4 text-sm font-medium text-white transition hover:opacity-90 disabled:opacity-50"
              >
                {chat.sending ? "Mengirim…" : "Kirim"}
              </button>
            </div>
          </form>
        </section>
      </div>
    </div>
  );
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
 * faceOf is the avatar of a message's author.
 *
 * Only a Bolu gets one: the crew table is keyed by the seeded names, and asking
 * it for anything else throws, so a user message — or a Bolu renamed in the
 * registry — falls back to the generic face rather than breaking the thread.
 */
function faceOf(conversation: ChatConversation | null, message: ChatMessage): BotShape | null {
  if (!message.agent_id) {
    return null;
  }
  return botOrFallback(nameOf(conversation, message));
}

/**
 * botOrFallback returns the crew face for a name.
 *
 * A Bolu renamed in the registry is not in the seeded table, so the fallback
 * picks a member by a hash of the name: the same Bolu keeps the same face across
 * reloads and two Bolu do not look alike, without inventing a name field the
 * shape does not have.
 */
function botOrFallback(name: string): BotShape {
  try {
    return bot(name);
  } catch {
    let hash = 0;
    for (let index = 0; index < name.length; index++) {
      hash = (hash * 31 + name.charCodeAt(index)) % 1000003;
    }

    const roster = ALL_BOTS;
    const member = roster[hash % roster.length] ?? allBot("Oren");
    return withFace(member, {});
  }
}

/** MessageRow renders one message, from the user or from a Bolu. */
function MessageRow({ message, name, face }: { message: ChatMessage; name: string; face: BotShape | null }) {
  const fromAgent = Boolean(message.agent_id);
  const notice = describeResult(message);

  return (
    <article className={fromAgent ? "flex items-start gap-2.5" : "flex justify-end"}>
      {fromAgent && face ? <BotSvg bot={face} className="mt-0.5 size-9 flex-none" /> : null}

      <div className={fromAgent ? "min-w-0 max-w-[560px]" : "max-w-[78%]"}>
        {fromAgent ? (
          <p className="mb-1 text-[13px] font-semibold text-bolu-body">{name}</p>
        ) : null}

        <div
          className={
            fromAgent
              ? "rounded-[6px_20px_20px_20px] border border-bolu-border bg-bolu-surface px-4 py-2.5"
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
            <p className="mt-2 rounded-lg border border-bolu-flag/30 bg-bolu-flag/8 px-2.5 py-1.5 text-xs text-bolu-flag">
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
