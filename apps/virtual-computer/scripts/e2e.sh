#!/usr/bin/env bash
#
# The evidence for the virtual-computer PoC: every claim the PR makes, checked against the
# running stack, in the order a reviewer would ask about them.
#
# This script is the PR's proof, so it is written to fail loudly. Each check prints exactly
# one line — `PASS <name>` or `FAIL <name> <detail>` — and the run ends with
# `SUMMARY: <n> passed, <m> failed`; a single FAIL exits non-zero. Everything is teed into
# $OUT/after-status.txt (docs/poc-evidence/virtual-computer by default), because the log is
# the artefact and a terminal scrollback is not.
#
# What each check is for:
#
#   preconditions                 the tools, the compose file, and the stub screenshot are
#                                 where this expects them
#   build / compose-up / health   the stack starts at all
#   auth-401                      /v1/* is not an open door
#   ensure-idempotent             a worker may call ensure before every task
#   navigate-fixture / navigate-no-egress-*
#                                 the sandbox browser reaches the internal network, and a URL
#                                 with no route fails inside the navigate timeout with a 502
#                                 that says why, instead of a 200 that lies
#   screenshot-*                  the capture is a real PNG of a real display, not the
#                                 placeholder the stub used to answer with
#   click / type / screenshot-typed / typed-screenshot-differs
#                                 input reaches the X server, and the screen shows it
#   exec-*                        a command runs inside the sandbox as bolu, not in a stub
#   cdp-version                   the DevTools endpoint is reachable through the control
#                                 plane, which is the only thing holding the sandbox secret
#   network-internal / no-published-ports / no-egress
#                                 the sandbox sits on an internal network, publishes nothing,
#                                 and cannot reach the internet
#   sandbox-* / gate-*            the host CAN route to a sandbox's address — docker puts it
#                                 on the bridge — and gets nothing out of it without the
#                                 gate's secret: Chromium's own DevTools port and the retired
#                                 socat port both refuse the host, the noVNC port is closed, a
#                                 401 comes back without X-Sandbox-Secret (before the path is
#                                 even looked at), /cdp and /novnc answer 200 with it,
#                                 /healthz is open, and anything else is a 404
#   novnc-*                       the desktop is reachable through the control plane only,
#                                 and only with the token — page and websocket handshake
#                                 alike
#   delete / delete-twice-404 / ensure-after-delete-running / volume-reused
#                                 delete ends the container, the volume outlives it, and a
#                                 second delete is still a 404
#   purge-*                       purge ends the container and the home volume with it
#   cleanup-*                     the admin route sweeps a stopped sandbox and its volume
# Usage, from the repository root:
#
#   apps/virtual-computer/scripts/e2e.sh
#   DOCKER="sudo docker" apps/virtual-computer/scripts/e2e.sh
#   STACK=vc2 apps/virtual-computer/scripts/e2e.sh            # a second stack, beside a demo
#   BUILD_NETWORK=host apps/virtual-computer/scripts/e2e.sh   # a daemon with no route out
#   PREBUILT=1 apps/virtual-computer/scripts/e2e.sh           # the images are already tagged
#   TEARDOWN=1 apps/virtual-computer/scripts/e2e.sh           # remove what this started
#
# Environment:
#   DOCKER                  the docker CLI. "sudo docker" is expected to work, which is why
#                           every use of it below is unquoted on purpose (shellcheck SC2086
#                           is silenced where it fires, with that reason).
#   COMPOSE                 the whole compose invocation. Default:
#                           "$DOCKER compose -p $STACK -f $COMPOSE_FILE".
#   COMPOSE_FILE            the compose file the run reads, and the one the precondition check
#                           below looks for — one definition, so the command that runs and the
#                           file that is checked cannot be two different files. Default
#                           apps/virtual-computer/docker-compose.poc.yml, resolved from the
#                           repository root this script puts itself in.
#   OUT                     artefact directory. Default docs/poc-evidence/virtual-computer.
#   STACK                   the name of the stack under test, and the only name this script
#                           takes as an input — every other one is derived from it: the compose
#                           project (`-p "$STACK"`), the two networks (${STACK}-internal,
#                           ${STACK}-edge), the two images (${STACK}-control:poc,
#                           ${STACK}-sandbox:poc), the control and fixture containers
#                           (${STACK}-control, ${STACK}-fixture), and the sandbox the runtime
#                           starts for a bot (${STACK}-<bot id>, volume ${STACK}-<bot id>-home).
#                           Default vcomputer, which is what the compose file's own defaults
#                           produce, so a default run uses the names this stack has always used.
#                           The derived names are exported as the VCOMPUTER_* variables the
#                           compose file reads, so the two sides cannot disagree about them.
#   BOT                     bot id under test. Default e2e-bot. A second bot, ${BOT}-stale, is
#                           started by the cleanup check and removed again by it.
#   VCOMPUTER_CONTROL_TOKEN the control plane's token, when the caller already has one: it is
#                           used as it is instead of generating one. Either way the log says
#                           which of the two happened, and the value itself is never printed —
#                           except inside the noVNC URL at the end, which is a page load and
#                           cannot carry a header.
#   VCOMPUTER_CONTROL_PORT  host port of the control plane, exported to compose so both sides
#                           agree on it. Default 18090 — deliberately not the compose default
#                           (8090), so a stack someone left running for a demo cannot answer
#                           this script's requests by accident.
#   VCOMPUTER_SANDBOX_GATE_PORT / VCOMPUTER_SANDBOX_NOVNC_PORT
#                           the sandbox's gate and noVNC ports, exported to compose for the
#                           same reason. Defaults 8443 and 6080, the sandbox image's own.
#   PREBUILT=1              the images are already tagged: no build runs at all, and `up` is
#                           called with --no-build. It skips the build section outright rather
#                           than only changing the `up` flags, because a build that runs anyway
#                           is a build that can fail a run whose images were fine.
#   TEARDOWN=1              end this run's state at the end: purge the session, sweep what is
#                           left through POST /v1/admin/cleanup?volumes=true, run the same
#                           sweep through scripts/cleanup.sh with VOLUMES=1, then `compose down`
#                           and remove the two sandbox names directly. Off by default: after a
#                           failure the containers and the per-bot volume are the state a
#                           reviewer wants to look at.
#
# The claim the sandbox checks exist for: a sandbox is NOT unreachable from the host. Docker
# puts the host on every bridge it creates, so the host can dial a sandbox's address; what
# keeps it out is the gate inside the sandbox, which refuses anything without that sandbox's
# X-Sandbox-Secret. Check 10 proves both halves of that: Chromium's own DevTools port and
# websockify both answer nothing on that address — they bind loopback, which is what makes the
# gate the only listener — and the gate's two doors answer 401 without the secret and 200 with
# it, while /healthz stays open for the liveness probe.
#
# Needs bash, curl, sed, grep, od, tr, cut, wc, head, sha256sum, mkdir, sleep, tee, date, and
# the docker CLI; openssl if present and /dev/urandom if not. No jq and no python3: the JSON
# read here is one level deep, and an evidence script that needs a package manager is an
# evidence script nobody reruns.

# A failed check must not stop the run: the point of the log is the whole list of what broke,
# not the first thing that did.
set -uo pipefail

# Every path below is relative to the repository root, so put ourselves there rather than
# trusting the caller's working directory (the same convention as the smoke scripts).
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1

DOCKER=${DOCKER:-docker}
# STACK comes first because COMPOSE and every container name below are built from it.
STACK=${STACK:-vcomputer}
BOT=${BOT:-e2e-bot}
STALE_BOT="${BOT}-stale"
# COMPOSE_FILE is defined before COMPOSE because the default invocation is built from it, and
# the precondition check below reads the same variable rather than a second copy of the path.
COMPOSE_FILE=${COMPOSE_FILE:-apps/virtual-computer/docker-compose.poc.yml}
COMPOSE=${COMPOSE:-"$DOCKER compose -p $STACK -f $COMPOSE_FILE"}
OUT=${OUT:-docs/poc-evidence/virtual-computer}
PORT=${VCOMPUTER_CONTROL_PORT:-18090}
# The control plane as this host reaches it, which is the address of every request below: the
# port is the one compose was told to publish (VCOMPUTER_CONTROL_PORT above), on loopback
# because the compose file binds it to 127.0.0.1 rather than to every interface.
BASE="http://127.0.0.1:${PORT}"
GATE_PORT=${VCOMPUTER_SANDBOX_GATE_PORT:-8443}
NOVNC_PORT=${VCOMPUTER_SANDBOX_NOVNC_PORT:-6080}
# The port socat used to publish Chromium's DevTools on, and the port Chromium itself binds
# inside the container. Both are checked below and for the same reason: the gate is meant to be
# the only way in, which is only true if the host cannot reach Chromium directly — and Chromium
# on 9222 is listening inside the container, loopback-bound, so the check has to look at the
# container's own address rather than assume nothing is there.
OLD_CDP_PORT=9223
CDP_PORT=9222
# Every name below is STACK plus a suffix, which is the point of STACK: with the default it
# reproduces the names this stack has always used, and with any other value it is a second
# stack that shares nothing with the first. The sandbox container and its volume are the
# runtime's names (VCOMPUTER_SANDBOX_NAME_PREFIX, which compose sets from VCOMPUTER_STACK
# below), not compose's own.
CONTROL_IMAGE="${STACK}-control:poc"
SANDBOX_IMAGE="${STACK}-sandbox:poc"
CONTROL_CONTAINER="${STACK}-control"
FIXTURE_CONTAINER="${STACK}-fixture"
INTERNAL_NETWORK="${STACK}-internal"
EDGE_NETWORK="${STACK}-edge"
SANDBOX_CONTAINER="${STACK}-${BOT}"
SANDBOX_VOLUME="${STACK}-${BOT}-home"
STALE_CONTAINER="${STACK}-${STALE_BOT}"
STALE_VOLUME="${STACK}-${STALE_BOT}-home"
# The stub screenshot is the byte string the old stub answered with: a fixed artefact in the
# repository, not a product of this run, so it is resolved from the repository root rather than
# from $OUT. An OUT pointed at a scratch directory must not be able to turn the check that
# catches a stub into a missing file.
STUB_SHOT="docs/poc-evidence/virtual-computer/stub-screenshot.png"

mkdir -p "$OUT"

# Everything below lands in the log as well as on the terminal. Fds 3 and 4 keep the real
# stdout and stderr so the tee can be closed and flushed before the script exits: the summary
# is the last line written, and losing it to a half-flushed pipe would be a small, stupid bug
# in the one file a reviewer reads first.
exec 3>&1 4>&2
exec > >(tee "$OUT/after-status.txt") 2>&1

printf '=== virtual computer PoC — e2e, %s ===\n' "$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
printf 'compose: %s\nstack:   %s\nbot:     %s\ncontrol: %s\n' "$COMPOSE_FILE" "$STACK" "$BOT" "$BASE"

# ---------------------------------------------------------------- counters and helpers

PASSED=0
FAILED=0
pass() { printf 'PASS %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf 'FAIL %s %s\n' "$1" "${2:-}"; FAILED=$((FAILED + 1)); }

log() { printf '\n== %s\n' "$*"; }

# expect <name> <got> <want> <detail>: one check, one line. The detail says what was expected
# of whom, because a FAIL has to be readable on its own, without the script beside it.
expect() {
    if [ "$2" = "$3" ]; then
        pass "$1"
    else
        fail "$1" "$4 (got ${2:-<empty>}, want ${3:-<empty>})"
    fi
}

# One token for the whole run, exported and handed to compose by interpolation.
# VCOMPUTER_CONTROL_TOKEN is required — the control plane refuses to start without it — so the
# value has to be in the environment before compose reads the file. A caller who already has a
# token passes it in that same variable and this run uses it; otherwise one is generated here,
# openssl first because it is the obvious way to make a secret, /dev/urandom as the fallback so
# a machine without openssl can still produce evidence. Which of the two happened is printed,
# and the value never is.
if [ -n "${VCOMPUTER_CONTROL_TOKEN:-}" ]; then
    TOKEN="$VCOMPUTER_CONTROL_TOKEN"
    printf 'token: using VCOMPUTER_CONTROL_TOKEN from the environment\n'
else
    TOKEN=$(openssl rand -hex 16 2>/dev/null || true)
    if [ -n "$TOKEN" ]; then
        printf 'token: generated for this run (openssl rand -hex 16)\n'
    else
        TOKEN=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
        printf 'token: generated for this run (no openssl here: od on /dev/urandom)\n'
    fi
fi
export VCOMPUTER_CONTROL_TOKEN="$TOKEN"

# The port and the rest of the names this run derives from STACK are given to the compose file
# so the two sides cannot disagree about them: the compose file interpolates
# VCOMPUTER_CONTROL_PORT into 127.0.0.1:${VCOMPUTER_CONTROL_PORT}:8090, and a control plane
# listening somewhere other than where this script curls it is a script that fails for the
# wrong reason. VCOMPUTER_STACK is also the sandbox name prefix the runtime uses, which is what
# makes the container and volume names in the checks below the runtime's own names rather than a
# guess.
export VCOMPUTER_CONTROL_PORT="$PORT"
export VCOMPUTER_STACK="$STACK"
export VCOMPUTER_CONTROL_IMAGE="$CONTROL_IMAGE"
export VCOMPUTER_CONTROL_CONTAINER="$CONTROL_CONTAINER"
export VCOMPUTER_FIXTURE_CONTAINER="$FIXTURE_CONTAINER"
export VCOMPUTER_SANDBOX_IMAGE="$SANDBOX_IMAGE"
export VCOMPUTER_INTERNAL_NETWORK="$INTERNAL_NETWORK"
export VCOMPUTER_EDGE_NETWORK="$EDGE_NETWORK"
export VCOMPUTER_SANDBOX_GATE_PORT="$GATE_PORT"
export VCOMPUTER_SANDBOX_NOVNC_PORT="$NOVNC_PORT"

# The timeout is generous on purpose: `ensure` starts a container and waits for Chromium's
# DevTools port, and the runtime polls for up to ~45s before it answers.
api() { curl -s -m 180 -H "Authorization: Bearer ${TOKEN}" "$@"; }
status() { curl -s -o /dev/null -m 180 -w '%{http_code}' -H "Authorization: Bearer ${TOKEN}" "$@"; }
# The same call with no credential at all, for the checks that are about the refusal.
status_open() { curl -s -o /dev/null -m 20 -w '%{http_code}' "$@"; }

# The host-side probes, which have to fail fast: "nothing is listening there" is the expected
# answer for most of them, so a 3s cap keeps a closed port from looking like a slow one.
# %{http_code} is 000 when curl got no response at all, which is what a refused connection and a
# timeout both look like from here.
code_open() { curl -s -o /dev/null -m 3 -w '%{http_code}' "$@" 2>/dev/null || true; }

# The two readers this script needs. sed rather than jq: the bodies are one line and one level
# deep, and a check that cannot run without a JSON tool is a check that silently stops
# checking.
json_str() { printf '%s' "$1" | sed -n "s/.*\"$2\": *\"\([^\"]*\)\".*/\1/p" | head -n 1; }
json_num() { printf '%s' "$1" | sed -n "s/.*\"$2\": *\(-\{0,1\}[0-9][0-9]*\).*/\1/p" | head -n 1; }
sha() { sha256sum "$1" | cut -d' ' -f1; }

# The sandbox's address on the internal network, and the secret its gate expects. Both are read
# out of the container docker itself describes — the runtime generated the secret and injected it
# as an env var — rather than recomputed here, so a check can only agree with the sandbox, never
# with a guess.
# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
sandbox_ip() { $DOCKER inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$SANDBOX_CONTAINER" 2>/dev/null; }
# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
sandbox_secret() { $DOCKER inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$SANDBOX_CONTAINER" 2>/dev/null | grep '^SANDBOX_SECRET=' | cut -d= -f2- | head -n 1; }

# The last thing the run does: teardown (only if asked), the summary, the exit status, and the
# flush of the tee.
finish() {
    if [ "${TEARDOWN:-0}" = "1" ]; then
        log "teardown (TEARDOWN=1)"
        # The order is the point. Purge first: it is the only path that removes the per-bot
        # volume through the API (a plain DELETE leaves it, which is what the volume-reused
        # check above is about). Then the admin sweep, which is the route that exists for
        # exactly this. Then the same sweep through the CLI, which is the path that still works
        # when the control plane is already gone. Then the two compose services. Each step
        # prints what it answered rather than staying quiet, because a teardown that
        # half-worked is the state a reviewer has to finish by hand.
        printf 'teardown: purge %s, admin cleanup %s\n' \
            "$(status -X DELETE "${BASE}/v1/sessions/${BOT}?purge=true")" \
            "$(api -o /dev/null -w '%{http_code}' -X POST "${BASE}/v1/admin/cleanup?volumes=true")"
        # VOLUMES=1 is not optional here: TEARDOWN=1 means this run's state goes away, and a
        # sandbox volume is this run's state. The script is the CLI equivalent of the admin
        # route above, so it also finds anything a failed run left stopped.
        VOLUMES=1 STACK="$STACK" DOCKER="$DOCKER" bash apps/virtual-computer/scripts/cleanup.sh ||
            printf 'teardown: cleanup.sh answered non-zero; see its output above\n'
        # shellcheck disable=SC2086  # COMPOSE is a word list; see the header
        $COMPOSE down
        # What the steps above could not reach: a sandbox that was still running (purge is the
        # only thing that removes those, and it needs a control plane that answers) and the
        # volumes, removed directly here so that TEARDOWN=1 leaves nothing behind even when the
        # control plane was already gone. Both are this run's own names rather than a sweep, and
        # both are allowed to find nothing — a purge that removed them already is the expected
        # case, and a missing container must not fail the teardown.
        # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
        $DOCKER rm -f "$SANDBOX_CONTAINER" "$STALE_CONTAINER" >/dev/null 2>&1 || true
        # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
        $DOCKER volume rm "$SANDBOX_VOLUME" "$STALE_VOLUME" >/dev/null 2>&1 || true
        printf 'torn down; the noVNC URL printed above is gone with it\n'
    fi

    printf '\nSUMMARY: %d passed, %d failed\n' "$PASSED" "$FAILED"
    exec 1>&3 2>&4
    # Let the tee drain before the shell goes away.
    sleep 1
    if [ "$FAILED" -ne 0 ]; then
        exit 1
    fi
    exit 0
}

# ------------------------------------------------------------------------- preconditions

log "preconditions"

missing=""
for tool in curl sed grep od tr cut wc head sha256sum mkdir sleep tee date; do
    command -v "$tool" >/dev/null 2>&1 || missing="${missing} ${tool}"
done
if [ -z "$missing" ]; then
    pass "preconditions"
else
    fail "preconditions" "missing tools:${missing}"
fi

if [ ! -f "$COMPOSE_FILE" ]; then
    fail "compose-file" "${COMPOSE_FILE} is not here (cwd is $(pwd)): this script runs from the repository root"
    finish
fi

# The stub screenshot is not a product of this run — it is the placeholder the old stub answered
# with, kept in the repository so that this run can prove a capture is not it — which is why it
# is resolved from the repository root rather than from $OUT. A run that cannot find it cannot
# tell a real capture from a stub, which is the point of check 5, so it stops here instead of
# failing that check later for a reason that looks like a sandbox problem.
if [ ! -f "$STUB_SHOT" ]; then
    fail "stub-screenshot" "${STUB_SHOT} is not here (cwd is $(pwd)): it is the placeholder every real capture is compared against"
    finish
fi

# --------------------------------------------------------------------------------- build

log "build: $CONTROL_IMAGE and $SANDBOX_IMAGE"

# The compose build is the default because it is the one command the README documents, and it is
# where the image tags live (the `vcomputer-sandbox` service exists only as a build target behind
# the `build` profile). BUILD_NETWORK=host is for the daemon that cannot reach a registry:
# build's default bridge has no DNS on some hosts, and `--network host` sidesteps it, at the cost
# of two builds instead of one compose invocation.
#
# PREBUILT=1 skips this section unconditionally. A caller who says the images are already tagged
# is saying a build cannot be part of this run, and the old shape — where the two images were
# built anyway unless BUILD_NETWORK=host, and only the `up` flags changed — was a run that could
# fail inside a build nobody asked for.
PREBUILT=${PREBUILT:-0}
if [ "$PREBUILT" = "1" ]; then
    printf 'note: PREBUILT=1, so no build runs here and `up` is called with --no-build\n'
    pass "build"
elif [ "${BUILD_NETWORK:-}" = "host" ]; then
    # shellcheck disable=SC2086  # DOCKER may be "sudo docker": the split is the point
    if $DOCKER build --network host -t "$CONTROL_IMAGE" apps/virtual-computer/control &&
        $DOCKER build --network host -t "$SANDBOX_IMAGE" apps/virtual-computer/sandbox; then
        pass "build"
    else
        fail "build" "$DOCKER build --network host failed; the failure is in the log above"
    fi
    # Both tags exist now, so `up` must not try to build them again.
    PREBUILT=1
else
    # shellcheck disable=SC2086  # COMPOSE is a word list; see the header
    if $COMPOSE --profile build build; then
        pass "build"
    else
        fail "build" "$COMPOSE --profile build build failed; the failure is in the log above"
    fi
fi

# ------------------------------------------------------------------------------------ up

log "up: control plane and fixture"

# The fixture is the page the browser opens. It lives on the internal network, so it is the only
# URL a sandbox can navigate to — which is what makes the navigate check meaningful.
up_flags=""
if [ "$PREBUILT" = "1" ]; then
    # `--no-build` keeps the tags that already exist instead of rebuilding them behind the
    # operator's back.
    up_flags="--no-build"
fi
# No service names: the services that are not behind a profile are exactly the control plane and
# the fixture, and naming them here would hardcode the compose service keys — which compose does
# not interpolate, and which this script deliberately does not know. What it knows is the
# container names, and those come from STACK.
# shellcheck disable=SC2086  # COMPOSE and the flag above are word lists by design
if $COMPOSE up -d $up_flags; then
    pass "compose-up"
else
    fail "compose-up" "$COMPOSE up -d failed: the failure is in the log above"
fi

# Nothing below can be read before the process is listening. Compose's own healthcheck is
# slower than this loop, so poll /health directly, up to 60 tries.
tries=0
while [ "$(status_open "${BASE}/health")" != "200" ]; do
    tries=$((tries + 1))
    if [ "$tries" -ge 60 ]; then
        break
    fi
    sleep 1
done
if [ "$tries" -ge 60 ]; then
    printf 'note: /health did not answer 200 within 60s; the checks below will say so too\n'
fi

# ----------------------------------------------------------------------------- the checks

log "1. /health is open and answers 200"
expect "health" "$(status_open "${BASE}/health")" 200 \
    "GET /health must be open (no token) and 200"

log "2. /v1/* without the token is 401"
expect "auth-401" \
    "$(status_open -X POST -H 'Content-Type: application/json' -d "{\"bot_id\":\"${BOT}\"}" "${BASE}/v1/sessions")" \
    401 "POST /v1/sessions without a bearer token must be refused"

log "3. ensure is idempotent"
# A worker calls ensure before every task, so the second call has to answer the same container
# rather than starting a second one. Both calls are made for real: the first one starts the
# sandbox, the second one must find it.
first=$(api -X POST -H 'Content-Type: application/json' -d "{\"bot_id\":\"${BOT}\"}" "${BASE}/v1/sessions")
second=$(api -X POST -H 'Content-Type: application/json' -d "{\"bot_id\":\"${BOT}\"}" "${BASE}/v1/sessions")
cid1=$(json_str "$first" container_id)
cid2=$(json_str "$second" container_id)
vol1=$(json_str "$first" volume)
if [ -n "$cid1" ] && [ "$cid1" = "$cid2" ]; then
    pass "ensure-idempotent"
else
    fail "ensure-idempotent" "container_id ${cid1:-<empty>} then ${cid2:-<empty>}: a second ensure must reuse the running container"
fi

log "4. navigate: the fixture answers, a URL with no route does not"
# The fixture's container name is a name on the internal network, so a 200 here proves the sandbox
# has a route to it and that the gate passed the request through to Chromium.
#
# One request answers both halves of the question: `-w '\n%{http_code}'` puts the status on its
# own last line, so the body says what happened and the status says what a caller branches on,
# without a second navigation.
nav=$(api -X POST -H 'Content-Type: application/json' \
    -d "{\"url\":\"http://${FIXTURE_CONTAINER}:8080/\"}" \
    -w '\n%{http_code}' "${BASE}/v1/sessions/${BOT}/navigate")
nav_body=$(printf '%s\n' "$nav" | sed '$d')
expect "navigate-fixture" "$(printf '%s\n' "$nav" | sed -n '$p')" 200 \
    "navigate to http://${FIXTURE_CONTAINER}:8080/ on the internal network must be accepted"
case "$nav_body" in
    *'"navigated":true'*) pass "navigate-fixture-body" ;;
    *) fail "navigate-fixture-body" "the body was ${nav_body:-<empty>}, want \"navigated\":true" ;;
esac

# The other half, and the one a worker depends on: the internal network has no route out, so a
# navigation to the internet cannot succeed — and it must not answer 200 as if it had, nor hang
# until something else times out. The control plane bounds the wait (VCOMPUTER_NAVIGATE_TIMEOUT,
# 15s in the compose file) and reports the failure as a 502 that says what failed, which is what
# makes a failed navigation distinguishable from a rejected URL (400).
nav=$(api -X POST -H 'Content-Type: application/json' \
    -d '{"url":"https://example.com"}' \
    -w '\n%{http_code}' "${BASE}/v1/sessions/${BOT}/navigate")
nav_body=$(printf '%s\n' "$nav" | sed '$d')
expect "navigate-no-egress-502" "$(printf '%s\n' "$nav" | sed -n '$p')" 502 \
    "navigate to https://example.com must fail with 502: the internal network has no route out"
missing=""
case "$nav_body" in *'"error"'*) ;; *) missing="${missing} an \"error\" key" ;; esac
case "$nav_body" in *'navigation failed'*) ;; *) missing="${missing} the words navigation failed" ;; esac
if [ -z "$missing" ]; then
    pass "navigate-no-egress-error"
else
    fail "navigate-no-egress-error" "the body was ${nav_body:-<empty>}, want an error that says the navigation failed (missing:${missing})"
fi

log "5. the screenshot is a real capture"
# The page has to paint before a capture means anything, and the capture is what separates the
# docker runtime from the placeholder the stub answers with: a PNG of a live display, not the
# same 1.5 KB byte string on every call.
sleep 4
shot="${OUT}/after-api-screenshot.png"
expect "screenshot" "$(api -X POST -o "$shot" -w '%{http_code}' "${BASE}/v1/sessions/${BOT}/screenshot")" \
    200 "POST screenshot must answer 200 with PNG bytes"

magic=$(od -An -N8 -tx1 "$shot" 2>/dev/null | tr -d ' \n')
expect "screenshot-png" "$magic" "89504e470d0a1a0a" "${shot} must start with the PNG signature"

size=$(wc -c < "$shot" 2>/dev/null | tr -d ' ')
if [ -n "$size" ] && [ "$size" -gt 10000 ]; then
    pass "screenshot-size"
else
    fail "screenshot-size" "${shot} is ${size:-<none>} bytes, want more than 10000 (the placeholder was ~1.5 KB)"
fi

# STUB_SHOT is the placeholder a stub answers with, and it is checked in the preconditions: a
# capture that is byte-identical to it is the stub answering, not a desktop.
if [ "$(sha "$shot")" != "$(sha "$STUB_SHOT")" ]; then
    pass "screenshot-not-stub"
else
    fail "screenshot-not-stub" "the capture is byte-identical to ${STUB_SHOT}: the runtime is still answering the stub"
fi

log "6. click into the fixture's input and type"
# The fixture page focuses its input on load, so the click lands on it and the text goes into
# it. The click is what proves xdotool can reach the display at all; the second screenshot is
# what proves the keystrokes arrived, because the API answering 200 only says it ran the
# command.
expect "click" \
    "$(status -X POST -H 'Content-Type: application/json' -d '{"x":320,"y":150}' "${BASE}/v1/sessions/${BOT}/click")" \
    200 "click at 320,150 must reach the X server through xdotool"
expect "type" \
    "$(status -X POST -H 'Content-Type: application/json' -d '{"text":"halo timbolu"}' "${BASE}/v1/sessions/${BOT}/type")" \
    200 "type must reach the X server through xdotool"

sleep 2
typed="${OUT}/after-api-typed.png"
expect "screenshot-typed" "$(api -X POST -o "$typed" -w '%{http_code}' "${BASE}/v1/sessions/${BOT}/screenshot")" \
    200 "the screenshot after typing must answer 200"
if [ -f "$typed" ] && [ "$(sha "$typed")" != "$(sha "$shot")" ]; then
    pass "typed-screenshot-differs"
else
    fail "typed-screenshot-differs" "the screen after typing is identical to the screen before it: the keystrokes did not arrive"
fi

log "7. exec runs inside the sandbox as bolu"
# whoami is the second half of the claim: the command must run in the sandbox as the desktop
# user, not as root in the control plane, and the answer must not be stubbed.
body=$(api -X POST -H 'Content-Type: application/json' -d '{"command":"echo hello-from-sandbox && whoami"}' "${BASE}/v1/sessions/${BOT}/exec")
stdout=$(json_str "$body" stdout)
missing=""
case "$stdout" in *hello-from-sandbox*) ;; *) missing="${missing} hello-from-sandbox" ;; esac
case "$stdout" in *bolu*) ;; *) missing="${missing} bolu" ;; esac
if [ -z "$missing" ]; then
    pass "exec-stdout"
else
    fail "exec-stdout" "stdout is '${stdout:-<empty>}', missing:${missing}"
fi
expect "exec-exit-code" "$(json_num "$body" exit_code)" 0 "the command must exit 0"
case "$body" in
    *'"stub":false'*) pass "exec-not-stub" ;;
    *) fail "exec-not-stub" "the answer says the command was stubbed: ${body:-<empty>}" ;;
esac

log "8. the DevTools endpoint is reachable through the control plane, and only through it"
# cdp_url is a path on the control plane rather than an address on the sandbox network: the
# control plane is the only thing holding the sandbox's secret, so a caller that drives the
# browser needs the control token and nothing else, and the sandbox's address never leaves the
# internal network. A 200 carrying Chromium's own version string therefore proves the whole chain
# answered — control plane, then the sandbox's gate, then Chromium — and a 401 or a 502 here says
# which link is missing.
session=$(api "${BASE}/v1/sessions/${BOT}")
cdp=$(json_str "$session" cdp_url)
if [ -z "$cdp" ]; then
    fail "cdp-version" "the session has no cdp_url: ${session:-<empty>}"
else
    version=$(api "${BASE}${cdp}/json/version")
    case "$version" in
        *Browser*) pass "cdp-version" ;;
        *) fail "cdp-version" "GET ${cdp}/json/version answered '${version:-<empty>}', want Chromium's version JSON" ;;
    esac
fi

log "9. the sandbox is isolated"
# Three claims that fail for three different reasons, so they are three checks: the network was
# created without `internal: true`, the runtime published a port, or the sandbox has a route out.
# Any one of them is a different bug with a different fix.
# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
internal=$($DOCKER network inspect "$INTERNAL_NETWORK" -f '{{.Internal}}' 2>/dev/null)
expect "network-internal" "$internal" "true" "${INTERNAL_NETWORK} must exist with internal: true"

# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
ports=$($DOCKER port "$SANDBOX_CONTAINER" 2>/dev/null)
expect "no-published-ports" "$ports" "" "${SANDBOX_CONTAINER} must publish no port: a sandbox is reached through the control plane, never from the host"

body=$(api -X POST -H 'Content-Type: application/json' -d '{"command":"curl -s -m 5 -o /dev/null https://example.com"}' "${BASE}/v1/sessions/${BOT}/exec")
code=$(json_num "$body" exit_code)
if [ -n "$code" ] && [ "$code" != "0" ]; then
    pass "no-egress"
else
    fail "no-egress" "curl to https://example.com inside the sandbox answered exit_code=${code:-<none>}, want non-zero: the internal network must have no route out"
fi

log "10. the sandbox address answers nothing without the gate's secret"
# The host CAN route to a sandbox: docker puts the host on every bridge it creates, so a
# container's address is dialable from here. That is exactly why the gate exists — it is the only
# listener on that address, it fronts Chromium's DevTools and websockify (both of which bind
# loopback), and it refuses anything that does not carry SANDBOX_SECRET. Each probe below fails
# for its own reason and has its own fix: Chromium's DevTools port answering the host at all,
# the port socat used to publish still answering, noVNC's own port reachable, the gate answering
# an unauthenticated caller without saying why, the gate letting a caller through with the
# secret but not without it, its health route asking for one, its second door left open, or a
# path it does not publish being proxied anyway.
ip=$(sandbox_ip)
secret=$(sandbox_secret)
if [ -z "$ip" ]; then
    fail "sandbox-address" "docker inspect of ${SANDBOX_CONTAINER} gave no address: the checks below cannot run"
elif [ -z "$secret" ]; then
    fail "sandbox-secret" "${SANDBOX_CONTAINER} has no SANDBOX_SECRET in its environment: nothing can reach its gate"
else
    expect "sandbox-cdp-port-closed" "$(code_open "http://${ip}:${OLD_CDP_PORT}/json/list")" 000 \
        "nothing may listen on ${ip}:${OLD_CDP_PORT}: that is the port socat exposed, and the gate replaced it"
    expect "sandbox-chromium-port-closed" "$(code_open "http://${ip}:${CDP_PORT}/json/list")" 000 \
        "Chromium binds its DevTools endpoint to loopback: ${ip}:${CDP_PORT} must refuse the host, or the gate is not the only way in"
    expect "sandbox-novnc-port-closed" "$(code_open "http://${ip}:${NOVNC_PORT}/vnc.html")" 000 \
        "websockify must listen on loopback only: ${ip}:${NOVNC_PORT} must refuse the host"
    expect "gate-401-without-secret" "$(code_open "http://${ip}:${GATE_PORT}/cdp/json/list")" 401 \
        "the gate must refuse /cdp to a request without X-Sandbox-Secret"

# The refusal has to say what it is: a caller that gets a bare 401 from a proxy cannot tell the
# gate's own lock from Chromium refusing a Host header, and those two have different fixes.
unauth=$(curl -s -m 3 "http://${ip}:${GATE_PORT}/cdp/json/list" 2>/dev/null || true)
case "$unauth" in
    *unauthorized*) pass "gate-401-body" ;;
    *) fail "gate-401-body" "the refusal body was '${unauth:-<empty>}', want the gate's own unauthorized error" ;;
esac

    expect "gate-cdp-with-secret" "$(code_open -H "X-Sandbox-Secret: ${secret}" "http://${ip}:${GATE_PORT}/cdp/json/list")" 200 \
        "the gate must proxy /cdp for a caller that has X-Sandbox-Secret"
    expect "gate-healthz-open" "$(code_open "http://${ip}:${GATE_PORT}/healthz")" 200 \
        "/healthz must answer 200 without a secret: it is the liveness probe, and it touches no upstream"
    expect "gate-novnc-with-secret" "$(code_open -H "X-Sandbox-Secret: ${secret}" "http://${ip}:${GATE_PORT}/novnc/vnc.html")" 200 \
        "the gate must proxy /novnc for a caller that has X-Sandbox-Secret: noVNC is a second door, and it is behind the same lock"
    # Two facts, and the order between them is the point. Without the secret an unknown path
    # is refused like any other request, because the gate checks the secret before it looks at
    # the path: a caller that is not allowed in does not get to learn which paths exist. With
    # the secret the same path is a 404, because now the answer is about the path.
    expect "gate-401-before-routing" "$(code_open "http://${ip}:${GATE_PORT}/nope")" 401 \
        "an unknown path without X-Sandbox-Secret must be 401, not 404: the secret is checked before the path, so the gate never tells an unauthenticated caller which routes it has"
    expect "gate-unknown-path-404" "$(code_open -H "X-Sandbox-Secret: ${secret}" "http://${ip}:${GATE_PORT}/nope")" 404 \
        "anything outside /cdp, /novnc and /healthz must be 404 for a caller that has the secret: the gate publishes two doors, not a general proxy"
fi

log "11. noVNC goes through the control plane, with the token and without it"
# No Authorization header on either request, on purpose: this is the browser's path, where the
# token arrives once as a query parameter and is traded for an HttpOnly cookie.
novnc="http://127.0.0.1:${PORT}/v1/sessions/${BOT}/novnc/vnc.html"
expect "novnc-with-token" "$(curl -s -o /dev/null -m 20 -w '%{http_code}' "${novnc}?token=${TOKEN}")" 200 \
    "the noVNC client through the proxy, with ?token=, must answer 200"
expect "novnc-without-token" "$(curl -s -o /dev/null -m 20 -w '%{http_code}' "$novnc")" 401 \
    "the same URL without the token must be refused"

# The websocket is the half of noVNC a page load never reaches: the client opens it after the
# page has loaded, and it is the one request in this system that neither of the two proxies may
# buffer or rewrite. Both of them are `httputil.ReverseProxy`, which answers an upgrade by
# hijacking the connection and copying bytes in both directions, and the only way to prove that
# survived the extra hop is a handshake that comes back 101 rather than a 200 with an error body.
# The subprotocol header is sent because noVNC's own client sends it, and the token is in the
# query string because a websocket handshake cannot carry an Authorization header. The cap is
# what ends this request: an upgrade that worked leaves the connection open, because that is what
# an upgraded connection is, so curl is stopped by the timeout rather than by the server — after
# it has already read the 101 it prints.
ws=$(curl -s -o /dev/null -m 5 -w '%{http_code}' \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
    -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Protocol: binary' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    "http://127.0.0.1:${PORT}/v1/sessions/${BOT}/novnc/websockify?token=${TOKEN}" 2>/dev/null || true)
expect "novnc-websocket-upgrade" "$ws" 101 \
    "the websocket handshake through the control plane and the gate must answer 101: without it noVNC loads a page it can never use"

log "12. delete ends the container, the volume outlives it"
code=$(status -X DELETE "${BASE}/v1/sessions/${BOT}")
case "$code" in
    2*) pass "delete" ;;
    *) fail "delete" "DELETE answered ${code}, want 2xx" ;;
esac
expect "delete-twice-404" "$(status -X DELETE "${BASE}/v1/sessions/${BOT}")" 404 \
    "a second DELETE must be 404: the session is gone, and answering 200 again would be a lie"

back=$(api -X POST -H 'Content-Type: application/json' -d "{\"bot_id\":\"${BOT}\"}" "${BASE}/v1/sessions")
expect "ensure-after-delete-running" "$(json_str "$back" state)" running \
    "ensure after a delete must start the session again"
vol2=$(json_str "$back" volume)
if [ -n "$vol1" ] && [ "$vol2" = "$vol1" ]; then
    pass "volume-reused"
else
    fail "volume-reused" "the volume after the restart is ${vol2:-<none>}, was ${vol1:-<none>}: the browser profile must outlive the container"
fi

# ------------------------------------------------------------------- the operator's URL

log "the session is up: open this in a browser"
# The token is in the URL on purpose — a page load cannot carry an Authorization header, and
# this is the line a human copies. It is a throwaway secret generated at the top of this run,
# and the log is the evidence file, so it stays in the log.
novnc_path=$(json_str "$back" novnc_url)
if [ -z "$novnc_path" ]; then
    novnc_path="/v1/sessions/${BOT}/novnc/vnc.html?autoconnect=true&resize=scale&path=v1/sessions/${BOT}/novnc/websockify"
fi
case "$novnc_path" in
    *\?*) sep='&' ;;
    *) sep='?' ;;
esac
printf 'noVNC: http://127.0.0.1:%s%s%stoken=%s\n' "$PORT" "$novnc_path" "$sep" "$TOKEN"

# --------------------------------------------------------- what an operator can end a session
# Both of these end a sandbox, and both sit after the URL above on purpose: that URL is a page a
# human opens, so it is printed while the session it names is still running. Nothing here is
# undone by re-ensuring the same bot — a purge removes the home volume as well — which is why
# they cannot stand in the middle of the checklist either.

log "13. purge ends the container and the home volume"
# The pair of the delete in check 12: the same route, asked to remove what a session owns rather
# than only what it runs. It is what a caller uses when the bot is finished rather than idle, and
# it is the difference between "the browser profile outlives the container" (volume-reused above)
# and "this bot has no home directory any more".
code=$(status -X DELETE "${BASE}/v1/sessions/${BOT}?purge=true")
case "$code" in
    2*) pass "purge" ;;
    *) fail "purge" "DELETE ?purge=true answered ${code}, want 2xx" ;;
esac

# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
if $DOCKER inspect "$SANDBOX_CONTAINER" >/dev/null 2>&1; then
    fail "purge-container-gone" "docker inspect ${SANDBOX_CONTAINER} still succeeds: purge must remove the container"
else
    pass "purge-container-gone"
fi

# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
if $DOCKER volume ls -q --filter label=vcomputer.bot 2>/dev/null | grep -qx "$SANDBOX_VOLUME"; then
    fail "purge-volume-gone" "${SANDBOX_VOLUME} is still listed: purge must remove the bot's home volume"
else
    pass "purge-volume-gone"
fi

log "14. the admin route sweeps a stopped sandbox and its volume"
# A second bot, started through the route a worker uses and then stopped with the docker CLI: a
# container that exists and is not running is exactly what the sweep is for, and a bot this run is
# not otherwise about keeps the two endings from interfering. `volumes=true` is the part that
# matters for TEARDOWN=1 — without it the volume survives its container, which is what the route
# does by design — and the two counters in the answer are what say the sweep did something rather
# than answering 200 to a request it ignored.
expect "cleanup-stale-ensure" \
    "$(status -X POST -H 'Content-Type: application/json' -d "{\"bot_id\":\"${STALE_BOT}\"}" "${BASE}/v1/sessions")" \
    200 "POST /v1/sessions for ${STALE_BOT} must start a second sandbox"
# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
$DOCKER stop "$STALE_CONTAINER" >/dev/null 2>&1 || true

# One request answers all three questions — the route answered, and it counted what it removed.
# The counters are the part that matters: a sweep that answers 200 and removes nothing is exactly
# the failure a route like this has.
cleaned=$(api -X POST -w '\n%{http_code}' "${BASE}/v1/admin/cleanup?volumes=true")
expect "cleanup-route" "$(printf '%s\n' "$cleaned" | sed -n '$p')" 200 \
    "POST /v1/admin/cleanup?volumes=true must answer 200"
cleaned=$(printf '%s\n' "$cleaned" | sed '$d')
expect "cleanup-removed-container" "$(json_num "$cleaned" removed_containers)" 1 \
    "the sweep must report the one stopped sandbox it removed: ${cleaned:-<empty>}"
expect "cleanup-removed-volume" "$(json_num "$cleaned" removed_volumes)" 1 \
    "?volumes=true must remove that sandbox's volume too, and count it: ${cleaned:-<empty>}"

# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
if $DOCKER inspect "$STALE_CONTAINER" >/dev/null 2>&1; then
    fail "cleanup-container-gone" "docker inspect ${STALE_CONTAINER} still succeeds: the sweep must remove a stopped sandbox"
else
    pass "cleanup-container-gone"
fi

# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
if $DOCKER volume ls -q --filter label=vcomputer.bot 2>/dev/null | grep -qx "$STALE_VOLUME"; then
    fail "cleanup-volume-gone" "${STALE_VOLUME} is still listed: ?volumes=true must remove the volume as well"
else
    pass "cleanup-volume-gone"
fi

finish
