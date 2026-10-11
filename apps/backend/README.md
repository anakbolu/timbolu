# Backend

Go modular monolith: one codebase, one Postgres, three deployable processes.

## Layout

```
cmd/
  api/                  # HTTP + WebSocket (Fiber)
  agent-worker/         # Temporal worker: the agent loop, tools, approvals
  integration-worker/   # Temporal worker: OAuth, provider APIs, webhooks
internal/
  container/            # the only place that calls constructors
  modules/<name>/       # domain · repository · service · handler
  platform/             # config, database, redis, temporal, logger, middleware,
                        # response, llm adapters
migrations/             # golang-migrate .up.sql / .down.sql, embedded in the binary
test/integration/       # cross-module tests with testcontainers-go
tools/                  # archcheck (dependency rules), migrate (schema runner)
```

Rules (checked by `make archcheck` in CI):

- `domain` imports only the standard library.
- `service` depends on domain interfaces, never on a transport or a driver.
- `handler` calls the service interface, never a repository.
- A module may only import another module's `domain`.
- `platform` holds infrastructure, never business logic.
- `container` is the single assembly point; `cmd/*` goes through it.

## Commands

```bash
make up              # full stack: postgres+pgvector, redis, temporal, api, workers
make backend         # API on the host
make test            # go test ./... (integration tests need Docker)
make cover           # tests + the 80% coverage gate
make lint            # golangci-lint
make archcheck       # dependency rules
make sqlc            # regenerate the query code
make mocks           # regenerate the domain mocks
make migrate-up      # apply migrations to $DATABASE_URL
make smoke-task      # the task runtime end to end against the compose stack
```

## Modules

| Module | Endpoints | Notes |
| --- | --- | --- |
| `health` | `GET /health`, `GET /ready` | Liveness never touches a dependency; readiness reports each one |
| `auth` | `/api/auth/*` | Register, verify, login, refresh, logout, password reset, Google |
| `workspace` | `/api/workspaces/*`, `/api/invitations/accept` | Onboarding, members, roles, invitations |
| `agent` | `/api/agents/*`, `/api/teams` | The Bolu registry: profiles, derived status, tools, grants |
| `llm` | `/api/llm/*`, `/api/agents/:id/model` | The model gateway: provider configuration, fallback chain, per-Bolu override, usage report |
| `chat` | `/api/conversations/*`, `/api/events*`, `/content/:ref` | Conversations with block messages, attachments, the activity stream, and the sandboxed content origin |
| `task` | `/api/tasks*` | The durable agent runtime: the task a Bolu runs, the rounds it recorded, the handoffs, and the cancel |

### Authentication

- Passwords are hashed with **argon2id** (64 MiB, 3 iterations, 2 lanes).
- The **access token** is a short-lived JWT (HS256) returned in the body; the
  client keeps it in memory. It carries the session id, so signing out or
  resetting a password invalidates it immediately.
- The **refresh token** lives in an `httpOnly` cookie scoped to `/api/auth`, is
  stored only as a SHA-256 hash, and is **rotated** on every refresh.
- Verification, reset, and invitation links are one-time tokens, also stored as
  hashes, with `used_at` enforcing single use.
- Repeated sign-in attempts are throttled with a Redis token bucket per address
  and per IP. A Redis outage degrades to no throttling rather than locking
  everyone out.

### Tenancy

Three middlewares guard the routes, in this order:

1. `requireUser` — verifies the access token and its session.
2. `requireVerified` — rejects an account whose email is not confirmed. It
   guards the routes that create or join tenancy.
3. `requireTenant` — reads the active workspace from `X-Workspace-Id` (or
   `?workspace_id=`), confirms the caller's membership, and stores the scope the
   handlers query with. A non-member gets **403**.

Every tenant query runs through `database.InScope`, which sets `app.user_id` and
`app.workspace_id` for the transaction. Row Level Security then filters every
statement, so a query that forgets `WHERE workspace_id = ...` still cannot read
another workspace. `workspaces` carries `owner_user_id` so a workspace can be
read back inside the transaction that creates it.

### Agent registry

- A workspace always starts with the six seeded Bolu, copied **inside the
  onboarding transaction** by the agent repository acting as the workspace
  module's `TemplateProvisioner`. A workspace can therefore never exist without
  its team.
- The display status is derived from tasks and drafts, never stored; only the
  rest switch is a column.
- A tool only reaches the model when its integration was granted to that Bolu,
  and a `read` grant keeps only the read-labelled tools.

### Model gateway

- **Providers are data.** A workspace stores its providers with a fallback
  order (`is_default`, then `priority`); a Bolu may pin one, bring its own
  endpoint, or override only the model. The effective chain is resolved per
  request: **Bolu override → workspace chain → platform default**, and the
  platform default is used only when the workspace configured nothing.
- **The secret never leaves the backend in clear.** An API key is sealed with
  AES-256-GCM (key from `SECRET_ENCRYPTION_KEY`) before it reaches the database,
  and responses only say whether a key is stored. An edit that sends no key
  keeps the stored one; clearing it is an explicit request.
- **Fallback is narrow on purpose.** A rate limit, an outage, a provider-side
  quota, or a model that cannot serve the request (no tools, no vision, prompt
  too long) moves to the next provider. A malformed request fails everywhere, so
  it returns at once instead of spending the whole chain.
- **Every call is recorded.** One provider call writes exactly one
  `usage_ledger` row (tokens, model, cost, task, purpose), and the provider that
  actually answered is what the row names. A call whose cost cannot be written
  returns an error rather than a silent success, and it is not retryable: the
  provider has already billed it.
- **The quota hook runs before the provider is called.** The live counter is
  Redis; when Redis is unavailable the check sums the ledger instead, so an
  outage cannot hand out a second allowance. `LLM_QUOTA_ENABLED=false` turns the
  check off for local development without touching the adapters.
- Streaming falls back **only before the first byte**: once a fragment reached
  the caller, restarting on another provider would duplicate the answer.

### Conversations

- **A message is a list of typed blocks** (`text`, `table`, `draft`, `chart`,
  `mermaid`, `html`), not a string. Each type has a JSON Schema in the domain
  package, applied *before* a message is stored, so a renderer is never handed a
  shape the backend did not check. The stored value is the block document itself,
  so what is written is exactly what a client receives.
- **The three block tools are `render_chart`, `render_mermaid`, and
  `render_html`.** An agent calls one rather than writing a block as free text,
  which is what makes validation possible. A block that fails validation is sent
  back to the model with the validator's own message — naming the offending field
  — and the repair loop is bounded (`DefaultRepairBudget`), so a block that never
  validates costs a fixed number of rounds and no more.
- **A reply is stored before it is finished.** A placeholder is written when the
  answer starts and updated as it grows, so the user sees text appear
  continuously, a client that joins late renders the same body, and a stream that
  dies leaves the tokens it produced behind, marked `partial` rather than lost.
  An answer that produced nothing is `failed`.
- **Pagination is by cursor, never offset.** A chat is append-heavy, so an offset
  shifts under the reader and page two would repeat or skip a message. The cursor
  names the last message of the previous page (timestamp in nanoseconds, plus id)
  and the index reads newest-first, so a page boundary is stable while messages
  arrive.
- **The activity stream is durable, and Redis is only the fast path.** Every
  event is a row in `activity_events` written *before* it is published, so a
  subscriber that reconnects replays the rows it missed from `last_event_id` and
  a dropped publish costs latency rather than an event.
- **A group message produces at most one reply.** A small model call picks the
  Bolu from its role and the message; the decision is recorded as a
  `router.decision` event and its cost lands in `usage_ledger` under the
  `routing` purpose, so "who answered and why" is answerable after the fact. When
  the model is unavailable a deterministic score picks instead and says so.
- **Sandboxed HTML is isolated, not sanitised.** The document is stored in object
  storage and served from the content origin under a policy with
  `connect-src 'none'`; the iframe is `sandbox="allow-scripts"` with no
  `allow-same-origin`, `allow-forms`, or `allow-popups`. The only message accepted
  from the frame is a clamped `resize`. The document's size is capped at 500 KB.
- **The content origin names its framing application.** `CONTENT_FRAME_ANCESTORS`
  is the app's own origin and must be set: the document is served from a different
  host by design, so a `frame-ancestors 'self'` policy would refuse every frame
  and the block would never appear.
- **Absence is an empty string on the wire, never a zero id.** `encoding/json`
  cannot omit a `uuid.UUID` — it is an array — so the wire types carry ids as
  strings; a typed field would publish
  `00000000-0000-0000-0000-000000000000` and a client would read it as an author.

### Task runtime

- **A task is a Temporal workflow, not a goroutine.** A chat message opens one,
  and the answer arrives out of process, so it survives the request that started
  it, a worker restart, and an approval that takes hours. The workflow id is the
  task id, so a duplicate dispatch is refused rather than silently replacing a
  finished task's history.
- **A task is opened by chat, a routine, a webhook, or another task — never by a
  browser.** `handoff` is not in the client whitelist: a chain of handoffs is
  opened by the runtime, and a client that could claim the trigger would make the
  chain look like something a person asked for.
- **The workflow is free of I/O.** Everything that touches a database, a model,
  or the clock is an activity, because a workflow is replayed from its history: a
  non-deterministic step would make the replay disagree with the run and the task
  would be stuck forever. The activity names are the workflow's own constants,
  and `TestEveryActivityNameHasAMethod` fails if a name and a registered method
  ever drift apart — the failure mode that would otherwise be a task stalling
  silently until its timeout.
- **Every round is recorded.** A thinking round, each tool call, each tool
  result, and the final answer become `task_steps` rows with the sequence the
  store assigns. The one-line summary is written on the task itself, so it
  survives the retention job that prunes the raw payloads.
- **Bounds are decided in one place.** `BeatTask` is where a step, token, cost, or
  cancel check happens, so "the task stopped because it hit a limit" has one
  implementation and produces the reason the user reads. Defaults:
  `TASK_MAX_STEPS=12`, `TASK_MAX_TOKENS=120000`, `TASK_MAX_HANDOFF_DEPTH=2`,
  `TASK_MAX_TOOL_RESULT_BYTES=16384`.
- **Two spend bounds, asked at two boundaries.** The period's token allowance
  stops a workspace from spending a month's tokens, and `PLAN_DAILY_COST_MICROS`
  stops one runaway task from spending a month in an afternoon. The task asks both
  at a clean round boundary — which is where it stops with a reason — and the
  gateway asks the same policy before every call, which is what stops a round from
  crossing the bound in the middle of it. One policy answers both places, so they
  cannot disagree. The live totals are two Redis windows (the period and the day,
  both following Jakarta), and the ledger's daily aggregation is the durable
  fallback: a Redis outage must not hand every workspace a fresh allowance.
- **A `write_external` tool is never executed directly.** It becomes a draft and
  the task is parked as `waiting_approval`; the workflow waits for the decision
  without holding a worker slot. An approval continues the round; a revision
  comes back to the model as a failed tool result. A deployment with no approval
  gate refuses the action rather than taking it, which is the safe direction.
- **`handoff` is performed by the workflow itself**, because it opens a child task
  and waits for its answer: that wait is what makes a chain traceable rather than
  a conversation. A refusal — a chain past the ceiling, a resting Bolu, a name
  that matches nobody — returns to the model as a failed tool result, so the
  parent can carry on.
- **A stale heartbeat is what "stuck" means.** The status alone cannot tell a
  working task from one whose worker was evicted, because both read `running`, so
  `health` is derived from `heartbeat_at` and never stored.
- **A tool reaches the model only if something can run it.** The registry applies
  both filters — the grant (EPIC 3) and an executor (EPIC 8) — so a tool nothing
  can execute is not offered, because offering it teaches the model to call it.

## Configuration

Configuration is read from `internal/platform/config/config.yaml` (or
`config-local.yaml` when `ENVIRONMENT=local`) and then overridden by environment
variables. Secrets never live in the YAML files: `DATABASE_URL`, `REDIS_URL`,
`TEMPORAL_HOST_PORT`, `TEMPORAL_API_KEY`, and `JWT_SECRET` all come from the
environment. See `.env.example` at the repository root.

The API and the workers start without Postgres, Redis, or Temporal so a
developer can run one process at a time. `/health` reports liveness and never
touches a dependency; `/ready` reports each dependency and answers 503 when a
configured one is down.

Environment variables worth knowing: `JWT_SECRET` (signs access tokens **and**
the OAuth state), `FRONTEND_URL` (the base of the emailed links and the Google
callback target), `GOOGLE_CLIENT_ID`/`GOOGLE_CLIENT_SECRET`/`GOOGLE_REDIRECT_URL`
(Google sign-in stays disabled while they are empty), `MAIL_DRIVER` (`log`
prints the links, `smtp` sends them), and the model gateway's
`SECRET_ENCRYPTION_KEY` (32 bytes, base64/hex/raw, seals the provider keys at
rest), `LLM_DEFAULT_*` (the provider the platform offers a workspace that
configured none), `LLM_QUOTA_ENABLED`/`LLM_TOKENS_PER_PERIOD`, and
`LLM_CHAIN_LIMIT`. The task runtime's bounds are `TASK_MAX_STEPS`,
`TASK_MAX_TOKENS`, `TASK_MAX_HANDOFF_DEPTH`, and `TASK_MAX_TOOL_RESULT_BYTES`;
a zero falls back to the product default. Object storage is `STORAGE_DRIVER`
(`local` writes to a directory, `s3` speaks the S3 API that MinIO, R2, B2, and
AWS share) plus
`STORAGE_LOCAL_ROOT` or `S3_*`, and `CONTENT_ORIGIN` is where sandboxed documents
are served from — a different origin from the app in every deployment.

Deployment and region decisions: `docs/adr/0001-temporal-dan-region.md`.
Test strategy and gates: `docs/testing.md`.
API contract: `docs/api.md`.

The web app routes are English: `/signup`, `/login`, `/verify`,
`/forgot-password`, `/join`, `/onboarding`, `/settings/team`, `/settings/model`,
`/chat`, `/groups`, `/office`.

The chat page is one thread at a time with a thread list, the group page manages
groups and shows who is in them, and the office draws each Bolu at a place
computed from its `agent.state` — the backend stores no coordinate.
