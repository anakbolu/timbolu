#!/usr/bin/env bash
#
# End-to-end smoke for the approval gate: an action that reaches outside Bolu is
# stopped at a draft a person decides on, and a revision sends the task back to
# its agent with the note.
#
# It runs the real API and the real agent worker against the compose Postgres,
# Redis, and Temporal, with the model gateway pointed at a stub. The point is the
# gate, not the provider.
#
# Three phases, because three things can only be wrong in the wiring:
#
#   1. a write_external tool call becomes a draft, the task parks, and the draft
#      is visible in the thread and in the API — nothing left;
#   2. a revision sends the task back to its agent with the note, and the agent
#      proposes again;
#   3. an approval carries the action out, and with no integration executor yet
#      it stops at that boundary and says so, rather than being recorded as sent.
#
# Usage: scripts/smoke-approval.sh
set -euo pipefail

cd "$(dirname "$0")/.."

API_PORT="${API_PORT:-8091}"
STUB_PORT="${STUB_PORT:-11435}"
DB="${DATABASE_URL:-postgres://employeebot:employeebot_secret@localhost:5432/employeebot?sslmode=disable}"

log() { printf '\n==> %s\n' "$*"; }
fail() { printf '\n!! %s\n' "$*" >&2; exit 1; }

cleanup() {
    stop_child() {
        local pid="${1:-}"
        [[ -n "$pid" ]] || return 0
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    }

    stop_child "${WORKER_PID:-}"
    stop_child "${API_PID:-}"
    stop_child "${STUB_PID:-}"
    stop_child "${GMAIL_PID:-}"
    [[ -n "${BIN_DIR:-}" ]] && rm -rf "$BIN_DIR"
    rm -f /tmp/smoke-approval-stub.py
}
trap cleanup EXIT

# --------------------------------------------------------------- preconditions

for port in "$API_PORT" "$STUB_PORT"; do
    if lsof -ti ":${port}" >/dev/null 2>&1; then
        fail "port ${port} is already in use; stop whatever is listening on it first"
    fi
done

command -v docker >/dev/null || fail "docker is required: the smoke uses the compose stack"
docker exec bolu-postgres pg_isready -U employeebot >/dev/null 2>&1 ||
    fail "the compose Postgres is not reachable; run 'make up' first"

# The compose workers poll the same Temporal task queue this smoke does, so a
# running one executes the smoke's task with its own model configuration: a bound
# this phase sets is then never the bound that stops it, and the phase fails for a
# reason that has nothing to do with the code. The check is cheap and the failure
# it prevents is expensive to diagnose.
for container in bolu-agent-worker bolu-integration-worker; do
    if docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null | grep -q true; then
        fail "${container} is running and polls the same task queue; stop it with 'docker stop bolu-api bolu-agent-worker bolu-integration-worker'"
    fi
done

# ------------------------------------------------------------- stub provider

cat > /tmp/smoke-approval-stub.py <<'PY'
import json, os, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

# The stub asks for a write_external action on the first round of every task, so
# the run always reaches the approval gate rather than depending on a real model
# choosing to send something.
MODE = os.environ.get("STUB_MODE", "send")

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        model = body.get("model", "stub")

        offered = [
            t["function"]["name"]
            for t in body.get("tools", [])
            if t.get("function", {}).get("name")
        ]

        # A send tool is what this smoke needs; without one the run is not
        # exercising the gate at all, so it says so instead of quietly answering.
        send = next((name for name in offered if name.endswith((".send", ".send_message"))), None)

        already_proposed = any(
            message.get("role") == "tool"
            for message in body.get("messages", [])
        )

        if MODE == "send" and send and not already_proposed:
            payload = {
                "id": "chatcmpl-approval",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": "",
                        "tool_calls": [{
                            "id": "call-send",
                            "type": "function",
                            "function": {
                                "name": send,
                                # Exactly what the tool accepts. An argument the
                                # tool's schema forbids is refused by the
                                # validator before anything is sent, which is a
                                # thing worth testing but not the thing this run
                                # is about.
                                "arguments": json.dumps({
                                    "to": "pelanggan@example.com",
                                    "subject": "Tagihan Maret",
                                    "body": "Mohon dibayar ya.",
                                }),
                            },
                        }],
                    },
                    "finish_reason": "tool_calls",
                }],
                "usage": {"prompt_tokens": 210, "completion_tokens": 25, "total_tokens": 235},
            }
        else:
            # After the gate answered, the agent reads the result and closes out.
            text = "Baik, saya tidak mengirimnya. Terima kasih catatannya."
            for message in body.get("messages", []):
                content = message.get("content") or ""
                if isinstance(content, list):
                    content = " ".join(part.get("text", "") for part in content)
                if "salah penerima" in content:
                    text = "Baik, penerimanya saya perbaiki dulu."
                    break
            payload = {
                "id": "chatcmpl-approval",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": text},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 150, "completion_tokens": 30, "total_tokens": 180},
            }

        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY

# ------------------------------------------------------------------ gmail stub
#
# The approval gate's other half is the executor EPIC 8 added: an approved action
# is carried out against a real provider. Google is stubbed here so the run proves
# the send without an account, and the stub records what it received so the smoke
# can check the message that arrived is the one the card showed.
cat > /tmp/smoke-approval-gmail.py <<'PY'
import base64, json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SENT = "/tmp/smoke-approval-gmail-sent.jsonl"

class Handler(BaseHTTPRequestHandler):
    def _send(self, payload, status=200):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        path = urlparse(self.path).path

        if path.endswith("/token"):
            form = parse_qs(body.decode())
            if form.get("grant_type", [""])[0] == "refresh_token":
                return self._send({"access_token": "ya29.refreshed", "expires_in": 3600})
            return self._send({"access_token": "ya29.connected", "refresh_token": "1//refresh",
                               "expires_in": 3600, "scope": "https://www.googleapis.com/auth/gmail.send"})

        if "/messages/send" in path:
            # The proof, written where the smoke reads it back.
            with open(SENT, "a") as log:
                log.write(body.decode() + "\n")
            return self._send({"id": "msg-sent", "threadId": "t1"})

        if path.endswith("/drafts"):
            return self._send({"id": "draft-1", "message": {"id": "m1"}})

        return self._send({"error": "unknown"}, 404)

    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path

        # The consent screen: it sends the browser back to the callback with a code
        # and the same state, which is what a real provider does.
        if path == "/auth":
            query = parse_qs(parsed.query)
            redirect = query.get("redirect_uri", [""])[0]
            state = query.get("state", [""])[0]
            self.send_response(302)
            self.send_header("Location", "{}?code=a-good-code&state={}".format(redirect, state))
            self.end_headers()
            return

        if path.endswith("/users/me/profile"):
            return self._send({"emailAddress": "toko@example.com"})
        return self._send({"error": "unknown"}, 404)

    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY

GMAIL_PORT="${GMAIL_PORT:-11436}"

start_gmail() {
    lsof -ti ":${GMAIL_PORT}" | xargs -r kill -9 2>/dev/null || true
    for _ in $(seq 1 20); do
        lsof -ti ":${GMAIL_PORT}" >/dev/null 2>&1 || break
        sleep 0.5
    done
    lsof -ti ":${GMAIL_PORT}" >/dev/null 2>&1 && fail "port ${GMAIL_PORT} did not free up"

    rm -f /tmp/smoke-approval-gmail-sent.jsonl
    python3 /tmp/smoke-approval-gmail.py "$GMAIL_PORT" &
    GMAIL_PID=$!

    for _ in $(seq 1 20); do
        curl -fsS "http://localhost:${GMAIL_PORT}/users/me/profile" >/dev/null 2>&1 && return 0
        kill -0 "$GMAIL_PID" 2>/dev/null || fail "the Gmail stub exited on startup"
        sleep 0.5
    done
    fail "the Gmail stub did not answer on :${GMAIL_PORT}"
}

stop_gmail() {
    [[ -n "${GMAIL_PID:-}" ]] || return 0
    kill "$GMAIL_PID" 2>/dev/null || true
    wait "$GMAIL_PID" 2>/dev/null || true
    GMAIL_PID=""
}

# ----------------------------------------------------------------- the binaries

log "building the API and the agent worker"
BIN_DIR="$(mktemp -d)"
CGO_ENABLED=0 go build -o "${BIN_DIR}/api" ./cmd/api
CGO_ENABLED=0 go build -o "${BIN_DIR}/agent-worker" ./cmd/agent-worker

start_stub() {
    stop_stub
    for _ in $(seq 1 20); do
        lsof -ti ":${STUB_PORT}" >/dev/null 2>&1 || break
        sleep 0.5
    done
    lsof -ti ":${STUB_PORT}" >/dev/null 2>&1 && fail "port ${STUB_PORT} did not free up"

    STUB_MODE="${1:-send}" python3 /tmp/smoke-approval-stub.py "$STUB_PORT" &
    STUB_PID=$!

    for _ in $(seq 1 20); do
        curl -fsS -X POST "http://localhost:${STUB_PORT}/v1/chat/completions" \
            -H 'Content-Type: application/json' -d '{"messages":[]}' >/dev/null 2>&1 && return 0
        kill -0 "$STUB_PID" 2>/dev/null || fail "the stub model provider exited on startup"
        sleep 0.5
    done
    fail "the stub model provider did not answer on :${STUB_PORT}"
}

stop_stub() {
    [[ -n "${STUB_PID:-}" ]] || return 0
    kill "$STUB_PID" 2>/dev/null || true
    wait "$STUB_PID" 2>/dev/null || true
    STUB_PID=""
}

start_stack() {
    run_with_env "HTTP_ADDRESS=:${API_PORT}
MAIL_DRIVER=log
FRONTEND_URL=http://localhost:3000
CONTENT_ORIGIN=http://localhost:${API_PORT}" \
        "${BIN_DIR}/api" > /tmp/smoke-approval-api.log 2>&1 &
    API_PID=$!

    run_with_env "MAIL_DRIVER=log" \
        "${BIN_DIR}/agent-worker" > /tmp/smoke-approval-worker.log 2>&1 &
    WORKER_PID=$!

    for _ in $(seq 1 90); do
        curl -fsS "${API}/health" >/dev/null 2>&1 && return 0
        kill -0 "$API_PID" 2>/dev/null || fail "the API exited during startup; see /tmp/smoke-approval-api.log"
        sleep 1
    done
    fail "the API did not become ready; see /tmp/smoke-approval-api.log"
}

stop_stack() {
    for pid in "${WORKER_PID:-}" "${API_PID:-}"; do
        [[ -n "$pid" ]] || continue
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    WORKER_PID=""
    API_PID=""
    sleep 2
}

log "starting the Gmail stub on :${GMAIL_PORT}"
start_gmail

log "starting the stub model provider on :${STUB_PORT}"
start_stub send

# The stack's configuration. It is a shell variable rather than an exported one,
# and each process is handed its environment explicitly in start_stack, so a typo
# is caught where it is read instead of becoming an exported variable that shadows
# the value a later phase set.
LLM_DEFAULT_ADAPTER=openai
LLM_DEFAULT_BASE_URL="http://localhost:${STUB_PORT}"
# The Gmail adapter is registered against the stub, so an approved action reaches
# the send boundary and goes through it.
GMAIL_CLIENT_ID=stub-client
GMAIL_CLIENT_SECRET=stub-secret
GMAIL_API_BASE="http://localhost:${GMAIL_PORT}"
GMAIL_AUTH_ENDPOINT="http://localhost:${GMAIL_PORT}/auth"
GMAIL_TOKEN_ENDPOINT="http://localhost:${GMAIL_PORT}/token"
LLM_DEFAULT_MODEL=stub
LLM_DEFAULT_API_KEY=stub-key
TASK_MAX_STEPS=6

# stack_env prints the environment both processes share, so the API and the worker
# cannot be started with different settings.
#
# It includes the key ring that seals integration tokens and the key that signs the
# value a connection flow carries. Without both, an account cannot be connected, and
# the approval this smoke is about has nothing to send through.
stack_env() {
    printf '%s\n' \
        "ENVIRONMENT=local" \
        "DATABASE_URL=${DB}" \
        "REDIS_URL=redis://localhost:6379/0" \
        "TEMPORAL_HOST_PORT=localhost:7233" \
        "LOG_LEVEL=info" \
        "LOG_FORMAT=console" \
        "SECRET_ENCRYPTION_KEY=local_dev_secret_key_32_byte_len" \
        "KMS_MASTER_KEY=local_dev_master_key_32_byte_len" \
        "KMS_MASTER_KEY_ID=smoke-v1" \
        "OAUTH_STATE_KEY=local_dev_oauth_state_key_32_bytes" \
        "OAUTH_REDIRECT_BASE=http://localhost:${API_PORT}" \
        "LLM_DEFAULT_ADAPTER=${LLM_DEFAULT_ADAPTER}" \
        "LLM_DEFAULT_BASE_URL=${LLM_DEFAULT_BASE_URL}" \
        "GMAIL_CLIENT_ID=${GMAIL_CLIENT_ID}" \
        "GMAIL_CLIENT_SECRET=${GMAIL_CLIENT_SECRET}" \
        "GMAIL_API_BASE=${GMAIL_API_BASE}" \
        "GMAIL_AUTH_ENDPOINT=${GMAIL_AUTH_ENDPOINT}" \
        "GMAIL_TOKEN_ENDPOINT=${GMAIL_TOKEN_ENDPOINT}" \
        "LLM_DEFAULT_MODEL=${LLM_DEFAULT_MODEL}" \
        "LLM_DEFAULT_API_KEY=${LLM_DEFAULT_API_KEY}" \
        "STORAGE_DRIVER=local" \
        "STORAGE_LOCAL_ROOT=.storage" \
        "TASK_MAX_STEPS=${TASK_MAX_STEPS:-6}"
}

# run_with_env runs one command with the shared environment plus extra pairs.
#
# It execs the command rather than running it as a child: the caller records `$!`
# to stop the process later, and a wrapper subshell would make that pid the
# wrapper's. Killing the wrapper leaves the real API or worker alive and still
# polling the task queue, and the next run's work is then executed by the previous
# run's binary — which is a confusing way to fail.
#
# Reading the pairs from a file keeps an argument with a space in it — the
# content-frame list — from being split into two.
run_with_env() {
    local extra="$1"
    shift

    local env_file
    env_file="$(mktemp)"
    stack_env > "$env_file"
    printf '%s\n' "$extra" >> "$env_file"

    set -a
    # shellcheck disable=SC1090
    . "$env_file"
    set +a
    rm -f "$env_file"

    exec "$@"
}

API="http://localhost:${API_PORT}/api"
start_stack

# ------------------------------------------------------------------ the account

log "registering and onboarding a workspace"
EMAIL="approval-$(date +%s)@example.com"
PASSWORD="Rahasia123!"
curl -fsS -X POST "${API}/auth/register" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

TOKEN=""
for _ in $(seq 1 20); do
    TOKEN=$(grep -o 'token=[A-Za-z0-9_-]*' /tmp/smoke-approval-api.log | tail -1 | cut -d= -f2 || true)
    [[ -n "$TOKEN" ]] && break
    sleep 1
done
[[ -n "$TOKEN" ]] || fail "no verification token in /tmp/smoke-approval-api.log"

curl -fsS -X POST "${API}/auth/verify-email" -H 'Content-Type: application/json' \
    -d "{\"token\":\"${TOKEN}\"}" >/dev/null

ACCESS=$(curl -fsS -X POST "${API}/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])')

ONBOARD=$(curl -fsS -X POST "${API}/workspaces/onboard" -H "Authorization: Bearer ${ACCESS}" \
    -H 'Content-Type: application/json' \
    -d '{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}')
WORKSPACE=$(echo "$ONBOARD" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["workspace"]["id"])')
AUTH=(-H "Authorization: Bearer ${ACCESS}" -H "X-Workspace-Id: ${WORKSPACE}")

# A workspace starts with no Bolu, so the smoke makes the one it needs the way a
# person does: it picks the team onboarding created and adds an agent to it.
TEAM=$(curl -fsS "${API}/teams" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([t for t in d if t["kind"]=="bolu"][0]["id"])')

AGENT=$(curl -fsS -X POST "${API}/agents" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"team_id\":\"${TEAM}\",\"name\":\"Oren\",\"template_key\":\"oren\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')
echo "Oren = ${AGENT}"

# The account is connected the way a person connects one: the flow is started,
# the provider's consent screen is followed, and the callback stores the sealed
# token. Seeding the row directly would leave `status = connected` with no token,
# which is exactly the lie the executor refuses.
log "connecting a Gmail account through the authorization flow"
CONSENT_BODY=$(curl -sS "${API}/integrations/gmail/connect?return_to=/dashboard/integrations" "${AUTH[@]}")
CONSENT=$(echo "$CONSENT_BODY" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["url"])' 2>/dev/null) ||
    fail "the flow could not be started: ${CONSENT_BODY}"

# The stub plays the consent screen: it answers with a redirect to the callback,
# carrying a code and the state the flow issued.
CALLBACK=$(curl -sS -o /dev/null -w '%{redirect_url}' "$CONSENT")
[[ -n "$CALLBACK" ]] || fail "the provider did not redirect back to the callback"

CALLBACK_BODY=$(curl -sS "$CALLBACK" 2>&1) ||
    fail "the callback did not answer: ${CALLBACK_BODY} at ${CALLBACK}"

CONNECTED=$(curl -fsS "${API}/integrations" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([c["integration_id"] for c in d if c["status"]=="connected"][0] if any(c["status"]=="connected" for c in d) else "")')
[[ -n "$CONNECTED" ]] || fail "the account was not connected, so the tool filter would be exercised against nothing"

# And the Bolu is granted it, which is what puts the tool in its list.
curl -fsS -X PUT "${API}/agents/${AGENT}/grants/${CONNECTED}" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d '{"permission":"read_write"}' >/dev/null

GRANTED=$(docker exec bolu-postgres psql -U employeebot -d employeebot -tAc \
    "SELECT count(*) FROM agent_grants WHERE workspace_id = '${WORKSPACE}' AND agent_id = '${AGENT}'")
[[ "$GRANTED" == "1" ]] || fail "the grant was not stored, so the tool filter would be exercised against nothing"

# An open window for the first phases, so an approved action reaches the send
# boundary rather than being held for tomorrow morning. Phase 4 narrows it again
# to test the hold itself.
docker exec -i bolu-postgres psql -U employeebot -d employeebot -q <<SQL
UPDATE workspaces SET work_hours_start = '00:00', work_hours_end = '23:59'
WHERE id = '${WORKSPACE}';
SQL

CONVERSATION=$(curl -fsS -X POST "${API}/conversations/direct" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

# wait_for_draft prints the newest draft's id, waiting for one to appear.
wait_for_draft() {
    local deadline=$((SECONDS + 60)) id=""
    while (( SECONDS < deadline )); do
        id=$(curl -fsS "${API}/drafts?limit=1" "${AUTH[@]}" |
            python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["draft_id"] if d else "")')
        [[ -n "$id" ]] && break
        sleep 1
    done
    printf '%s' "$id"
}

draft_status() {
    curl -fsS "${API}/drafts/$1" "${AUTH[@]}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])'
}

task_status() {
    curl -fsS "${API}/tasks/$1" "${AUTH[@]}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])'
}

wait_for_task_status() {
    local task="$1" want="$2" deadline=$((SECONDS + 60)) status=""
    while (( SECONDS < deadline )); do
        status=$(task_status "$task")
        [[ "$status" == "$want" ]] && break
        sleep 1
    done
    printf '%s' "$status"
}

# --------------------------------------------------- phase 1: the gate stops it

log "phase 1: an action that reaches outside Bolu becomes a draft"
curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Tolong tagih pelanggan A ya.","reply":true}' >/dev/null

DRAFT_ID=$(wait_for_draft)
[[ -n "$DRAFT_ID" ]] || fail "no draft was created for a write_external tool call"
log "draft ${DRAFT_ID}"

DRAFT_JSON=$(curl -fsS "${API}/drafts/${DRAFT_ID}" "${AUTH[@]}")
echo "$DRAFT_JSON" | python3 -m json.tool

python3 - "$DRAFT_JSON" <<'PY'
import json, sys
draft = json.loads(sys.argv[1])["data"]
assert draft["status"] == "pending", draft["status"]
assert draft["action_kind"].endswith((".send", ".send_message")), draft["action_kind"]
assert draft["payload"].get("to") == "pelanggan@example.com", draft["payload"]
assert draft["title"], "the card needs a title"
# Nothing may have left: the whole point of the gate.
assert not draft.get("sent_at"), "the action left without a decision"
print("  fine: pending draft, payload intact, nothing sent")
PY

PHASE1_TASK=$(echo "$DRAFT_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["task_id"])')
PARKED=$(wait_for_task_status "$PHASE1_TASK" "waiting_approval")
[[ "$PARKED" == "waiting_approval" ]] ||
    fail "the task did not park on its draft; status is ${PARKED}"

TASK_JSON=$(curl -fsS "${API}/tasks/${PHASE1_TASK}" "${AUTH[@]}")
echo "$TASK_JSON" | python3 -c '
import json, sys
task = json.load(sys.stdin)["data"]
print("  task status={} health={} waiting={}".format(
    task["status"], task["health"], task.get("waiting_reason", "")))
assert task["health"] == "waiting", task["health"]
assert task["waiting_reason"], "a parked task must say what it waits for"
'

log "the card is in the thread it came from"
curl -fsS "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" | python3 scripts/print-thread.py

# ------------------------------------------------------ phase 2: the revision

log "phase 2: a revision sends the task back to its agent with the note"
curl -fsS -X POST "${API}/drafts/${DRAFT_ID}/revise" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d '{"note":"salah penerima"}' >/dev/null

REVISED=$(draft_status "$DRAFT_ID")
[[ "$REVISED" == "revise" ]] || fail "the draft is ${REVISED}, not revise"

TRAIL=$(curl -fsS "${API}/drafts/${DRAFT_ID}/revisions" "${AUTH[@]}")
echo "$TRAIL" | python3 -c '
import json, sys
trail = json.load(sys.stdin)["data"]
assert trail, "the revision must be on the trail"
assert trail[0]["note"] == "salah penerima", trail[0]
print("  trail: revisi {} - {}".format(trail[0]["revision"], trail[0]["note"]))
'

# The agent carries on with the note, so the task leaves the parked state.
RESUMED=$(wait_for_task_status "$PHASE1_TASK" "succeeded")
[[ "$RESUMED" == "succeeded" ]] ||
    fail "the task did not carry on after the revision; status is ${RESUMED}"

log "the agent was told what to change"
curl -fsS "${API}/tasks/${PHASE1_TASK}/steps" "${AUTH[@]}" | python3 scripts/print-steps.py

# ------------------------------------------------------ phase 3: the approval

log "phase 3: an approved action is carried out against the provider"
curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Tagih lagi ya, yang tadi.","reply":true}' >/dev/null

SECOND=""
for _ in $(seq 1 60); do
    SECOND=$(curl -fsS "${API}/drafts?status=pending&limit=1" "${AUTH[@]}" |
        python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["draft_id"] if d else "")')
    [[ -n "$SECOND" ]] && break
    sleep 1
done
[[ -n "$SECOND" ]] || fail "the second task did not open a draft"
log "draft ${SECOND}"

curl -fsS -X POST "${API}/drafts/${SECOND}/approve" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d '{"note":"kirim"}' >/dev/null

APPROVED=$(draft_status "$SECOND")
[[ "$APPROVED" == "approved" || "$APPROVED" == "sent" ]] ||
    fail "the decision was not stored; status is ${APPROVED}"

SECOND_TASK=$(curl -fsS "${API}/drafts/${SECOND}" "${AUTH[@]}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["task_id"])')
FINAL=$(wait_for_task_status "$SECOND_TASK" "succeeded")

log "the decision reached the task"
curl -fsS "${API}/tasks/${SECOND_TASK}" "${AUTH[@]}" | python3 -m json.tool

# The executor EPIC 8 added is what makes this the whole promise: an action a
# person approved is carried out, and the task succeeds because it actually did.
[[ "$FINAL" == "succeeded" ]] || fail "the approved action did not go through; status is ${FINAL}"

SENT=$(draft_status "$SECOND")
[[ "$SENT" == "sent" ]] || fail "the approved action was not recorded as sent; status is ${SENT}"

# And the provider received it. The stub writes down what it was asked to send, so
# the message that arrived can be compared with the card a person approved.
[[ -s /tmp/smoke-approval-gmail-sent.jsonl ]] ||
    fail "nothing reached the provider, so the approval proved nothing"

python3 - <<'PY'
import base64, json

with open("/tmp/smoke-approval-gmail-sent.jsonl") as handle:
    sent = [json.loads(line) for line in handle if line.strip()]

assert len(sent) == 1, "exactly one message must have been sent, got {}".format(len(sent))

raw = sent[0]["raw"]
message = base64.urlsafe_b64decode(raw + "=" * (-len(raw) % 4)).decode()

# The body is the payload the card showed, so what a person approved is what left.
assert "pelanggan@example.com" in message, message
assert "Tagihan Maret" in message, message
print("  the provider received the message the card showed")
PY

log "the decision is on the audit trail"
curl -fsS "${API}/audit?limit=10" "${AUTH[@]}" | python3 -c '
import json, sys
rows = json.load(sys.stdin)["data"]
actions = [row["action"] for row in rows]
assert "draft.created" in actions, actions
assert "draft.approved" in actions, actions

# Every proposal is recorded once and every decision once. Counting per draft is
# what makes that checkable: two drafts in this run means two creations and two
# decisions, no more.
created = {}
for row in rows:
    created.setdefault(row["object_id"], []).append(row["action"])

for draft_id, acts in created.items():
    print("  {} -> {}".format(draft_id, ", ".join(sorted(acts))))
    assert acts.count("draft.created") == 1, (draft_id, acts)
    assert acts.count("draft.approved") + acts.count("draft.revised") <= 1, (draft_id, acts)

# The send is traceable to the person who allowed it.
approved = [row for row in rows if row["action"] == "draft.approved"]
assert approved, "an approval must be recorded"
assert approved[0].get("actor_user_id"), "an approval names the person who made it"
print("  approval attributed to the user, not the Bolu")
'

log "the policy screen reports every external action"
curl -fsS "${API}/approval-policies" "${AUTH[@]}" | python3 -c '
import json, sys
policies = json.load(sys.stdin)["data"]
assert policies, "a workspace always has policies"
for policy in policies:
    print("  {:<26} {}".format(policy["action_kind"], policy["mode"]))
assert all(p["mode"] == "always_draft" for p in policies), "the default must be conservative"
'

# -------------------------------------------------------- phase 4: the hold

log "phase 4: an approval outside working hours is held, not sent"
# A window that excludes right now, whichever hour this runs at: a one-minute
# window at midnight, so "now" is almost never inside it. The hour is compared in
# the workspace's own zone, which is what the rule reads.
docker exec -i bolu-postgres psql -U employeebot -d employeebot -q <<SQL
UPDATE workspaces SET work_hours_start = '00:00', work_hours_end = '00:01'
WHERE id = '${WORKSPACE}';
SQL

curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Tagih sekali lagi ya.","reply":true}' >/dev/null

THIRD=""
for _ in $(seq 1 60); do
    THIRD=$(curl -fsS "${API}/drafts?status=pending&limit=1" "${AUTH[@]}" |
        python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["draft_id"] if d else "")')
    [[ -n "$THIRD" ]] && break
    sleep 1
done
[[ -n "$THIRD" ]] || fail "the third task did not open a draft"
log "draft ${THIRD}"

curl -fsS -X POST "${API}/drafts/${THIRD}/approve" "${AUTH[@]}"     -H 'Content-Type: application/json' -d '{"note":"kirim"}' >/dev/null

THIRD_TASK=$(curl -fsS "${API}/drafts/${THIRD}" "${AUTH[@]}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["task_id"])')

# The task was already `waiting_approval` before the decision — that is where the
# draft parked it. Waiting on that status would race ahead of the hold, so the
# wait is on the hold itself: the draft only carries `not_before` once the runtime
# has carried the approved action out and found the window closed.
HELD_DRAFT=""
for _ in $(seq 1 60); do
    HELD_DRAFT=$(curl -fsS "${API}/drafts/${THIRD}" "${AUTH[@]}")
    if echo "$HELD_DRAFT" | grep -q '"not_before":"'; then
        break
    fi
    # A hold that never arrives becomes a failure: the alternative is a task that
    # sent something outside working hours, which is the bug this phase exists for.
    STATUS=$(echo "$HELD_DRAFT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])')
    [[ "$STATUS" == "approved" || "$STATUS" == "pending" ]] ||
        fail "the draft is ${STATUS}; expected it to be held"
    sleep 1
done

echo "$HELD_DRAFT" | grep -q '"not_before":"' ||
    fail "an approval outside working hours was not held"
echo "$HELD_DRAFT" | python3 -c '
import json, sys
draft = json.load(sys.stdin)["data"]
assert draft["status"] == "approved", draft["status"]
assert draft.get("not_before"), "a hold must say until when"
assert draft.get("blocked_reason"), "a hold must say why"
assert not draft.get("sent_at"), "a held action has not gone out"
print("  held: {} -> {}".format(draft["blocked_reason"], draft["not_before"]))
'

HELD_TASK=$(curl -fsS "${API}/tasks/${THIRD_TASK}" "${AUTH[@]}")
echo "$HELD_TASK" | python3 -c '
import json, sys
task = json.load(sys.stdin)["data"]
assert task["status"] == "waiting_approval", task["status"]
assert task["health"] == "waiting", task["health"]
assert "jam kerja" in task.get("waiting_reason", ""), task.get("waiting_reason")
print("  task parked: status={} health={}".format(task["status"], task["health"]))
'

echo
echo "SMOKE OK"
echo "  a write_external action became a draft, the task parked, and nothing left;"
echo "  a revision sent the task back to its agent with the note;"
echo "  an approved action was carried out, and the provider received exactly the"
echo "  message the card showed a person;"
echo "  an approval outside working hours was held, and the task said so."
