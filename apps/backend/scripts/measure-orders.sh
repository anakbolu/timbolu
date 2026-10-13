#!/usr/bin/env bash
#
# Measures what one order costs: the MVP flow run repeatedly, with the numbers read from
# `usage_ledger` rather than estimated.
#
# It is the same stack the smoke runs — the real API and the real agent worker against the compose
# Postgres, Redis, and Temporal, with the model pointed at the shared stub — because the thing being
# measured is the flow the product runs, not the flow a test can script around it.
#
# What it is, and what it is not:
#
#   - **It is the shape of the cost.** How many model calls one order takes, how many tokens each of
#     those calls carries, and how that grows as the flow goes on. That is the number a package price
#     is built on, and it does not depend on the provider.
#   - **It is not the rupiah.** The stub's prices are the platform's table applied to a stub's token
#     counts; a provider that charges differently gives a different figure for the same shape. Re-run
#     this with a real provider (which is what staging is for) for the money.
#
# The orders are sent one at a time and each is waited for, so the tasks are sequential: a fleet
# running twenty at once would measure contention, not one order.
#
# Usage: scripts/measure-orders.sh [count]
set -euo pipefail

cd "$(dirname "$0")/.."

COUNT="${1:-20}"
API_PORT="${API_PORT:-8093}"
STUB_PORT="${STUB_PORT:-11439}"
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
    [[ -n "${BIN_DIR:-}" ]] && rm -rf "$BIN_DIR"
}
trap cleanup EXIT

# --------------------------------------------------------------- preconditions

for port in "$API_PORT" "$STUB_PORT"; do
    if lsof -ti ":${port}" >/dev/null 2>&1; then
        fail "port ${port} is already in use; stop whatever is listening on it first"
    fi
done

command -v docker >/dev/null || fail "docker is required: the measurement uses the compose stack"
docker exec bolu-postgres pg_isready -U employeebot >/dev/null 2>&1 ||
    fail "the compose Postgres is not reachable; run 'make up' first"

for container in bolu-agent-worker bolu-integration-worker; do
    if docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null | grep -q true; then
        fail "${container} is running and polls the same task queue; stop it with 'docker stop bolu-api bolu-agent-worker bolu-integration-worker'"
    fi
done

# ------------------------------------------------------------- the catalogue

# Five products, so the measured orders are not all the same line: a price per order that was measured
# on one product would be a price per one product.
CATALOGUE=(
    '{"sku":"KOPI-1","name":"Kopi Arabika","price_idr":15000,"unit":"kg"}'
    '{"sku":"SUSU-1","name":"Kopi Susu","price_idr":20000,"unit":"pcs"}'
    '{"sku":"GULA-1","name":"Gula Aren","price_idr":18000,"unit":"kg"}'
    '{"sku":"TEH-1","name":"Teh Melati","price_idr":12000,"unit":"box"}'
    '{"sku":"ROTI-1","name":"Roti Sisir","price_idr":9000,"unit":"pcs"}'
)
PRODUCT_NAMES="Kopi Arabika,Kopi Susu,Gula Aren,Teh Melati,Roti Sisir"

# ------------------------------------------------------------- stub provider

log "starting the stub model provider on :${STUB_PORT}"
STUB_PRODUCTS="${PRODUCT_NAMES}" STUB_MODE=measure python3 scripts/order-stub.py "$STUB_PORT" &
STUB_PID=$!

for _ in $(seq 1 20); do
    curl -fsS -X POST "http://localhost:${STUB_PORT}/v1/chat/completions" \
        -H 'Content-Type: application/json' -d '{"messages":[]}' >/dev/null 2>&1 && break
    kill -0 "$STUB_PID" 2>/dev/null || fail "the stub model provider exited on startup"
    sleep 0.5
done

# ----------------------------------------------------------------- the binaries

log "building the API and the agent worker"
BIN_DIR="$(mktemp -d)"
CGO_ENABLED=0 go build -o "${BIN_DIR}/api" ./cmd/api
CGO_ENABLED=0 go build -o "${BIN_DIR}/agent-worker" ./cmd/agent-worker

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
        "KMS_MASTER_KEY_ID=measure-v1" \
        "OAUTH_STATE_KEY=local_dev_oauth_state_key_32_bytes" \
        "LLM_DEFAULT_ADAPTER=openai" \
        "LLM_DEFAULT_BASE_URL=http://localhost:${STUB_PORT}" \
        "LLM_DEFAULT_MODEL=stub" \
        "LLM_DEFAULT_API_KEY=stub-key" \
        "STORAGE_DRIVER=local" \
        "STORAGE_LOCAL_ROOT=.storage" \
        "TASK_MAX_STEPS=8"
}

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

run_with_env "HTTP_ADDRESS=:${API_PORT}
MAIL_DRIVER=log
FRONTEND_URL=http://localhost:3000
CONTENT_ORIGIN=http://localhost:${API_PORT}" \
    "${BIN_DIR}/api" > /tmp/measure-orders-api.log 2>&1 &
API_PID=$!

run_with_env "MAIL_DRIVER=log" \
    "${BIN_DIR}/agent-worker" > /tmp/measure-orders-worker.log 2>&1 &
WORKER_PID=$!

API="http://localhost:${API_PORT}/api"
for _ in $(seq 1 90); do
    curl -fsS "${API}/health" >/dev/null 2>&1 && break
    kill -0 "$API_PID" 2>/dev/null || fail "the API exited during startup; see /tmp/measure-orders-api.log"
    sleep 1
done

# ------------------------------------------------------------------ the account

log "registering and onboarding the measuring workspace"
EMAIL="measure-$(date +%s)@example.com"
PASSWORD="Rahasia123!"
curl -fsS -X POST "${API}/auth/register" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

TOKEN=""
for _ in $(seq 1 20); do
    TOKEN=$(grep -o 'token=[A-Za-z0-9_-]*' /tmp/measure-orders-api.log | tail -1 | cut -d= -f2 || true)
    [[ -n "$TOKEN" ]] && break
    sleep 1
done
[[ -n "$TOKEN" ]] || fail "no verification token in /tmp/measure-orders-api.log"

curl -fsS -X POST "${API}/auth/verify-email" -H 'Content-Type: application/json' \
    -d "{\"token\":\"${TOKEN}\"}" >/dev/null

ACCESS=$(curl -fsS -X POST "${API}/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])')

ONBOARD=$(curl -fsS -X POST "${API}/workspaces/onboard" -H "Authorization: Bearer ${ACCESS}" \
    -H 'Content-Type: application/json' \
    -d '{"name":"Kopi Ukur","business_field":"F&B","timezone":"Asia/Jakarta"}')
WORKSPACE=$(echo "$ONBOARD" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["workspace"]["id"])')
AUTH=(-H "Authorization: Bearer ${ACCESS}" -H "X-Workspace-Id: ${WORKSPACE}")

TEAM=$(curl -fsS "${API}/teams" "${AUTH[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([t for t in d if t["kind"]=="bolu"][0]["id"])')

AGENT=$(curl -fsS -X POST "${API}/agents" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"team_id\":\"${TEAM}\",\"name\":\"Oren\",\"template_key\":\"oren\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

log "seeding the catalogue"
for product in "${CATALOGUE[@]}"; do
    curl -fsS -X POST "${API}/orders/products" "${AUTH[@]}" \
        -H 'Content-Type: application/json' -d "$product" >/dev/null
done

CONVERSATION=$(curl -fsS -X POST "${API}/conversations/direct" "${AUTH[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')

# order_count is how many orders the workspace has.
#
# It reads the rows rather than `meta.count`, and that is not a detail: the list endpoints report the
# number of rows in the *page*, not the size of the result set, so asking with `limit=1` would answer
# "1" forever. A caller that wants a total has to read the rows it asked for.
order_count() {
    curl -fsS "${API}/orders?limit=200" "${AUTH[@]}" |
        python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))'
}

# ------------------------------------------------------------------ the orders

log "sending ${COUNT} orders, one at a time"
SENT_STARTED=$SECONDS
NAMES=("Kopi Arabika" "Kopi Susu" "Gula Aren" "Teh Melati" "Roti Sisir")
UNITS=("kg" "pcs" "kg" "box" "pcs")

for ((index = 0; index < COUNT; index++)); do
    product="${NAMES[$((index % ${#NAMES[@]}))]}"
    unit="${UNITS[$((index % ${#UNITS[@]}))]}"
    quantity=$(( (index % 3) + 1 ))
    phone="62811100$(printf '%04d' "$index")"

    curl -fsS -X POST "${API}/conversations/${CONVERSATION}/messages" "${AUTH[@]}" \
        -H 'Content-Type: application/json' \
        -d "{\"text\":\"Halo, saya mau pesan ${quantity} ${unit} ${product} ya. Nomor saya ${phone}.\",\"reply\":true}" \
        >/dev/null

    # Each order is waited for before the next is sent, so the measurement is one order at a time.
    deadline=$((SECONDS + 90))
    while (( SECONDS < deadline )); do
        landed=$(order_count)
        (( landed >= index + 1 )) && break
        sleep 1
    done
    (( landed >= index + 1 )) || fail "order $((index + 1)) was never recorded"
    printf '  %2d/%d  %s %s %s\n' "$((index + 1))" "$COUNT" "$quantity" "$unit" "$product"
done

sending_seconds=$((SECONDS - SENT_STARTED))

orders=$(order_count)
[[ "$orders" == "$COUNT" ]] || fail "expected ${COUNT} orders, found ${orders}"

# ------------------------------------------------------------------ the numbers

log "reading the numbers back from the product's own report"
curl -fsS "${API}/llm/usage/report?days=30" "${AUTH[@]}" > /tmp/measure-orders-report.json
curl -fsS "${API}/orders?limit=${COUNT}" "${AUTH[@]}" > /tmp/measure-orders-orders.json

python3 - "$COUNT" "$sending_seconds" <<'PY'
import json
import statistics
import sys

count = int(sys.argv[1])
seconds = int(sys.argv[2])
report = json.load(open("/tmp/measure-orders-report.json"))["data"]
order_rows = json.load(open("/tmp/measure-orders-orders.json"))["data"]

# The report's per-task rows are the join: each measured order's task is one row, so the order's own
# token count is the product's answer rather than a sum this script did itself.
by_task = {row["task_id"]: row for row in report["by_task"]}

print()
print("  orders                       {}".format(len(order_rows)))
print("  model calls                  {}".format(report["totals"]["calls"]))
print("  tokens in / out              {} / {}".format(
    report["totals"]["input_tokens"], report["totals"]["output_tokens"]))
print("  cost, micro-rupiah           {}".format(report["totals"]["cost_micros"]))
print()

per_order = []
for order in order_rows:
    task = order.get("task_id", "")
    row = by_task.get(task)
    if row is None:
        continue
    per_order.append(row)

if len(per_order) != len(order_rows):
    raise SystemExit("only {} of {} orders have a task row; the join must cover every order".format(
        len(per_order), len(order_rows)))

for row in sorted(per_order, key=lambda item: item["total_tokens"]):
    print("  {:<10} calls={:<3} tokens={:<6} cost_micros={}".format(
        row["task_status"][:10], row["calls"], row["total_tokens"], row["cost_micros"]))

tokens = [row["total_tokens"] for row in per_order]
calls = [row["calls"] for row in per_order]
costs = [row["cost_micros"] for row in per_order]

print()
print("  per order: calls {}..{} (mode {}), tokens {}..{} (median {:.0f}), cost median {}".format(
    min(calls), max(calls), statistics.mode(calls),
    min(tokens), max(tokens), statistics.median(tokens),
    statistics.median(costs)))
print("  total for {} orders: {} tokens, {} micro-rupiah".format(
    count, sum(tokens), sum(costs)))
print("  wall clock: {}s for {} orders, {:.1f}s per order end to end".format(
    seconds, count, seconds / count))

# The claim the numbers are for: twenty orders cost twenty orders. A flow that accumulated state per
# task — a growing history, a memory written every round — would show up here as a rising trend, and
# that is the thing a package price cannot survive.
first, last = tokens[: len(tokens) // 2], tokens[len(tokens) // 2 :]
if statistics.mean(last) > statistics.mean(first) * 1.5:
    raise SystemExit(
        "the cost per order grows with the number of orders ({} -> {})".format(
            statistics.mean(first), statistics.mean(last)))

print("  the second half costs {:.0f}% of the first half, so nothing accumulates per order".format(
    100 * statistics.mean(last) / statistics.mean(first)))
PY

echo
echo "MEASURE OK"
echo "  the numbers above come from usage_ledger through the product's own report;"
echo "  the rupiah column is the platform's price table applied to a stub's tokens, so"
echo "  the shape is what to read here, and staging with a real provider is what prices it."
