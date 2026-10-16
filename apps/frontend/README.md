# Frontend

Next.js (App Router) + TypeScript + Tailwind v4. One app serves four surfaces:
the landing page, the auth screens, the workspace, and the operator surface under
`/admin`.

There is no `tailwind.config`. The design system lives in
[`src/app/globals.css`](src/app/globals.css): an `@theme` block of tokens, a
`@layer base` block, the ported motion, and the Bolu character's own classes.

## Theming

The product has three themes — light, dark, and "follow the system" — and the
choice is a class on `<html>`, not the media query alone:

- `.dark` is what Tailwind's `dark:` variant is bound to (`@custom-variant dark`
  at the top of `globals.css`), so a person who wants the dark theme on a machine
  that reports light can have it.
- `src/lib/theme.ts` owns the choice: it reads and writes `bolu_theme` in
  `localStorage`, resolves `system` against `prefers-color-scheme`, and paints the
  class, the `data-theme` attribute, and the browser's own `theme-color`.
- `THEME_INIT_SCRIPT` in that module is the same decision written for the HTML
  parser. It is inlined into `<head>` by `src/app/layout.tsx` and runs before the
  first paint, which is the only way a stored theme can be applied without a flash
  of the other one — `localStorage` is not readable while the server renders.
- `src/components/ui/theme-toggle.tsx` is the control, mounted in the workspace
  sidebar, the admin header, the landing nav (and its phone drawer), the auth
  screens, and as a card on Pengaturan. It reads through `useTheme`, so two
  toggles on one page cannot disagree.

`theme-color` is deliberately not declared in the `viewport` export. A declared
one is rendered by React, which re-inserts it after every navigation — landing
after the inline script had already decided — so the script creates and owns that
tag instead.

## Colour

Everything drawn by CSS uses a token. A component never names a colour, and a
token never names a theme, which is what lets one set of screens be correct in
both.

- `--color-bolu-*` is the product palette: `bg`, `ink`, `muted`, `body`, `border`,
  `panel`, `surface`, `line`, `chip`, `track`, `accent`, and the semantic
  `lunas`/`belum`/`flag`/`live`/`rest`.
- `.dark` re-derives that palette rather than inverting it. The ladder keeps its
  order — `bg` under `panel` under `surface` — and the semantic colours move the
  other way, because a green that reads on white is too dark to read on black.
- `--color-bolu-solid` / `--color-bolu-solid-ink` are the one pair that exists
  because `ink` does two jobs: it is the text colour (which inverts) and it is the
  fill of a lit nav row, a primary button, and an outgoing chat bubble (which must
  not stay dark on a dark page). A fill that carries its own ink uses this pair.
- The shadcn tokens under `components/ui/` are a bridge, not a second palette:
  each one is defined as a `var()` of a Bolu token, which is why a registry
  component looks like the rest of the product in either theme.

Three things do **not** follow the theme, and each is deliberate:

| Stays fixed | Why |
| --- | --- |
| `lib/crew.ts` colours and tints, `characters.tsx`, `bolu.tsx`, `bot-svg.tsx` | A Bolu's colour is who it is, not a surface. Text on a tint uses `--color-bolu-on-tint`, because a tint is always light. |
| The application tiles in `integrations-view.tsx`, the Google mark, the auth scene's mock room | Third-party brand colours and an illustration. |
| The QR code in `whatsapp-panel.tsx` | A scanner needs dark modules on a light field. |

Two renderers paint outside the cascade and cannot read a token, so each reads the
palette off the document at draw time and redraws when the theme changes:
`components/chat/echarts-adapter.ts` (`readChartPalette`) for the charts, and
`components/chat/mermaid-view.tsx` for the diagrams. Mermaid is initialised with
`theme: "base"`, which is the only one of its themes that honours
`themeVariables` — the named ones overwrite them.

## Commands

```bash
npm ci
npm run dev     # next dev
npm run build   # next build
npm run lint    # eslint
```

`NEXT_PUBLIC_API_URL` and `NEXT_PUBLIC_CONTENT_ORIGIN` are inlined at build time;
see [`docs/deployment.md`](../../docs/deployment.md).

The registry components under `components/ui/` are vendored, not installed, and
this repo edits them: the radii and the palette are narrowed to this product's
scale in `globals.css`, and a change that belongs to the theme belongs there
rather than in the component.
