# Sandbox desktop for the Bolu virtual computer (PoC skeleton).
#
# One long-lived container per workspace holds an X server, a window manager, a
# VNC bridge and a Chromium browser. The control plane is a separate Go service:
# it starts this container, mounts the session volume and forwards the noVNC
# port. No secret is ever baked into the image; the control plane injects what
# the session needs at runtime.
#
# The image only ships the pieces; the startup order lives in entrypoint.sh and
# is fixed: Xvfb -> fluxbox -> x11vnc -> noVNC (websockify) -> Chromium.
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
# - chromium              : the browser Bolu actually drives (CDP on 9222)
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
      ca-certificates \
      tini \
 && rm -rf /var/lib/apt/lists/*

# Non-root desktop user. uid/gid 1000 matches the owner used for the session
# volume, so a mounted /home/bolu does not need a chown dance at startup.
RUN groupadd --gid 1000 bolu \
 && useradd --uid 1000 --gid 1000 --create-home --home-dir /home/bolu --shell /bin/bash bolu

# The desktop contract the control plane relies on: one display, one geometry,
# three ports. entrypoint.sh reads these instead of hardcoding values.
ENV DISPLAY=:99 \
    SCREEN_GEOMETRY=1280x800x24 \
    VNC_PORT=5900 \
    NOVNC_PORT=6080 \
    CDP_PORT=9222 \
    HOME=/home/bolu

# 6080 is the only port the control plane should publish: it serves noVNC over
# HTTP, and noVNC reaches the X display through websockify. 5900 is the raw VNC
# port and stays internal to the Docker network, so it is never exposed to the
# outside. 9222 is a stub for the Chromium DevTools protocol (CDP) endpoint the
# control plane will use later to drive the browser without screenshots; it is
# documented here but not meant to be published either.
EXPOSE 6080 5900 9222

# Persistent session state: the Chromium profile lives in /home/bolu/profile,
# so a named volume mounted here survives container restarts and keeps Bolu
# logged in. Without a volume the container still runs, it just forgets.
VOLUME /home/bolu

# The noVNC page is the cheapest honest liveness probe: it goes through
# websockify, which is only listening if Xvfb, x11vnc and websockify all came
# up. Chromium is deliberately not part of this check (see entrypoint.sh: if it
# dies, the container is expected to exit and be restarted).
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
