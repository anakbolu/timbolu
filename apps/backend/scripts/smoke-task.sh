#!/usr/bin/env bash
#
# End-to-end smoke for the task runtime: a chat message becomes a durable task,
# the task calls a model, records its rounds, and writes the answer back into the
# conversation.
#
# It runs the real API and the real agent worker against the compose Postgres,
# Redis, and Temporal, and points the model gateway at a stub that speaks the
# OpenAI-compatible format. The point is the wiring, not the provider: what is
# proven is that the message survives the request, the workflow runs out of
# process, the rounds are recorded, and the answer lands where it was asked for.
#
# Two phases run, because the two things that can only be wrong in the wiring are
# the happy path and the bound:
#
#   1. a message is answered, and the answer appears in its own thread;
#   2. with a one-step allowance, the task stops with a reason the user can read
#      rather than looping or hanging.
#
# Usage: scripts/smoke-task.sh
set -euo pipefail

cd "$(dirname "$0")/.."

API_PORT="${API_PORT:-8090}"
STUB_PORT="${STUB_PORT:-11434}"
DB="${DATABASE_URL:-postgres://employeebot:employeebot_secret@localhost:5432/employeebot?sslmode=disable}"

log() { printf '\n==> %s\n' "$*"; }
fail() { printf '\n!! %s\n' "$*" >&2; exit 1; }

cleanup() {
    # Each child is reaped rather than only signalled: a process the shell still
    # knows about is reported as "Terminated" when the script exits, which reads
    # as a failure on an otherwise clean run.
    stop_child() {
        local pid="${1:-}"
        [[ -n "$pid" ]] || return 0
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    }

    stop_child "${WORKER_PID:-}"
    stop_child "${API_PID:-}"
    stop_child "${STUB_PID:-}"

    [[ -n "${BIN_DIR:-}" ]] && rm -rf "$BIN_DIR"
    rm -f /tmp/smoke-llm-stub.py
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

# ------------------------------------------------------------- stub provider

cat > /tmp/smoke-llm-stub.py <<'PY'
import json, os, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

# The stub has two modes. In "echo" it answers the prompt's own instruction, so a
# test can assert the answer travelled all the way back. In "tool" it always asks
# for a tool call, which is what makes the runtime loop until a bound stops it.
MODE = os.environ.get("STUB_MODE", "echo")

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        model = body.get("model", "stub")

        offered = [
            t["function"]["name"]
            for t in body.get("tools", [])
            if t.get("function", {}).get("name") and t["function"]["name"] != "handoff"
        ]

        if MODE == "tool" and offered:
            payload = {
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": "",
                        "tool_calls": [{
                            "id": "call-smoke",
                            "type": "function",
                            "function": {"name": offered[0], "arguments": "{\"q\":\"pesanan\"}"},
                        }],
                    },
                    "finish_reason": "tool_calls",
                }],
                "usage": {"prompt_tokens": 200, "completion_tokens": 20, "total_tokens": 220},
            }
        else:
            text = "Hari ini ada 4 pesanan: 2 lunas, 2 menunggu pembayaran."
            for message in body.get("messages", []):
                content = message.get("content") or ""
                if isinstance(content, list):
                    content = " ".join(part.get("text", "") for part in content)
                match = re.search(r"stub-echo:([^\n]+)", content)
                if match:
                    text = match.group(1).strip()
            payload = {
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 120, "completion_tokens": 40, "total_tokens": 160},
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

# ----------------------------------------------------------------- the binaries

# The binaries are built first and then run directly. `go run` would leave the
# compiled child behind when its wrapper is killed, and a leftover worker keeps
# polling the task queue: the next run would then be executed by a binary built
# from older source, which is a confusing way to fail.
log "building the API and the agent worker"
BIN_DIR="$(mktemp -d)"
CGO_ENABLED=0 go build -o "${BIN_DIR}/api" ./cmd/api
CGO_ENABLED=0 go build -o "${BIN_DIR}/agent-worker" ./cmd/agent-worker

# --------------------------------------------------------------- phase 1: happy

# start_stub replaces whatever stub is running with one in the given mode.
#
# Replacing it rather than adding a second one is the point: the two phases need
# different answers from the provider, and a second stub that failed to bind
# would leave the first one answering — which is how a "the bound stopped it"
# assertion can pass for the wrong reason.
start_stub() {
    local mode="$1"

    stop_stub
    for _ in $(seq 1 20); do
        lsof -ti ":${STUB_PORT}" >/dev/null 2>&1 || break
        sleep 0.5
    done
    lsof -ti ":${STUB_PORT}" >/dev/null 2>&1 && fail "port ${STUB_PORT} did not free up"

    STUB_MODE="$mode" python3 /tmp/smoke-llm-stub.py "$STUB_PORT" &
    STUB_PID=$!

    # The stub must be serving before the API is asked anything, or the first
    # model call fails and the phase fails for a reason unrelated to the test.
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

log "starting the stub model provider on :${STUB_PORT} (echo mode)"
start_stub echo

export ENVIRONMENT=local
export DATABASE_URL="$DB"
export REDIS_URL="redis://localhost:6379/0"
export TEMPORAL_HOST_PORT="localhost:7233"
export HTTP_ADDRESS=":${API_PORT}"
export FRONTEND_URL="http://localhost:3000"
# The log mail driver prints the verification link at info level, so the smoke
# reads the link the same way a developer does.
export LOG_LEVEL=info
export LOG_FORMAT=console
export MAIL_DRIVER=log
export SECRET_ENCRYPTION_KEY="local_dev_secret_key_32_byte_len"
export LLM_DEFAULT_ADAPTER=openai
export LLM_DEFAULT_BASE_URL="http://localhost:${STUB_PORT}"
export LLM_DEFAULT_MODEL=stub
export LLM_DEFAULT_API_KEY=stub-key
export STORAGE_DRIVER=local
export STORAGE_LOCAL_ROOT=.storage
export CONTENT_ORIGIN="http://localhost:${API_PORT}"
export TASK_MAX_STEPS=12
export TASK_MAX_TOKENS=120000

start_stack() {
    log "starting the API on :${API_PORT}"
    "${BIN_DIR}/api" > /tmp/smoke-api.log 2>&1 &
    API_PID=$!

    log "starting the agent worker"
    "${BIN_DIR}/agent-worker" > /tmp/smoke-worker.log 2>&1 &
    WORKER_PID=$!

    for _ in $(seq 1 90); do
        curl -fsS "${API}/health" >/dev/null 2>&1 && return 0
        kill -0 "$API_PID" 2>/dev/null || fail "the API exited during startup; see /tmp/smoke-api.log"
        sleep 1
    done
    fail "the API did not become ready; see /tmp/smoke-api.log"
}

stop_stack() {
    # Reaped, not only signalled: a child the shell still knows about is
    # reported as "Terminated" when the script exits.
    for pid in "${WORKER_PID:-}" "${API_PID:-}"; do
        [[ -n "$pid" ]] || continue
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    WORKER_PID=""
    API_PID=""
    # The worker's poll loop takes a moment to release the queue; without the
    # wait a second worker could start against the same queue and the next phase
    # would be executed by the previous binary.
    sleep 2
}

API="http://localhost:${API_PORT}/api"
start_stack

log "registering an account"
EMAIL="smoke-$(date +%s)@example.com"
PASSWORD="Rahasia123!"
curl -fsS -X POST "${API}/auth/register" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

TOKEN=""
for _ in $(seq 1 20); do
    TOKEN=$(grep -o 'token=[A-Za-z0-9_-]*' /tmp/smoke-api.log | tail -1 | cut -d= -f2 || true)
    [[ -n "$TOKEN" ]] && break
    sleep 1
done
[[ -n "$TOKEN" ]] || fail "no verification token in /tmp/smoke-api.log"

log "verifying the address and logging in"
curl -fsS -X POST "${API}/auth/verify-email" -H 'Content-Type: application/json' \
    -d "{\"token\":\"${TOKEN}\"}" >/dev/null
SESSION=$(curl -fsS -X POST "${API}/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}")
ACCESS=$(echo "$SESSION" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])')

log "onboarding a workspace"
ONBOARD=$(curl -fsS -X POST "${API}/workspaces/onboard" -H "Authorization: Bearer ${ACCESS}" \
    -H 'Content-Type: application/json' \
    -d '{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}')
WORKSPACE=$(echo "$ONBOARD" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["workspace"]["id"])')
AUTH=(-H "Authorization: Bearer ${ACCESS}" -H "X-Workspace-Id: ${WORKSPACE}")

AGENT=$(curl -fsS "${API}/agents" "${AUTH[@]}" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([a for a in d if a["name"]=="Oren"][0]["id"])')
echo "Oren = ${AGENT}"

# A Bolu is only offered the tools whose integration it was granted, and no
# endpoint connects one yet: OAuth is EPIC 8. The smoke seeds the integration and
# the grant directly, so the filter it exercises is the real one and not a
# bypass.
log "seeding a granted integration for Oren"
docker exec -i bolu-postgres psql -U employeebot -d employeebot -v ON_ERROR_STOP=1 -q <<SQL
INSERT INTO integrations (workspace_id, app, account_label, status)
VALUES ('${WORKSPACE}', 'gmail', 'smoke@example.com', 'connected');

INSERT INTO agent_grants (workspace_id, agent_id, integration_id, permission)
SELECT '${WORKSPACE}', '${AGENT}', i.id, 'read'
FROM integrations i
WHERE i.workspace_id = '${WORKSPACE}' AND i.app = 'gmail';
SQL

GRANTED=$(docker exec bolu-postgres psql -U employeebot -d employeebot -tAc \
    "SELECT count(*) FROM agent_grants WHERE workspace_id = '${WORKSPACE}' AND agent_id = '${AGENT}'")
[[ "$GRANTED" == "1" ]] || fail "the grant was not stored, so the tool filter would be exercised against nothing"

log "opening the 1:1 thread with Oren"
CONVERSATION=$(curl -fsS -X POST "${API}/conversations/direct" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT}\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

log "sending a message that must become a durable task"
curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"stub-echo:Ada 4 pesanan hari ini.","reply":true}' >/dev/null

TASK=$(curl -fsS "${API}/tasks?limit=1" "${AUTH[@]}" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["task_id"] if d else "")')
[[ -n "$TASK" ]] || fail "no task was opened for the message"
log "task ${TASK}"

# wait_for_task prints the terminal status. The task itself is fetched again
# afterwards: a function that both polls and leaves a value behind would have to
# run in the current shell, and the caller would then have to remember that.
wait_for_task() {
    local task="$1" status=""
    for _ in $(seq 1 60); do
        status=$(curl -fsS "${API}/tasks/${task}" "${AUTH[@]}" |
            python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])')
        case "$status" in
            succeeded|failed|canceled) printf '%s' "$status"; return 0 ;;
        esac
        sleep 1
    done
    printf '%s' "$status"
}

STATUS=$(wait_for_task "$TASK")
TASK_JSON=$(curl -fsS "${API}/tasks/${TASK}" "${AUTH[@]}")
echo "$TASK_JSON" | python3 -m json.tool
[[ "$STATUS" == "succeeded" ]] || fail "the task did not succeed: ${STATUS}"

log "the rounds the task recorded"
curl -fsS "${API}/tasks/${TASK}/steps" "${AUTH[@]}" | python3 scripts/print-steps.py

log "the answer, in the thread it was asked in"
curl -fsS "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" | python3 scripts/print-thread.py

# --------------------------------------------------------------- phase 2: bound

log "restarting with a one-step allowance, to prove a bound stops the task"
stop_stack
export TASK_MAX_STEPS=1
start_stub tool
start_stack

log "sending a second message"
curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"stub-echo:perlu alat","reply":true}' >/dev/null

BOUND_TASK=$(curl -fsS "${API}/tasks?limit=1" "${AUTH[@]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["task_id"])')
log "task ${BOUND_TASK}"

BOUND_STATUS=$(wait_for_task "$BOUND_TASK")
BOUND_JSON=$(curl -fsS "${API}/tasks/${BOUND_TASK}" "${AUTH[@]}")
echo "$BOUND_JSON" | python3 -m json.tool
[[ "$BOUND_STATUS" == "failed" ]] || fail "a task past its step allowance must fail: ${BOUND_STATUS}"

# The reason has to name the step bound. A task that failed for any other
# reason would also be "failed", and asserting only the status would accept it.
python3 - "$BOUND_JSON" <<'PY'
import json, sys
task = json.loads(sys.argv[1])["data"]
reason = task.get("stopped_reason", "")
if "batas langkah" not in reason:
    raise SystemExit(f"the stop reason does not name the step bound: {reason!r}")
if task["step_count"] != 1:
    raise SystemExit(f"the task ran {task['step_count']} rounds, not the one it was allowed")
PY

REASON=$(echo "$BOUND_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("stopped_reason",""))')
[[ -n "$REASON" ]] || fail "a task that stopped on a bound must say why"
echo "reason: ${REASON}"

log "the rounds it recorded before stopping"
BOUND_STEPS=$(curl -fsS "${API}/tasks/${BOUND_TASK}/steps" "${AUTH[@]}")
echo "$BOUND_STEPS" | python3 scripts/print-steps.py

# The tool mode answers with a tool call, so a run that recorded no tool call
# means the stub was not in the mode this phase asked for.
python3 - "$BOUND_STEPS" <<'PY'
import json, sys
kinds = [step["kind"] for step in json.loads(sys.argv[1])["data"]]
if "tool_call" not in kinds:
    raise SystemExit(f"the stub never asked for a tool, so this phase proved nothing: {kinds}")
PY

echo
echo "SMOKE OK"
echo "  a chat message became a durable task, ran out of process, and answered in its thread;"
echo "  a task past its step allowance stopped with a reason a person can read."
