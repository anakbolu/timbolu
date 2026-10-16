#!/usr/bin/env bash
#
# Entrypoint for the Bolu sandbox desktop (PoC skeleton).
#
# This script starts, in this order: Xvfb -> fluxbox -> x11vnc -> noVNC
# (websockify) -> Chromium. The order is not cosmetic. The X server has to be
# listening before anything can draw on it; the window manager has to attach to
# the display before Chromium asks for a frame, otherwise the browser window is
# unmanaged; x11vnc has to be up before websockify, because websockify only
# proxies to a VNC port that already exists. Chromium is last because it is the
# process whose life defines the session: if it dies, the container dies.
#
# Nothing here reads a secret from the image. The control plane injects what a
# session needs (currently only VNC_PASSWORD, which is optional) at runtime.

set -euo pipefail

# Short timestamped logger. Everything goes to stderr so the container logs stay
# in one stream, which is what the control plane tails.
log() {
  printf '%s [sandbox] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

DISPLAY="${DISPLAY:-:99}"
SCREEN_GEOMETRY="${SCREEN_GEOMETRY:-1280x800x24}"
VNC_PORT="${VNC_PORT:-5900}"
NOVNC_PORT="${NOVNC_PORT:-6080}"
CDP_PORT="${CDP_PORT:-9222}"
HOME="${HOME:-/home/bolu}"

# Xvfb writes its socket at /tmp/.X11-unix/X<n>, where <n> is the display number
# taken from $DISPLAY (":99" -> "X99"). We poll this path to know the server is
# ready instead of sleeping for a guessed amount of time.
display_number="${DISPLAY#:}"
display_number="${display_number%%.*}"
x_socket="/tmp/.X11-unix/X${display_number}"

# pids holds every child we spawn, in start order, and is what the teardown
# kills. supervised holds the subset whose death must end the container; the
# window manager is deliberately not in it (see step 2).
pids=()
supervised=()

# start_child <name> <critical|optional> <command...>
#
# Spawns a child in the background, remembers its PID and logs what it is. A
# "critical" child is supervised: if it exits, the container exits with it.
# An "optional" child is only cleaned up on shutdown, never watched.
start_child() {
  local name="$1" kind="$2" pid
  shift 2
  "$@" &
  pid=$!
  pids+=("$pid")
  if [[ "$kind" == "critical" ]]; then
    supervised+=("$pid")
  fi
  log "started ${name} (pid ${pid}, ${kind}): $*"
}

# Teardown used both by signals and by normal exit. TERM first so the children
# can flush (Chromium writes its profile on the way out), KILL only if they are
# still alive after a short grace period.
shutdown() {
  local pid alive
  if (( ${#pids[@]} == 0 )); then
    return 0
  fi
  log "shutting down ${#pids[@]} child process(es)"
  for pid in "${pids[@]}"; do
    kill -TERM "$pid" 2>/dev/null || true
  done
  for _ in {1..20}; do
    alive=0
    for pid in "${pids[@]}"; do
      if kill -0 "$pid" 2>/dev/null; then
        alive=1
      fi
    done
    if (( alive == 0 )); then
      break
    fi
    sleep 0.25
  done
  for pid in "${pids[@]}"; do
    kill -KILL "$pid" 2>/dev/null || true
  done
  # The password file is consumed and deleted by x11vnc itself; this is the
  # safety net for the case where x11vnc died before it could read it.
  rm -f /tmp/x11vnc.pass
  return 0
}

trap 'log "received SIGTERM"; shutdown; exit 0' TERM
trap 'log "received SIGINT"; shutdown; exit 0' INT
trap shutdown EXIT

# 1. Xvfb - the display itself. Virtual, in userspace, no GPU, no /dev/kvm. It
#    serves a unix socket in /tmp only (-nolisten tcp keeps it off the network),
#    and -ac disables the X access-control list so the unprivileged clients
#    below (x11vnc, Chromium) can connect without an xauth cookie dance.
log "starting display ${DISPLAY} at ${SCREEN_GEOMETRY}"
start_child "Xvfb" critical Xvfb "$DISPLAY" -screen 0 "$SCREEN_GEOMETRY" -nolisten tcp -ac

# Wait for the socket before anything tries to use the display. 20 seconds is
# generous for a cold container; if it never appears, fail loudly rather than
# start a desktop nobody can see.
for _ in {1..80}; do
  if [[ -S "$x_socket" ]]; then
    break
  fi
  sleep 0.25
done
if [[ ! -S "$x_socket" ]]; then
  log "ERROR: X socket ${x_socket} never appeared; giving up"
  exit 1
fi

# A socket on disk is necessary but not sufficient: Xvfb may still be finishing
# its setup. xdpyinfo is a real X client round trip, so when it answers, the
# clients below will be able to connect too. This is also why x11-utils is in
# the image.
for _ in {1..40}; do
  if xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done
if ! xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
  log "ERROR: display ${DISPLAY} never answered xdpyinfo; giving up"
  exit 1
fi
log "display ${DISPLAY} is ready (socket ${x_socket})"

# 2. fluxbox - a tiny window manager. Best effort: Chromium renders fine without
#    it, it only loses decorations and window placement, so this child is
#    started as "optional" and its death never takes the session down.
start_child "fluxbox" optional fluxbox

# 3. x11vnc - exports the X display as a VNC server. It binds to localhost only:
#    the container's VNC port is reachable by noVNC inside the same Docker
#    network and is never published to the outside, which is why the PoC ships
#    no password at all. If a deployment later wants one, the control plane sets
#    VNC_PASSWORD and we hand it over through a file that x11vnc deletes after
#    reading (never through the command line, where it would show up in ps).
vnc_args=(
  -display "$DISPLAY"
  -rfbport "$VNC_PORT"
  -localhost
  -forever
  -shared
  -quiet
  -o /tmp/x11vnc.log
)
if [[ -n "${VNC_PASSWORD:-}" ]]; then
  umask 077
  printf '%s\n' "$VNC_PASSWORD" > /tmp/x11vnc.pass
  unset VNC_PASSWORD
  vnc_args+=(-passwdfile read:/tmp/x11vnc.pass)
  log "x11vnc will require the password injected by the control plane"
else
  vnc_args+=(-nopw)
  log "no VNC_PASSWORD set; relying on the internal Docker network for access"
fi
start_child "x11vnc" critical x11vnc "${vnc_args[@]}"

# 4. noVNC - websockify serves the static noVNC client from the distro package
#    and proxies the websocket traffic to the VNC port above. This is the only
#    port the control plane publishes (6080) and the only way a human looks at
#    the session.
start_child "noVNC/websockify" critical websockify --web=/usr/share/novnc "$NOVNC_PORT" "localhost:${VNC_PORT}"

# 5. Chromium - the session itself, and the reason the container exists. It runs
#    in the background like the others, but the supervision loop below treats
#    its exit as fatal. The flags are deliberate:
#      --user-data-dir=$HOME/profile : profile on the persistent volume, so
#                                      logins survive a restart
#      --window-size=1280,800        : matches SCREEN_GEOMETRY, so noVNC shows
#                                      the full window without scrolling
#      --no-first-run, --no-default-browser-check : no onboarding dialogs in a
#                                      session nobody is sitting in front of
#      --disable-dev-shm-usage       : containers get a small /dev/shm by
#                                      default, and Chromium crashes without it
#      --remote-debugging-address=0.0.0.0 --remote-debugging-port=$CDP_PORT
#                                    : CDP stub, so the control plane can later
#                                      drive the browser instead of clicking
#                                      through screenshots
# TODO: --no-sandbox is required because the container runs without user
#       namespaces (no /dev/kvm, no privileged mode, so Chromium's own sandbox
#       cannot set itself up here). This is a PoC compromise: it weakens the
#       browser's own isolation and MUST NOT be enabled in production. The
#       production path in docs/riset-microvm.md is gVisor on Kubernetes, where
#       the runtime provides the real sandbox and this flag goes away.
start_child "chromium" critical chromium \
  --user-data-dir="${HOME}/profile" \
  --window-size=1280,800 \
  --no-first-run \
  --no-default-browser-check \
  --disable-dev-shm-usage \
  --no-sandbox \
  --remote-debugging-address=0.0.0.0 \
  --remote-debugging-port="$CDP_PORT"

log "desktop is up: noVNC on ${NOVNC_PORT}, VNC internal on ${VNC_PORT}, CDP stub on ${CDP_PORT}"

# Supervision: block until the first supervised child exits, then tear the rest
# down. A dead Chromium means an unusable session, so the container exits and
# the control plane decides whether to restart it; the same applies to a dead
# Xvfb or websockify. `wait -n` reacts to whichever child goes first instead of
# blocking on one specific PID, and fluxbox is left out on purpose.
set +e
wait -n "${supervised[@]}"
status=$?
set -e
log "a supervised child process exited with status ${status}; stopping the sandbox"
exit "$status"
