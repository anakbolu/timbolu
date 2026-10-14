# Testing and quality gates

Everything in this document is enforced by `make ci` locally.

**No workflow enforces them.** The CI workflow was replaced by
`.github/workflows/deployment.yml`, which calls the Easypanel deploy webhooks on a
push to `main` and runs no tests, and `release.yml` is disabled. Every CI run before
that had already failed in about three seconds without producing a step log, which is
the shape of an account that cannot execute workflows rather than of a failing gate.
So **run `make ci` locally and say so in the pull request**, because nothing automated
is enforcing the gates.

## The gates

| Gate | Command | What it protects |
| --- | --- | --- |
| Formatting | `gofmt -l ./cmd ./internal ./migrations ./tools ./test` | No unformatted file is merged |
| Dependency rules | `make archcheck` | Module boundaries (see below) |
| Lint | `make lint` (`golangci-lint run ./...`) | Static analysis; the linter set lives in `apps/backend/.golangci.yml`, the version is pinned to v2.5.0 in CI |
| Generated code | `sqlc generate && git diff --exit-code -- internal/modules` | `sqlcgen` matches the migrations and queries |
| Generated mocks | `mockery --config .mockery.yaml && git diff --exit-code -- internal/modules` | Committed mocks match the domain interfaces |
| Migrations | `go run ./tools/migrate up`, `down`, `up` | Both directions of every migration work |
| Tests | `make test` (`go test ./...`) | Unit and integration tests |
| Race detector | `go test ./... -race` | Data races |
| Coverage | `make cover` (`scripts/coverage.sh`) | Total coverage ≥ 80% |

## Linting

`apps/backend/.golangci.yml` declares the linter set explicitly so a locally
installed golangci-lint behaves like CI. CI pins the version
(`golangci-lint@v2.5.0`); install the same version locally to avoid drift:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
```

Beyond the standard set, the config enables `bodyclose`, `errorlint`, `nilerr`,
`unconvert`, `wastedassign`, `misspell`, `predeclared`, `usestdlibvars`,
`tparallel` and `thelper`, and runs `gofmt`/`goimports` as formatters.
Generated packages (`sqlcgen`, `mocks`) are excluded.

## Dependency rules (archcheck)

`tools/archcheck` parses the imports of every package and fails on:

- `domain` importing anything but the standard library — no Fiber, pgx, Zap, or
  sqlc types, and no `internal/platform`.
- `domain` importing another layer of its own module.
- `service` importing a transport or a driver (Fiber, pgx, Redis, Temporal
  client/worker, `database/sql`).
- `handler` importing a repository; handlers only call the service interface.
- One module importing another module's `repository`, `service`, or `handler`.
  Only `domain` contracts may be shared.
- `platform` importing a business module or the container: platform holds
  infrastructure without business logic.
- A module importing `internal/container`.
- `cmd/*` importing a module directly: binaries go through the container.
- A layer using a platform package outside its allow-list (`repository` →
  `database`, `redis`; `service` → `logger`, `config`; `handler` → `logger`,
  `middleware`, `response`).

Generated packages (`sqlcgen`, `mocks`), `tools/`, and `migrations/` are exempt.

## Test layers

| Layer | Location | Pattern |
| --- | --- | --- |
| `domain` | `internal/modules/*/domain` | No tests: pure declarations |
| `service` | `internal/modules/*/service/service_test.go` | Unit tests with mockery mocks of the domain interfaces |
| `handler` | `internal/modules/*/handler/handler_test.go` | HTTP tests through `app.Test()`, with a mocked service |
| `repository` | `internal/modules/*/repository/postgres_test.go` + `test/integration` | sqlc query through a real Postgres |
| `platform` | `internal/platform/*` | Unit tests, plus integration tests for the real clients |
| Cross-module | `test/integration` | testcontainers-go: Postgres+pgvector, Redis, Temporal |
| Workflow | `internal/platform/temporal/*_test.go` | Temporal `testsuite` |

### Mocks

Mocks are generated from the domain interfaces and live next to them:

```bash
make mocks   # mockery --config .mockery.yaml
```

Add an interface to `.mockery.yaml` when a new module's service needs one.

### Integration tests

`test/integration` starts real containers with testcontainers-go. Docker must be
running; when it is not, `testcontainers.SkipIfProviderIsNotHealthy(t)` skips
the test instead of failing it, so `go test ./...` stays usable on a machine
without Docker.

What they prove:

- `TestMigrationsRunBothWays` — up, down, and up again on a fresh database.
- `TestSchemaObjectsExist` — pgvector, the six Bolu templates, and the trial
  plan exist after migrating.
- `TestRowLevelSecurityIsolatesWorkspaces` — a tenant can neither read nor write
  another workspace's rows, and an empty `app.workspace_id` matches nothing.
  This test connects as a non-superuser role on purpose: a superuser bypasses
  Row Level Security, so testing as one would prove nothing.
- `TestHealthRepositoryPingAgainstPostgres` — the sqlc query runs through the
  repository.
- `TestMigrationsRunBothWays` runs against its own throwaway database and **drops
  the whole schema** between its up and its down. Point it at a database you care
  about — the compose one, say — and its last `up` leaves the schema empty and the
  data gone. Use `migratedDatabase(t)`'s own scratch containers, or a database you
  do not mind losing, when running the migration targets by hand.
- `TestRedisClientAgainstARealServer` — the shared Redis client connects.
- `TestWorkerRunsAgainstTemporal` — the container connects to Temporal, the
  worker registers a workflow, the workflow executes, and the worker stops
  cleanly.
- `TestMemoryVectorSearchNeverCrossesAWorkspace`, `TestPrivateMemoryDoesNotLeakBetweenBolu`
  — the memory gates that can only be proven against a database: two workspaces
  with the same fact and the same vector, one query, and only the caller's row comes
  back; and a private memory is invisible to another Bolu in the *same* workspace.
- `TestDeletingAMemoryRemovesItFromTheSearch`, `TestSupersededMemoryStaysAsHistory`,
  `TestChunksSettleToReadyOrFailed` — the delete is a real delete (the row and the
  retrieval record that named it go, and the search stops finding it), a superseded
  memory leaves exactly one active version, and a chunk whose embedding failed is
  never searchable while still being visible to a person.
- `TestThePipelineEndToEnd` — the module's own use cases against Postgres: a
  request is stored and confirmed, the next round is given it, and forgetting it
  removes it from the search.
- `TestTaskSurvivesAWorkerRestart` — the gate the durable runtime exists for: a
  worker is killed while a model call is in flight, a second worker takes over,
  and the task finishes with each completed round recorded exactly once. The
  model and the tools are stubs; what is under test is the durable execution.
- `TestARoutineHasExactlyOneScheduleAndKeepsItAcrossARestart` — E11.1's gate against
  a real Temporal server and a real Postgres: one routine leaves exactly one
  schedule, registering it twice still leaves one, and the schedule is there and
  firing at the same time after the process is rebuilt over the same rows.
- `TestATriggerRoutineRegistersNoCronSchedule` — E11.2: an event-driven routine gets
  no schedule at all, and turning it into a daily one registers exactly one.
- `TestTheNextRunFollowsTheWorkspaceTimezone` — E11.3: moving the workspace from WIB
  to WITA moves every schedule one hour earlier in absolute time while the cadence
  still reads "06:00", and the times the API reports agree with the engine's.
- `TestOneExecutionIsOneRunAndOneTask` — E11.4, driven by a real schedule firing
  through Temporal: one tick opens exactly one `routine_run` and one `task`, and the
  task's own outcome reaches the run by itself. The workspace has no model provider,
  so the task fails — which is the point: the run must end `failed` with the task's
  own reason rather than staying `running` while its task is already done.
- `TestARoutineWithNothingToDoNeverCallsAModel` — E11.5: a trigger routine with an
  empty queue finishes as `skipped`, opens no task, and adds no `usage_ledger` row.
  The ledger is read because the claim is about cost.
- `TestAPausedRoutineDoesNotRunAndDoesNotDisappear` — E11.5's other half: the
  schedule stays and is paused, the routine stays listed, a tick that arrives anyway
  is refused, and resuming brings the same one schedule back.
- `TestADeliveryStartsTheRoutineWatchingIt` — the trigger path over the real webhook
  endpoint: a signed delivery starts the routine watching that event, the run claims
  the delivery so a second routine cannot act on it, and the run's task is opened
  with the routine's name.
- `TestDeletingARoutineKeepsItsHistory` — E11.1's second gate: the schedule goes, the
  runs stay, and a run whose routine is gone still reads as a line because the name
  was snapshotted.
- `TestASessionBelongsToOneWorkspace`, `TestANumberIsInvisibleToAnotherWorkspace` — E9.1's
  gate against Postgres: one workspace's number, messages, queue, and sealed session
  are invisible from another, and the policy refuses a write from outside rather
  than the query's filter being careful.
- `TestASessionIsSealedWithItsWorkspacesOwnKey` — the confidentiality half, with the
  real key ring: the same bytes sealed for one workspace do not open for another,
  and a byte changed in the ciphertext is refused. `TestForgettingASessionKeepsTheNumber`
  is the other side: ending a session keeps the number and what customers wrote.
- `TestOneWhatsappMessageIsOneRow` — E9.4: a redelivery of the same WhatsApp message
  is one row and answers with the row it already has, which is what stops a second
  task for one customer question. `TestAMessageWithoutAnAttachmentIsNotPendingMedia`
  is the receive path's other half: a message is recorded before its bytes are
  fetched, and a failed download is recorded rather than left pending forever.
- `TestOneMessageGoesToOneSender` — E9.5: the read and the claim are one statement, so
  a second worker of the same fleet finds nothing. `TestAMessageADeadGatewayLeftIsReturned`
  is what a takeover does with the rows a replaced gateway left in flight.
- `TestTheCeilingIsCountedFromWhatLeft`, `TestAHeldMessageKeepsItsPlace` — E9.6: the
  hourly ceiling is counted from the messages that actually left, and reaching it
  pushes the queue to later rather than parking it as failed.
- `TestAPhoneNumberBelongsToOneWorkspace` — one WhatsApp session belongs to the
  number, so a second tenant linking it is refused with this module's own error
  rather than a constraint violation, because Row Level Security hides the row that
  would have said so.
- `TestTheRowKnowsWhatAPersonChangedAndWhatTheGatewaySaw` — the number's own
  bookkeeping: the fields a person edits are written together and keep the ones they
  do not name, a status carries the reason, the heartbeat moves `last_seen_at` forward
  only (a late report from a replaced gateway cannot walk "is it alive" backwards), and
  `TestAcceptingTheWarningTwiceKeepsTheFirstMoment` is the risk's timestamp: the first
  acceptance stands.
- `TestAMessageIsFoundByWhatsappsOwnIdAndKeepsItsTask` — WhatsApp's id is the lookup
  key a redelivery reads, and the task a message opened stays on the row. Attaching a
  task to a message that is not there, or is another workspace's, is answered as "not
  found" rather than as done: the statement updates nothing in that case, and the
  first version answered success, which a test caught.
- `TestTheDirectoryIsReadWithoutATenant` — the one unscoped read in the codebase,
  and the test says what justifies it: it carries the tenant to scope under and
  nothing else.
- `TestEveryTriggerIsStoredWithItsOwnStatus`, `TestHandoffChainIsStoredWithItsDepthAndParent`,
  `TestStepsAreRecordedInOrder`, `TestTaskRuntimeIsTenantScoped` — the task
  schema, the handoff chain, and tenant isolation, against a real Postgres.

### Billing (EPIC 12)

The billing module's own tests are three layers, and each exists for something the
others cannot see:

- `domain/billing_test.go` — the arithmetic, which is what a careless refactor changes
  silently: when a workspace stops working (inside the period, then the grace window,
  then nothing but reads), what the allowance is (the package plus what was bought),
  and which thresholds a period crossed. A zero allowance is *unlimited* rather than
  exhausted, and the test says so, because reading it as zero would refuse every call
  of a package that bounds no tokens.
- `service/` — the decisions: which mirror of the usage is telling the truth when the
  two disagree, which thresholds are reported and in what order, when an invoice is
  issued, what a payment does, and that a replayed delivery buys nothing twice. The
  clock is a field rather than the wall clock, and that is not a convenience: every
  rule here is about a moment.
- `handler/` — the wire: that the numbers a screen needs are on it, that an absent
  value is an empty string rather than a zero uuid, and that each refusal maps to the
  status a screen branches on. `unverifiable_payment` is 400 rather than 500 because a
  provider told "internal error" would redeliver a body that can never verify.
- `internal/platform/payment/duitku` — the adapter, and the signature is the part that
  matters: it is checked against a body the test signed itself, with the wrong key, with
  an amount edited after signing, and with a callback for another merchant. `OrderID` is
  asserted to be short enough for Duitku's fifty-character field, because a truncated one
  would leave a callback naming something this build cannot find.
- `test/integration/billing_test.go` — against a real Postgres, the claims a mock would
  only restate: a workspace created through onboarding's own transaction has exactly one
  live trial subscription and the period it grants; the allowance is measured against the
  real ledger and a top-up adds to it in the period it was bought in; a settled invoice
  cannot be settled again; the billing tables are tenant-isolated by policy (naming
  another workspace's invoice by id is not enough, and a write from outside is refused by
  the policy rather than by the filter); the package's caps reach the Bolu registry and the
  member invitation; and the one unscoped read — the sweep's queue — is what tells the
  sweep which tenant to open.
- `test/integration/billing_quota_test.go` — E12.2's gate, and the one property only a
  real Redis and a real Postgres can answer: **concurrent callers sharing one allowance
  must not each get the whole of it.** Fifty callers start at once against an allowance of
  ten; the exact count that passes is not the property — the check is a read followed by a
  decision, so callers already inside when the allowance runs out finish — what must hold
  is that the allowance is *shared*, that the overshoot is bounded by the concurrency
  rather than unbounded, and that what they recorded is what the counter reads back and
  what the sweep then writes into the durable mirror. The other half is
  `TestTheCounterAndTheLedgerAgreeAfterReconciliation`, in both directions: a counter that
  holds more than the row (calls since the last reconciliation) and a Redis that lost its
  keys while the row remembers. The second is why the check takes the larger of the two —
  a flushed Redis must not hand a workspace its month again.
- `internal/container/billing_wiring_test.go` — where the read-only guard sits. It is
  registered on four route groups rather than inside each service, so "everything that
  writes stops" is one decision and a group somebody forgot is a write that would go
  through. The test enumerates the writes across every module, asserts reads are still
  answered, asserts the billing routes are exempt, and asserts that a subscription that
  cannot be *read* does not open writes.

The payment path is verified end to end against a stand-in for Duitku
(`internal/platform/payment/duitku`'s own shape): the inquiry is signed and answered, a
signed callback settles the invoice and the tokens land, a replayed callback buys nothing
twice, and a callback with a tampered signature is refused with 400.

### Audit and retention (EPIC 13)

Two properties, and both are about what happens to data after the fact — which is
why they are integration tests rather than units: a mock would restate the rule
instead of exercising the statement that enforces it.

- `test/integration/audit_test.go` — **every sensitive action leaves exactly one
  line, with the actor on it.** The four families the epic names are all here: a
  provider created, a connection granted, an invitation sent, and a memory a person
  removed — each recorded by its own module through its own adapter, which is what
  makes "every sensitive action" a claim about the deployment rather than about
  three of its modules. The actor is asserted over the *whole* trail in one loop,
  because four adapters are four places a dropped actor could hide. It runs through the real container, so the three
  adapters (workspace, integration hub, and model gateway) are under test, and the
  translation between each module's entry shape and the trail's is the thing that
  only the container can get wrong — a dropped actor would otherwise be a field
  nobody checks. The reads are asserted by what a person asks: one action, an action
  *prefix* ("show me the grant history"), the object that was touched, the Bolu that
  acted, and the person; and by two abutting windows holding every entry exactly
  once, because a half-open window is the difference between a report and a double
  count. One test is about refusal: an entry that cannot be encoded is rejected
  rather than stored as something else.
- `test/integration/retention_test.go` — **a step's content has a clock on it and its
  shape does not.** A step older than the window comes out of a real pass with
  `input` and `output` emptied and everything the history and the cost report are
  built from still there — the sequence, the kind, the labels, the tokens, the cost,
  and the task's one-line summary. What is younger than the window is untouched, and
  so is a workspace whose turn has not come, which is the assertion behind "nothing
  is removed outside the policy". A second pass over the same window removes nothing,
  which is what makes the job safe to re-run and safe on two instances at once. An
  attachment is asserted to leave as two halves: the row is gone *and* the bytes are
  gone from storage, because a row removed without its object is a leak no query will
  ever find — and the file inside the window still reads back. The queue is asserted
  to be filled by the database rather than by a caller's habit, since a workspace
  missing from it is one whose customer's words are kept forever.
- `internal/container/retention_wiring_test.go` — the container's own mistakes: a
  deployment without a database leaves the job unwired, the configured days become
  the windows (including the two different readings of a zero), and each adapter
  passes the workspace and the cutoff through rather than swapping one for the other.
- `internal/modules/retention/service/service_test.go` — the pass: what is pruned and
  what is not, that one workspace's failure does not stop the fleet and is still
  recorded on the run, that a pass which cannot read its queue still leaves a run
  row, and that an unavailable pruner is a zero rather than a failure.

### Tracing (EPIC 13)

The spans are asserted where they are started, and the properties that matter are as much about what
is absent as what is present:

- `internal/platform/telemetry/telemetry_test.go` — the provider: an unknown exporter is a startup
  failure rather than a deployment that silently reports nothing; a deployment with no exporter still
  has valid trace ids; sampling is parent-based, so a trace a caller sampled survives a ratio of zero
  *and* keeps the caller's trace id; the W3C header round-trips through the global propagator, which
  is the one the middleware uses; and a ratio outside 0..1 is clamped rather than handed to the
  sampler.
- `internal/platform/middleware/tracing_test.go` — the HTTP span. The load-bearing one is
  `TestTheRequestLogCarriesTheSpansTraceID`: the trace id in the log line and in the response header
  must be the span's own, or a line and the trace it names are two different requests. The second is
  `TestTheQueryStringNeverReachesTheSpan`, which is a security property rather than tidiness — the
  event stream authenticates from the query string, so a span holding the URL holds a token. The
  other two are that a failure's status is the status a trace shows, and that a service starting its
  own span from the request's context is a child of the request.
- `internal/modules/task/activity/tracing_test.go` — the task's own spans: the tool span is named for
  the tool (the question "which one was slow" is not answered by a call id), a model round names the
  provider and the model that *answered* so a fallback is visible, a nested step keeps its parent's
  trace id, and a deployment with no tracer still runs. The prompt, the arguments, and the results are
  asserted to be absent from every attribute.
- `internal/platform/temporal/logger_test.go` — the SDK's own lines go into the deployment's log: the
  key/value pairs become fields, a child logger does not mutate its parent (the SDK derives a
  workflow's logger from a shared one, so a mutation would put one workflow's id on another's lines),
  a dangling pair is still named, and the level is the deployment's.
- `internal/container/telemetry_wiring_test.go` — the container: the provider is installed for a
  deployment that named no exporter, an unknown exporter stops it, a container without telemetry asks
  Temporal for no interceptor and gets a no-op tracer rather than a nil dereference, and each binary
  names itself unless the deployment named the service.

The end-to-end check is by hand, because it is the one thing a unit test cannot show:
`OTEL_EXPORTER=console make smoke-task` prints both processes' spans, and the API's request span, the
worker's workflow and activity spans, and the model and tool spans under them all carry one trace id.

### Metrics (EPIC 13)

- `internal/platform/metrics/metrics_test.go` — the instruments themselves, read back through a
  manual reader: every product measure lands with the attributes the chart queries for, an unknown
  label value becomes `other` (which is the cardinality rule, and the only thing that makes these
  metrics affordable), an attempt number is bucketed rather than counted, the in-flight depth comes
  back down, the Prometheus endpoint serves the deployment's own measurements, a second provider in
  the same process does not share the first's registry, and a nil provider tolerates every recording
  call — which is what lets the modules call through unconditionally.
- `internal/modules/task/activity/metrics_test.go` — the runtime's own measures: a finished task
  reports the numbers the *stored row* holds (so a chart and the dashboard cannot disagree), the
  tool's integration is derived from its name, a call this deployment could not serve is counted as a
  failure rather than disappearing, a draft's readiness is measured from the task opening, and a
  direct call — which is not an activity — reports no retry rather than panicking.
- `internal/modules/approval/service/metrics_test.go` and `llm/service/metrics_test.go` — the
  decision (with the store's own status and revision counter) and the model call (with the purpose
  the caller stated, and whether the chain had to move on). Both assert that opening a draft is *not*
  a decision, which is what keeps a rate's numerator and denominator the same population.
- `internal/container/metrics_wiring_test.go` — the deployment's side: no exporter means no provider,
  an unknown one stops the process, the scrape route exists only when something is being scraped, the
  one adapter satisfies all three module ports, and no tenant appears as a label.
- `internal/container/ops_server_test.go` — the operations listener a worker serves: liveness answers
  while a dependency is down (which is what keeps an orchestrator from restarting a healthy process
  forever), readiness answers 503 while one is down, the scrape route exists only when something is
  scraped, and the listener is off entirely unless the deployment named an address.

### The usage and cost screen (EPIC 13)

- `internal/modules/llm/handler/report_test.go` — the report as the screen reads it: both breakdowns
  with the rupiah figure beside the micros, the window and the moment it started, the CSV as a
  rectangular table whose rows all carry every column (a shorter preamble is a file a strict reader
  refuses), and a 503 when the deployment has no reporting port rather than an empty period that
  would read as "this workspace spent nothing".
- `internal/modules/task/handler/attention_test.go` — the attention list: three of the four states
  and not the fourth, the order (failures first, because a parked task is holding work that is
  otherwise done), the Temporal link naming the namespace, the workflow *and* the run, no link when
  the deployment named no UI, and no link on a task that never reached the engine.
- `internal/container/usage_route_test.go` — the container's own registration, which is the property
  no handler test can see: the report under the API prefix where the tenant guard is, and
  `/tasks/attention` registered *before* `/tasks/:taskID` — Fiber matches in registration order, so
  the reverse would read "attention" as a task id.
- `test/integration/usage_report_test.go` — against a real Postgres, the reconciliation the epic asks
  for: a known set of ledger rows read back three ways, the total covering a call made outside any
  task, and the per-task rows deliberately *not* claiming it.

The end-to-end check is the smoke's own reconciliation: it runs the API and the worker, produces a
succeeded task and a failed one, and then compares the report, the export, and the attention list
against those rows (`scripts/check-usage.py`).

### Data policy and erasure (EPIC 13)

- `internal/modules/retention/service/erasure_test.go` — the order the two halves happen in
  (`objects` then `rows`, asserted as a sequence, because the reverse is what loses the storage
  keys), the evidence carrying the *names* read before the rows that hold them, each failure mode
  keeping its record without retrying, an incomplete deployment refusing rather than half-erasing,
  and an unreadable requester not stopping the erasure the person is entitled to.
- `internal/modules/retention/handler/handler_test.go` — the wire: the person comes from the session
  rather than the body (a test sends somebody else's id in the body and asserts it is ignored), a
  failure says nothing was half-removed, an incomplete deployment answers 503, and a workspace with no
  record answers 404.
- `test/integration/erasure_test.go` — the issue's own verification, against a real Postgres and a
  real object store: a workspace is filled through the modules' own services — conversation, message,
  attachment, task with steps, memory **with a vector**, **an audit line** — then erased, and then
  the **schema is asked** what is left. The audit line is not decoration: `audit_logs` is append-only
  and refuses a delete from anywhere but an erasure, so a fixture that leaves the trail empty never
  fires the trigger and a missing `app.audit_erase` goes unnoticed. That is exactly what happened —
  the fixture's own comment claimed a draft with its audit lines and wrote neither, and the erasure
  was broken for every workspace that had ever recorded a sensitive action. The test now fails without
  that flag, which is the property worth keeping.
  The count reads `information_schema` for the same reason the
  erasure's own count does, so a table the test forgot to check cannot make it pass; `data_erasures`
  is excluded deliberately, because it is the one table holding a workspace id without a reference and
  counting it would fail the test on the evidence that the erasure worked. The file asserts three
  things a mock cannot: no row anywhere, no object under either prefix, and the workspace row gone
  rather than flagged — plus the record still readable afterwards with its name.

### The order module (EPIC 14)

Six layers, and each exists for something the others cannot see:

- `internal/modules/order/domain/order_test.go` — the arithmetic a price is read through, which is
  what a careless refactor changes silently: micro-rupiah to rupiah and back, a fractional price
  keeping its sen, and `Rupiah` written the way a person writes it (no decimals on a whole amount, a
  fraction that rounds up carrying into the whole, a rounding artifact below a sen not written at
  all). Plus the message a model reads when it asks for something the catalogue does not sell — which
  is the whole of what it gets, so the test asserts it names what was asked for, what the catalogue
  has **with the price**, and what to do next.
- `internal/modules/order/service/service_test.go` — the decisions the service itself makes, with
  mockery mocks: `Lookup` marking an exact name or sku (case-folded) and leaving `Found` nil for a
  fuzzy-only answer while still returning the candidates; `Record` refusing an empty item list, a
  blank product name, and a non-positive quantity **before** the repository is called; the trimming;
  `DeleteProduct` on an absent id; and the filter limits.
- `internal/modules/order/adapter/adapter_test.go` — the tool contract, with a hand-written fake
  (the point is the JSON, not the mockery): the three tools are `app.action` with the right labels
  and schema-shaped arguments, an unknown tool is `ErrUnknownTool`, arguments that are not JSON are
  `ErrSchemaViolation` for all three, `find_product` answers a **success** for a product that does not
  exist (never an error — a model that receives a failure retries with a name it invents), and
  `record_order` forwards the scope's `AgentID`/`TaskID` and returns the port's error unresolved when
  the service refuses.
- `internal/modules/order/handler/handler_test.go` — the wire: prices travel in rupiah and are stored
  in micro-rupiah (`15000` in, `15_000_000_000` inside, `15000` back out), an edit sends only the
  fields it names with the rest nil, the status mapping a screen branches on (`duplicate_product`
  409, `not_found` 404, `invalid_input` 400), the identifiers omitted on the wire when they are zero,
  and the tenant of the request reaching the service rather than a zero value.
- `internal/container/order_wiring_test.go` — the dispatch rules, which are the product's rather than
  the module's: a `write_external` call refused at the executor even when the Bolu holds the grant (the
  second lock on the approval gate's door), a tool the Bolu was not offered refused even though the
  model asked for it, a `Connectionless` tool running with no connection and **without consulting the
  hub for a grant or a connection**, a connected tool resolving exactly the granted connection, the
  scope carrying `AgentID`/`TaskID` so a recorded order can be attributed, a failed tool answered as
  an `IsError` result rather than as a Go error that would end the task, and a catalogue row no adapter
  claims being dropped from the list the model is offered.
- `test/integration/order_test.go` — the claims only a real Postgres can answer: the catalogue is
  tenant-isolated by policy rather than by a filter; a recorded line carries the **catalogue's** price,
  name, and unit, with the generated line total and the order total agreeing; a later price change does
  not rewrite it; an unknown product rolls back **everything**, including the customer upsert; two
  orders from the same phone leave one customer; `Lookup` is case-folded and returns candidates; and
  the three `order.*` rows in `tool_catalog` agree with what the adapter declares — the agreement the
  code comments promise, and the one thing that needs both a database read and a Go read to check.

### Trying it by hand

`make demo` is not a check: it is the stack a person clicks through. It exists because `make up`
starts the product with no model provider, so the first task a person opens fails with "no provider is
configured" — and because the catalogue has to be filled through the API, which is not a screen the
product has yet. It runs a model stub and a mailbox stub on the host, points the backend at them
through `docker-compose.demo.yml`, and seeds an account, a workspace, a sales Bolu, five products, the
workspace memory, and a Gmail connection completed through the provider's consent flow.

Three things about it are worth keeping when it is changed, because each cost a debugging round:

- **the model stub must serve `/v1/embeddings`.** A memory is embedded before it is stored, so a
  deployment that can chat but cannot embed cannot remember anything — including the main memory the
  template's persona reads. It answers 500 with `provider returned 0 vectors for 1 inputs` and the
  message is not obviously about embeddings.
- **an unknown product is looked up, not answered from the stub.** The candidates live in the tool's
  own answer, so a stub that decides for itself hides the behaviour the epic asks for: the catalogue
  answers, with the products it does have.
- **a person's flow needs a channel, or there is no draft.** The Bolu composes the invoice and looks for
  a `*.send` tool; with none it says so and no card appears. The mailbox is connected for exactly that
  reason, and what it received is appended to `/tmp/bolu-demo-gmail-sent.jsonl`, so approving the card in
  the browser and then reading that file is how "approved" and "went out" are told apart by hand. The
  *assertion* on those two being the same message is the smoke's phase 5, not the demo's.

### Measuring what an order costs (EPIC 14)

`make measure-orders` (`scripts/measure-orders.sh`) runs the same stack as the smoke and sends the
MVP flow twenty times, one order at a time, then reads the numbers back from `usage_ledger` **through
the product's own report** — the same read the usage screen makes — so the figure is the product's
answer rather than a sum a script did itself.

Two properties are asserted rather than printed, and both are about the shape a package price stands
on: every measured order has a row in the report (a join that silently missed one would price a
package on half the orders), and the second half of the run does not cost more than 1.5× the first —
a flow that accumulated history or memory per task would drift upward, and that drift is what makes a
per-task price wrong six months later.

The rupiah figure it prints is the platform's price table applied to a stub's token counts, so the
run says so rather than implying a price: pricing needs staging with a real provider, which is the
part of E14.5 this repository cannot do for itself.

### The gate report (EPIC 14)

`make gate-report` (`scripts/gate-report.sh`) prints the numbers Gerbang 1 is decided on — tasks by
outcome, tool failures per integration, drafts approved without a revision, and the token and cost
spend per task and per order. It reads the database rather than the API, because the question is
about the rows themselves including the ones a workspace filter would hide, and it prints each
threshold beside its number rather than asserting it: it is a report for a decision, not a gate.

Two things it is careful about, and both are the reason it exists as a script instead of four queries
in somebody's shell history: **each definition lives in one place** — so the first and the fourth
version of "how many drafts were approved cleanly" cannot disagree — and **it says what it cannot
answer**. The rupiah column is the platform's price table times a provider's token counts, so against
the stub it is nearly zero and the script says so rather than letting a reader take it for a price.

### The end-to-end smoke

`make smoke-task` runs the API and the agent worker on the host against the
compose Postgres, Redis, and Temporal, with the model gateway pointed at a stub
that speaks the [OI]-compatible format. It is not a gate — it needs the compose
stack up — but it is what proves the wiring that unit tests script away:

1. a chat message becomes a durable task, the task runs out of process, records
   its rounds, and writes the answer into the thread it was asked in;
2. with `TASK_MAX_STEPS=1`, a task that keeps asking for a tool stops as failed
   with a `stopped_reason` a person can read, rather than looping;
3. the memory pipeline, which is the part that only exists in the wiring: a
   message asks to remember something and is confirmed by the module that stored
   the row; a later task answers with what was remembered, which proves the memory
   reached the provider rather than only the database (the stub only says so when
   it finds the text in its own prompt); a quiet conversation is read by the
   extraction workflow and the small talk in it is dropped; a fact said again with
   a new value leaves one active memory and the older version as history; a secret
   is refused; a document is chunked, embedded, and stored ready; and a delete
   takes the row out of the table for good;
4. **the usage screen's own reads** (`scripts/check-usage.py`), reconciled against
   the task rows the phase just produced: the report covers the failed task and
   counts at least its rounds, the per-Bolu and per-task breakdowns do not exceed
   the total, the CSV is the *same* read (its total equals the screen's), and the
   attention list carries the failed task with a Temporal link that names its
   workflow and run. It runs only when the deployment is asked for the other
   verification — `OTEL_METRICS_EXPORTER=prometheus` also makes the same script
   check the counters against the task row (`scripts/check-metrics.py`), including
   that the depth is back to zero across the two processes' counters.

`make smoke-approval` runs the same stack against the approval gate: a
`write_external` tool call becomes a draft and the task parks; a revision sends the
task back to its agent with the note; an approval is carried out against the provider
and the task succeeds because it actually did; and an approval outside working hours
is held with a reason the user can read.

`make smoke-order` runs it against the MVP flow — a customer's message becomes an
order in the workspace's own data, and the invoice that follows waits for a person.
Three phases, each a promise the product makes rather than a code path:

1. **the price comes from the catalogue.** The stub calls `order.find_product` and
   then `order.record_order` with a product name and a quantity and nothing else,
   because that is all the schema accepts. The order that lands is asserted field by
   field against the catalogue row — description, unit, unit price, the line total the
   database computed, and the order's total from its lines — and the recorded call is
   read back off the trail to show it never carried a price at all. The Bolu and the
   task are on the order.
2. **an order that cannot be taken is not taken.** The customer names a product the
   catalogue does not sell (`Kopi Arabika Gayo 1 kg`), the call is refused, the model
   is told what the catalogue does have — the suggestion list works in both directions,
   so a customer who says *more* than the catalogue does still gets an answer — and the
   order count is still one. Nothing was written, including the customer.
3. **nothing leaves without a person.** The Bolu reads the order back, composes the
   invoice with the total, and hands it to the sending tool; the draft that appears
   carries the recipient, the order's code, and the total, it appears in the thread as
   a card, the task parks as `waiting_approval`, `sent_at` is empty, and **the provider
   has received nothing**. The sending tool is `write_external`, which is what put it
   through the gate rather than out of the door.
4. **what a person approves is what leaves.** The draft is approved; the task finishes by
   itself (approving carries the action out without another model call), the draft reads
   `sent`, and the provider stub received **exactly one** message whose body carries the
   order's code, its line, and its total — the numbers a customer would quote back. The
   decision is on the audit trail with the person who made it.

The channel is read off the tools the deployment actually offers (`*.send` or
`*.send_message`), so the run exercises the flow rather than one integration: a deployment
with a WhatsApp gateway and one with only a mailbox take the same path and differ only in
which adapter runs at the end.

EPIC 9 has no workflow of its own: the gateway is a process, not a workflow. What it
does have is a **lease**, and that is tested against a real Redis in
`internal/platform/whatsapp/lease_test.go` rather than a fake, because the property
is "the value expires on its own" — a fake would only restate the test. The five
cases are: one instance holds a number and a second is refused, a lease whose owner
stops renewing is taken over without anybody cleaning up, a clean shutdown hands over
immediately, a predecessor's late release cannot delete its successor's lease, and
two numbers do not share one.

The seam between the API and the gateway is tested as HTTP rather than as a
function call, in `gateway_test.go`. It is the only place the QR could leak: the
gateway speaks for every tenant it serves, so a caller without a token is refused,
and a call without a workspace is refused — an operation that did not know whose
number it acts on would act on anybody's.

The gateway's device store is the one part of it with a fixed interface it does not
own, so every method is exercised in `store_roundtrip_test.go`: identities,
sessions, pre-keys, sender keys, app-state keys, versions and mutations, contacts and
chat settings, message secrets, privacy tokens, the event buffer, and the LID
mappings. A method that answers wrongly there is a handshake that fails or a message
that cannot be read, and the only symptom is "the number does not work". Two of those
methods are also where a real defect was found and fixed: the phone matching behind a
delete and a migration was a prefix match, so deleting or migrating `628999` took
`6289990`'s sessions with it; and the PN→LID migration re-keyed sessions by the JID
rather than by its signal address, so the sessions it moved were never found again —
the address is `<user>_<agent>:<device>`, where the agent carries the domain and a
LID's is `_1`. Both are stated in the test rather than in a comment elsewhere,
because the correct form is exactly what the previous version got wrong.

The pairing flow's own order is tested in `pairing_test.go`, because that order is the
part that cannot be recovered afterwards: the session is written *before* the workspace
is told, so a crash in between leaves a number the fleet can serve rather than one that
claims to be linked with nothing behind it. Each way a pairing can end — an expired
code, an outdated client, a phone without linked devices, a refusal with and without
words, a passkey prompt, a channel that closes without a verdict — is checked to leave
a reason a person can act on, and a paired attempt is checked not to be turned back
into a refusal by a late error event. `engine_flow_test.go` covers the fleet around it:
what is kept when the directory cannot be read, when a lease is taken over, and when a
renewal cannot be answered; the handler's routing of a message, a connect, a disconnect
and a refusal; and the drain's retry-versus-park decision.

Phase 3 needs the extraction workflow to be registered under the name the schedule
starts it with, which is a contract no compiler checks. The workflow's own test
(`TestTheWorkflowNameIsTheFunctionName`) pins it, because the failure it prevents
is a silent stall: the run is scheduled, no worker recognises the type, and the
retries look like a slow conversation.

A routine's run is registered the same way, under the name the scheduler starts it
with (`routine-<uuid>`), and its own test pins the two together
(`TestTheWorkflowNameIsTheFunctionName` in the routine workflow package). The
failure it prevents is the same silent stall, one level out: the schedule fires, no
worker knows `RoutineRunWorkflow`, and a routine that never runs looks like a
routine with nothing to do.

The worker-restart guarantee is not covered here: it is
`TestTaskSurvivesAWorkerRestart` in `test/integration`, which kills a real worker
mid-task and proves the completed rounds are not repeated.

**The compose workers poll the same Temporal task queue the smokes do**, so a
running `bolu-agent-worker` executes a smoke's task with its own model
configuration — `TASK_MAX_STEPS=1` is then never the bound that stops it, and the
smoke fails for a reason that has nothing to do with the code. Stop them first:

```bash
docker stop bolu-api bolu-agent-worker bolu-integration-worker
make smoke-task && make smoke-approval && make smoke-order
docker start bolu-api bolu-agent-worker bolu-integration-worker
```

### Coverage

`scripts/coverage.sh` runs `go test ./... -coverpkg=./...`. Instrumenting every
package means a package without tests counts as 0% instead of disappearing from
the report. Because each test binary then emits a profile for the whole module,
the script merges the profiles by taking the highest count per block before
computing the total.

Excluded from the gate, as the architecture document requires: generated code
(`sqlcgen`, `mocks`), `cmd/`, and — as development tooling rather than product
code — `tools/` and `migrations/`.

Override the threshold when you need to check a smaller change:
`MIN_COVERAGE=0 make cover`.

## Adding a module

1. `internal/modules/<name>/{domain,repository,service,handler}`.
2. Domain interfaces first; generate mocks with `make mocks`.
3. Wire the repository, service, and handler in `internal/container`.
4. Register the routes in `container.registerRoutes`.
5. Add `internal/modules/<name>/repository/queries/*.sql`, add the module to
   `sqlc.yaml`, and run `make sqlc`.
