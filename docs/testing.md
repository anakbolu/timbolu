# Testing and quality gates

Everything in this document is enforced by `make ci` locally and by
`.github/workflows/ci.yml` on every pull request. A failing gate blocks the
merge.

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
- `TestRedisClientAgainstARealServer` — the shared Redis client connects.
- `TestWorkerRunsAgainstTemporal` — the container connects to Temporal, the
  worker registers a workflow, the workflow executes, and the worker stops
  cleanly.
- `TestTaskSurvivesAWorkerRestart` — the gate the durable runtime exists for: a
  worker is killed while a model call is in flight, a second worker takes over,
  and the task finishes with each completed round recorded exactly once. The
  model and the tools are stubs; what is under test is the durable execution.
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
   with a `stopped_reason` a person can read, rather than looping.

The worker-restart guarantee is not covered here: it is
`TestTaskSurvivesAWorkerRestart` in `test/integration`, which kills a real worker
mid-task and proves the completed rounds are not repeated.

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
