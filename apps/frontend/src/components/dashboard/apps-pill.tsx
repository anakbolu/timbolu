"use client";

/**
 * The apps pill, and the tiles it draws.
 *
 * Pengaturan and Integrasi name a connected application by its own mark rather
 * than by a paragraph about it: one pill carries the label and the marks of what
 * is connected, and the panel behind it is where the detail lives. The tile table
 * is the artboard's own (`APPS` in design/keluarga-bolu-dashboard.html), so an
 * application the design did not carry still gets a tile derived from its name
 * instead of a blank square.
 */

import { ChevronRight, Plug } from "lucide-react";

/** AppMeta is how one application is drawn: its monogram, colour, name, category. */
type AppMeta = { mono: string; tile: string; name: string; cat: string };

/**
 * TILES is the artboard's application table. `whatsapp` is the design's `wa`,
 * spelled the way the hub's own records spell it.
 */
const TILES: Record<string, AppMeta> = {
  gmail: { mono: "Gm", tile: "#C5362B", name: "Gmail", cat: "Email & kalender" },
  gcal: { mono: "Ca", tile: "#2F6FD6", name: "Google Calendar", cat: "Email & kalender" },
  outlook: { mono: "Ou", tile: "#0F5FA8", name: "Outlook", cat: "Email & kalender" },
  drive: { mono: "Dr", tile: "#1A7F45", name: "Google Drive", cat: "Dokumen & file" },
  sheets: { mono: "Sh", tile: "#127A43", name: "Google Sheets", cat: "Dokumen & file" },
  dropbox: { mono: "Db", tile: "#0050D1", name: "Dropbox", cat: "Dokumen & file" },
  notion: { mono: "No", tile: "#2B2840", name: "Notion", cat: "Dokumen & file" },
  whatsapp: { mono: "WA", tile: "#1E7D4A", name: "WhatsApp Business", cat: "Chat" },
  slack: { mono: "Sl", tile: "#611F69", name: "Slack", cat: "Chat" },
  jira: { mono: "Ji", tile: "#0C55C4", name: "Jira", cat: "Proyek & developer" },
  github: { mono: "GH", tile: "#24292F", name: "GitHub", cat: "Proyek & developer" },
  trello: { mono: "Tr", tile: "#0067A3", name: "Trello", cat: "Proyek & developer" },
  tokopedia: { mono: "To", tile: "#2E8B3E", name: "Tokopedia", cat: "Toko online" },
  shopee: { mono: "Sp", tile: "#C9401F", name: "Shopee", cat: "Toko online" },
  instagram: { mono: "IG", tile: "#A72D72", name: "Instagram", cat: "Media sosial" },
  midtrans: { mono: "Mi", tile: "#002855", name: "Midtrans", cat: "Keuangan" },
};

/** tileFor resolves an application's tile, falling back to its own name. */
export function tileFor(app: string): AppMeta {
  const known = TILES[app];
  if (known) return known;
  const words = app.split(/[-_\s.]+/).filter(Boolean);
  const mono = words.length > 1 ? words[0][0] + words[1][0] : app.slice(0, 2);
  return { mono: mono.toUpperCase(), tile: "#5E5B70", name: app, cat: "Lainnya" };
}

/** AppTile is one application's monogram square. */
export function AppTile({
  app,
  size = 40,
  className = "",
}: {
  app: string;
  size?: number;
  className?: string;
}) {
  const { mono, tile } = tileFor(app);
  return (
    <span
      aria-hidden
      className={`flex flex-none items-center justify-center rounded-md font-display font-bold text-white ${className}`}
      style={{ width: size, height: size, backgroundColor: tile, fontSize: size * 0.36 }}
    >
      {mono}
    </span>
  );
}

/**
 * ConnectedTiles draws what is connected as overlapping marks, capped so the pill
 * keeps its width: past the cap the count says the rest.
 */
export function ConnectedTiles({
  apps,
  size = 26,
  max = 4,
}: {
  apps: string[];
  size?: number;
  max?: number;
}) {
  const shown = apps.slice(0, max);
  const rest = apps.length - shown.length;

  return (
    <span className="flex flex-none items-center">
      {shown.map((app, index) => (
        <AppTile
          key={`${app}-${index}`}
          app={app}
          size={size}
          className={`ring-2 ring-bolu-surface ${index === 0 ? "" : "-ml-2"}`}
        />
      ))}
      {rest > 0 ? (
        <span
          aria-hidden
          className="-ml-2 flex flex-none items-center justify-center rounded-md bg-bolu-panel font-display font-bold text-bolu-ink ring-2 ring-bolu-surface"
          style={{ width: size, height: size, fontSize: size * 0.34 }}
        >
          +{rest}
        </span>
      ) : null}
    </span>
  );
}

/**
 * AppsPill is the one button these screens name a connection with: the label,
 * then the marks of what is behind it.
 */
export function AppsPill({
  label,
  apps,
  onClick,
  expanded = false,
  className = "",
}: {
  label: string;
  apps: string[];
  onClick: () => void;
  expanded?: boolean;
  className?: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-expanded={expanded}
      aria-haspopup="dialog"
      className={`flex min-h-[46px] cursor-pointer items-center gap-2.5 rounded-full border border-bolu-chip bg-bolu-surface px-4 text-[15px] font-semibold text-bolu-ink hover:border-bolu-accent ${className}`}
    >
      <span className="truncate">{label}</span>
      {apps.length === 0 ? (
        <Plug size={18} strokeWidth={2.2} aria-hidden className="flex-none text-bolu-muted" />
      ) : (
        <ConnectedTiles apps={apps} />
      )}
    </button>
  );
}

/** PillLink is one destination, in the same pill language, without the marks. */
export function PillLink({
  href,
  label,
  onClick,
  className = "",
}: {
  href: string;
  label: string;
  onClick?: () => void;
  className?: string;
}) {
  return (
    <a
      href={href}
      onClick={onClick}
      className={`flex min-h-[46px] items-center gap-2.5 rounded-full border border-bolu-border bg-bolu-surface px-4 text-[15px] font-semibold text-bolu-ink no-underline hover:border-bolu-accent hover:text-bolu-ink ${className}`}
    >
      <span className="truncate">{label}</span>
      <ChevronRight size={16} strokeWidth={2.5} aria-hidden className="ml-auto flex-none text-bolu-muted" />
    </a>
  );
}
