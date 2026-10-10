"use client";

/**
 * The block renderers.
 *
 * A message is a list of typed blocks, and each type has exactly one component
 * here. That is the point of the block shape: the backend validates a document
 * against its schema, so a renderer receives a shape it can trust and never has
 * to defend itself against a half-valid one.
 *
 * Markdown is rendered without raw HTML: an agent writes prose, not markup, and
 * allowing it would turn every message into a script injection into the app. The
 * one block that may contain script is `html`, and it is framed from a separate
 * origin inside a sandbox.
 */

import dynamic from "next/dynamic";
import { useEffect, useMemo, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";

import {
  contentURL,
  decideDraft,
  ensureActiveWorkspace,
  type DraftBlock,
  type HTMLBlock,
  type MessageBlock,
  type TableBlock,
  type TextBlock,
} from "@/lib/api";

/**
 * The two block types that touch the document are loaded only in the browser.
 *
 * ECharts measures an element and Mermaid renders into a live document, so
 * neither can run during server rendering; the specifier is still a literal, and
 * the library is a static import inside the module it names.
 */
const ChartView = dynamic(() => import("./chart-view"), {
  ssr: false,
  loading: () => <p className="text-sm text-bolu-muted">Menyiapkan grafik…</p>,
});

const MermaidView = dynamic(() => import("./mermaid-view"), {
  ssr: false,
  loading: () => <p className="text-sm text-bolu-muted">Menyiapkan diagram…</p>,
});

/** Block renders one block, or says so when its type is one it does not know. */
export function Block({ block }: { block: MessageBlock }) {
  switch (block.type) {
    case "text":
      return <TextBlockView block={block} />;
    case "table":
      return <TableView block={block} />;
    case "draft":
      return <DraftCard block={block} />;
    case "chart":
      return <ChartView block={block} />;
    case "mermaid":
      return <MermaidView block={block} />;
    case "html":
      return <HTMLView block={block} />;
    default:
      // A block type this client does not know is shown as such rather than
      // dropped: a message that silently loses part of itself is worse.
      return (
        <p className="rounded-xl border border-dashed border-bolu-border px-3 py-2 text-sm text-bolu-muted">
          Jenis blok yang belum dikenal aplikasi ini.
        </p>
      );
  }
}

/** TextBlockView renders Markdown, without raw HTML. */
function TextBlockView({ block }: { block: TextBlock }) {
  return (
    <div className="bolu-markdown text-[15px] leading-relaxed">
      <Markdown
        remarkPlugins={[remarkGfm]}
        // Raw HTML from an agent is dropped rather than sanitised: prose is what
        // a text block is for, and the interactive block exists for the rest.
        skipHtml
        components={{
          a: ({ children, href }) => (
            <a href={href} target="_blank" rel="noreferrer noopener" className="underline">
              {children}
            </a>
          ),
          table: ({ children }) => (
            <div className="my-2 overflow-x-auto">
              <table className="w-full border-collapse text-sm">{children}</table>
            </div>
          ),
          th: ({ children }) => (
            <th className="border-b border-bolu-border px-2 py-1 text-left font-semibold">
              {children}
            </th>
          ),
          td: ({ children }) => <td className="border-b border-bolu-line px-2 py-1">{children}</td>,
        }}
      >
        {block.markdown}
      </Markdown>
    </div>
  );
}

/** TableView renders a table block: the columns and rows the schema validated. */
function TableView({ block }: { block: TableBlock }) {
  return (
    <figure className="my-1 overflow-x-auto">
      {block.title ? (
        <figcaption className="mb-1 text-sm font-semibold">{block.title}</figcaption>
      ) : null}
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr>
            {block.columns.map((column, index) => (
              <th
                key={`${column}-${index}`}
                className="border-b border-bolu-border px-2 py-1 font-semibold"
                style={{ textAlign: block.align?.[index] ?? "left" }}
              >
                {column}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {block.rows.map((row, rowIndex) => (
            <tr key={rowIndex}>
              {row.map((cell, cellIndex) => (
                <td
                  key={cellIndex}
                  className="border-b border-bolu-line px-2 py-1"
                  style={{ textAlign: block.align?.[cellIndex] ?? "left" }}
                >
                  {cell}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </figure>
  );
}

const DRAFT_STATUS: Record<string, { label: string; className: string }> = {
  pending: { label: "Menunggu kamu", className: "bg-bolu-flag/12 text-bolu-flag" },
  approved: { label: "Disetujui", className: "bg-bolu-live/12 text-bolu-lunas" },
  revise: { label: "Minta revisi", className: "bg-bolu-panel text-bolu-body" },
  sent: { label: "Terkirim", className: "bg-bolu-live/12 text-bolu-lunas" },
  canceled: { label: "Dibatalkan", className: "bg-bolu-panel text-bolu-rest" },
};

/**
 * DraftCard is the approval card.
 *
 * It offers the decision inline, because a person decides where they are already
 * reading: sending them to another page to approve what the thread just showed
 * them is how a wrong action gets approved out of impatience. The card is one
 * source of truth with the dashboard — both read the same draft and both write
 * through the same endpoint.
 *
 * The controls appear only while a decision is still open. Once one is made the
 * card is a record, and the thread is rewritten in place by the runtime, so a
 * reload shows what happened rather than a stale button.
 */
function DraftCard({ block }: { block: DraftBlock }) {
  const [note, setNote] = useState("");
  const [revising, setRevising] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The decision this card made, if any. It is a *remembered answer* rather than
  // a copy of the prop: the block is what the server says, and the two are
  // combined at render time. Mirroring the prop into state would need an effect
  // and would show a stale status between the decision and the rewrite.
  const [decided, setDecided] = useState<DraftBlock["status"] | null>(null);

  const serverStatus = block.status ?? "pending";
  const status = decided ?? serverStatus;
  const current = DRAFT_STATUS[status] ?? DRAFT_STATUS.pending;

  // Only the server's own `pending` is open. Once this card answered, the
  // controls are gone even before the thread is rewritten, so a double click
  // cannot send a second decision.
  const open = serverStatus === "pending" && decided === null;

  async function decide(decision: "approve" | "revise" | "cancel") {
    // Resolved rather than only read, so a decision is not refused on a device
    // that has no workspace stored yet.
    const workspaceId = await ensureActiveWorkspace();
    if (!workspaceId) {
      setError("Pilih workspace dulu.");
      return;
    }

    setBusy(true);
    setError(null);

    const result = await decideDraft(workspaceId, block.draft_id, decision, note.trim());
    setBusy(false);

    if (!result.ok) {
      // A second decision is refused by the backend, which is the point: the
      // first answer stands. Saying so is better than showing the button again.
      setError(
        result.code === "draft_already_decided"
          ? "Draf ini sudah kamu putuskan."
          : result.message,
      );
      return;
    }

    setDecided(result.data.status);
    setRevising(false);
    setNote("");
  }

  return (
    <div className="my-1 rounded-2xl border border-bolu-border bg-bolu-surface px-4 py-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="text-xs uppercase tracking-wide text-bolu-muted">
            {block.action_kind ?? "Draf"}
          </p>
          <p className="truncate font-semibold">{block.title ?? "Draf menunggu persetujuan"}</p>
        </div>
        <span className={`shrink-0 rounded-full px-2.5 py-1 text-xs font-medium ${current.className}`}>
          {current.label}
        </span>
      </div>

      {block.summary ? <p className="mt-1.5 text-sm text-bolu-body">{block.summary}</p> : null}

      {block.fields && block.fields.length > 0 ? (
        <dl className="mt-2 space-y-1 text-sm">
          {block.fields.map(([label, value], index) => (
            <div key={`${label}-${index}`} className="flex gap-2">
              <dt className="w-28 shrink-0 text-bolu-muted">{label}</dt>
              <dd className="min-w-0 break-words">{value}</dd>
            </div>
          ))}
        </dl>
      ) : null}

      {/* A held action says why and until when, which is what answers "why has
          this not gone out" without leaving the thread. */}
      {block.blocked_reason ? (
        <p className="mt-2 rounded-xl bg-bolu-panel px-3 py-2 text-sm text-bolu-body">
          {block.blocked_reason}
          {block.not_before ? ` · ${new Date(block.not_before).toLocaleString("id-ID")}` : null}
        </p>
      ) : null}

      {block.revision && block.revision > 0 ? (
        <p className="mt-2 text-xs text-bolu-muted">
          Sudah direvisi {block.revision}×.
        </p>
      ) : null}

      {open ? (
        <div className="mt-3 space-y-2">
          {revising ? (
            <label className="block">
              <span className="text-sm text-bolu-muted">Apa yang perlu diubah?</span>
              <textarea
                value={note}
                onChange={(event) => setNote(event.target.value)}
                rows={2}
                className="mt-1 w-full rounded-xl border border-bolu-border bg-bolu-page px-3 py-2 text-sm"
                placeholder="Mis. penerimanya salah, harusnya Budi"
              />
            </label>
          ) : null}

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              disabled={busy}
              onClick={() => decide("approve")}
              className="rounded-full bg-bolu-accent px-3.5 py-1.5 text-sm font-medium text-white disabled:opacity-50"
            >
              {busy ? "Menyimpan…" : "Setujui"}
            </button>

            {revising ? (
              <>
                <button
                  type="button"
                  disabled={busy || note.trim() === ""}
                  onClick={() => decide("revise")}
                  className="rounded-full border border-bolu-border px-3.5 py-1.5 text-sm font-medium disabled:opacity-50"
                >
                  Kirim revisi
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => {
                    setRevising(false);
                    setNote("");
                  }}
                  className="rounded-full px-3 py-1.5 text-sm text-bolu-muted"
                >
                  Batal
                </button>
              </>
            ) : (
              <button
                type="button"
                disabled={busy}
                onClick={() => setRevising(true)}
                className="rounded-full border border-bolu-border px-3.5 py-1.5 text-sm font-medium disabled:opacity-50"
              >
                Minta revisi
              </button>
            )}

            <button
              type="button"
              disabled={busy}
              onClick={() => decide("cancel")}
              className="rounded-full px-3 py-1.5 text-sm text-bolu-muted disabled:opacity-50"
            >
              Batalkan
            </button>
          </div>
        </div>
      ) : (
        <a
          href="/dashboard"
          className="mt-2.5 inline-block text-sm font-medium text-bolu-accent hover:underline"
        >
          Buka di Dasbor →
        </a>
      )}

      {error ? <p className="mt-2 text-sm text-bolu-flag">{error}</p> : null}
    </div>
  );
}

/**
 * HTMLView frames one sandboxed document from the content origin.
 *
 * Three things together make agent-written script safe to run:
 * - the sandbox omits allow-same-origin, so the document has an opaque origin and
 *   cannot read the application's cookies, storage, or DOM;
 * - the content origin serves it under a policy with `connect-src 'none'`, so it
 *   cannot reach the network even from inside the frame;
 * - the only message accepted from it is a resize, so it cannot drive the app.
 */
function HTMLView({ block }: { block: HTMLBlock }) {
  const frame = useRef<HTMLIFrameElement>(null);
  const [height, setHeight] = useState(320);
  const [expanded, setExpanded] = useState(false);

  const source = useMemo(() => contentURL(block.content_ref), [block.content_ref]);

  useEffect(() => {
    function onMessage(event: MessageEvent) {
      // The source check is what makes the channel narrow: only the frame this
      // component rendered may resize it, and only with a resize message.
      if (event.source !== frame.current?.contentWindow) {
        return;
      }

      const data: unknown = event.data;
      if (typeof data !== "object" || data === null) {
        return;
      }
      const candidate = data as { type?: unknown; height?: unknown };
      if (candidate.type !== "resize" || typeof candidate.height !== "number") {
        return;
      }

      // The height is clamped: a document claiming to be ten thousand pixels
      // tall must not be able to push the rest of the conversation away.
      setHeight(Math.min(Math.max(candidate.height, 120), 1200));
    }

    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  return (
    <figure className="my-1">
      <div className="mb-1 flex flex-wrap items-baseline justify-between gap-2">
        <figcaption className="text-sm font-semibold">
          {block.title ?? "Konten interaktif"}
          <span className="ml-2 text-xs font-normal text-bolu-muted">
            Konten interaktif dibuat Bolu
          </span>
        </figcaption>
        <button
          type="button"
          onClick={() => setExpanded((value) => !value)}
          className="rounded-lg border border-bolu-border px-2.5 py-1 text-xs transition hover:bg-bolu-panel/60"
        >
          {expanded ? "Perkecil" : "Layar penuh"}
        </button>
      </div>

      <iframe
        ref={frame}
        title={block.title ?? "Konten interaktif dibuat Bolu"}
        src={source}
        // No allow-same-origin, no allow-forms, no allow-popups: the document
        // gets a script context and nothing else.
        sandbox="allow-scripts"
        referrerPolicy="no-referrer"
        loading="lazy"
        className="w-full rounded-xl border border-bolu-border bg-white"
        style={{ height: expanded ? "80vh" : height }}
      />

      {block.caption ? <p className="mt-1 text-xs text-bolu-muted">{block.caption}</p> : null}
    </figure>
  );
}
