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
#   preconditions                 the tools and the compose file are where this expects them
#   build / compose-up / health   the stack starts at all
#   auth-401                      /v1/* is not an open door
#   ensure-idempotent             a worker may call ensure before every task
#   navigate-fixture              the sandbox browser reaches the internal network
#   screenshot-*                  the capture is a real PNG of a real display, not the
#                                 placeholder the stub used to answer with
#   click / type / screenshot-typed / typed-screenshot-differs
#                                 input reaches the X server, and the screen shows it
#   exec-*                        a command runs inside the sandbox as bolu, not in a stub
#   cdp-version                   socat exposes Chromium's DevTools port on the container's
#                                 address, which is the only address anything can reach
#   network-internal / no-published-ports / no-egress
#                                 the sandbox sits on an internal network, publishes nothing,
#                                 and cannot reach the internet
#   novnc-*                       the desktop is reachable through the control plane only,
#                                 and only with the token
#   delete / delete-twice-404 / ensure-after-delete-running / volume-reused
#                                 delete ends the container, the volume outlives it, and a
#                                 second delete is still a 404
#
# Usage, from the repository root:
#
#   apps/virtual-computer/scripts/e2e.sh
#   DOCKER="sudo docker" apps/virtual-computer/scripts/e2e.sh
#   BUILD_NETWORK=host apps/virtual-computer/scripts/e2e.sh   # a daemon with no route out
#   PREBUILT=1 apps/virtual-computer/scripts/e2e.sh           # the images are already tagged
#   TEARDOWN=1 apps/virtual-computer/scripts/e2e.sh           # remove what this started
#
# Environment:
#   DOCKER                  the docker CLI. "sudo docker" is expected to work, which is why
#                           every use of it below is unquoted on purpose (shellcheck SC2086
#                           is silenced where it fires, with that reason).
#   COMPOSE                 the whole compose invocation. Default:
#                           "$DOCKER compose -f apps/virtual-computer/docker-compose.poc.yml".
#   OUT                     artefact directory. Default docs/poc-evidence/virtual-computer.
#   VCOMPUTER_CONTROL_PORT  host port of the control plane, exported to compose so both sides
#                           agree on it. Default 18090 — deliberately not the compose default
#                           (8090), so a stack someone left running for a demo cannot answer
#                           this script's requests by accident.
#   BOT                     bot id under test. Default e2e-bot.
#   PREBUILT=1              the two images are already tagged: do not build, do not rebuild
#                           on `up`.
#   BUILD_NETWORK=host      build each image with `docker build --network host` instead of
#                           `compose --profile build build`.
#   TEARDOWN=1              stop the stack at the end. Off by default: after a failure the
#                           containers and the per-bot volume are the state a reviewer wants
#                           to look at.
#
# Needs bash, curl, sed, od, tr, cut, wc, sha256sum, and the docker CLI; openssl if present
# and /dev/urandom if not. No jq and no python3: the JSON read here is one level deep, and an
# evidence script that needs a package manager is an evidence script nobody reruns.

# A failed check must not stop the run: the point of the log is the whole list of what broke,
# not the first thing that did.
set -uo pipefail

# Every path below is relative to the repository root, so put ourselves there rather than
# trusting the caller's working directory (the same convention as the smoke scripts).
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1

DOCKER=${DOCKER:-docker}
COMPOSE=${COMPOSE:-"$DOCKER compose -f apps/virtual-computer/docker-compose.poc.yml"}
OUT=${OUT:-docs/poc-evidence/virtual-computer}
PORT=${VCOMPUTER_CONTROL_PORT:-18090}
BOT=${BOT:-e2e-bot}
BASE="http://127.0.0.1:${PORT}"
COMPOSE_FILE="apps/virtual-computer/docker-compose.poc.yml"
CONTROL_IMAGE="vcomputer-control:poc"
SANDBOX_IMAGE="vcomputer-sandbox:poc"

mkdir -p "$OUT"

# Everything below lands in the log as well as on the terminal. Fds 3 and 4 keep the real
# stdout and stderr so the tee can be closed and flushed before the script exits: the summary
# is the last line written, and losing it to a half-flushed pipe would be a small, stupid bug
# in the one file a reviewer reads first.
exec 3>&1 4>&2
exec > >(tee "$OUT/after-status.txt") 2>&1

printf '=== virtual computer PoC — e2e, %s ===\n' "$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
printf 'compose: %s\nbot:     %s\ncontrol: %s\n' "$COMPOSE_FILE" "$BOT" "$BASE"

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

# One token for the whole run: generated here, exported, and handed to compose by
# interpolation. VCOMPUTER_CONTROL_TOKEN is required — the control plane refuses to start
# without it — so the value has to be in the environment before compose reads the file.
# openssl is the happy path; /dev/urandom is the fallback so a machine without openssl can
# still produce evidence.
TOKEN=$(openssl rand -hex 16 2>/dev/null || true)
if [ -z "$TOKEN" ]; then
    TOKEN=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
fi
export VCOMPUTER_CONTROL_TOKEN="$TOKEN"

# Same reasoning for the port: the compose file interpolates this variable into
# 127.0.0.1:${VCOMPUTER_CONTROL_PORT}:8090, and a control plane listening somewhere other than
# where this script curls it is a script that fails for the wrong reason.
export VCOMPUTER_CONTROL_PORT="$PORT"

# The timeout is generous on purpose: `ensure` starts a container and waits for Chromium's
# DevTools port, and the runtime polls for up to ~45s before it answers.
api() { curl -s -m 180 -H "Authorization: Bearer ${TOKEN}" "$@"; }
status() { curl -s -o /dev/null -m 180 -w '%{http_code}' -H "Authorization: Bearer ${TOKEN}" "$@"; }
# The same call with no credential at all, for the checks that are about the refusal.
status_open() { curl -s -o /dev/null -m 20 -w '%{http_code}' "$@"; }

# The two readers this script needs. sed rather than jq: the bodies are one line and one level
# deep, and a check that cannot run without a JSON tool is a check that silently stops
# checking.
json_str() { printf '%s' "$1" | sed -n "s/.*\"$2\": *\"\([^\"]*\)\".*/\1/p" | head -n 1; }
json_num() { printf '%s' "$1" | sed -n "s/.*\"$2\": *\(-\{0,1\}[0-9][0-9]*\).*/\1/p" | head -n 1; }
sha() { sha256sum "$1" | cut -d' ' -f1; }

# The last thing the run does: teardown (only if asked), the summary, the exit status, and the
# flush of the tee.
finish() {
    if [ "${TEARDOWN:-0}" = "1" ]; then
        log "teardown (TEARDOWN=1)"
        # The per-bot volume is left behind even here: it is the bot's home directory, and
        # deleting a home directory is a data decision, not a cleanup step. Remove it by hand
        # with `$DOCKER volume rm vcomputer-<bot>-home`.
        # shellcheck disable=SC2086  # COMPOSE is a word list; see the header
        $COMPOSE down
        # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
        $DOCKER rm -f "vcomputer-${BOT}" >/dev/null 2>&1 || true
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
for tool in curl sed od tr cut wc sha256sum mkdir sleep; do
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

# --------------------------------------------------------------------------------- build

log "build: $CONTROL_IMAGE and $SANDBOX_IMAGE"

# The compose build is the default because it is the one command the README documents, and it
# is where the image tags live (the `vcomputer-sandbox` service exists only as a build target
# behind the `build` profile). BUILD_NETWORK=host is for the daemon that cannot reach a
# registry: build's default bridge has no DNS on some hosts, and `--network host` sidesteps
# it — at the cost of two builds instead of one compose invocation.
PREBUILT=${PREBUILT:-0}
if [ "${BUILD_NETWORK:-}" = "host" ]; then
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

# The fixture is the page the browser opens. It lives on the internal network, so it is the
# only URL a sandbox can navigate to — which is what makes the navigate check meaningful.
up_flags=""
if [ "$PREBUILT" = "1" ]; then
    # `--no-build` keeps the tags that already exist instead of rebuilding them behind the
    # operator's back.
    up_flags="--no-build"
fi
# shellcheck disable=SC2086  # COMPOSE and the flag above are word lists by design
if $COMPOSE up -d $up_flags vcomputer-control vcomputer-fixture; then
    pass "compose-up"
else
    fail "compose-up" "$COMPOSE up -d failed (does the compose file still name the services vcomputer-control and vcomputer-fixture?)"
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

log "4. navigate the sandbox browser at the fixture"
# vcomputer-fixture is a name on the internal network, so a 200 here proves the sandbox has a
# route to it and that Chromium's HTTP endpoint accepted the new target.
expect "navigate-fixture" \
    "$(status -X POST -H 'Content-Type: application/json' -d '{"url":"http://vcomputer-fixture:8080/"}' "${BASE}/v1/sessions/${BOT}/navigate")" \
    200 "navigate to the fixture on the internal network must be accepted"

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

stub="${OUT}/stub-screenshot.png"
if [ ! -f "$stub" ]; then
    fail "screenshot-not-stub" "${stub} is missing, so there is nothing to compare the capture against"
elif [ "$(sha "$shot")" != "$(sha "$stub")" ]; then
    pass "screenshot-not-stub"
else
    fail "screenshot-not-stub" "the capture is byte-identical to ${stub}: the runtime is still answering the stub"
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

log "8. the CDP port is reachable from the control plane"
# cdp_url is an address on the internal network, so the only place it can be reached from is a
# container on that network — the control plane. wget from there is the check that socat
# exposed Chromium's DevTools port on the container's address and not only on the sandbox's
# own loopback.
session=$(api "${BASE}/v1/sessions/${BOT}")
cdp=$(json_str "$session" cdp_url)
if [ -z "$cdp" ]; then
    fail "cdp-version" "the session has no cdp_url: ${session:-<empty>}"
else
    # shellcheck disable=SC2086  # DOCKER may be "sudo docker"
    version=$($DOCKER exec vcomputer-control wget -qO- "${cdp}/json/version" 2>&1)
    case "$version" in
        *Browser*) pass "cdp-version" ;;
        *) fail "cdp-version" "wget ${cdp}/json/version from the control plane answered '${version}'" ;;
    esac
fi

log "9. the sandbox is isolated"
# Three claims that fail for three different reasons, so they are three checks: the network was
# created without `internal: true`, the runtime published a port, or the sandbox has a route
# out. Any one of them is a different bug with a different fix.
# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
internal=$($DOCKER network inspect vcomputer-internal -f '{{.Internal}}' 2>/dev/null)
expect "network-internal" "$internal" "true" "vcomputer-internal must exist with internal: true"

# shellcheck disable=SC2086  # DOCKER may be "sudo docker"
ports=$($DOCKER port "vcomputer-${BOT}" 2>/dev/null)
expect "no-published-ports" "$ports" "" "vcomputer-${BOT} must publish no port: a sandbox is reached through the control plane, never from the host"

body=$(api -X POST -H 'Content-Type: application/json' -d '{"command":"curl -s -m 5 -o /dev/null https://example.com"}' "${BASE}/v1/sessions/${BOT}/exec")
code=$(json_num "$body" exit_code)
if [ -n "$code" ] && [ "$code" != "0" ]; then
    pass "no-egress"
else
    fail "no-egress" "curl to https://example.com inside the sandbox answered exit_code=${code:-<none>}, want non-zero: the internal network must have no route out"
fi

log "10. noVNC goes through the control plane, with the token and without it"
# No Authorization header on either request, on purpose: this is the browser's path, where the
# token arrives once as a query parameter and is traded for an HttpOnly cookie.
novnc="http://127.0.0.1:${PORT}/v1/sessions/${BOT}/novnc/vnc.html"
expect "novnc-with-token" "$(curl -s -o /dev/null -m 20 -w '%{http_code}' "${novnc}?token=${TOKEN}")" 200 \
    "the noVNC client through the proxy, with ?token=, must answer 200"
expect "novnc-without-token" "$(curl -s -o /dev/null -m 20 -w '%{http_code}' "$novnc")" 401 \
    "the same URL without the token must be refused"

log "11. delete ends the container, the volume outlives it"
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
    novnc_path="/v1/sessions/${BOT}/novnc/vnc.html?autoconnect=1&resize=scale&path=v1/sessions/${BOT}/novnc/websockify"
fi
case "$novnc_path" in
    *\?*) sep='&' ;;
    *) sep='?' ;;
esac
printf 'noVNC: http://127.0.0.1:%s%s%stoken=%s\n' "$PORT" "$novnc_path" "$sep" "$TOKEN"

finish
