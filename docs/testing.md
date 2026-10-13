# Testing and quality gates

Everything in this document is enforced by `make ci` locally and by
`.github/workflows/ci.yml` on every pull request.

**The workflow does not currently run.** Every CI run since the first commit has
failed in about three seconds without producing a step log, which is the shape of
an account that cannot execute workflows rather than of a failing gate — nothing
in this document can fail that fast. The red check on a pull request therefore says
nothing about the change, and the merge button is not blocked by it. Until that is
resolved, **run `make ci` locally and say so in the pull request**, because the
automated gates are not enforcing anything.

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
   takes the row out of the table for good.

`make smoke-approval` runs the same stack against the approval gate: a
`write_external` tool call becomes a draft and the task parks; a revision sends the
task back to its agent with the note; an approval reaches the send boundary and
stops there honestly, because no integration executor exists yet (EPIC 8); and an
approval outside working hours is held with a reason the user can read.

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
make smoke-task && make smoke-approval
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
