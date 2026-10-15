# syntax=docker/dockerfile:1
#
# The WhatsApp gateway image.
#
# It is its own Dockerfile rather than a BINARY argument on the backend's because
# the gateway is a module of its own: it does not import the backend, it carries
# no Temporal client, and it serves no product API. Building it from the backend's
# module would pull a dependency graph the process never uses and would make a
# change to either module rebuild both.
#
# The build context is the repository root, because the gateway imports the shared
# `packages/` module and a Docker build does not read `go.work`. The `replace`
# directive in apps/whatsapp-gw/go.mod points at the sibling copy.

FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

# Dependency layer: only re-downloaded when the manifests change. The shared
# module's manifest is copied first because the gateway's `replace` resolves to
# it, so `go mod download` cannot run until it is there.
COPY packages/go.mod packages/go.sum ./packages/
COPY apps/whatsapp-gw/go.mod apps/whatsapp-gw/go.sum ./apps/whatsapp-gw/
RUN cd apps/whatsapp-gw && go mod download

COPY packages ./packages
COPY apps/whatsapp-gw ./apps/whatsapp-gw

# CGO is disabled so the binary is static and runs on the slim runtime image.
RUN cd apps/whatsapp-gw && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/wa-gateway

FROM alpine:3.22

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

# The seam listener the API reaches, which is also what the process is told
# through WA_ADDRESS. The operations listener adds /ready and /metrics beside it
# when the deployment names one.
EXPOSE 8090

CMD ["./app"]
