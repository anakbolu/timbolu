# Deployment

How Bolu is built, released, and rolled back. Written so that somebody who has never released this
repository can do it from this file alone, and so that every step that is still manual says so.

## The images

Five images come out of one repository, and each carries one process:

| Image | Dockerfile | Build arg | What it is |
| --- | --- | --- | --- |
| `api` | `apps/backend/Dockerfile` | `BINARY=api` | The HTTP and WebSocket surface, plus the billing sweep, the retention pass, and the staged sweep |
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

`NEXT_PUBLIC_GOOGLE_LOGIN_ENABLED` is a third build argument, and it decides whether the sign-in
pages render the Google button at all. It is `false` unless a deployment has registered a Google
client, because a button that leads to a `google_not_configured` answer is worse than no button.
On the backend the matching rollout switch is `AUTH_REQUIRE_EMAIL_VERIFICATION`: it is `true` by
default, and setting it `false` lets a fresh account use the product without confirming its address
(see [api.md](./api.md)). Both are deployment decisions, so they belong in the environment rather
than in the shared config files.

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

## Deploying on push to `main`

`.github/workflows/deployment.yml` is the everyday path: a push to `main` redeploys whichever
services the commit touched, by calling each service's Easypanel deploy webhook (`/api/deploy/<token>`).
There are no tests in it — the gates are a separate concern — and the mapping is path -> service:

| Changed | Redeployed |
| --- | --- |
| `apps/frontend/**` | `timbolu` (the web app) |
| `apps/backend/**` | `timbolu-backend`, `timbolu-agent-worker`, `timbolu-integration-worker` |
| `apps/whatsapp-gw/**` | `timbolu-wa-gateway` |
| `packages/**` | every Go service — `timbolu-backend`, the two workers, and `timbolu-wa-gateway` |
| `.dockerignore`, `Makefile`, `go.work`, `go.work.sum`, `docker-compose*.yml` | every service — `timbolu`, the three backend services, and `timbolu-wa-gateway` |
| `apps/backend/migrations/**` | `timbolu-migrate`, applied first |

`apps/whatsapp-gw` is a module of its own, so a change to it rolls only the gateway; `packages/` is
the module both Go processes import, so a change there rolls all four Go services. The root build
files belong to no app, which is why they used to match nothing and a commit touching only
`.dockerignore` finished green with every deploy job skipped. They shape every image — `.dockerignore`
decides what enters every build context, and a wrong line there is what broke the gateway build — so
they roll everything, the frontend included.

### What each Easypanel service is configured with

A path in the table above is a path that *triggers* a service, not the directory the service builds
from: every Go service builds from the repository root, because each module imports the shared
`packages/` module and a Docker build does not read `go.work`. The service's own settings carry that,
so they are stated here rather than left to whoever set the service up first:

| Service | Source path | Dockerfile | Build arg | Notes |
| --- | --- | --- | --- | --- |
| `timbolu` | `/apps/frontend` | `Dockerfile` | — | `NEXT_PUBLIC_*` are build args, see above |
| `timbolu-backend` | `/` | `apps/backend/Dockerfile` | `BINARY=api` | serves the internal seam on `:8091` |
| `timbolu-agent-worker` | `/` | `apps/backend/Dockerfile` | `BINARY=agent-worker` | |
| `timbolu-integration-worker` | `/` | `apps/backend/Dockerfile` | `BINARY=integration-worker` | |
| `timbolu-wa-gateway` | `/` | `apps/whatsapp-gw/Dockerfile` | — | listens on `:8090`, never published |
| `timbolu-migrate` | `/` | `apps/backend/Dockerfile` | `BINARY=../tools/migrate` | one-shot, runs `up` |

The build arg is not a separate Easypanel field: Easypanel passes a service's environment values to
the Docker build as build arguments, so the `BINARY=…` line in the service's environment is what
selects the binary, and the same variable is present in the container at runtime. The Build Path is
both where the Dockerfile path is resolved from and what the build context is, which is why the
Dockerfile paths above are repository-relative while the source path is `/`.

**All four data processes share one storage directory.** `STORAGE_DRIVER=local` with
`STORAGE_LOCAL_ROOT=/data/storage` means the bytes live on the filesystem, and the gateway is the
process that *writes* an inbound attachment while the API is the process that *reads* it back to
serve it. Easypanel names a volume per service, so a per-service volume would give each process its
own empty directory and an attachment stored by one would be a 404 in the other. Each of
`timbolu-wa-gateway`, `timbolu-agent-worker`, and `timbolu-integration-worker` therefore bind-mounts
the API's own volume directory at the same path:

```text
/var/lib/docker/volumes/timbolu_timbolu-backend_storage/_data  ->  /data/storage
```

The API keeps its named volume, which is what owns the directory; the others mount it, so there is one
copy of the bytes and no service that can be rolled into an empty one.

The gateway's environment carries the seam in the direction it does not already have: `WA_API_URL`
(`http://timbolu_timbolu-backend:8091`) and `WA_GATEWAY_TOKEN`, the same token the API reaches the
gateway with. `INTERNAL_ADDRESS=:8091` on `timbolu-backend` is what makes that listener exist; it is
never published, and leaving it empty is what a deployment without a gateway wants.

The migration job runs before the others and they wait for it, so the schema is applied before a new
image serves traffic — the same order as the release flow, and for the same reason. Note what the wait
covers: the workflow waits for the migration to be *triggered*, not for it to finish, because the
deploy hooks are fire-and-forget. Each hook call is retried (`--retry 5 --retry-all-errors`) rather
than trusted once: the hook answers as soon as the build is queued, so a dropped connection is not a
failed deployment, and treating it as one leaves that service on the previous image.

`timbolu-temporal` and `timbolu-temporal-ui` run upstream images and have no path in this repository,
so nothing in the workflow triggers them.

What this workflow needs, once — six repository secrets, one per service:

| Name | What it is |
| --- | --- |
| `TIMBOLU_FRONTEND_DEPLOY_HOOK` | The `timbolu` service's deploy webhook URL |
| `TIMBOLU_BACKEND_DEPLOY_HOOK` | `timbolu-backend` |
| `TIMBOLU_AGENT_WORKER_DEPLOY_HOOK` | `timbolu-agent-worker` |
| `TIMBOLU_INTEGRATION_WORKER_DEPLOY_HOOK` | `timbolu-integration-worker` |
| `TIMBOLU_WA_GATEWAY_DEPLOY_HOOK` | `timbolu-wa-gateway` |
| `TIMBOLU_MIGRATE_DEPLOY_HOOK` | `timbolu-migrate` |

Each URL is the service's own, from its Easypanel page (Deploy Webhook), and it is a credential:
whoever holds it can trigger a deployment.

### Proving the deploy actually landed

**A green `Deployment` run is not a deploy.** The workflow calls each service's deploy webhook
(`/api/deploy/<token>`), and the hook answers when Easypanel has *queued* the build; the build itself
runs on the panel's server after that HTTP call has already returned. A green run therefore proves
the hook was accepted and nothing more: a build that fails on the panel, a rollout that never replaces
the container, or a hook pointing at the wrong service all leave the run green while production keeps
serving the previous image.

The workflow's `verify` job (`Verify the deploy landed`) narrows that gap, and it is the automated
form of this section. It fails when a deploy job that should have run failed or was skipped. For a
backend change it computes the fingerprint from the tree the run checked out and polls `/api/health`
for up to eight minutes until production reports it. For a frontend change it polls `/login` until the
page's chunk is newer than the commit. Its last step polls `/api/ready` and `/` for three minutes, and
that step proves only that the services are alive — it can pass on the previous image.

What no run can do is finish the deploy. When a build does not land, the evidence and the fix are on
the panel, so the checks below are how somebody with access proves a change is live.

#### Frontend

Next writes content-hashed file names, so the assets a page serves name the build they came from.
Build the tree you merged with the **same `NEXT_PUBLIC_*` values the `timbolu` service carries**:
they are inlined into the bundle, so a different value changes the bytes — and therefore the chunk
names — even for an identical tree.

```console
$ cd apps/frontend
$ npm ci
$ NEXT_PUBLIC_API_URL=https://timbolu.com \
  NEXT_PUBLIC_CONTENT_ORIGIN=https://timbolu-timbolu-backend.auyxz7.easypanel.host \
  NEXT_PUBLIC_GOOGLE_LOGIN_ENABLED=false \
  npm run build
$ shasum -a 256 .next/static/chunks/*.css
3971f367ab9c9f81991cba66d89d2b927da60a4ca85891032e7da99dec440db0  .next/static/chunks/25cvzt95clchu.css
$ grep -o '/_next/static/chunks/[^"]*\.js' .next/server/app/login.html | sort -u
```

The same values, read back from production (`25cvzt95clchu.css` is the name the local build above
produced; substitute the one your build prints):

```console
$ curl -s 'https://timbolu.com/login?verify=1' | grep -o '/_next/static/chunks/[^"]*\.js' | sort -u
$ curl -s 'https://timbolu.com/_next/static/chunks/25cvzt95clchu.css' | shasum -a 256
3971f367ab9c9f81991cba66d89d2b927da60a4ca85891032e7da99dec440db0  -
```

Equal chunk lists mean production serves the build this tree produces; a name in one list and not the
other means it serves a different build. The stylesheet hash is the weaker half, and the reason is
worth stating: the CSS chunk changes only when the CSS does, so it proves the build for a change that
touched it and says nothing about one that did not. The page's chunk list moves with the page's own
code, which is why the workflow compares a chunk's `Last-Modified` against the commit time rather than
these names.

#### Backend

The API answers the remaining question itself, because `GET /api/health` (`GET /health` too) carries
the build identity alongside the liveness status:

```console
$ curl -s https://timbolu.com/api/health | jq .data
{
  "status": "ok",
  "commit": "unknown",
  "built_at": "unknown",
  "build": "8c258b2de0c32616"
}
```

`build` is the first 16 hex characters of the SHA-256 over every Go file of the backend and the shared
module, in sorted order — computed inside the image at build time, not passed in:

```console
$ find apps/backend packages -name '*.go' -type f | LC_ALL=C sort | xargs cat | shasum -a 256 | cut -c1-16
8c258b2de0c32616
```

Run that from the tree you merged. Equal values are the only proof that the Go binary serving traffic
is that tree; a different value means the previous image is still up, so the deploy is not done, and
an absent `build` means the image predates the field. The hash is over content only, so the same
source always yields the same value and any change to a `.go` file changes it.

`commit` and `built_at` stay `"unknown"` on Easypanel for a structural reason: build arguments in
Easypanel come from a service's environment, which is static per service, so nothing can name a commit
that only exists after a push; and `.git` is excluded from the build context, so the Dockerfile cannot
read it. `built_at` has the same problem for a different reason — an Easypanel build has no
per-build variable to fill it. The Dockerfile already accepts `GIT_COMMIT` and `BUILD_TIME` build
arguments, so once the panel is reachable those two can be filled in (a build pipeline that passes
`--build-arg GIT_COMMIT=$(git rev-parse --short HEAD) --build-arg BUILD_TIME=$(date -u +%FT%TZ)` sets
both); until then `build` is the deploy identity, and `"unknown"` in the other two is the honest
answer rather than a broken probe. A local `go run` answers `"unknown"` for all three, which is why
the probes are only evidence of a *container* deploy.

#### Failure modes this repository has already hit

| Symptom | What it means | What the access holder does on the panel |
| --- | --- | --- |
| `/api/health` has no `data.build` | The serving binary predates the build-identity field (#303), so no backend build has landed since 2026-10-28T20:25Z. This is #329. | `timbolu-backend` → Deployments → the last build after the merge time: did it fail, and with what error? Then check the service's environment still carries `BINARY=api`. |
| The hook answered `success` and the container was not replaced | A rollout, environment, or build-arg problem on the panel side, not in this repository (#329, #200). | Same Deployments list. A green build with the old container still serving is the rollout's problem; a missing `BINARY=api` builds the wrong binary. |
| The frontend build fails on `Module not found: @timbolu/contracts/events` | The tree was broken and is fixed in-repo by #328 — but a repository fix only lands once the panel's build is green again. | `timbolu` → Deployments → the last build: read the error. A failing build never produces the chunk names above, so the frontend stays on the previous image however often the workflow reruns. |

None of these is fixable from this repository: each needs the build log or the service settings, and
no panel credential lives here. The repository's part is to make the failure visible — the fingerprint
and chunk checks above — and to keep the build green.

#### The honesty rule

A change is **live** only with the evidence above: the backend fingerprint equal to the tree you
merged, or the frontend's CSS hash and page chunk names equal to a local build of that tree. Three
things are routinely mistaken for that evidence, and none of them is it:

- A green `Deployment` run — the hook was accepted; the build was not proven (#322, #329).
- A green `/api/ready` — it proves the API reaches its dependencies, which the *old* image does just as
  well.
- A `200` on the page — it proves a frontend is serving, not which one.

So when a build has not landed, say so. "Merged, not deployed yet" is an accurate state, and a bug
that is fixed on `main` and still visible in production is usually this rather than a second bug.

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

The virtual computer PoC is not part of `make up`: it is a compose project of its own
(`apps/virtual-computer/docker-compose.poc.yml`) and the product starts none of it. The worker's
three `VCOMPUTER_*` settings come from `.env` through `docker-compose.yml`, and reaching a running
control plane needs the optional overlay, which puts that worker on the PoC's network
(`docker-compose.vcomputer.yml`, and `apps/virtual-computer/README.md` for the two commands).
