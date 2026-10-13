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
# Three phases run, because the things that can only be wrong in the wiring are
# the happy path, the bound, and the memory pipeline:
#
#   1. a message is answered, and the answer appears in its own thread;
#   2. with a one-step allowance, the task stops with a reason the user can read
#      rather than looping or hanging;
#   3. a memory request is stored and confirmed, a later task is given what was
#      remembered, a quiet conversation is extracted, a corrected fact replaces
#      its predecessor, and a delete takes the row out of the search.
#
# Usage: scripts/smoke-task.sh
#
# The trace of the run is asked for with the environment: OTEL_EXPORTER=console
# prints every span of both processes, which is how "one task, one trace" is
# checked by hand — the API's request span, the worker's workflow and activity
# spans, and the model and tool spans inside them all carry one trace id. The
# default is no exporter, so a run that is not being read leaves no output behind.
#
# OTEL_METRICS_EXPORTER=prometheus turns the run into a deployment a scraper can
# read: curl localhost:${API_PORT}/metrics while it runs, and the four product
# measures are the numbers the tasks below produced.
set -euo pipefail

cd "$(dirname "$0")/.."

API_PORT="${API_PORT:-8090}"
STUB_PORT="${STUB_PORT:-11434}"
# The worker has no HTTP API of its own, so it serves its operations endpoint on a port of its own:
# /health, /ready, and /metrics. That is what makes the task measures, recorded in that process,
# scrapeable at all — and what an orchestrator would probe.
WORKER_OPS_ADDRESS="${WORKER_OPS_ADDRESS:-:9101}"
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

cat > /tmp/smoke-llm-stub.py <<'PY'
import hashlib, json, math, os, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

# The stub has three modes:
#
#   echo    answers the prompt's own "stub-echo:" instruction, so a test can
#           assert the answer travelled all the way back;
#   tool    always asks for a tool call, which is what makes the runtime loop
#           until a bound stops it;
#   memory  answers the extraction question with a JSON candidate list, and
#           answers anything else with the workspace memory it was given — which
#           is what proves the memory reached the provider as part of the prompt.
#
# It also serves /v1/embeddings, so the memory pipeline can be driven end to end
# without a real provider: the vectors come from a word-hash, which makes two
# texts that share words measurably closer than two that do not.
MODE = os.environ.get("STUB_MODE", "echo")
DIMENSIONS = 1536

# NEEDLE is the text the memory phase plants and then looks for in the prompt.
NEEDLE = os.environ.get("STUB_NEEDLE", "")

def embed(text):
    """embed turns text into a unit vector by hashing its words."""
    vector = [0.0] * DIMENSIONS
    for word in re.findall(r"[\w.]+", text.lower()):
        digest = hashlib.sha256(word.encode()).digest()
        vector[int.from_bytes(digest[:4], "big") % DIMENSIONS] += 1.0

    norm = math.sqrt(sum(value * value for value in vector)) or 1.0
    return [value / norm for value in vector]

def flatten(messages):
    """flatten renders every message's text, so a needle can be searched for."""
    parts = []
    for message in messages:
        content = message.get("content") or ""
        if isinstance(content, list):
            content = " ".join(part.get("text", "") for part in content)
        parts.append(content)
    return "\n".join(parts)

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        model = body.get("model", "stub")

        if self.path.endswith("/embeddings"):
            inputs = body.get("input") or []
            if isinstance(inputs, str):
                inputs = [inputs]
            payload = {
                "object": "list",
                "model": model,
                "data": [{"object": "embedding", "index": i, "embedding": embed(text)}
                         for i, text in enumerate(inputs)],
                "usage": {"prompt_tokens": 12, "total_tokens": 12},
            }
            return self.answer(payload)

        prompt = flatten(body.get("messages", []))

        offered = [
            t["function"]["name"]
            for t in body.get("tools", [])
            if t.get("function", {}).get("name") and t["function"]["name"] != "handoff"
        ]

        if MODE != "memory" and "Balas HANYA JSON" in prompt:
            # A phase that does not drive the memory pipeline still ends up with an
            # extraction, because every conversation is read once it goes quiet. It
            # proposes nothing, so the pass is a no-op rather than a parse failure.
            payload = {
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": '{"memories":[]}'},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 200, "completion_tokens": 4, "total_tokens": 204},
            }

        elif MODE == "memory" and "Balas HANYA JSON" in prompt:
            # The extraction question. The candidate is read out of the transcript
            # rather than invented, and a transcript that states no fact yields
            # none: a stub that made one up would plant the same memory in every
            # conversation, and the assertions about which fact came from where
            # would then pass for the wrong reason.
            #
            # The *newest* statement wins, which is what a reader of the thread would
            # do and what makes a correction observable: the transcript is oldest
            # first, so a fact mentioned twice with different numbers has to answer
            # with the later one or a corrected value can never be seen to replace
            # the old.
            #
            # The small talk rides along whenever a fact is found, so the pipeline's
            # own filter is what has to drop it.
            matches = re.findall(r"ongkir[^\n]*bandung[^\n]*?(\d+)\s*ribu", prompt, re.I)

            candidates = []
            if matches:
                candidates = [
                    {"content": f"Ongkir Bandung {matches[-1]} ribu", "kind": "fact", "layer": "main"},
                    {"content": "oke", "kind": "fact", "layer": "main"},
                ]

            payload = {
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": json.dumps({"memories": candidates}),
                    },
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 300, "completion_tokens": 40, "total_tokens": 340},
            }
        elif MODE == "memory" and NEEDLE and NEEDLE in prompt:
            # The memory reached the provider as part of the prompt, and the
            # answer says so: this is the assertion the phase reads.
            payload = {
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": "Aku ingat: " + NEEDLE},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 200, "completion_tokens": 20, "total_tokens": 220},
            }
        elif MODE == "memory":
            payload = {
                "id": "chatcmpl-smoke",
                "object": "chat.completion",
                "model": model,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": "tidak ada memori di prompt"},
                    "finish_reason": "stop",
                }],
                "usage": {"prompt_tokens": 120, "completion_tokens": 40, "total_tokens": 160},
            }
        elif MODE == "tool" and offered:
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

        self.answer(payload)

    def answer(self, payload):
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

    STUB_MODE="$mode" STUB_NEEDLE="${NEEDLE:-}" python3 /tmp/smoke-llm-stub.py "$STUB_PORT" &
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

# The stack's configuration, and the two bounds a phase changes. They are shell
# variables rather than exported ones: each process is handed its environment
# explicitly in start_stack, so a typo here is caught when it is read instead of
# becoming an exported variable that shadows the value a later phase set.
#
# The log mail driver prints the verification link at info level, which is how the
# smoke reads it the same way a developer does.
LLM_DEFAULT_ADAPTER=openai
LLM_DEFAULT_BASE_URL="http://localhost:${STUB_PORT}"
LLM_DEFAULT_MODEL=stub
LLM_DEFAULT_API_KEY=stub-key
TASK_MAX_STEPS=12
TASK_MAX_TOKENS=120000

# stack_env prints the environment both processes share, so the API and the worker
# cannot be started with different settings.
stack_env() {
    printf '%s\n' \
        "ENVIRONMENT=local" \
        "DATABASE_URL=${DB}" \
        "REDIS_URL=redis://localhost:6379/0" \
        "TEMPORAL_HOST_PORT=localhost:7233" \
        "TEMPORAL_UI_BASE_URL=${TEMPORAL_UI_BASE_URL:-http://localhost:8233}" \
        "LOG_LEVEL=info" \
        "LOG_FORMAT=console" \
        "SECRET_ENCRYPTION_KEY=local_dev_secret_key_32_byte_len" \
        "LLM_DEFAULT_ADAPTER=${LLM_DEFAULT_ADAPTER}" \
        "LLM_DEFAULT_BASE_URL=${LLM_DEFAULT_BASE_URL}" \
        "LLM_DEFAULT_MODEL=${LLM_DEFAULT_MODEL}" \
        "LLM_DEFAULT_API_KEY=${LLM_DEFAULT_API_KEY}" \
        "STORAGE_DRIVER=local" \
        "STORAGE_LOCAL_ROOT=.storage" \
        "TASK_MAX_STEPS=${TASK_MAX_STEPS:-12}" \
        "TASK_MAX_TOKENS=${TASK_MAX_TOKENS:-120000}" \
        "MEMORY_QUIET_SECONDS=${MEMORY_QUIET_SECONDS:-}" \
        "MEMORY_SIMILARITY=${MEMORY_SIMILARITY:-}"
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

# start_stack boots the API and the worker for one phase.
#
# It takes the phase's name because each phase's log must survive the next one:
# the logs were a single fixed path once, so the evidence for a failure in the
# bound phase was overwritten by the memory phase's own startup — which is exactly
# when somebody is trying to read it. The paths are also what the failure messages
# below point at, so they have to be the ones the phase actually wrote.
start_stack() {
    local phase="$1"
    API_LOG="/tmp/smoke-api-${phase}.log"
    WORKER_LOG="/tmp/smoke-worker-${phase}.log"

    log "starting the API on :${API_PORT}"
    ENVIRONMENT=local DATABASE_URL="$DB" REDIS_URL="redis://localhost:6379/0" \
    TEMPORAL_HOST_PORT="localhost:7233" HTTP_ADDRESS=":${API_PORT}" \
    TEMPORAL_UI_BASE_URL="${TEMPORAL_UI_BASE_URL:-http://localhost:8233}" \
    FRONTEND_URL="http://localhost:3000" LOG_LEVEL=info LOG_FORMAT=console \
    MAIL_DRIVER=log SECRET_ENCRYPTION_KEY="local_dev_secret_key_32_byte_len" \
    LLM_DEFAULT_ADAPTER="$LLM_DEFAULT_ADAPTER" \
    LLM_DEFAULT_BASE_URL="$LLM_DEFAULT_BASE_URL" \
    LLM_DEFAULT_MODEL="$LLM_DEFAULT_MODEL" \
    LLM_DEFAULT_API_KEY="$LLM_DEFAULT_API_KEY" \
    STORAGE_DRIVER=local STORAGE_LOCAL_ROOT=.storage \
    CONTENT_ORIGIN="http://localhost:${API_PORT}" \
    TASK_MAX_STEPS="${TASK_MAX_STEPS:-12}" TASK_MAX_TOKENS="${TASK_MAX_TOKENS:-120000}" \
    MEMORY_QUIET_SECONDS="${MEMORY_QUIET_SECONDS:-}" \
    MEMORY_SIMILARITY="${MEMORY_SIMILARITY:-}" \
    OTEL_EXPORTER="${OTEL_EXPORTER:-}" OTEL_ENDPOINT="${OTEL_ENDPOINT:-}" \
    OTEL_SAMPLE_RATIO="${OTEL_SAMPLE_RATIO:-}" OTEL_INSECURE="${OTEL_INSECURE:-}" \
    OTEL_METRICS_EXPORTER="${OTEL_METRICS_EXPORTER:-}" \
    OTEL_METRICS_ENDPOINT="${OTEL_METRICS_ENDPOINT:-}" \
        "${BIN_DIR}/api" > "$API_LOG" 2>&1 &
    API_PID=$!

    log "starting the agent worker"
    ENVIRONMENT=local DATABASE_URL="$DB" REDIS_URL="redis://localhost:6379/0" \
    TEMPORAL_HOST_PORT="localhost:7233" LOG_LEVEL=info LOG_FORMAT=console \
    SECRET_ENCRYPTION_KEY="local_dev_secret_key_32_byte_len" \
    LLM_DEFAULT_ADAPTER="$LLM_DEFAULT_ADAPTER" \
    LLM_DEFAULT_BASE_URL="$LLM_DEFAULT_BASE_URL" \
    LLM_DEFAULT_MODEL="$LLM_DEFAULT_MODEL" \
    LLM_DEFAULT_API_KEY="$LLM_DEFAULT_API_KEY" \
    STORAGE_DRIVER=local STORAGE_LOCAL_ROOT=.storage \
    TASK_MAX_STEPS="${TASK_MAX_STEPS:-12}" TASK_MAX_TOKENS="${TASK_MAX_TOKENS:-120000}" \
    MEMORY_QUIET_SECONDS="${MEMORY_QUIET_SECONDS:-}" \
    MEMORY_SIMILARITY="${MEMORY_SIMILARITY:-}" \
    OTEL_EXPORTER="${OTEL_EXPORTER:-}" OTEL_ENDPOINT="${OTEL_ENDPOINT:-}" \
    OTEL_SAMPLE_RATIO="${OTEL_SAMPLE_RATIO:-}" OTEL_INSECURE="${OTEL_INSECURE:-}" \
    OTEL_METRICS_EXPORTER="${OTEL_METRICS_EXPORTER:-}" \
    OTEL_METRICS_ENDPOINT="${OTEL_METRICS_ENDPOINT:-}" \
    OPS_ADDRESS="${WORKER_OPS_ADDRESS}" \
        "${BIN_DIR}/agent-worker" > "$WORKER_LOG" 2>&1 &
    WORKER_PID=$!

    for _ in $(seq 1 90); do
        curl -fsS "${API}/health" >/dev/null 2>&1 && return 0
        kill -0 "$API_PID" 2>/dev/null || fail "the API exited during startup; see ${API_LOG}"
        sleep 1
    done
    fail "the API did not become ready; see ${API_LOG}"
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
start_stack chat

log "registering an account"
EMAIL="smoke-$(date +%s)@example.com"
PASSWORD="Rahasia123!"
curl -fsS -X POST "${API}/auth/register" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

TOKEN=""
for _ in $(seq 1 20); do
    TOKEN=$(grep -o 'token=[A-Za-z0-9_-]*' "$API_LOG" | tail -1 | cut -d= -f2 || true)
    [[ -n "$TOKEN" ]] && break
    sleep 1
done
[[ -n "$TOKEN" ]] || fail "no verification token in ${API_LOG}"

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

# A workspace starts with no Bolu, so the smoke makes the one it needs the way a
# person does: it picks the team onboarding created and adds an agent to it.
TEAM=$(curl -fsS "${API}/teams" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([t for t in d if t["kind"]=="bolu"][0]["id"])')

AGENT=$(curl -fsS -X POST "${API}/agents" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"team_id\":\"${TEAM}\",\"name\":\"Oren\",\"template_key\":\"oren\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')
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

# The metrics are checked only when the deployment was asked for them, because
# the default is to report nothing: a scrape of a deployment with no exporter is a
# 404 by design, and asserting the numbers there would be asserting the wrong
# thing. What is checked is the reconciliation the epic asks for — the counters
# the chart reads must agree with the task rows the phase above just produced.
if [[ "${OTEL_METRICS_EXPORTER:-}" == "prometheus" ]]; then
    log "the deployment's own measurements, scraped from /metrics"
    # Both processes are scraped, because the work is split between them: the API accepts a task,
    # the worker runs it and records what it spent. A scrape of one alone would read a depth that
    # never comes back down, which is exactly what the first version of this check caught.
    API_METRICS=$(curl -fsS "http://localhost:${API_PORT}/metrics")
    WORKER_METRICS=$(curl -fsS "http://localhost${WORKER_OPS_ADDRESS}/metrics")
    printf '%s\n' "$API_METRICS" "$WORKER_METRICS" | grep -E '^bolu_' | head -30

    python3 scripts/check-metrics.py "$WORKER_METRICS" "$TASK_JSON" "$API_METRICS"
fi

# --------------------------------------------------------------- phase 2: bound

log "restarting with a one-step allowance, to prove a bound stops the task"
stop_stack
TASK_MAX_STEPS=1
start_stub tool
start_stack bound

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

# The usage screen's own reads, checked while the failure this phase just produced
# is on the books: the report must cover it, the export must be the same read, and the
# attention list must carry it with a link that names its workflow.
log "the usage screen's reads: the report, the export, and what needs attention"
USAGE_JSON=$(curl -fsS "${API}/llm/usage/report?days=30" "${AUTH[@]}")
printf '%s\n' "$USAGE_JSON" | python3 -m json.tool | head -30

USAGE_CSV=$(curl -fsS "${API}/llm/usage/report?days=30&format=csv" "${AUTH[@]}")
ATTENTION_JSON=$(curl -fsS "${API}/tasks/attention" "${AUTH[@]}")

python3 scripts/check-usage.py "$USAGE_JSON" "$BOUND_JSON" "$USAGE_CSV" "$ATTENTION_JSON" \
    "${TEMPORAL_UI_BASE_URL:-}"

# --------------------------------------------------------------- phase 3: memory

# The memory pipeline is the one thing that only exists in the wiring: a message
# asks for something to be remembered, a later task is given it, a quiet
# conversation is read, a corrected fact replaces its predecessor, and a delete
# takes the row out of the search.
#
# The stub is in "memory" mode for all of it: it answers the extraction question
# with a candidate list, and answers anything else by naming the needle it found in
# its own prompt — which is how the phase proves the memory reached the provider
# rather than only the database.

NEEDLE="Jam buka 08.00-21.00"

log "restarting with a short quiet period and a low similarity floor"
stop_stack
TASK_MAX_STEPS=12
# Five seconds is long enough to prove the wait is a wait and short enough to
# watch; the similarity floor is lowered because the stub's vectors come from word
# hashes, and two versions of one fact share most of their words rather than all
# of them.
# Empty means the configuration file's own value applies, which is the one the
# phases that do not assert an extraction should run with.
MEMORY_QUIET_SECONDS=5
MEMORY_SIMILARITY=0.6
start_stub memory
start_stack memory

MEMORY_AUTH=(-H "Authorization: Bearer ${ACCESS}" -H "X-Workspace-Id: ${WORKSPACE}")

log "asking Oren to remember something, in the chat"
MEMORY_REPLY=$(curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"text\":\"ingat ya, ${NEEDLE}\",\"reply\":true}")

python3 - "$MEMORY_REPLY" <<'PY'
import json, sys
result = json.loads(sys.argv[1])
if not result.get("success"):
    raise SystemExit(f"the memory request was refused: {result}")
data = result["data"]
if not data.get("reply_message_id"):
    raise SystemExit("the request produced no reply, so nothing was confirmed")
PY

# The confirmation is written by the memory module itself, so it names the row
# that exists rather than what a model guessed.
sleep 2
THREAD=$(curl -fsS "${API}/conversations/${CONVERSATION}/messages" "${MEMORY_AUTH[@]}")
python3 - "$THREAD" "$NEEDLE" <<'PY'
import json, sys
messages = json.loads(sys.argv[1])["data"]["messages"]
needle = sys.argv[2]
text = json.dumps(messages)
if "aku ingat" not in text.lower():
    raise SystemExit("no confirmation was written into the thread")
if needle not in text:
    raise SystemExit(f"the confirmation does not name what was stored: {needle!r} is absent")
PY

log "the memory that was stored"
STORED=$(curl -fsS "${API}/memories?layer=main" "${MEMORY_AUTH[@]}")
echo "$STORED" | python3 -m json.tool
python3 - "$STORED" "$NEEDLE" <<'PY'
import json, sys
memories = json.loads(sys.argv[1])["data"]
needle = sys.argv[2]
matching = [m for m in memories if needle in m["content"]]
if not matching:
    raise SystemExit(f"the memory is not in the list: {needle!r}")
if matching[0]["created_by"] != "user":
    raise SystemExit(f"a direct request must be recorded as the user's own: {matching[0]['created_by']!r}")
if matching[0].get("layer") != "main":
    raise SystemExit(f"a request with no addressee belongs to the main memory: {matching[0].get('layer')!r}")
if "superseded_by" in matching[0]:
    raise SystemExit("a memory nothing replaced must carry no replacement id")
PY

log "a later task must answer with what was remembered"
curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"jam buka toko jam berapa?","reply":true}' >/dev/null

MEMORY_TASK=$(curl -fsS "${API}/tasks?limit=1" "${MEMORY_AUTH[@]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["task_id"])')
MEMORY_STATUS=$(wait_for_task "$MEMORY_TASK")
[[ "$MEMORY_STATUS" == "succeeded" ]] || fail "the task that should use the memory did not succeed: ${MEMORY_STATUS}"

RECALLED=$(curl -fsS "${API}/conversations/${CONVERSATION}/messages" "${MEMORY_AUTH[@]}")
python3 - "$RECALLED" "$NEEDLE" <<'PY'
import json, sys
messages = json.loads(sys.argv[1])["data"]["messages"]
needle = sys.argv[2]

# The page is newest-first, so picking "the last answer" by position would read the
# oldest one. The newest answer is chosen by its own timestamp instead, which is
# what makes the assertion about this task rather than about a page order.
answers = [m for m in messages if m.get("agent_id")]
if not answers:
    raise SystemExit("the task wrote no answer into the thread")
newest = max(answers, key=lambda m: m.get("created_at", ""))

if f"Aku ingat: {needle}" not in json.dumps(newest):
    raise SystemExit(f"the answer does not show the memory reached the model: {json.dumps(newest)[:300]}")
PY

log "a quiet conversation is extracted into a memory"
# A second conversation, so the extraction is about this thread rather than the one
# above. Two turns are enough: one message is not a conversation.
EXTRACTION_CONVERSATION=$(curl -fsS -X POST "${API}/conversations/direct" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT}\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

for text in "ongkir ke Bandung sekarang 18 ribu ya" "oke siap"; do
    curl -fsS -X POST "${API}/conversations/${EXTRACTION_CONVERSATION}/messages" "${MEMORY_AUTH[@]}" \
        -H 'Content-Type: application/json' \
        -d "{\"text\":\"${text}\",\"reply\":true}" >/dev/null
done

# The quiet period is five seconds; the wait gives the timer, the extraction
# workflow, and the embedding three times that.
EXTRACTED=""
for _ in $(seq 1 12); do
    EXTRACTED=$(curl -fsS "${API}/memories?layer=main" "${MEMORY_AUTH[@]}" \
        | python3 -c "import json,sys; print(sum(1 for m in json.load(sys.stdin)['data'] if m['created_by']=='extraction'))" || echo 0)
    [[ "$EXTRACTED" -gt 0 ]] && break
    sleep 5
done
[[ "$EXTRACTED" -gt 0 ]] || fail "the quiet conversation was never extracted"

log "the small talk was dropped and the fact was kept"
EXTRACTED_ROWS=$(curl -fsS "${API}/memories?layer=main" "${MEMORY_AUTH[@]}")
python3 - "$EXTRACTED_ROWS" <<'PY'
import json, sys
memories = json.loads(sys.argv[1])["data"]
extracted = [m for m in memories if m["created_by"] == "extraction"]
if not any("Bandung" in m["content"] for m in extracted):
    raise SystemExit(f"the extracted fact is missing: {[m['content'] for m in extracted]}")
# "oke" is twelve characters of nothing, and the pipeline's floor is what catches
# it when the model proposes it anyway.
if any(m["content"].strip(".").lower() == "oke" for m in extracted):
    raise SystemExit(f"small talk was stored as a memory: {[m['content'] for m in extracted]}")
PY

log "a corrected fact replaces its predecessor"
# The same fact, said again with a new value, in a fresh quiet conversation.
CORRECTION_CONVERSATION=$(curl -fsS -X POST "${API}/conversations/direct" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT}\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

for text in "ongkir Bandung naik jadi 21 ribu" "terima kasih"; do
    curl -fsS -X POST "${API}/conversations/${CORRECTION_CONVERSATION}/messages" "${MEMORY_AUTH[@]}" \
        -H 'Content-Type: application/json' \
        -d "{\"text\":\"${text}\",\"reply\":true}" >/dev/null
done

# The second conversation states a new number for the same fact, and the stub reads
# it out of the transcript: the pipeline must replace the older memory rather than
# keep two versions of one fact.
#
# The wait is on the settled state rather than on a fixed sleep, and it is on the
# invariant itself rather than on the first sight of the new value: a memory is
# written and its predecessor superseded in two statements, so a read that lands
# between them sees both versions active and would otherwise report a race as a
# violation.
HISTORY=""
for _ in $(seq 1 12); do
    HISTORY=$(curl -fsS "${API}/memories?layer=main&history=true" "${MEMORY_AUTH[@]}")
    if echo "$HISTORY" | python3 -c '
import json, sys
memories = json.load(sys.stdin)["data"]
bandung = [m for m in memories if "Bandung" in m["content"] and "ribu" in m["content"]]
active = [m for m in bandung if not m.get("superseded_by")]
if len(active) == 1 and "21 ribu" in active[0]["content"]:
    raise SystemExit(0)
raise SystemExit(1)
'; then
        break
    fi
    sleep 5
done

echo "$HISTORY" | python3 -m json.tool
python3 - "$HISTORY" <<'PY'
import json, sys
memories = json.loads(sys.argv[1])["data"]
bandung = [m for m in memories if "Bandung" in m["content"] and "ribu" in m["content"]]

active = [m for m in bandung if not m.get("superseded_by")]
replaced = [m for m in bandung if m.get("superseded_by")]

if len(active) != 1:
    raise SystemExit(f"exactly one version must be active, not {len(active)}: {[m['content'] for m in bandung]}")
if "21 ribu" not in active[0]["content"]:
    raise SystemExit(f"the active version is not the newest one: {active[0]['content']!r}")
if len(replaced) != 1:
    raise SystemExit(f"exactly one version must be history: {[m['content'] for m in replaced]}")
if "18 ribu" not in replaced[0]["content"]:
    raise SystemExit(f"the history is not the older version: {replaced[0]['content']!r}")
PY

log "a memory that asks for a secret is refused"
# No -f here: a refusal is the answer this phase is reading, so curl must not turn
# it into an exit code before the body can be inspected.
REFUSED=$(curl -sS -w '\n%{http_code}' -X POST "${API}/memories" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"content":"kartu pelanggan 4111 1111 1111 1111"}')
python3 - "$REFUSED" <<'PY'
import json, sys
body, _, status = sys.argv[1].rpartition("\n")
if status.strip() != "422":
    raise SystemExit(f"a secret must be refused with 422, not {status.strip()}: {body[:200]}")

result = json.loads(body)
if result.get("success"):
    raise SystemExit("a card number was stored as a memory")
if result["error"]["code"] != "sensitive_content":
    raise SystemExit(f"the refusal does not name the reason: {result['error']}")
PY

log "a document is chunked, embedded, and searchable"
DOCUMENT=$(curl -fsS -X POST "${API}/knowledge" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"source":"ongkir.txt","text":"Katalog ongkir.\n\nBandung 15 ribu di bawah satu kilogram.\n\nSurabaya 25 ribu di bawah satu kilogram."}')
echo "$DOCUMENT" | python3 -m json.tool
python3 - "$DOCUMENT" <<'PY'
import json, sys
chunks = json.loads(sys.argv[1])["data"]
if not chunks:
    raise SystemExit("the document was stored as no chunks")
for chunk in chunks:
    if chunk["status"] != "ready":
        raise SystemExit(f"a chunk was not embedded: {chunk}")
PY

log "deleting a memory takes it out of the search for good"
DOOMED=$(curl -fsS -X POST "${API}/memories" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"content":"Catatan sementara yang akan dihapus"}' \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

curl -fsS -X DELETE "${API}/memories/${DOOMED}" "${MEMORY_AUTH[@]}" >/dev/null

AFTER=$(curl -fsS "${API}/memories?q=Catatan%20sementara" "${MEMORY_AUTH[@]}")
python3 - "$AFTER" <<'PY'
import json, sys
memories = json.loads(sys.argv[1])["data"]
if memories:
    raise SystemExit(f"a deleted memory is still listed: {memories}")
PY

# The row is gone from the table as well, not flagged: the check reads the
# database the way the delete's own statement would.
GONE=$(docker exec bolu-postgres psql -U employeebot -d employeebot -tAc \
    "SELECT count(*) FROM memories WHERE id = '${DOOMED}'")
[[ "$GONE" == "0" ]] || fail "the delete left the row behind: ${GONE} rows"

log "the extraction switch can be turned off"
curl -fsS -X PATCH "${API}/agents/${AGENT}/memory-extraction" "${MEMORY_AUTH[@]}" \
    -H 'Content-Type: application/json' -d '{"enabled":false}' >/dev/null

SWITCH=$(curl -fsS "${API}/agents" "${MEMORY_AUTH[@]}" \
    | python3 -c "import json,sys; d=json.load(sys.stdin)['data']; print([a for a in d if a['id']=='${AGENT}'][0].get('memory_extraction_enabled'))")
[[ "$SWITCH" == "False" ]] || fail "the extraction switch did not turn off: ${SWITCH}"

echo
echo "SMOKE OK"
echo "  a chat message became a durable task, ran out of process, and answered in its thread;"
echo "  a task past its step allowance stopped with a reason a person can read;"
echo "  a memory request was stored and confirmed, a later task was given it, a quiet"
echo "  conversation was extracted, a corrected fact replaced its predecessor, a secret"
echo "  was refused, a document was indexed, and a delete removed the row for good."
