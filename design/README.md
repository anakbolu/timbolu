# Design sources

Everything in this folder is a **reference**, not production code. The mockups
carry the design's precise values — colours, sizes, spacing, radii, layout — and
the frontend replicates them in its own components rather than copying the
markup. When a port and a reference disagree, the reference wins.

## Artboards

| File | What it is | Ported into |
| --- | --- | --- |
| `timbolu-landing.html` | Landing page | `apps/frontend/src/components/bolu/*` |
| `timbolu-dashboard.html` | Dashboard: five views (Dasbor, Rutinitas, Kantor Tim Bolu, Integrasi, Pengaturan) plus the chat thread | `apps/frontend/src/components/dashboard/*`, `components/chat/*` |
| `dashboard-README.md` | Notes on the dashboard export | — |

`timbolu-dashboard.html` is a Design Component: an `<x-dc>` template plus a
small logic class, rendered by `support.js` with React from `vendor/`. Serve the
folder (`python3 -m http.server`) and open it; some browsers block the scripts over
`file://`. Its inline `style="…"` attributes and its `<helmet><style>` block are
where the design's values live, so a port is checked against those rather than
against the shape of the markup — the artboard draws everything into one column of
`div`s, and the app keeps each screen on its own route.

The five views are switched by the sidebar; there is no separate URL per view in
the artboard, while the app gives each screen its own route. The artboard's own
logic class — the crew shapes, the application tiles and the draft copy — is inline
in the file, which is where the port reads it from.

## `refs/`

Source fragments and earlier exports the ports were taken from.

| File | What it is | Ported into |
| --- | --- | --- |
| `data.js` | Crew data: shapes, mouths, members, flow, copy | `apps/frontend/src/lib/crew.ts` |
| `anims.css` | Bob/blink/glance keyframes | the motion block in `apps/frontend/src/app/globals.css` |
| `template.html` | Landing artboard template | `apps/frontend/src/components/bolu/*` |
| `copy.txt` | Landing copy, verbatim | the components' text |
| `karakter-bolu-ekspresif.html` | Bolu the character as a generator: 16 body shapes, 10 expressions, 12 colours, feet, and the face props | `apps/frontend/src/components/bolu/characters.tsx` and `bolu.tsx` |

`characters.tsx` holds the shapes, expressions and palette; `bolu.tsx` renders
them. Live previews of either the artboards or the generator are separate
exported pages, so the `.html` files here are the only place their values live.

## Dark theme

None of these artboards has a dark variant: they are the light theme only, and the
values above are the light values. The dark theme in `apps/frontend/src/app/globals.css`
is therefore **not a port** — it is derived from this palette by keeping each family's
hue and moving it along the ladder, so "the reference wins" does not apply to it.
A change to a light value here still has to be reflected in the `.dark` block by hand,
because nothing generates one from the other.
