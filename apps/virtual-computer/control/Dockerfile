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
# It runs as root, and that is a leftover rather than a requirement: this process never
# opens /var/run/docker.sock — the CLI inside it is pointed at the socket proxy with
# DOCKER_HOST (docker-compose.poc.yml) — and it binds an unprivileged port, so nothing in
# the image needs uid 0. It stays root because the CLI is the stock docker:27-cli user and
# the PoC has no reason to add a second one; the privilege is written down here because it
# is no longer what protects the host. The proxy's allowlist is: no socket in this container,
# and the Engine API it can reach is containers, volumes, exec, start and stop — no images, no
# swarm, no secrets, no plugins, no system. That list is per API *section*, which is coarser
# than it sounds: CONTAINERS also admits restart, kill and rm, and it lets a caller create a
# container with any HostConfig, so a compromised control plane remains a serious incident
# (docs/adr/0003-virtual-computer.md §3.3, §3.7). The token (VCOMPUTER_CONTROL_TOKEN) and the
# loopback-only port still gate the HTTP surface.
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
