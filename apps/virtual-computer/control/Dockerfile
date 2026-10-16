# The control plane's own image: a static binary on a scratch-like base.
#
# The build context is this directory alone, not the repository root, and that is the
# whole reason it is cheap: the control module depends on nothing but the standard
# library, so no `replace` directive, no go.work, and no sibling copy of packages/ is
# needed. The backend's image is the opposite and its Dockerfile says so.
FROM golang:1.26-bookworm AS build

WORKDIR /src
# The module file first, so a source-only change reuses the dependency layer. There is
# nothing to download today; the split stays because the first dependency added should
# not cost a full rebuild.
COPY go.mod ./
RUN go mod download

COPY . .
# CGO off: the binary is a static HTTP server, and the runtime image has no libc.
RUN CGO_ENABLED=0 go build -trimpath -o /out/vcomputer-control .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/vcomputer-control /vcomputer-control

# The control plane speaks for every bot at once, so the port is the sandbox network's
# business and never the public one. Publishing it is a PoC convenience; the compose
# file binds it to 127.0.0.1 for exactly that reason.
EXPOSE 8090

USER nonroot:nonroot
ENTRYPOINT ["/vcomputer-control"]
