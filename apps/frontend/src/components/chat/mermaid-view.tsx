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

import { useResolvedTheme } from "@/hooks/use-theme";
import type { MermaidBlock } from "@/lib/api";
import type { ResolvedTheme } from "@/lib/theme";

/**
 * configure tells Mermaid what colour to draw a diagram in.
 *
 * Mermaid writes its own `<style>` into the SVG it produces, so it does not read
 * this app's tokens and cannot be left to the cascade: without this, a diagram
 * arrives wearing Mermaid's own greys on this product's page, and its dark theme
 * is a different dark from ours. The palette below is read off the document at
 * draw time — the same approach the chart adapter takes — so the diagram is the
 * product's palette rather than a second one beside it.
 *
 * Nothing here is conditional on which theme is active: the tokens already are
 * the answer, and reading them after the class has been applied is what makes one
 * function enough for both.
 */
function configure(): void {
  const style = getComputedStyle(document.documentElement);
  const read = (name: string) => style.getPropertyValue(name).trim();

  const ink = read("--color-bolu-ink");
  const body = read("--color-bolu-body");
  const muted = read("--color-bolu-muted");
  const border = read("--color-bolu-border");
  const panel = read("--color-bolu-panel");
  const surface = read("--color-bolu-surface");
  const hover = read("--color-bolu-hover");
  const accent = read("--color-bolu-accent");

  mermaid.initialize({
    startOnLoad: false,
    // "base" is the only theme that honours `themeVariables`: the named themes
    // each carry their own palette and overwrite the variables given here, which
    // is how a diagram ends up in Mermaid's dark rather than in this product's.
    theme: "base",
    // strict keeps a diagram from reaching outside itself: no links, no scripts.
    securityLevel: "strict",
    fontFamily: "inherit",
    themeVariables: {
      background: surface,
      // A node is a surface, not a panel: the diagram sits inside a message
      // bubble, and a box drawn in the bubble's own colour would have no edge.
      primaryColor: surface,
      primaryTextColor: ink,
      primaryBorderColor: border,
      secondaryColor: panel,
      secondaryTextColor: body,
      secondaryBorderColor: border,
      tertiaryColor: hover,
      tertiaryTextColor: body,
      tertiaryBorderColor: border,
      // The lines and the arrowheads.
      lineColor: muted,
      textColor: body,
      // A cluster is a grouping, so it is drawn as a panel behind its members.
      clusterBkg: panel,
      clusterBorder: border,
      // The one colour that means "this product" in a diagram: the notes and the
      // active edges, in the same blue the rest of the app uses.
      noteBkgColor: panel,
      noteTextColor: body,
      noteBorderColor: border,
      edgeLabelBackground: surface,
      // Mermaid's own blue for an active or highlighted edge.
      activeTaskBkgColor: accent,
      activeTaskBorderColor: accent,
      titleColor: ink,
    },
  });
}

export default function MermaidView({ block }: { block: MermaidBlock }) {
  const theme = useResolvedTheme();
  // Keyed by the theme as well as the code: Mermaid bakes its colours into the
  // markup it returns, so a theme change has to draw the diagram again rather
  // than restyle it.
  return <MermaidDiagram key={`${theme}:${block.code}`} block={block} theme={theme} />;
}

/**
 * MermaidDiagram draws one piece of Mermaid source.
 *
 * It is keyed by the code, so a regenerated diagram is a fresh mount: the
 * "rendering" state starts correct without an effect writing it, and a stale
 * render cannot land on the new source.
 */
function MermaidDiagram({ block, theme }: { block: MermaidBlock; theme: ResolvedTheme }) {
  const container = useRef<HTMLDivElement>(null);
  // The initial state is "rendering", and a code change returns to it because the
  // component is keyed by the code below rather than by a state write here.
  const [state, setState] = useState<"rendering" | "ready" | "failed">("rendering");
  const [message, setMessage] = useState("");

  useEffect(() => {
    let disposed = false;

    void (async () => {
      try {
        configure();
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
  }, [block.code, theme]);

  return (
    <figure className="my-1">
      {block.title ? (
        <figcaption className="mb-1 text-sm font-semibold">{block.title}</figcaption>
      ) : null}

      {state === "failed" ? (
        <p className="rounded-md border border-bolu-flag/30 bg-bolu-flag/8 px-3 py-2 text-sm text-bolu-flag">
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
