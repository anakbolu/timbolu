# Sandbox desktop for the Bolu virtual computer (PoC).
#
# One container per bot holds an X server, a window manager, a VNC bridge, a
# Chromium browser, and the tools that drive them: `scrot` for the screenshot,
# `xdotool` for the click and the typing. The control plane is a separate Go
# service: it creates this container, mounts the session volume, runs those
# tools through `docker exec`, and proxies noVNC. No secret is ever baked into
# the image; the control plane injects what the session needs at runtime.
#
# The image only ships the pieces; the startup order lives in entrypoint.sh and
# is fixed: Xvfb -> fluxbox -> x11vnc -> noVNC (websockify) -> Chromium ->
# vcomputer-gate. The gate is started last and is the only listener on the
# container's own address: Chromium and websockify bind loopback, and the gate is
# what terminates the DevTools and noVNC traffic the control plane sends.
#
# Isolation rules from docs/riset-microvm.md apply here too: no --privileged,
# no /dev/kvm, no Docker socket, and the browser runs as a non-root user. The
# production path for stronger isolation is gVisor on Kubernetes, where the
# container boundary is real (see the --no-sandbox TODO in entrypoint.sh).

# The gate is a module of its own with a dependency graph of its own (the
# standard library and nothing else), so it is built in a stage and copied in as
# a static binary; the runtime image has no Go toolchain. The build context is
# ./sandbox, so gate/ is in scope and no `replace` directive or sibling copy of
# packages/ is needed. It is the same base image tag the control plane builds on.
FROM golang:1.26-bookworm AS gate

WORKDIR /src
COPY gate/ /src/
# CGO off, -s -w: the binary is a static HTTP proxy and is never debugged in
# place. There is deliberately no `go mod download` layer: the module imports
# nothing outside the standard library, so there is no go.sum to fetch and the
# copy above is the whole build context.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/vcomputer-gate .

FROM debian:bookworm-slim

# Every package the desktop needs, in one layer so the apt lists can be deleted
# in the same step. Sizes worth knowing: chromium is by far the heaviest entry
# (~500 MB unpacked) and it is also the only one that could be swapped for a
# smaller build if image size ever becomes a problem. Nothing is dropped in the
# PoC, so there is no commented-out package below.
#
# - xvfb, x11-utils       : headless X server, plus the clients used to probe it
# - x11vnc                : exports the X display as VNC, bound to localhost
# - novnc, websockify     : the browser-side VNC client and the ws<->tcp bridge.
#                           The assets land in /usr/share/novnc (what
#                           entrypoint.sh serves) and /usr/bin/websockify comes
#                           from the python3-websockify dependency that the
#                           websockify package pulls in.
# - chromium              : the browser Bolu actually drives (CDP on 127.0.0.1)
# - scrot, xdotool        : capture the display, and move/click/type on it
# - fluxbox               : tiny window manager, so Chromium gets a real frame
# - fonts-dejavu-core, fonts-liberation : without fonts, pages render as boxes
# - procps, curl          : process tools and the healthcheck probe
# - ca-certificates       : TLS trust store, Chromium needs it for HTTPS
# - tini                  : PID 1 that reaps zombies and forwards signals
#
# The gate is not a package: it is built in the stage at the top of this file and
# copied in as a static binary, so it adds nothing to the list above.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb \
      x11-utils \
      x11vnc \
      novnc \
      websockify \
      chromium \
      fluxbox \
      fonts-dejavu-core \
      fonts-liberation \
      procps \
      curl \
      scrot \
      xdotool \
      ca-certificates \
      tini \
 && rm -rf /var/lib/apt/lists/*

# Non-root desktop user. uid/gid 1000 matches the owner used for the session
# volume, so a mounted /home/bolu does not need a chown dance at startup.
RUN groupadd --gid 1000 bolu \
 && useradd --uid 1000 --gid 1000 --create-home --home-dir /home/bolu --shell /bin/bash bolu

# fluxbox's default init points session.screen0.rootCommand at `fbsetbg -l`, and
# that call ends in an fbsetbg error dialog on the desktop — a stray xmessage
# window in every screenshot QA takes. This init drops the command and the
# background setting, so the window manager has nothing to shell out to. It is
# bolu's file, not root's: fluxbox rewrites ~/.fluxbox/init when it exits, so the
# file has to stay writable by the user that runs it.
RUN install -d -o bolu -g bolu -m 0755 /home/bolu/.fluxbox \
 && printf '%s\n' \
      'session.screen0.rootCommand:' \
      'session.screen0.background: none' \
      > /home/bolu/.fluxbox/init \
 && chown bolu:bolu /home/bolu/.fluxbox/init

# Belt to that init's braces: if fluxbox ever regenerates or overrides the file
# above, it falls back to the default rootCommand, which shells out to `fbsetbg`.
# This no-op shadows it — /usr/local/bin precedes /usr/bin in PATH, so the name
# resolves here first and the call succeeds silently instead of ending in an error
# dialog. It also covers the volume case: a /home/bolu volume that already exists
# shadows the image's copy of the init file, and the wrapper is the half of the fix
# that survives that. It is a workaround for a cosmetic bug in the window manager,
# not a tool the session is meant to use.
RUN printf '%s\n' '#!/bin/sh' 'exit 0' > /usr/local/bin/fbsetbg \
 && chmod 0755 /usr/local/bin/fbsetbg

# The desktop contract the control plane relies on: one display, one geometry,
# three ports. entrypoint.sh reads these instead of hardcoding values, and the
# control plane passes what a session needs per container
# (docker-compose.poc.yml, VCOMPUTER_SANDBOX_*).
#
# GATE_PORT is the only port reachable from outside this container: the gate binds
# it on the container's address, and the control plane proxies its /cdp and /novnc
# doors, which is how Session.CDPURL and Session.NoVNCURL — both paths on the
# control plane — reach it. CDP_LOCAL_PORT is where Chromium itself listens, on
# loopback only, and NOVNC_PORT is where websockify listens, also on loopback only;
# the gate is the only process that can reach either.
# SANDBOX_SECRET is not here on purpose: it is injected at runtime, never baked
# into the image.
ENV DISPLAY=:99 \
    SCREEN_GEOMETRY=1280x800x24 \
    VNC_PORT=5900 \
    NOVNC_PORT=6080 \
    GATE_PORT=8443 \
    CDP_LOCAL_PORT=9222 \
    HOME=/home/bolu

# Documentation only: a sandbox container is never published, and the control
# plane reaches the gate over the internal network instead — the host has no route
# to it (docs/adr/0003-virtual-computer.md §3.6). 8443 is the default of the
# GATE_PORT contract above, which the runtime may override per session. The other
# ports are deliberately absent from this line: 6080 (noVNC/websockify), 5900 (raw
# VNC) and 9222 (Chromium's DevTools) all listen on loopback only, so the gate is
# the single door into the container.
EXPOSE 8443

# Persistent session state: the Chromium profile lives in /home/bolu/profile,
# so a named volume mounted here survives container restarts and keeps Bolu
# logged in. Without a volume the container still runs, it just forgets.
VOLUME /home/bolu

# The gate's health route is the cheapest honest liveness probe: it is open (no
# secret) and answers without touching an upstream, so it only fails when the gate
# itself is gone. Because the gate is the last child entrypoint.sh starts, a 200
# also means Xvfb, x11vnc, websockify and Chromium were up when it started. The
# probe reads ${GATE_PORT} rather than a literal, so a deployment that moves the
# port moves the check with it — the same reason the control plane passes the port
# in as -e. Chromium is deliberately not part of this check (see entrypoint.sh: if
# it dies, the container is expected to exit and be restarted).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS -o /dev/null "http://127.0.0.1:${GATE_PORT}/healthz" || exit 1

# The gate comes from the build stage at the top of this file: it is the only
# listener on the container's address, and the only process that may reach the
# loopback ports behind it. Nothing else is needed from that stage, which is why
# the binary is static (CGO off) and the runtime image stays a plain Debian user
# space.
COPY --from=gate /out/vcomputer-gate /usr/local/bin/vcomputer-gate

# The entrypoint orchestrates the whole desktop stack; it is the only script in
# the image and must be executable for tini to run it.
COPY --chown=root:root entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

# Everything below this line runs unprivileged. The X server is a userspace
# process here (Xvfb), so no root, no device and no capability is required.
USER bolu

# tini is PID 1: it forwards SIGTERM/SIGINT to the entrypoint and reaps the
# children that the entrypoint starts. Without it, orphaned Xvfb/x11vnc
# processes would linger until the runtime force-kills the container.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
