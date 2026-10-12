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
| `approval` | `/api/drafts*`, `/api/approval-policies`, `/api/audit` | The outbound gate: the draft a Bolu proposes, the policy that decides whether a person is asked, and the trail |
| `memory` | `/api/memories*`, `/api/knowledge*` | What a Bolu remembers and what it knows: two memory layers, the knowledge base, the retrieval that reaches a prompt, and the extraction that reads a quiet conversation |
| `routine` | `/api/routines*`, `/api/routine-runs` | Work a Bolu runs on its own: the four cadences, the Temporal Schedule behind each one, the run history, and the cheap check that skips a model call |
| `whatsapp` | `/api/whatsapp/*` | The number the shop answers on: the link flow, who answers it, what customers wrote, and the queue that paces what leaves. The connection itself lives in the `wa-gateway` process |
| `billing` | `/api/billing/*` | The package a workspace is on, the allowance it grants, the tokens it bought on top, and what it owes. Payments go through Duitku; the quota is what gates every model call |

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
  outage cannot hand out a second allowance. When the billing module is wired, the
  allowance and the day's ceiling come from the workspace's *package* rather than
  the deployment's configured value; `LLM_QUOTA_ENABLED=false` still turns the
  check off for local development without touching the adapters.
- Streaming falls back **only before the first byte**: once a fragment reached
  the caller, restarting on another provider would duplicate the answer.

### Billing, packages, and quota

- **What a workspace may spend is derived, never remembered.** The package grants
  tokens per period plus what was bought on top; what is used is the live Redis
  counter reconciled with `usage_ledger`, and **the larger of the two wins**. That
  direction is the point: a Redis that lost its keys must not hand a workspace its
  month again. A stored "remaining" would be a number that can be wrong.
- **A workspace always has exactly one live subscription.** The trial package is
  created inside the onboarding transaction, by the workspace repository acting as
  this module's `SubscriptionProvisioner`, so a workspace cannot exist without a
  package. The migration backfilled every workspace that already existed.
- **Two bounds, asked at two boundaries.** The period's token allowance and the
  package's daily cost ceiling are asked by the gateway before every call and by the
  task runtime at a clean round boundary, so a task stops with a reason rather than
  halfway through a call. The refusal names what was used and what the package
  grants.
- **80% and 100% are reported once per period each**, and a period that jumps past
  both reports both in order: the 80% message is the one that gives somebody time to
  act. The marks live on `quota_periods` because the state itself is derived.
- **Past the grace window a workspace is read-only, and the way out is always
  open.** Reads and exports keep working and nothing is deleted; writes and new work
  are refused with `402`. The billing endpoints are exempt, because a workspace that
  cannot pay would be read-only forever if the call that fixes it were refused too.
- **One invoice per period, however often it is asked for.** A partial unique index
  where the status is `pending` is what makes a repeated sweep and a screen write one
  row between them, and the statement is `ON CONFLICT DO NOTHING` so the recovery
  read runs in a transaction that was never aborted. It is partial so that an
  **expired** invoice is re-issuable.
- **Two signatures, in Duitku's own two orders.** The inquiry signs
  `merchantCode + merchantOrderId + paymentAmount`; the callback signs
  `merchantCode + amount + merchantOrderId`. The callback's recomputation is the
  whole security of that route, so it is compared in constant time and a failure is
  answered **400** rather than 500 — a provider told "internal error" would keep
  redelivering a body that can never verify.
- **The reference recorded is the merchant order id, not Duitku's own reference**,
  because the callback names the order id. Recording the other would leave every
  payment unmatched and every period unextended, with a callback that arrived and was
  accepted for an invoice nothing could find.
- **A payment is applied once**, by a statement guarded on `status = 'pending'`, so a
  replayed webhook buys nothing twice. A payment smaller than the invoice does not buy
  it.
- **Changing a package is deferred to the next period** when the current one was
  already paid for: a bigger package mid-month would either be free or need a
  proration nobody asked for. A trial has nothing to protect, so a change there
  applies at once.

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

### Approval and drafts

- **An action that reaches outside Bolu waits for a person.** A tool labelled
  `write_external` never runs: it becomes a draft, the task parks as
  `waiting_approval`, and the workflow waits — without holding a worker slot.
- **An action kind with no stored policy is `always_draft`.** That is enforced in
  the service, not by hoping every workspace has a row: a missing policy is the
  safe answer. Onboarding also seeds an explicit conservative row for every kind,
  so the policy screen always shows something to edit and "no policy for sending
  email" is a state that cannot exist.
- **`auto` is a real decision with a ceiling.** A workspace may allow one kind of
  action to go out unattended, but only while the value it carries is within
  `max_amount`. An action whose amount cannot be read is treated as unbounded, so
  a ceiling cannot be satisfied by a payload that never states one. An auto-send
  is still recorded: the draft row is written and marked sent, so "what did the
  agents send" has an answer even when nobody was asked.
- **Approving carries the action out and ends the task, with no further model
  call.** The person approved what to send; asking the model to produce it again
  would spend tokens to reach a decision already made. A revision does the
  opposite: it returns the task to its agent with the note, bounded by
  `MaxRevisions` so a reviewer and an agent cannot argue past the task's
  allowance.
- **A revision ends the draft it was made against.** The agent produces a new
  one, so the old row keeps `status = 'revise'` and the trail keeps what the
  person was looking at. `draft_revisions` is what makes "why was this sent,
  after three attempts" answerable.
- **The working day decides when, not whether.** An action approved outside the
  workspace's hours is held with a reason and a `not_before`, and the workflow
  waits on a Temporal timer rather than re-checking: the wait is durable, so a
  worker that dies during it resumes it. The hours are read at the moment the
  decision is carried out, so changing them or the timezone recomputes the hold.
- **A decision is recorded once.** The store only decides a draft that is still
  `pending`, so a duplicate signal changes nothing and a second send cannot
  happen. The chat card and the dashboard both write through the same endpoint,
  which is what keeps the two surfaces from disagreeing.
- **Every step is auditable.** `draft.created`, `draft.approved`,
  `draft.revised`, `draft.sent`, and `policy.updated` land in `audit_logs` with
  the actor, so a send can be traced to its draft, its task, and the person who
  allowed it.
- **Nothing is marked sent that did not leave.** With no integration executor yet
  (EPIC 8 owns it) the action stops at the send boundary and the task fails with
  that reason, rather than recording a send that never happened.

### Memory and knowledge (EPIC 10)

- **A memory and a knowledge chunk are different things.** A memory is short,
  written by a person or by extraction, and travels in every call. A chunk is one
  piece of a document, and it is retrieved by similarity to the question. Storing
  them in one table would make "what must I always send" and "what might be
  relevant" the same question.
- **Nothing is stored without a vector.** A memory or a chunk the search can never
  find is worse than a refused write, because it looks like the Bolu knows
  something and then never uses it. A write therefore fails when no provider can
  embed; a *read* degrades instead, because a task must still answer.
- **A write is filtered before the row exists.** Card numbers, passwords,
  one-time codes, and identity numbers are refused for all three doors: a direct
  request, the extractor, and a document. The card check is the Luhn checksum
  rather than a digit count, so an invoice number is not refused along with a card.
- **The main memory is capped, and the cap is stated.** Pinned first, then newest;
  a memory that does not fit is skipped whole rather than cut in the middle,
  because half a fact read as a fact is worse than a fact that arrives using the
  write boundary. When nothing fits at all the newest one is truncated, because an
  empty memory section reads as a Bolu that forgot everything.
- **The retrieved block is marked as data.** A document chunk travels inside
  `<data sumber="...">` … `</data>`, and the closing tag is escaped in the content,
  so a document cannot end the block early and speak as the workspace's own words.
  That marker is what the prompt test asserts.
- **The two halves are cached differently.** The persona, the instructions, and the
  main memory are the stable prefix a provider with prompt caching keeps between
  calls; the retrieved block changes with the question and is sent as its own
  block. Joining them would spend a cache write on every task and lose the hits the
  stable half earns. `ChatRequest.Retrieved` is that seam.
- **Memory is loaded once per task, not once per round.** The retrieved text is
  therefore identical across the rounds of one task, which is what lets the second
  round hit the cache instead of paying for a fresh prefix.
- **A corrected fact replaces its predecessor.** A new memory within
  `MEMORY_SIMILARITY` of an old one supersedes it: the old row keeps
  `superseded_by` and stays readable as history, and the search sees one memory.
  Two versions of "ongkir Bandung" in one prompt is worse than one.
- **Extraction is a durable wait, not a sleep.** `SignalWithStart` on every message
  restarts one workflow's timer, so a thread is read after it stops rather than
  while it is being written, and the wait survives a deploy. A conversation that
  never settles ends its run instead of growing one workflow's history without end.
- **A delete is permanent, and it takes its measurements with it.** There is no
  soft-delete column; `retrieval_usages` rows cascade, because a record naming a
  row that no longer exists is a count nobody can interpret. The audit trail says
  what was deleted and never copies the text.
- **The audit trail is written through the module that owns it.** The rows live in
  the approval module's `audit_logs`; the container adapts the two, so neither
  module reaches into the other.
- **Embedding is part of the gateway, not a second configuration.** The same
  fallback chain and the same ledger serve it: one provider call writes one
  `usage_ledger` row with `purpose = embedding`. A provider that has no embedding
  route advertises `Embeddings: false` and is skipped rather than asked and
  refused — which is why Anthropic says so instead of failing at call time.

### Routines and scheduling (EPIC 11)

- **A routine that exists always has a schedule — unless it has no clock at all.**
  Creating one registers its schedule before it is reported as created, so a routine
  that could not be scheduled is a failed write rather than a silent one that never
  runs. A trigger routine is the exception that proves it: its cadence is the event
  that starts it, so it needs no engine, and demanding one would make "setiap ada
  email masuk" unsavable in a deployment without a scheduler.
- **One schedule per routine, enforced by the database.** `schedule_id` is derived
  from the routine's id and carries a unique index, so registering the same routine
  twice leaves exactly one schedule — not because the service is careful, but
  because a duplicate cannot be inserted. This is the epic's first gate: a routine
  must never execute twice for one slot.
- **The engine fires in the workspace's timezone.** A routine has no zone of its
  own. Every schedule is registered in `workspace.tz`, which is the whole of E11.3:
  changing the zone re-registers the schedules and every future run moves, and the
  times a screen shows are computed from the same place, so a list cannot promise a
  time the engine never fires. Nothing is cached, so there is nothing to invalidate.
- **A trigger routine gets no Temporal Schedule at all.** A cron expression for
  something that happens when it happens would fire on an empty queue and spend
  tokens on nothing. The delivery starts it instead: the hub hands a queued event to
  the routines watching it, and the run claims the delivery in the same statement,
  so two routines watching one event cannot both act on it.
- **One execution is one `routine_run` and one `task`.** The task is what gives a
  run a durable workflow and an approval gate, so a run without one is refused
  rather than recorded as an empty success. A run that could not open its task is
  `failed` with the reason, and the next tick still fires — one bad run must not
  stop a routine forever.
- **The history shows the task's own line, and its own reason when it failed.** The
  task runs out of process and reports back through the runtime's outcome sink,
  which is why the sink resolves the routine service when it is called rather than
  when it is installed: the task runtime is assembled *before* the routines, so a
  sink captured at assembly time would hold nil forever and every run would stay
  `running` while its task was already done.
- **A run that finds nothing to do costs nothing, and the saving is recorded.** A
  routine watching events reads its queue, and a routine whose Bolu is resting stops
  there; either way the run is `skipped` with the reason and no model is called. The
  claim is about cost, so `usage_ledger` is where it is proven: a skipped run adds no
  row.
- **Pausing is pausing the schedule, not deleting the routine.** The routine stays
  listed — a routine somebody turned off is one they still want to see — and a tick
  that arrives anyway is refused. Resuming brings the same one schedule back.
- **Deleting a routine keeps its history.** The link is dropped to null and the
  routine's name was snapshotted onto the run when it ran, so the history still reads
  as a line. "What did this routine do last month" is exactly the question somebody
  asks after turning it off, and it is the worst direction to be wrong in.
- **A schedule change is visible immediately.** Create, update, pause, and resume
  answer with the routine *and* its next run times, read from the same write that
  changed them, so a form cannot show a stale time beside a cadence it just saved.

### WhatsApp (EPIC 9)

- **The connection is a separate process, and that is not a preference.** A WhatsApp
  session is a socket that stays open. The API serves requests and is replaced at
  any moment; a number whose socket died with the request that touched it is a
  number that is never online. `cmd/wa-gateway` owns the sockets, and the API
  reaches it over an internal address it is not publicly reachable at.
- **One number has one owner, and the owner has a deadline.** Two processes on one
  session send every message twice, which is how a number gets restricted. A lease
  in Redis decides, it is renewed on an interval, and a failed renewal makes the
  holder stand down *immediately* — because a failed renewal means a successor is
  already connecting. Redis rather than Postgres because the value is liveness: a
  lease that survived the process holding it is exactly the bug this prevents.
- **The session is sealed with the workspace's own key, and bound to it.** The
  device state is persisted as one document sealed through the same key ring the hub
  seals tokens with, with the workspace id as associated data. A blob cannot be
  opened under another tenant's key, and cannot be replayed against another tenant
  even if the row is copied. The cache — contacts, chat settings, message secrets,
  privacy tokens — is deliberately *not* persisted: WhatsApp re-sends it, and keeping
  it would grow the sealed blob with data nothing here reads.
- **The fleet reads one table without a tenant, and the migration says why.** The
  gateway is a fleet worker rather than a tenant, and it has to discover which
  tenants have a number before it can open any workspace's scope at all. What makes
  it safe is what it holds — a routing pair and nothing else — plus the seal above,
  which is what turns a directory row naming the wrong tenant into an unopenable
  blob rather than another tenant's session.
- **A QR is a capability, so it travels on its own socket.** The workspace event
  stream reaches every member; a code sent there would let a colleague link a number
  into somebody else's workspace. The attempt has its own socket, authenticated as
  the person who started it, and it closes the moment the attempt ends. The code is
  drawn from a matrix the server rendered rather than from an image, because the
  browser has no QR encoder and an `<img>` cannot send a header.
- **The warning is what unblocks the link, and nothing else does.** Linking over an
  unsupported route is a risk somebody takes, so it is accepted explicitly and before
  an attempt starts. The product's answer to that risk is the pace below, not a
  disclaimer.
- **A message is recorded before it is acted on.** The row is what makes a
  redelivery harmless — WhatsApp retries when an acknowledgement is slow — and it is
  what a person reads when they ask what a customer said. Only a message this call
  created opens anything, so a retry cannot become a second task.
- **A routine watching the event owns the work.** An inbound message is announced as
  `whatsapp.message_received` first, and the Bolu assigned to the number answers only
  when nothing is watching. A routine names the Bolu that should act, which is a
  better answer than a rule invented in the module.
- **A group is not a customer.** A group message and a message this number sent are
  both recorded — what was said is what a person reads — and neither opens work.
- **The ceiling is a pace, not a refusal.** A number that spent its hour keeps its
  queue: the messages are pushed to later rather than parked as failed, and the
  owners are told they are paced rather than lost. The count is of messages that
  actually left, so a counter cannot drift away from reality. A hold gives the
  attempt back; a real failure keeps it, which is how a message that keeps failing is
  eventually parked rather than retried forever.
- **Media is fetched after the message is recorded.** A 40 MB video must not hold up
  the listener, so the receive path records a message with a pending attachment and a
  media pass fetches it. A download that fails is recorded with its reason rather
  than left pending forever, and the message itself is already in front of a person.
- **The adapter declares that it needs no token, and the executor honours it.** A
  linked number is paired by a phone, so there is nothing for the hub's token columns
  to hold. Sealing an empty string to look like a token would make the row read as a
  connection with a credential it never had, and the next person to debug it would
  hunt for a token that was never issued. `Credentialled` is how an adapter says so,
  and the default — an adapter that says nothing — still needs one.
- **The WhatsApp tools are offered only when a gateway is reachable.** The adapter
  resolves the module when it is called rather than when it is installed, because the
  hub is assembled before the module — a linked number is a connection the hub shows
  — and an adapter holding a nil service would offer tools nobody could run, which
  teaches the model to call them.
- **The official Business Platform slots in behind the same interface.** Everything
  WhatsApp-specific is behind `domain.Transport`, which two things already implement
  — the whatsmeow engine and the HTTP client — so the third is a third
  implementation rather than a second design.

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
are served from — a different origin from the app in every deployment. Memory is
bounded by `MEMORY_MAIN_TOKENS` (what travels in every call),
`MEMORY_RECALL_MEMORIES` and `MEMORY_RECALL_KNOWLEDGE` (what one round retrieves),
`MEMORY_SIMILARITY` (when a new memory replaces an old one), and
`MEMORY_QUIET_SECONDS` (how long a conversation must be still before it is read).

Deployment and region decisions: `docs/adr/0001-temporal-dan-region.md`.
Test strategy and gates: `docs/testing.md`.
API contract: `docs/api.md`.

The web app routes are English: `/signup`, `/login`, `/verify`,
`/forgot-password`, `/join`, `/onboarding`, `/settings/team`, `/settings/model`,
`/chat`, `/groups`, `/office`.

The chat page is one thread at a time with a thread list, the group page manages
groups and shows who is in them, and the office draws each Bolu at a place
computed from its `agent.state` — the backend stores no coordinate.
