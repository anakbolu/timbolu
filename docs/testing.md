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
- `TestEveryTriggerIsStoredWithItsOwnStatus`, `TestHandoffChainIsStoredWithItsDepthAndParent`,
  `TestStepsAreRecordedInOrder`, `TestTaskRuntimeIsTenantScoped` — the task
  schema, the handoff chain, and tenant isolation, against a real Postgres.

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
