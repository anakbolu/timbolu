#!/usr/bin/env bash
#
# End-to-end smoke for the MVP flow: a customer's message becomes an order in the workspace's own
# data, and the invoice that follows waits for a person.
#
# It runs the real API and the real agent worker against the compose Postgres, Redis, and Temporal,
# with the model gateway pointed at a stub. The point is the flow — the tools, the catalogue, and the
# gate — not the provider, and not WhatsApp: what a linked number adds is where the message is
# delivered, and every step up to that boundary is the same code.
#
# Three phases, each of which is a promise the product makes:
#
#   1. **The price comes from the catalogue, not from the model.** The stub calls the tools with the
#      product name and a quantity and nothing else, because that is all the tool's schema accepts;
#      the order that lands carries the catalogue's own price, name, and unit, and a total the database
#      computed.
#   2. **An order that cannot be taken is not taken partially.** A product the catalogue does not have
#      refuses the whole order, the model is told what the catalogue does have, and nothing is written.
#   3. **Nothing leaves without a person.** The invoice the Bolu composes becomes a draft with the total
#      on it, and with the draft pending nothing has been sent.
#
# Usage: scripts/smoke-order.sh
set -euo pipefail

cd "$(dirname "$0")/.."

API_PORT="${API_PORT:-8092}"
STUB_PORT="${STUB_PORT:-11437}"
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
    rm -f /tmp/smoke-order-stub.py /tmp/smoke-order-gmail.py
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

# The compose workers poll the same Temporal task queue this smoke does, so a running one executes the
# smoke's task with its own model configuration and the run tests something other than this change.
for container in bolu-agent-worker bolu-integration-worker; do
    if docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null | grep -q true; then
        fail "${container} is running and polls the same task queue; stop it with 'docker stop bolu-api bolu-agent-worker bolu-integration-worker'"
    fi
done

# ------------------------------------------------------------- stub provider

cat > /tmp/smoke-order-stub.py <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

# The stub walks each phase's flow the way the template's persona describes it. It is deliberately
# scripted rather than clever: a smoke that depends on a model choosing the right tool tests the model.
#
#   take     find the price, record the order, answer the customer. The order phase.
#   unknown  record a product the catalogue does not have, then answer. The refusal phase.
#   invoice  read the customer's orders, then compose the invoice and hand it to the sending tool.
#            The phase that ends parked on a draft.
#   incomplete  record an order whose quantity is zero, then ask the customer. The phase where a
#            half-read message must not become a line on an invoice.
MODE = os.environ.get("STUB_MODE", "take")

KNOWN = {"product": "Kopi Arabika", "quantity": 2}
# A product the catalogue does not sell, named the way a customer would name it: more specific than the
# catalogue entry, which is the case where the suggestion list has to work backwards — the customer's
# words contain the catalogue's name, not the other way round.
UNKNOWN = {"product": "Kopi Arabika Gayo 1 kg", "quantity": 1}
CUSTOMER = {"customer_name": "Bu Sari", "customer_phone": "628111222333"}


def calls_by_id(body):
    """Which tool each call id belongs to, read off the assistant turns.

    A result message carries only the id of the call it answers — not the name — so the name has to come
    from the call itself. This is the mapping the stub needs to know what it already asked for.
    """
    names = {}
    for message in body.get("messages", []):
        if message.get("role") != "assistant":
            continue
        for call in message.get("tool_calls") or []:
            function = call.get("function") or {}
            names[call.get("id")] = function.get("name")

    return names


def called(body, name):
    """Whether the model already made one call to `name`."""
    return name in calls_by_id(body).values()


def result_text(body, name):
    """The content of the newest result for `name`, or an empty string."""
    by_id = calls_by_id(body)
    results = []
    for message in body.get("messages", []):
        if message.get("role") != "tool":
            continue
        if by_id.get(message.get("tool_call_id")) != name:
            continue
        content = message.get("content") or ""
        if isinstance(content, list):
            content = " ".join(part.get("text", "") for part in content)
        results.append(content)

    return results[-1] if results else ""


def call(name, arguments, call_id):
    return {
        "id": "chatcmpl-" + call_id,
        "object": "chat.completion",
        "model": "stub",
        "choices": [{
            "index": 0,
            "message": {
                "role": "assistant",
                "content": "",
                "tool_calls": [{
                    "id": call_id,
                    "type": "function",
                    "function": {"name": name, "arguments": json.dumps(arguments)},
                }],
            },
            "finish_reason": "tool_calls",
        }],
        "usage": {"prompt_tokens": 320, "completion_tokens": 40, "total_tokens": 360},
    }


def answer(text):
    return {
        "id": "chatcmpl-order",
        "object": "chat.completion",
        "model": "stub",
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": text},
            "finish_reason": "stop",
        }],
        "usage": {"prompt_tokens": 280, "completion_tokens": 48, "total_tokens": 328},
    }


def total_from(result):
    """The total the recording tool answered with, as a sentence a customer reads."""
    try:
        recorded = json.loads(result)
    except (ValueError, TypeError):
        return "Rp 0"

    return "Rp {:g}".format(recorded.get("total_rupiah", 0))


def decide(body, offered):
    if MODE == "incomplete":
        if not called(body, "order.record_order"):
            # The product is real and the quantity is not: zero is schema-valid and meaningless, which
            # is exactly the shape of a model that half-read the message.
            return call("order.record_order", dict(CUSTOMER, items=[{"product": KNOWN["product"], "quantity": 0}]), "record")

        return answer("Maaf Bu, jumlahnya berapa ya? Saya catat setelah jelas.")

    if MODE == "unknown":
        if not called(body, "order.record_order"):
            return call("order.record_order", dict(CUSTOMER, items=[UNKNOWN]), "record-unknown")

        return answer("Produk itu belum ada di katalog kami, Bu. Mau saya catat produk lain?")

    if MODE == "invoice":
        if not called(body, "order.read_orders"):
            return call("order.read_orders", {"customer_phone": CUSTOMER["customer_phone"], "limit": 5}, "read")

        read = result_text(body, "order.read_orders")
        orders = json.loads(read).get("pesanan") if read else []
        latest = orders[0] if orders else {}
        total = latest.get("total_rupiah", 0)
        # The line keys are the tool's own — the adapter names them in Indonesian, the same language a
        # Bolu answers in — so the invoice a person reads is composed from what the tool answered rather
        # than from a guess about its shape.
        lines = ", ".join(
            "{} {} {}".format(line.get("jumlah"), line.get("satuan", ""), line.get("produk", ""))
            for line in latest.get("baris", [])
        )
        message = "Bu Sari, tagihan pesanan {}: {} - total Rp {:g}. Mohon dikonfirmasi ya.".format(
            latest.get("kode", ""), lines, total)

        # The channel is whichever one this deployment has, which is what "the available channel" means
        # in the epic. It is read off the tools the model was offered rather than assumed, so this run
        # works with WhatsApp when a number is linked and with a mailbox when the gateway is not part of
        # the deployment — the flow above the send boundary is the same code either way.
        send = next((name for name in offered if name.endswith((".send", ".send_message"))), "")
        if not send:
            return answer("Tagihan sudah siap, tapi belum ada kanal pengiriman yang tersambung.")

        if send.endswith(".send_message"):
            return call(send, {"to": CUSTOMER["customer_phone"], "text": message}, "send")

        return call(send, {
            "to": "pelanggan@example.com",
            "subject": "Tagihan pesanan {}".format(latest.get("kode", "")),
            "body": message,
        }, "send")

    # take
    if not called(body, "order.find_product"):
        return call("order.find_product", {"product": KNOWN["product"]}, "find")
    if not called(body, "order.record_order"):
        return call("order.record_order", dict(CUSTOMER, note="Kirim ke Jalan Melati 3", items=[KNOWN]), "record")

    return answer("Baik Bu Sari, pesanannya sudah kami catat: 2 kg Kopi Arabika, total {}. Terima kasih.".format(
        total_from(result_text(body, "order.record_order"))))


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        offered = [t["function"]["name"] for t in body.get("tools", []) if t.get("function", {}).get("name")]

        payload = decide(body, offered)

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
# The invoice needs a channel that leaves the building, and a mailbox is the one this deployment can
# offer without a linked WhatsApp number. Google is stubbed so the whole flow runs without an account:
# the consent screen the flow is started against, the token it stores, and what it received when a
# person approved the send.
cat > /tmp/smoke-order-gmail.py <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SENT = "/tmp/smoke-order-gmail-sent.jsonl"


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
            with open(SENT, "a") as log:
                log.write(body.decode() + "\n")
            return self._send({"id": "msg-sent", "threadId": "t1"})

        if path.endswith("/drafts"):
            return self._send({"id": "draft-1", "message": {"id": "m1"}})

        return self._send({"error": "unknown"}, 404)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/auth":
            query = parse_qs(parsed.query)
            redirect = query.get("redirect_uri", [""])[0]
            state = query.get("state", [""])[0]
            self.send_response(302)
            self.send_header("Location", "{}?code=a-good-code&state={}".format(redirect, state))
            self.end_headers()
            return

        if parsed.path.endswith("/users/me/profile"):
            return self._send({"emailAddress": "toko@example.com"})

        return self._send({"error": "unknown"}, 404)

    def log_message(self, *args):
        pass


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY

GMAIL_PORT="${GMAIL_PORT:-11438}"

start_gmail() {
    lsof -ti ":${GMAIL_PORT}" | xargs -r kill -9 2>/dev/null || true
    for _ in $(seq 1 20); do
        lsof -ti ":${GMAIL_PORT}" >/dev/null 2>&1 || break
        sleep 0.5
    done
    lsof -ti ":${GMAIL_PORT}" >/dev/null 2>&1 && fail "port ${GMAIL_PORT} did not free up"

    rm -f /tmp/smoke-order-gmail-sent.jsonl
    python3 /tmp/smoke-order-gmail.py "$GMAIL_PORT" &
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

    STUB_MODE="${1:-take}" python3 /tmp/smoke-order-stub.py "$STUB_PORT" &
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

# run_with_env runs one command with the shared environment plus extra pairs.
#
# It execs rather than wrapping: the caller records `$!` to stop the process later, and a wrapper
# subshell would make that pid the wrapper's — killing the wrapper would leave the real API or worker
# alive and still polling the task queue, and the next run's work would be executed by the previous
# run's binary.
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
        "LLM_DEFAULT_ADAPTER=openai" \
        "LLM_DEFAULT_BASE_URL=http://localhost:${STUB_PORT}" \
        "GMAIL_CLIENT_ID=stub-client" \
        "GMAIL_CLIENT_SECRET=stub-secret" \
        "GMAIL_API_BASE=http://localhost:${GMAIL_PORT}" \
        "GMAIL_AUTH_ENDPOINT=http://localhost:${GMAIL_PORT}/auth" \
        "GMAIL_TOKEN_ENDPOINT=http://localhost:${GMAIL_PORT}/token" \
        "OAUTH_REDIRECT_BASE=http://localhost:${API_PORT}" \
        "LLM_DEFAULT_MODEL=stub" \
        "LLM_DEFAULT_API_KEY=stub-key" \
        "STORAGE_DRIVER=local" \
        "STORAGE_LOCAL_ROOT=.storage" \
        "TASK_MAX_STEPS=8"
}

start_stack() {
    run_with_env "HTTP_ADDRESS=:${API_PORT}
MAIL_DRIVER=log
FRONTEND_URL=http://localhost:3000
CONTENT_ORIGIN=http://localhost:${API_PORT}" \
        "${BIN_DIR}/api" > /tmp/smoke-order-api.log 2>&1 &
    API_PID=$!

    run_with_env "MAIL_DRIVER=log" \
        "${BIN_DIR}/agent-worker" > /tmp/smoke-order-worker.log 2>&1 &
    WORKER_PID=$!

    for _ in $(seq 1 90); do
        curl -fsS "${API}/health" >/dev/null 2>&1 && return 0
        kill -0 "$API_PID" 2>/dev/null || fail "the API exited during startup; see /tmp/smoke-order-api.log"
        sleep 1
    done
    fail "the API did not become ready; see /tmp/smoke-order-api.log"
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
start_stub take

API="http://localhost:${API_PORT}/api"
start_stack

# ------------------------------------------------------------------ the account

log "registering and onboarding a workspace"
EMAIL="order-$(date +%s)@example.com"
PASSWORD="Rahasia123!"
curl -fsS -X POST "${API}/auth/register" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

TOKEN=""
for _ in $(seq 1 20); do
    TOKEN=$(grep -o 'token=[A-Za-z0-9_-]*' /tmp/smoke-order-api.log | tail -1 | cut -d= -f2 || true)
    [[ -n "$TOKEN" ]] && break
    sleep 1
done
[[ -n "$TOKEN" ]] || fail "no verification token in /tmp/smoke-order-api.log"

curl -fsS -X POST "${API}/auth/verify-email" -H 'Content-Type: application/json' \
    -d "{\"token\":\"${TOKEN}\"}" >/dev/null

ACCESS=$(curl -fsS -X POST "${API}/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])')

ONBOARD=$(curl -fsS -X POST "${API}/workspaces/onboard" -H "Authorization: Bearer ${ACCESS}" \
    -H 'Content-Type: application/json' \
    -d '{"name":"Kopi Sinar","business_field":"F&B","timezone":"Asia/Jakarta"}')
WORKSPACE=$(echo "$ONBOARD" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["workspace"]["id"])')
AUTH=(-H "Authorization: Bearer ${ACCESS}" -H "X-Workspace-Id: ${WORKSPACE}")

# The Bolu the flow is for: the template the sales role ships with.
TEAM=$(curl -fsS "${API}/teams" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([t for t in d if t["kind"]=="bolu"][0]["id"])')

AGENT_JSON=$(curl -fsS -X POST "${API}/agents" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"team_id\":\"${TEAM}\",\"name\":\"Oren\",\"template_key\":\"oren\"}")
AGENT=$(echo "$AGENT_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

# The template has to be carrying the flow, or the tools are never offered and the rest of this script
# tests nothing. That is the claim E14.4 makes about it being data rather than code.
echo "$AGENT_JSON" | python3 -c '
import json, sys
agent = json.load(sys.stdin)["data"]
tools = agent.get("tools") or []
for want in ("order.find_product", "order.record_order", "whatsapp.send_message"):
    assert want in tools, "the sales template does not carry {}: {}".format(want, tools)
print("  template tools: {}".format(", ".join(tools)))
'

# The invoice needs a channel that leaves the building, so the Bolu is given one the way a person
# gives it: the flow is started, the provider's consent screen is followed, and the callback stores the
# sealed token. Seeding the row directly would leave `status = connected` with no credential.
log "connecting a Gmail account and granting it to the Bolu"
CONSENT_BODY=$(curl -sS "${API}/integrations/gmail/connect?return_to=/dashboard/integrations" "${AUTH[@]}")
CONSENT=$(echo "$CONSENT_BODY" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["url"])' 2>/dev/null) ||
    fail "the flow could not be started: ${CONSENT_BODY}"

CALLBACK=$(curl -sS -o /dev/null -w '%{redirect_url}' "$CONSENT")
[[ -n "$CALLBACK" ]] || fail "the provider did not redirect back to the callback"
curl -sS "$CALLBACK" >/dev/null || fail "the callback did not answer"

CONNECTED=$(curl -fsS "${API}/integrations" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(next((c["integration_id"] for c in d if c["status"]=="connected"), ""))')
[[ -n "$CONNECTED" ]] || fail "the account was not connected, so the invoice would have no channel"

curl -fsS -X PUT "${API}/agents/${AGENT}/grants/${CONNECTED}" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d '{"permission":"read_write"}' >/dev/null

docker exec -i bolu-postgres psql -U employeebot -d employeebot -q <<SQL
UPDATE workspaces SET work_hours_start = '00:00', work_hours_end = '23:59'
WHERE id = '${WORKSPACE}';
SQL

# --------------------------------------------------------------- the catalogue

log "seeding the catalogue through the API, the way a person maintains it"
for product in '{"sku":"KOPI-1","name":"Kopi Arabika","price_idr":15000,"unit":"kg"}' \
               '{"sku":"SUSU-1","name":"Kopi Susu","price_idr":20000,"unit":"pcs"}'; do
    curl -fsS -X POST "${API}/orders/products" "${AUTH[@]}" \
        -H 'Content-Type: application/json' -d "$product" >/dev/null
done

curl -fsS "${API}/orders/products" "${AUTH[@]}" | python3 -c '
import json, sys
products = json.load(sys.stdin)["data"]
assert len(products) == 2, products
prices = {p["name"]: p["price_idr"] for p in products}
assert prices["Kopi Arabika"] == 15000, prices
print("  catalogue: {}".format(prices))
'

CONVERSATION=$(curl -fsS -X POST "${API}/conversations/direct" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

order_count() {
    curl -fsS "${API}/orders" "${AUTH[@]}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["meta"]["count"])'
}

latest_order() {
    curl -fsS "${API}/orders?limit=1" "${AUTH[@]}" |
        python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(json.dumps(d[0]) if d else "")'
}

task_status() {
    curl -fsS "${API}/tasks/$1" "${AUTH[@]}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])'
}

wait_for_task_status() {
    local task="$1" want="$2" deadline=$((SECONDS + 90)) status=""
    while (( SECONDS < deadline )); do
        status=$(task_status "$task")
        [[ "$status" == "$want" ]] && break
        sleep 1
    done
    printf '%s' "$status"
}

wait_for_order_count() {
    local want="$1" deadline=$((SECONDS + 60)) count=""
    while (( SECONDS < deadline )); do
        count=$(order_count)
        [[ "$count" == "$want" ]] && break
        sleep 1
    done
    printf '%s' "$count"
}

steps_of() {
    curl -fsS "${API}/tasks/$1/steps" "${AUTH[@]}"
}

# -------------------------------------------------- phase 1: the order is taken

log "phase 1: the message becomes an order priced from the catalogue"
curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Saya mau pesan 2 kg Kopi Arabika ya, kirim ke Jalan Melati 3.","reply":true}' >/dev/null

COUNT=$(wait_for_order_count 1)
[[ "$COUNT" == "1" ]] || fail "no order was recorded; the count is ${COUNT}"

ORDER=$(latest_order)
echo "$ORDER" | python3 -m json.tool

python3 - "$ORDER" <<'PY'
import json, sys
order = json.loads(sys.argv[1])

# The price is the catalogue's, and the line is what the catalogue says it is: the stub never sent a
# price — the tool's schema has no field for one — so anything else here would be a number nobody gave.
assert order["status"] == "recorded", order["status"]
assert order["customer_phone"] == "628111222333", order["customer_phone"]
assert order["customer_name"] == "Bu Sari", order["customer_name"]
assert order["code"], "an order a person can quote needs a code"

line = order["lines"][0]
assert line["description"] == "Kopi Arabika", line
assert line["unit"] == "kg", line
assert line["quantity"] == 2, line
assert line["unit_price_idr"] == 15000, line
assert line["line_total_idr"] == 30000, line
assert order["total_idr"] == 30000, order

# The Bolu and the task that took it are on the order, which is what makes "who took this" answerable.
assert order.get("agent_id"), order
print("  recorded {}: 2 kg Kopi Arabika = {}".format(order["code"], order["total_idr"]))
PY

TASK1=$(echo "$ORDER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["task_id"])')
FINAL1=$(wait_for_task_status "$TASK1" "succeeded")
[[ "$FINAL1" == "succeeded" ]] || fail "the task that took the order is ${FINAL1}, not succeeded"

log "the rounds it recorded"
STEPS1=$(steps_of "$TASK1")
echo "$STEPS1" | python3 scripts/print-steps.py

python3 - "$STEPS1" <<'PY'
import json, sys

# A step's input and output are JSON documents the API returns as JSON, so they arrive as objects;
# a pruned step would carry neither, and this is not a pruned step.
def document(value):
    if isinstance(value, str):
        return json.loads(value)
    return value


steps = json.loads(sys.argv[1])["data"]

# The call and its result are two steps: the arguments are on the call, the answer on the result. That
# separation is the runtime's, and reading them apart is what lets this check both.
def step(kind):
    return [s for s in steps if s.get("kind") == kind and s.get("tool_name") == "order.record_order"]


calls, results = step("tool_call"), step("tool_result")
assert calls and results, "the recorded order must be on the trail as a call and a result"

arguments = document(calls[0]["input"])
assert arguments["items"][0]["product"] == "Kopi Arabika", arguments
assert arguments["items"][0]["quantity"] == 2, arguments
assert "price" not in json.dumps(arguments).lower(), \
    "the model must not be able to name a price: {}".format(arguments)

result = document(results[0]["output"])
assert result["kode"], result
assert result["total_rupiah"] == 30000, result
assert result["baris"][0]["harga_satuan_rupiah"] == 15000, result
print("  the call carried a product and a quantity, and the tool answered with the total")
PY

# ------------------------------------------------- phase 2: the refusal

log "phase 2: a product the catalogue does not have refuses the whole order"
stop_stub
start_stub unknown

curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Tambah 1 Kopi Arabika Gayo 1 kg ya.","reply":true}' >/dev/null

# The task ends in a non-failure state: a refusal is an answer the model reads, not a broken task.
PHASE2=0
for _ in $(seq 1 90); do
    PHASE2=$(curl -fsS "${API}/orders" "${AUTH[@]}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["meta"]["count"])')
    [[ "$PHASE2" == "1" ]] || break
    sleep 1
done
[[ "$PHASE2" == "1" ]] || fail "the unknown product was recorded anyway: ${PHASE2} orders"

TASK2=$(curl -fsS "${API}/tasks?agent_id=${AGENT}&limit=1" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["task_id"] if d else "")')
[[ -n "$TASK2" ]] || fail "the second message opened no task"
wait_for_task_status "$TASK2" "succeeded" >/dev/null

STEPS2=$(steps_of "$TASK2")
echo "$STEPS2" | python3 scripts/print-steps.py

python3 - "$STEPS2" <<'PY'
import json, sys

steps = json.loads(sys.argv[1])["data"]

refused = [s for s in steps if s.get("kind") == "tool_result" and s.get("tool_name") == "order.record_order"]
assert refused, "the call that was refused must be on the trail"

output = refused[0]["output"]
text = output if isinstance(output, str) else json.dumps(output)
assert "Kopi Arabika Gayo" in text, "the refusal must name what was asked for: {}".format(text)
assert "Kopi Arabika" in text, "the refusal must name what the catalogue does have: {}".format(text)
print("  the catalogue answered with what it has, and nothing was written")
PY

# --------------------------------------- phase 3: an order that is not complete

log "phase 3: an order whose quantity is not clear is asked about, not guessed at"
stop_stub
start_stub incomplete

curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Pesan Kopi Arabika ya, jumlahnya nyusul.","reply":true}' >/dev/null

TASK3=$(curl -fsS "${API}/tasks?agent_id=${AGENT}&limit=1" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["task_id"] if d else "")')
[[ -n "$TASK3" ]] || fail "the third message opened no task"
wait_for_task_status "$TASK3" "succeeded" >/dev/null

STEPS3=$(steps_of "$TASK3")
echo "$STEPS3" | python3 scripts/print-steps.py

python3 - "$STEPS3" <<'PY'
import json, sys

steps = json.loads(sys.argv[1])["data"]
refused = [s for s in steps if s.get("kind") == "tool_result" and s.get("tool_name") == "order.record_order"]
assert refused, "the call with an impossible quantity must be on the trail"

output = refused[0]["output"]
text = output if isinstance(output, str) else json.dumps(output)
assert "Kopi Arabika" in text, "the refusal must name the line: {}".format(text)
assert "nol" in text or "0" in text, "the refusal must say what was wrong with the quantity: {}".format(text)
print("  the catalogue refused the line and named it")
PY

COUNT3=$(order_count)
[[ "$COUNT3" == "1" ]] || fail "an incomplete order was written anyway: ${COUNT3} orders"

# --------------------------------------------- phase 4: the invoice waits

log "phase 4: the invoice becomes a draft and nothing leaves"
stop_stub
start_stub invoice

curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"text":"Tolong susun tagihannya ya.","reply":true}' >/dev/null

DRAFT_ID=""
for _ in $(seq 1 90); do
    DRAFT_ID=$(curl -fsS "${API}/drafts?status=pending&limit=1" "${AUTH[@]}" |
        python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["draft_id"] if d else "")')
    [[ -n "$DRAFT_ID" ]] && break
    sleep 1
done
[[ -n "$DRAFT_ID" ]] || fail "the invoice did not become a draft"

DRAFT=$(curl -fsS "${API}/drafts/${DRAFT_ID}" "${AUTH[@]}")
echo "$DRAFT" | python3 -m json.tool

python3 - "$DRAFT" <<'PY'
import json, sys
draft = json.loads(sys.argv[1])["data"]

# The promise, stated as an assertion: a message that reaches a customer is a decision a person made.
# The channel is whichever one this deployment has, so what is asserted is what the promise is about —
# a send tool, a recipient, the total the order actually has, and nothing sent yet.
assert draft["action_kind"].endswith((".send", ".send_message")), draft["action_kind"]
assert draft["status"] == "pending", draft["status"]
assert not draft.get("sent_at"), "the message left without a decision"
assert draft["payload"].get("to"), draft["payload"]

payload = json.dumps(draft["payload"])
assert "30000" in payload, "the invoice must carry the total the order has: {}".format(payload)
assert "P-" in payload, "the invoice must quote the order's code: {}".format(payload)
print("  pending {}: {}".format(draft["action_kind"], draft["title"]))
PY

# And nothing reached the provider: the gate is what stands between a Bolu composing a message and a
# customer receiving one, so the stub that would have received it must have received nothing.
[[ ! -s /tmp/smoke-order-gmail-sent.jsonl ]] ||
    fail "a message reached the provider before anybody approved it"

# And the card an approver actually sees: the draft has to be in the thread it came from, as a block
# the screen renders, not only as a row the API can read. "Menampilkannya sebagai kartu persetujuan" is
# the epic's wording, so the check is the block.
log "the card is in the thread it came from"
curl -fsS "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" > /tmp/smoke-order-thread.json
python3 - "$DRAFT_ID" <<'PY'
import json, sys

draft_id = sys.argv[1]
messages = json.load(open("/tmp/smoke-order-thread.json"))["data"]["messages"]

cards = [
    block
    for message in messages
    for block in (message.get("blocks") or [])
    if block.get("type") == "draft" and block.get("draft_id") == draft_id
]
assert cards, "the draft must appear in the thread as a card: {}".format(
    [b.get("type") for m in messages for b in (m.get("blocks") or [])])
assert cards[0].get("status") == "pending", cards[0]
print("  card in the thread: {} - {}".format(cards[0].get("action_kind"), cards[0].get("title")))
PY

DRAFT_TASK=$(echo "$DRAFT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["task_id"])')
PARKED=$(wait_for_task_status "$DRAFT_TASK" "waiting_approval")
[[ "$PARKED" == "waiting_approval" ]] ||
    fail "the task did not park on the invoice; status is ${PARKED}"

echo
echo "SMOKE OK"
echo "  a customer's message became an order priced from the workspace's own catalogue,"
echo "  with the Bolu and the task that took it recorded;"
echo "  a product the catalogue does not have refused the whole order and was answered"
echo "  with what the catalogue does have;"
echo "  an order whose quantity was not clear was asked about rather than guessed at;"
echo "  the invoice became a draft carrying the total, the task parked, and nothing left."
