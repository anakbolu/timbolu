# syntax=docker/dockerfile:1
#
# One Dockerfile builds the three Bun images (api, agent-worker,
# integration-worker). The APP build argument selects which entrypoint runs, so
# every image carries the same source and only the command differs — a TypeScript
# tree has no per-binary compile step, which is the whole reason the Go
# Dockerfile needed three builds and this one does not.
#
# **Debian slim, not Alpine.** `@temporalio/worker` loads a native core-bridge
# (`releases/x86_64-unknown-linux-gnu/index.node`) that is linked against glibc.
# On Alpine's musl the worker dies at import with
#
#   ERR_DLOPEN_FAILED ... linked against glibc (DT_NEEDED libdl.so.2),
#   but this Bun build uses musl
#
# The API never loads that module, so an Alpine API image works and an Alpine
# worker image cannot start — which is exactly how this was found: the api
# container came up healthy and both workers never appeared.
#
# **The build context is the repository root**, not this directory: that is what
# the deploy platform passes (and what the Go Dockerfile assumes too), so every
# COPY is prefixed with `apps/backend-js/`. Build it locally with:
#
#   docker build -f apps/backend-js/Dockerfile -t bolu-js:api .

FROM oven/bun:1.4-slim AS deps

WORKDIR /app

# Dependency layer: only re-downloaded when the manifests change.
COPY apps/backend-js/package.json apps/backend-js/bun.lock ./
# --production leaves the dev types out of the image; the runtime never needs
# tsc, because Bun runs the TypeScript directly.
RUN bun install --frozen-lockfile --production

FROM oven/bun:1.4-slim

ARG APP=api

WORKDIR /app

# ca-certificates for the provider and OAuth calls; tzdata because the product
# reasons about Asia/Jakarta work hours.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --system bolu && useradd --system --gid bolu --create-home bolu && \
    # The local storage driver writes here. Created and owned by the runtime
    # user so a mounted volume inherits the ownership instead of arriving
    # root-owned and unwritable.
    mkdir -p /data/storage && chown -R bolu:bolu /data

COPY --from=deps /app/node_modules ./node_modules
COPY apps/backend-js/package.json apps/backend-js/tsconfig.json apps/backend-js/bun.lock ./
COPY apps/backend-js/src ./src

# The YAML defaults ship with the image; every secret and endpoint is supplied
# through the environment. The loader reads them from src/platform/config.
ENV ENVIRONMENT=production
ENV NODE_ENV=production
# ARG is build-time only; the CMD needs the value at runtime, so it is promoted
# to an environment variable. `--build-arg APP=agent-worker` still selects it.
ENV APP=${APP}

USER bolu

# The API and both workers listen on 8080; the API's internal seam listener is on
# 8091 and is never published.
EXPOSE 8080 8091

# Shell form so ${APP} expands. `exec` so the process is PID 1 and receives
# SIGTERM directly — the entrypoints install their own shutdown handlers.
CMD ["sh", "-c", "exec bun run src/cmd/${APP}.ts"]
