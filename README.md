# Bolu (Employee Bot)

Bots that do human tasks. Setiap bot punya **komputer virtual** dan **sesi persisten** — bukan sekadar chat.

## Product vision

Bolu adalah platform bot yang mengerjakan pekerjaan manusia. Setiap Bolu adalah satu karakter bulat yang lucu — dua mata dan satu mulut — dengan komputer sendiri.

**Core**

- Login owner perusahaan + onboarding (tujuan bisnis, siapa mereka).
- Satu user bisa punya banyak perusahaan (1 user → many companies).
- Satu perusahaan bisa punya banyak bot (1 company → many bots), dan **setiap bot = satu komputer**.
- Bot berkolaborasi dalam grup dan berbagi satu workspace.
- Manusia memegang pintu keluar: semua aksi ke pihak luar lewat persetujuan (default draf).

**Bot traits**

- Karakter bulat lucu: 2 mata + mulut.
- Catatan personal dan catatan global.
- Auto-learn perilaku user.
- Akses secrets / environment.
- Komputer, terminal, dan browser — dengan persistence.

**Billing**

Indonesia dulu: transfer bank / virtual account. **Tidak ada Stripe.**

## Monorepo map

```
.
├── apps/
│   ├── backend/       # Go modular monolith — Fiber, pgx + sqlc, Temporal, Zap
│   ├── whatsapp-gw/   # Go — the WhatsApp gateway: whatsmeow, sessions, media
│   └── frontend/      # Next.js (App Router) + TypeScript + Tailwind v4
├── packages/          # the shared module both Go processes import
├── docs/
│   ├── adr/           # architecture decision records
│   ├── testing.md     # test layers and CI gates
│   ├── deployment.md  # the images, the migration step, and the environments
│   ├── e2e-mvp.md     # the MVP flow's end-to-end scenario and its measured cost per task
│   ├── riset-tool-kantor.md # the office tools offices actually use, and which to connect next
│   └── riset-heartbeat.md   # the five heartbeats Bolu already has, and where they lie
├── go.work            # the three-module dev workspace
├── docker-compose.yml
├── Makefile
└── .env.example
```

The Go code is three modules. `apps/backend` is the product: workspaces, agents, tasks,
approvals, and the WhatsApp *records* — who is linked, who answers, what was sent. `apps/whatsapp-gw`
is the gateway: it owns the WhatsApp socket (whatsmeow), the sealed device state, the fleet
directory, and the media bytes. They are separate processes that talk over two internal HTTP seams —
the API tells the gateway to send or link, and the gateway tells the API what arrived and what left —
and neither imports the other. `packages/` is the code they share: the WhatsApp contract, the two
seams, and the platform libraries (config, database, redis, kms, crypto, storage, logger, telemetry).

Backend layout, dependency rules, and commands: [`apps/backend/README.md`](apps/backend/README.md).
Gateway layout: [`apps/whatsapp-gw/README.md`](apps/whatsapp-gw/README.md).

## How to run

```bash
cp .env.example .env
make up
```

### Trying it by hand

`make up` starts the product but gives it no model provider, so the first task fails with "no provider
is configured" — and the catalogue has to be filled in through the API, because maintaining it is a
screen the product does not have yet. `make demo` closes both gaps:

```bash
make demo        # a stack you can click through
```

It starts a model stub and a mailbox stub on the host, points the backend at them, and seeds a
workspace you can use immediately — `demo@bolu.local` / `Rahasia123!`, with a sales Bolu, five
catalogue products, the workspace memory, and Gmail connected through its own consent flow. It prints
what to try and what each step should show. `make demo-stop` stops what it started.

The answers come from a scripted stub rather than a model, so what you are trying is the product —
catalogue prices, the draft, the approval, the audit trail, the usage screen — and not the model's
judgement. For a real model, fill in `LLM_DEFAULT_*` in `.env` and run `make up` as usual.

### The end-to-end checks

They run against the same stack, each one stopping the compose workers first so the run's task is not
executed by another process's model configuration:

```bash
docker stop bolu-api bolu-agent-worker bolu-integration-worker
make smoke-task && make smoke-approval && make smoke-order
make measure-orders   # what one order costs, measured twenty times from usage_ledger
make gate-report      # the numbers Gerbang 1 is decided on, read from the database
docker start bolu-api bolu-agent-worker bolu-integration-worker
```

`make smoke-order` is the MVP flow: a customer's message becomes an order priced from the
workspace's own catalogue, and the invoice that follows is a draft a person approves.
`make measure-orders` runs that flow twenty times and reads the cost back from `usage_ledger`, which
is where the numbers in [`docs/e2e-mvp.md`](docs/e2e-mvp.md) come from.

`make up` starts Postgres (with pgvector), Redis, Temporal and its UI, runs the
migrations, then starts the API, both Temporal workers, the WhatsApp gateway, and the frontend.

| Service      | URL                              |
| ------------ | -------------------------------- |
| Frontend     | http://localhost:3000            |
| Backend      | http://localhost:8080/health     |
| Readiness    | http://localhost:8080/ready      |
| Gateway      | http://localhost:8090/health     |
| Temporal UI  | http://localhost:8233            |
| Postgres     | `localhost:5432` (`employeebot` / `employeebot_secret` / `employeebot`) |

The API's internal seam listener — the one the gateway calls — is on `:8091` and is never published.

### Running without Docker

```bash
make frontend   # cd apps/frontend && npm run dev
make backend    # cd apps/backend && go run ./cmd/api
make wa-gateway # cd apps/whatsapp-gw && go run ./cmd/wa-gateway
```

Install frontend dependencies inside `apps/frontend` (`npm ci`) — there is no
root `package.json`, so `npm install` at the repo root does nothing.

## Make targets

| Target                        | Description                                          |
| ----------------------------- | ---------------------------------------------------- |
| `up` / `down` / `logs`        | Manage the local stack                               |
| `frontend` / `backend` / `wa-gateway` | Run one process on the host                   |
| `test` / `cover`              | Every module's suite, and the tests plus each 80% coverage gate |
| `test-backend` / `test-gateway` / `test-packages` | One module's suite        |
| `lint` / `archcheck`          | golangci-lint per module, and the backend's dependency rules |
| `sqlc` / `mocks`              | Regenerate the query code and the domain mocks, per module |
| `migrate-up` / `-down`        | Apply or revert migrations against `$DATABASE_URL`   |
| `ci`                          | Run the same gates as CI locally                     |
| `help`                        | List every target                                    |

## Roadmap

The backlog is tracked as epics and their tasks in GitHub issues: **EPIC 1–14**,
each epic being a sub-issue tree whose tasks are worked in order. The legacy
issue set (#1–#11) is mapped onto those epics in comments on each issue.

The MVP flow's end-to-end scenario — what it proves, the test data, and the steps with their
expected results — is [`docs/e2e-mvp.md`](docs/e2e-mvp.md); `make smoke-order` runs the first eight
steps of it against the compose stack.

Decisions that shape the infrastructure live in [`docs/adr`](docs/adr):
Temporal Cloud vs self-host and the data region are recorded in
[ADR 0001](docs/adr/0001-temporal-dan-region.md), and the UU PDP decisions —
where data is stored, who processes it, and how a deletion request is executed —
in [ADR 0002](docs/adr/0002-kepatuhan-uu-pdp.md). What customers are shown is
[`docs/kebijakan-data.md`](docs/kebijakan-data.md): retention in plain words and the
erasure flow, which is also a screen at `/settings/data`.

## Stack

- **Backend:** Go 1.26, Fiber v2, pgx + sqlc, golang-migrate, Temporal,
  Redis, Zap, PostgreSQL 16 + pgvector.
- **Frontend:** Next.js 16 (App Router) + TypeScript, Tailwind CSS v4, animasi
  murni CSS/SVG — tanpa file gambar, tanpa library animasi.
- **Gateway:** Go 1.26, whatsmeow, Redis lease, object storage for media,
  PostgreSQL for the sealed sessions.
- **Local infra:** Docker Compose (`postgres`, `redis`, `temporal`, `migrate`,
  `api`, `agent-worker`, `integration-worker`, `wa-gateway`, `frontend`).

Frontend routes today: `/` (the marketing page — a hero, the crew, how it works, a
look at the workspace, the FAQ, and the closing call to action) and `/pricing` (the
three packages, with the same prices and member caps the `plans` table holds, plus the
note that payment is by bank transfer or virtual account rather than Stripe — reached
from the navigation, which it was not before). The signed-in workspace lives under
`apps/frontend/src/app/(workspace)`:
`/dashboard` (the front door — the roster, the live activity feed, the drafts waiting,
and the week's finished work), `/routines` (the routines screen: the form
that sets a cadence, the routines per Bolu with their next run in the workspace's own
zone, the pause switch, and the run history), `/dashboard/integrations` (the hub: connected
accounts, grants per Bolu, MCP servers), `/settings` (workspace profile, working
hours, approval policies), `/settings/team`, `/settings/model`, `/chat` (one thread
rather than a thread list — the sidebar's roster and groups are the way in, and a
link carries the Bolu or the group in the query), `/bolu/new` (the one screen that
makes a Bolu: the body it is drawn as, its name, and its job), `/groups`, `/dashboard/usage` (what the
workspace spent, read three ways over one window, with the tasks that need a person),
and `/tasks` with `/tasks/[taskID]` (the task history, and one task's rounds and
hand-offs). The operator surface lives outside that group, under `/admin`, because it
belongs to the deployment rather than to a workspace: `/admin/dashboard` (the account
counts) and `/admin/users` (every account, its platform role, and the one button that
changes it) — no roster, no drafts, and no `X-Workspace-Id`, since an account that
belongs to no workspace still has to be administrable. Its frame is shadcn's sidebar
(`components/ui/*`, the registry's own source, retuned in `globals.css` to this
palette), and its gate reads the platform role from `/auth/me` — the API checks it
again on every request, so the gate is a door rather than the lock.
`/tasks/[taskID]` is the app's first dynamic route: the layout above it
reads the workspace from client hooks, so the route file carries `export const
instant = false` — this repo builds with `cacheComponents`, which calls an unbounded
segment a blocking route. Integrasi also carries the WhatsApp card: the number is linked by scanning
a code, the warning is read before the code exists, and what customers wrote and what
the workspace asked to send are shown side by side. Every one of them reads the workspace through
`components/dashboard/workspace-data.tsx`, which holds one copy of the roster,
the conversations, the drafts and the tasks and keeps them current from the event
stream. The sidebar's roster is arranged by hand: a Bolu is dragged up, down, or into
the other team, the layout is sent whole and stored per team (`agents.sort_order`), and
each team's header folds it away — remembered per browser, because which sections are
open is about the screen rather than about the team. The Dashboard artboard is `design/keluarga-bolu-dashboard.html`; when a
port and the reference disagree, the reference wins. Rutinitas reads and writes the
routine module's own endpoints: a cadence is registered in the workspace's timezone,
the next times come from the same translation the engine uses, and the history shows
the task's own one-line summary. Belum ada integrasi pembayaran; auth, workspace,
agent, approval, LLM, memori, rutinitas dan WhatsApp sudah tersambung ke backend.
