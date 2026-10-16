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
# socat (which exposes Chromium's loopback DevTools port on the container's
# address).
#
# Isolation rules from docs/riset-microvm.md apply here too: no --privileged,
# no /dev/kvm, no Docker socket, and the browser runs as a non-root user. The
# production path for stronger isolation is gVisor on Kubernetes, where the
# container boundary is real (see the --no-sandbox TODO in entrypoint.sh).

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
# - socat                 : exposes Chromium's loopback CDP port on the
#                           container's own address, which is the only way the
#                           control plane can reach it (new Chromium ignores
#                           --remote-debugging-address=0.0.0.0)
# - fluxbox               : tiny window manager, so Chromium gets a real frame
# - fonts-dejavu-core, fonts-liberation : without fonts, pages render as boxes
# - procps, curl          : process tools and the healthcheck probe
# - ca-certificates       : TLS trust store, Chromium needs it for HTTPS
# - tini                  : PID 1 that reaps zombies and forwards signals
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
      socat \
      ca-certificates \
      tini \
 && rm -rf /var/lib/apt/lists/*

# Non-root desktop user. uid/gid 1000 matches the owner used for the session
# volume, so a mounted /home/bolu does not need a chown dance at startup.
RUN groupadd --gid 1000 bolu \
 && useradd --uid 1000 --gid 1000 --create-home --home-dir /home/bolu --shell /bin/bash bolu

# The desktop contract the control plane relies on: one display, one geometry,
# four ports. entrypoint.sh reads these instead of hardcoding values, and the
# control plane passes NOVNC_PORT, CDP_PORT and SCREEN_GEOMETRY per container
# (docker-compose.poc.yml, VCOMPUTER_SANDBOX_*).
#
# CDP_PORT is the port socat listens on, on the container's address: it is what
# the control plane builds Session.CDPURL from. CDP_LOCAL_PORT is where Chromium
# itself listens, on loopback only.
ENV DISPLAY=:99 \
    SCREEN_GEOMETRY=1280x800x24 \
    VNC_PORT=5900 \
    NOVNC_PORT=6080 \
    CDP_PORT=9223 \
    CDP_LOCAL_PORT=9222 \
    HOME=/home/bolu

# Documentation only: a sandbox container is never published, and the control
# plane reaches noVNC and CDP over the internal network instead — the host has no
# route to either (docs/adr/0003-virtual-computer.md §3.6). Both numbers are the
# defaults of the NOVNC_PORT/CDP_PORT contract above, which the runtime overrides
# per session (VCOMPUTER_SANDBOX_NOVNC_PORT / VCOMPUTER_SANDBOX_CDP_PORT) and
# which the control plane turns into Session.NoVNCURL and Session.CDPURL. 5900 is
# raw VNC and stays inside the container network too.
EXPOSE 6080 9223

# Persistent session state: the Chromium profile lives in /home/bolu/profile,
# so a named volume mounted here survives container restarts and keeps Bolu
# logged in. Without a volume the container still runs, it just forgets.
VOLUME /home/bolu

# The noVNC page is the cheapest honest liveness probe: it goes through
# websockify, which is only listening if Xvfb, x11vnc and websockify all came
# up. The probe reads ${NOVNC_PORT} rather than a literal, so a deployment that
# moves the port moves the check with it — the same reason the control plane
# passes the port in as -e. Chromium is deliberately not part of this check (see
# entrypoint.sh: if it dies, the container is expected to exit and be restarted).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS -o /dev/null "http://127.0.0.1:${NOVNC_PORT}/vnc.html" || exit 1

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
