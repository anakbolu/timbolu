# The control plane's own image: a static binary, plus the docker CLI it drives.
#
# The build context is this directory alone, not the repository root, and that is the
# whole reason it is cheap: the control module depends on nothing but the standard
# library, so no `replace` directive and no sibling copy of packages/ is needed. The
# backend's image is the opposite and its Dockerfile says so.
FROM golang:1.26-bookworm AS build

WORKDIR /src
# The module file first, so a source-only change reuses the dependency layer. There is
# nothing to download today; the split stays because the first dependency added should
# not cost a full rebuild.
COPY go.mod ./
RUN go mod download

COPY . .
# CGO off: the binary is a static HTTP server.
RUN CGO_ENABLED=0 go build -trimpath -o /out/vcomputer-control .

# The runtime image carries the docker CLI, because that is how this control plane
# talks to the daemon: one `docker` command per operation, argv only, never a shell
# (docs/adr/0003-virtual-computer.md §3.2). It is the same client an operator would use
# by hand, which is what makes every call reproducible from a terminal.
#
# It runs as root, and that is a real trade-off, not an oversight: the process needs to
# write to /var/run/docker.sock, and the socket is owned by root. Anyone who can reach
# this container's HTTP surface can start a container as root on the host, so the
# token (VCOMPUTER_CONTROL_TOKEN) is not optional and the port is bound to the host's
# loopback only. The PoC accepts this because the alternative — a Docker API proxy that
# filters requests — is more machinery than the PoC can justify; a deployment that
# needs it puts the socket behind such a proxy or moves to a runtime with an API
# (docs/adr/0003-virtual-computer.md §3.3, §3.7).
FROM docker:27-cli

# The docker CLI needs nothing from the build stage but the binary; keep the layers
# apart so a rebuild of the binary does not re-download the CLI.
COPY --from=build /out/vcomputer-control /usr/local/bin/vcomputer-control

# The control plane speaks for every bot at once, so its port is the sandbox network's
# business and never the public one. The compose file binds it to 127.0.0.1 for exactly
# that reason.
EXPOSE 8090

# wget comes from busybox in this image and is what the compose healthcheck uses.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8090/health || exit 1

ENTRYPOINT ["/usr/local/bin/vcomputer-control"]
