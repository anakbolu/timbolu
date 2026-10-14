# Deployment

How Bolu is built, released, and rolled back. Written so that somebody who has never released this
repository can do it from this file alone, and so that every step that is still manual says so.

## The images

Five images come out of one repository, and each carries one process:

| Image | Dockerfile | Build arg | What it is |
| --- | --- | --- | --- |
| `api` | `apps/backend/Dockerfile` | `BINARY=api` | The HTTP and WebSocket surface, plus the billing sweep and the retention pass |
| `agent-worker` | `apps/backend/Dockerfile` | `BINARY=agent-worker` | Polls the `bolu-agent` Temporal queue and runs tasks |
| `integration-worker` | `apps/backend/Dockerfile` | `BINARY=integration-worker` | Polls the `bolu-integration` queue and runs the hub's own workflows |
| `wa-gateway` | `apps/whatsapp-gw/Dockerfile` | — | Holds the WhatsApp sockets, the sealed sessions, and the media bytes |
| `frontend` | `apps/frontend/Dockerfile` | — | The web app |

```bash
# The backend is one image, one binary: the same Dockerfile with a different build argument.
docker build -f apps/backend/Dockerfile --build-arg BINARY=agent-worker -t bolu/agent-worker .

# The gateway is a module of its own, so it has its own Dockerfile and one binary.
docker build -f apps/whatsapp-gw/Dockerfile -t bolu/wa-gateway .
```

The backend and the gateway both import the shared `packages/` module, and a Docker build does not read
`go.work`, so **both build with the repository root as the context** and resolve `packages/` through the
`replace` directive in their own `go.mod`.

They scale separately: the agent worker is the one that saturates, and a gateway image has no business
carrying the API's code or its Temporal client. `docker-compose.yml` builds all of them for local
development.

The frontend is built with `NEXT_PUBLIC_API_URL` and `NEXT_PUBLIC_CONTENT_ORIGIN` **as build
arguments**, because `NEXT_PUBLIC_*` values are inlined into the client bundle: a frontend built for
staging cannot be pointed at production afterwards. `NEXT_PUBLIC_CONTENT_ORIGIN` is the origin
sandboxed HTML documents are framed from and it must be a different origin from the app — a bundle
built without it falls back to the API's own URL, which is the sandbox escape the separate origin
exists to prevent.

## The migration step

**Migrations are applied before the new images serve traffic, by a separate step.** That ordering is
the whole design: a rollout that starts a new image before the schema matches it is an outage for as
long as the migration takes, and a rollback that reverts only the image leaves the schema ahead of the
code.

The migration runner is the repository's own (`apps/backend/tools/migrate`), which embeds the SQL
files with `go:embed` so the image and the migrations cannot disagree; `golang-migrate` applies them
with an advisory lock, which is what makes two runners safe.

```bash
# In the release pipeline, or by hand against a named database.
cd apps/backend
DATABASE_URL='postgres://…' go run ./tools/migrate up
DATABASE_URL='postgres://…' go run ./tools/migrate version
```

| Command | What it does |
| --- | --- |
| `up` | Applies every pending migration |
| `down` | Reverts **everything**, to an empty schema |
| `version` | Prints the applied version and whether it is dirty |

### Rolling back

Two different failures, two different answers, and mixing them up is how a rollback becomes a second
outage:

1. **The new images are bad, the migration was fine** — roll the images back. `docker-compose.yml` and
   the release workflow both tag every build with the commit sha, which is what a rollback names:

   ```bash
   docker pull "$REGISTRY/$PREFIX/api:$PREVIOUS_SHA"
   # …and point the service at it. The schema is unchanged, so nothing else is required.
   ```

2. **The migration itself is bad** — the images were never started, so the fix is the migration. A
   migration that failed part way leaves the schema *dirty*; `migrate version` says so. What to do is
   decided per migration, and the honest rule is:

   - A migration that failed before changing anything: fix the SQL and apply it again.
   - A migration that half-applied: write the *forward* fix as a new migration. Going back is only
     safe when nobody has served traffic on the new schema, and `down` reverts everything.
   - `down` is for a release that was never started, and for a staging environment being rebuilt from
     scratch. It is not a production rollback.

Every migration in this repository ships a `.down.sql` that reverses its own `.up.sql`, and
`TestMigrationsRunBothWays` proves both directions work against a real Postgres before anything is
released. What that test cannot prove is that a *downgrade* is lossless — dropping a column drops its
data — which is why the rule above is "forward fix" rather than "roll back".

## Environments

`ENVIRONMENT` selects the configuration, and each environment is a separate deployment with its own
database:

| `ENVIRONMENT` | Config file | For |
| --- | --- | --- |
| `local` | `config-local.yaml` | A developer's machine; docker-compose |
| `staging` | `config.yaml` + `config-staging.yaml` | The E2E scenario, with test accounts |
| `production` | `config.yaml` + `config-production.yaml` | Customer traffic |

The base file supplies every default; the environment's own file overrides only what differs, so a
log level or a retention window lives in one place and is not copied into two hundred-line files that
drift apart. **Every key is checked against the struct it maps to**: a typo in an overlay is a startup
failure rather than a setting that silently does nothing.

**Secrets are never in these files.** They arrive from the environment of the deployment:
`DATABASE_URL`, `REDIS_URL`, `TEMPORAL_HOST_PORT` and `TEMPORAL_API_KEY`, `JWT_SECRET`,
`SECRET_ENCRYPTION_KEY` (which seals provider API keys), `KMS_MASTER_KEY_ID`/`KMS_MASTER_KEY` (the key
ring every workspace's data key is wrapped with), the mail credentials, the Google and Gmail OAuth
credentials, and the payment provider's. `.env.example` lists them with what each one is for.

## Managed services

Per [ADR 0001](../docs/adr/0001-temporal-dan-region.md), everything that holds customer data lives in
**Jakarta** (`ap-southeast-3`) and Temporal Cloud lives in Singapore:

| Component | What it must be | Why |
| --- | --- | --- |
| PostgreSQL 16 + `pgvector` | Managed, **daily backups and point-in-time recovery**, in Jakarta | It is the only copy of the customer's data |
| Redis | Managed, in Jakarta | Live counters and rate-limit buckets; losing it costs pacing, not data |
| Object storage | S3-compatible, in Jakarta, **versioning off** | Attachment and sandboxed-content bytes; versioning would keep the copies an erasure is supposed to remove |
| Temporal | Temporal Cloud, Singapore | Holds workflow *history* — ids and timings, never content (claim check) |

Two operational requirements follow from that table and are not yet automated, so they are stated
rather than implied:

- **The erasure path must be verified against backups.** Restoring a backup taken before an erasure
  restores the data the erasure removed. Until a retention policy for backups exists
  ([ADR 0002](../docs/adr/0002-kepatuhan-uu-pdp.md), open questions), an erasure request is complete
  for the live database and the bucket, and the backup window is the gap.
- **Object storage must not have versioning or a soft-delete window enabled**, for the same reason.

## Health and shutdown

Every process serves probes and stops in an orderly way. A process with no HTTP API of its own serves
its probes on `OPS_ADDRESS` (`/health`, `/ready`, and `/metrics`); the API serves them on its own
listener.

| Process | Liveness | Readiness | Endpoint | Shutdown |
| --- | --- | --- | --- | --- |
| `api` | `GET /health` | `GET /ready` | own listener (`HTTP_ADDRESS`) | `ShutdownWithTimeout(10s)`, then the container closes |
| `agent-worker` | `GET /health` | `GET /ready` | `OPS_ADDRESS` | context cancel → worker stops polling; the container closes |
| `integration-worker` | `GET /health` | `GET /ready` | `OPS_ADDRESS` | same |
| `wa-gateway` | `GET /health` (own listener, with the gateway's routes) | `GET /ready` | `WA_ADDRESS` for the API, `OPS_ADDRESS` for the probes | engine stops and releases its leases first, then `server.Shutdown(15s)` |

- **Liveness never touches a dependency.** A probe that reported unhealthy because Postgres was down
  would have an orchestrator restart a process that is running perfectly, forever. That is readiness'
  job.
- **Readiness checks every configured dependency** and answers 503 with per-component detail while one
  is down, which is what a rollout waits for (`release.yml` polls `/ready`).
- **A gateway's lease is released on shutdown, before the server stops.** That is what makes a clean
  restart a takeover in milliseconds rather than a wait for the TTL — and what keeps two processes from
  holding one WhatsApp session.
- **Workers stop polling before the container closes.** An activity already running finishes; a task
  that was not picked up stays in the queue for the next worker, because Temporal owns it.

## Releasing

`.github/workflows/release.yml` runs on a tag (`v*`) or on demand, in this order:

1. **Gates** — each module's test suite with the race detector, plus its own 80% coverage gate.
2. **Images** — the three backend images, the gateway, and the frontend, pushed to
   `$REGISTRY/$IMAGE_PREFIX` tagged with both the commit sha and the ref.
3. **Migrations** — `tools/migrate up` against the environment's `DATABASE_URL`.
4. **Rollout** — only when asked for. It calls the environment's `DEPLOY_HOOK`, then polls `/ready`.

What a deployment must provide, once:

| Name | Kind | What it is |
| --- | --- | --- |
| `REGISTRY`, `IMAGE_PREFIX` | repository variable | Where images go |
| `REGISTRY_USERNAME`, `REGISTRY_PASSWORD` | secret | Registry credentials (defaults to GHCR and `GITHUB_TOKEN`) |
| `DATABASE_URL` | environment secret | The environment's own database |
| `DEPLOY_HOOK` | environment secret | The environment's own rollout — a Kubernetes rollout, an ECS update, a script |
| `STAGING_API_URL` | repository variable | Where the health wait polls; empty skips the wait |

Everything else — the Kubernetes manifests, the ECS task definitions, the systemd units — belongs to
the deployment rather than to this repository, because none of it is a fact about the product. What
this repository owns is the **order** (gate → images → migrate → rollout) and the images themselves.

## Running it locally

`make up` starts the whole stack from `docker-compose.yml`: Postgres with `pgvector`, Redis, Temporal
with its UI, a one-shot `migrate` service, the API, both workers, the WhatsApp gateway, and the
frontend. The compose file is the same shape as a deployment with everything on one host, which is why
it is worth reading before writing manifests: the services, the environment and the ordering are all
there.
