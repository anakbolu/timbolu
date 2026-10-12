# Design sources

Everything in this folder is a **reference**, not production code. The mockups
carry the design's precise values — colours, sizes, spacing, radii, layout — and
the frontend replicates them in its own components rather than copying the
markup. When a port and a reference disagree, the reference wins.

## Artboards

| File | What it is | Ported into |
| --- | --- | --- |
| `keluarga-bolu-landing.html` | Landing page | `apps/frontend/src/components/bolu/*` |
| `keluarga-bolu-dashboard.html` | Dashboard | `apps/frontend/src/components/chat/*`, `components/dashboard/*` |
| `dashboard-logic.js` | Dashboard artboard logic | the state in the dashboard components |
| `dashboard-README.md` | Notes on the dashboard export | — |

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
