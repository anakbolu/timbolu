# syntax=docker/dockerfile:1
#
# One Dockerfile builds the four backend images (api, agent-worker,
# integration-worker, wa-gateway). The BINARY build argument selects which cmd/
# package is compiled, so every image carries only the binary it runs.

FROM golang:1.26-alpine AS builder

ARG BINARY=api

WORKDIR /src

RUN apk add --no-cache git ca-certificates

# Dependency layer: only re-downloaded when the manifests change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is disabled so the binary is static and runs on the slim runtime image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BINARY}

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
# supplied through the environment (see .env.example).
COPY --from=builder /src/internal/platform/config ./internal/platform/config

ENV ENVIRONMENT=production

USER bolu

# The API and both workers listen on 8080; the WhatsApp gateway listens on 8090,
# which is also what it is told through WA_ADDRESS. Exposing both is what lets one
# Dockerfile serve every binary.
EXPOSE 8080 8090

CMD ["./app"]
