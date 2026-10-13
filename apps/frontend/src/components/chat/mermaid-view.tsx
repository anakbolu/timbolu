"use client";

/**
 * The Mermaid view: a diagram, validated before it is shown.
 *
 * Mermaid fails by throwing on a malformed document, and that failure is what
 * the repair loop acts on: the error travels back as an event and the agent
 * writes the code again. A diagram that stays broken is shown as a status rather
 * than as its source, so a user never reads Mermaid in the chat.
 *
 * Loaded only in the browser: Mermaid renders into a live document, and it is
 * reached through `next/dynamic` with `ssr: false` for that reason. The library
 * itself is a static import here.
 */

import { useEffect, useRef, useState } from "react";
import mermaid from "mermaid";

import type { MermaidBlock } from "@/lib/api";

mermaid.initialize({
  startOnLoad: false,
  // A diagram in a chat is a figure, not a splash of colour.
  theme: "neutral",
  // strict keeps a diagram from reaching outside itself: no links, no scripts.
  securityLevel: "strict",
  fontFamily: "inherit",
});

export default function MermaidView({ block }: { block: MermaidBlock }) {
  return <MermaidDiagram key={block.code} block={block} />;
}

/**
 * MermaidDiagram draws one piece of Mermaid source.
 *
 * It is keyed by the code, so a regenerated diagram is a fresh mount: the
 * "rendering" state starts correct without an effect writing it, and a stale
 * render cannot land on the new source.
 */
function MermaidDiagram({ block }: { block: MermaidBlock }) {
  const container = useRef<HTMLDivElement>(null);
  // The initial state is "rendering", and a code change returns to it because the
  // component is keyed by the code below rather than by a state write here.
  const [state, setState] = useState<"rendering" | "ready" | "failed">("rendering");
  const [message, setMessage] = useState("");

  useEffect(() => {
    let disposed = false;

    void (async () => {
      try {
        // parse() is the check the architecture document asks for, and it throws
        // with the offending line, which is what makes the repair loop useful.
        await mermaid.parse(block.code);
        const { svg } = await mermaid.render(`mermaid-${uniqueID()}`, block.code);

        if (disposed || !container.current) {
          return;
        }
        container.current.innerHTML = svg;
        setState("ready");
      } catch (cause) {
        if (disposed) {
          return;
        }
        setMessage(cause instanceof Error ? cause.message : "Diagram tidak bisa dibaca.");
        setState("failed");
      }
    })();

    return () => {
      disposed = true;
    };
  }, [block.code]);

  return (
    <figure className="my-1">
      {block.title ? (
        <figcaption className="mb-1 text-sm font-semibold">{block.title}</figcaption>
      ) : null}

      {state === "failed" ? (
        <p className="rounded border border-bolu-flag/30 bg-bolu-flag/8 px-3 py-2 text-sm text-bolu-flag">
          Diagram ini belum bisa digambar. Bolu sedang memperbaikinya.
          <span className="mt-0.5 block text-xs text-bolu-muted">{message}</span>
        </p>
      ) : null}

      {state === "rendering" ? (
        <p className="text-sm text-bolu-muted">Menggambar diagram…</p>
      ) : null}

      <div ref={container} className={`overflow-x-auto ${state === "ready" ? "" : "hidden"}`} />
    </figure>
  );
}

/**
 * uniqueID gives each render its own element id.
 *
 * Mermaid writes into the document by id while it renders, so two diagrams with
 * the same id would overwrite each other.
 */
function uniqueID(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}
