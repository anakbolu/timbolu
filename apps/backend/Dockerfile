# syntax=docker/dockerfile:1
#
# One Dockerfile builds the three backend images (api, agent-worker,
# integration-worker). The BINARY build argument selects which cmd/ package is
# compiled, so every image carries only the binary it runs.
#
# The build context is the repository root rather than this directory, because
# the backend imports the shared `packages/` module and a Docker build does not
# read `go.work`. Both modules are copied in and the `replace` directive in
# apps/backend/go.mod points at the sibling copy, which is the same layout a
# checkout has.
#
# The WhatsApp gateway is a module of its own and has its own Dockerfile; it is
# no longer built from here, because it no longer shares this module's dependency
# graph or its binary.

FROM golang:1.26-alpine AS builder

ARG BINARY=api
# The deploy identity the build is told it has. Both default to "unknown" and
# currently always stay "unknown" in Easypanel: it has no separate build-argument
# field, so a build argument can only come from a service environment, which is
# static per service and cannot name a commit that appears at push time. They are
# declared so a build that does know them can pass them.
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /src

RUN apk add --no-cache git ca-certificates

# Dependency layer: only re-downloaded when the manifests change. The shared
# module's manifest is copied first because the backend's `replace` resolves to
# it, so `go mod download` cannot run until it is there.
COPY packages/go.mod packages/go.sum ./packages/
COPY apps/backend/go.mod apps/backend/go.sum ./apps/backend/
RUN cd apps/backend && go mod download

COPY packages ./packages
COPY apps/backend ./apps/backend

# The deploy identity the build can compute by itself: a hash of every Go file
# it compiles. `.git` is excluded from the build context, so a commit SHA is not
# readable here, but the tree is: the same source always yields the same value,
# and any source change yields a different one. Computed after both COPYs (the
# file set is the image's own) and before the build, then read back by the next
# step. `sha256sum`, `sort`, `xargs`, and `cut` are busybox applets in this
# image, so no extra package is needed.
RUN find apps/backend packages -name '*.go' -type f | LC_ALL=C sort | xargs cat | sha256sum | cut -c1-16 > /tmp/fingerprint

# CGO is disabled so the binary is static and runs on the slim runtime image.
# The -X values are what GET /api/health reports: a caller compares the build
# fingerprint with the one computed from the merged tree, which is the only way
# to prove from outside that this image — not the previous one — is serving.
RUN cd apps/backend && FINGERPRINT="$(cat /tmp/fingerprint)" && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/anakbolu/timbolu/apps/backend/internal/platform/buildinfo.Fingerprint=$FINGERPRINT -X github.com/anakbolu/timbolu/apps/backend/internal/platform/buildinfo.Commit=$GIT_COMMIT -X github.com/anakbolu/timbolu/apps/backend/internal/platform/buildinfo.BuiltAt=$BUILD_TIME" -o /out/app ./cmd/${BINARY}

FROM alpine:3.22

ARG BINARY=api

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S bolu && adduser -S -G bolu bolu && \
    # The local storage driver writes here. The directory is created and owned by
    # the runtime user, so a mounted volume inherits the ownership instead of
    # arriving root-owned and unwritable.
    install -d -o bolu -g bolu /data/storage

COPY --from=builder /out/app ./app
# The non-local YAML defaults ship with the image; every secret and endpoint is
# supplied through the environment (see .env.example). They live under the shared
# module's path, which is where the loader looks for them in a container.
COPY --from=builder /src/packages/platform/config ./packages/platform/config

ENV ENVIRONMENT=production

USER bolu

# The API and both workers listen on 8080. The internal seam listener the API
# opens for the gateway is on 8091 and is never published.
EXPOSE 8080 8091

CMD ["./app"]
