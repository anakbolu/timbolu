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
│   ├── backend/     # Go modular monolith — Fiber, pgx + sqlc, Temporal, Zap
│   └── frontend/    # Next.js (App Router) + TypeScript + Tailwind v4
├── docs/
│   ├── adr/         # architecture decision records
│   ├── testing.md   # test layers and CI gates
│   ├── deployment.md # the three images, the migration step, and the environments
│   └── e2e-mvp.md   # the MVP flow's end-to-end scenario and its measured cost per task
├── docker-compose.yml
├── Makefile
└── .env.example
```

Backend layout, dependency rules, and commands: [`apps/backend/README.md`](apps/backend/README.md).

## How to run

```bash
cp .env.example .env
make up
```

The end-to-end checks run against that stack, each one stopping the compose workers first so the
run's task is not executed by another process's model configuration:

```bash
docker stop bolu-api bolu-agent-worker bolu-integration-worker
make smoke-task && make smoke-approval && make smoke-order
make measure-orders   # what one order costs, measured twenty times from usage_ledger
docker start bolu-api bolu-agent-worker bolu-integration-worker
```

`make smoke-order` is the MVP flow: a customer's message becomes an order priced from the
workspace's own catalogue, and the invoice that follows is a draft a person approves.
`make measure-orders` runs that flow twenty times and reads the cost back from `usage_ledger`, which
is where the numbers in [`docs/e2e-mvp.md`](docs/e2e-mvp.md) come from.

`make up` starts Postgres (with pgvector), Redis, Temporal and its UI, runs the
migrations, then starts the API, both Temporal workers, and the frontend.

| Service      | URL                              |
| ------------ | -------------------------------- |
| Frontend     | http://localhost:3000            |
| Backend      | http://localhost:8080/health     |
| Readiness    | http://localhost:8080/ready      |
| Temporal UI  | http://localhost:8233            |
| Postgres     | `localhost:5432` (`employeebot` / `employeebot_secret` / `employeebot`) |

### Running without Docker

```bash
make frontend   # cd apps/frontend && npm run dev
make backend    # cd apps/backend && go run ./cmd/api
```

Install frontend dependencies inside `apps/frontend` (`npm ci`) — there is no
root `package.json`, so `npm install` at the repo root does nothing.

## Make targets

| Target                   | Description                                          |
| ------------------------ | ---------------------------------------------------- |
| `up` / `down` / `logs`   | Manage the local stack                               |
| `frontend` / `backend`   | Run one process on the host                          |
| `test` / `cover`         | Test suite, and tests plus the 80% coverage gate     |
| `lint` / `archcheck`     | golangci-lint, and the module dependency rules       |
| `sqlc` / `mocks`         | Regenerate the query code and the domain mocks       |
| `migrate-up` / `-down`   | Apply or revert migrations against `$DATABASE_URL`   |
| `ci`                     | Run the same gates as CI locally                     |
| `help`                   | List every target                                    |

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
- **Local infra:** Docker Compose (`postgres`, `redis`, `temporal`, `migrate`,
  `api`, `agent-worker`, `integration-worker`, `frontend`).

Frontend routes today: `/` (hero + 8 bot), `/pricing` (tier placeholder, catatan
billing transfer bank), and the signed-in workspace under `apps/frontend/src/app/(workspace)`:
`/dashboard` (the front door — the roster, the live activity feed, the drafts waiting,
and the week's finished work), `/dashboard/routines` (the routines screen: the form
that sets a cadence, the routines per Bolu with their next run in the workspace's own
zone, the pause switch, and the run history), `/office` (the room, where each
Bolu stands by its derived state), `/dashboard/integrations` (the hub: connected
accounts, grants per Bolu, MCP servers), `/settings` (workspace profile, working
hours, approval policies), `/settings/team`, `/settings/model`, `/chat`, and
`/groups`. Integrasi also carries the WhatsApp card: the number is linked by scanning
a code, the warning is read before the code exists, and what customers wrote and what
the workspace asked to send are shown side by side. Every one of them reads the workspace through
`components/dashboard/workspace-data.tsx`, which holds one copy of the roster,
the conversations, the drafts and the tasks and keeps them current from the event
stream. The Dashboard artboard is `design/keluarga-bolu-dashboard.html`; when a
port and the reference disagree, the reference wins. Rutinitas reads and writes the
routine module's own endpoints: a cadence is registered in the workspace's timezone,
the next times come from the same translation the engine uses, and the history shows
the task's own one-line summary. Belum ada integrasi pembayaran; auth, workspace,
agent, approval, LLM, memori, rutinitas dan WhatsApp sudah tersambung ke backend.
