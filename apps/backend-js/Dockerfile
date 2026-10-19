# syntax=docker/dockerfile:1
#
# One Dockerfile builds the three Bun images (api, agent-worker,
# integration-worker). The APP build argument selects which entrypoint runs, so
# every image carries the same source and only the command differs — a TypeScript
# tree has no per-binary compile step, which is the whole reason the Go
# Dockerfile needed three builds and this one does not.
#
# **The build context is the repository root**, not this directory: that is what
# the deploy platform passes (and what the Go Dockerfile assumes too), so every
# COPY is prefixed with `apps/backend-js/`. Build it locally with:
#
#   docker build -f apps/backend-js/Dockerfile -t bolu-js:api .

FROM oven/bun:1.4-alpine AS deps

WORKDIR /app

# Dependency layer: only re-downloaded when the manifests change.
COPY apps/backend-js/package.json apps/backend-js/bun.lock ./
# --production leaves the dev types out of the image; the runtime never needs
# tsc, because Bun runs the TypeScript directly.
RUN bun install --frozen-lockfile --production

FROM oven/bun:1.4-alpine

ARG APP=api

WORKDIR /app

# ca-certificates for the provider and OAuth calls; tzdata because the product
# reasons about Asia/Jakarta work hours.
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S bolu && adduser -S -G bolu bolu && \
    # The local storage driver writes here. Created and owned by the runtime
    # user so a mounted volume inherits the ownership instead of arriving
    # root-owned and unwritable.
    install -d -o bolu -g bolu /data/storage

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
