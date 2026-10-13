#!/usr/bin/env bash
#
# A stack you can click through, with a workspace that already has a catalogue and a Bolu.
#
# `make up` gives you the product running; it does not give you a *provider*, so the first thing a task
# does is fail with "no provider is configured". This starts the model stub on the host, points the two
# backend processes that call a model at it, and seeds a workspace the way a person would through the
# API — register, verify, onboard, add a Bolu from a template, fill the catalogue.
#
# It is deliberately idempotent: running it again reuses the same account, so a password written down
# once keeps working through many restarts.
#
# Usage: scripts/demo.sh
set -euo pipefail

# The module root is found from the script's own location, so it does not matter what directory it is
# invoked from — `make` runs it from the repository root, and a person may run it from anywhere.
BACKEND="$(cd "$(dirname "$0")/.." && pwd)"
ROOT="$(cd "${BACKEND}/../.." && pwd)"

cd "$ROOT"

API_PORT="${API_PORT:-8080}"
DEMO_STUB_PORT="${DEMO_STUB_PORT:-11441}"
DEMO_EMAIL="${DEMO_EMAIL:-demo@bolu.local}"
DEMO_PASSWORD="${DEMO_PASSWORD:-Rahasia123!}"
DEMO_WORKSPACE="${DEMO_WORKSPACE:-Toko Demo}"
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.demo.yml)

log() { printf '\n==> %s\n' "$*"; }
fail() { printf '\n!! %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || fail "docker is required"
docker info >/dev/null 2>&1 || fail "the docker daemon is not reachable"

# --------------------------------------------------------------- the stub model

# The stub is on the host, so the containers reach it through `host.docker.internal` — which is what the
# override file points them at. It is restarted every time, because a stub left over from a previous run
# holds the port and serves whatever mode it was started with.
log "starting the stub model provider on :${DEMO_STUB_PORT}"
if lsof -ti ":${DEMO_STUB_PORT}" >/dev/null 2>&1; then
    lsof -ti ":${DEMO_STUB_PORT}" | xargs -r kill 2>/dev/null || true
    sleep 1
fi

# The catalogue the seeds below create: the stub has to know the names to look them up by.
STUB_MODE=demo \
    STUB_PRODUCTS="Kopi Arabika,Kopi Susu,Gula Aren,Teh Melati,Roti Sisir" \
    nohup python3 "${BACKEND}/scripts/order-stub.py" "$DEMO_STUB_PORT" > /tmp/bolu-demo-stub.log 2>&1 &
STUB_PID=$!

for _ in $(seq 1 20); do
    curl -fsS -X POST "http://localhost:${DEMO_STUB_PORT}/v1/chat/completions" \
        -H 'Content-Type: application/json' -d '{"messages":[]}' >/dev/null 2>&1 && break
    kill -0 "$STUB_PID" 2>/dev/null || fail "the stub exited on startup; see /tmp/bolu-demo-stub.log"
    sleep 0.5
done
printf '    stub pid %s, mode demo\n' "$STUB_PID"

# ---------------------------------------------------------------- the mailbox stub
#
# The invoice needs a channel that leaves the building. Rather than leave the demo unable to reach the
# draft, a stand-in for Google runs on the host and the connection flow is completed against it — the
# same flow a person would click, through the provider's consent screen, so what the deployment stores
# is a real connection with a real (stub) token.
GMAIL_PORT="${GMAIL_PORT:-11442}"

log "starting the mailbox stub on :${GMAIL_PORT}"
if lsof -ti ":${GMAIL_PORT}" >/dev/null 2>&1; then
    lsof -ti ":${GMAIL_PORT}" | xargs -r kill 2>/dev/null || true
    sleep 1
fi

DEMO_SENT_LOG=/tmp/bolu-demo-gmail-sent.jsonl
rm -f "$DEMO_SENT_LOG"
SMOKE_SENT_LOG="$DEMO_SENT_LOG" \
    nohup python3 "${BACKEND}/scripts/gmail-stub.py" "$GMAIL_PORT" > /tmp/bolu-demo-gmail.log 2>&1 &
GMAIL_PID=$!

for _ in $(seq 1 20); do
    curl -fsS "http://localhost:${GMAIL_PORT}/users/me/profile" >/dev/null 2>&1 && break
    kill -0 "$GMAIL_PID" 2>/dev/null || fail "the mailbox stub exited; see /tmp/bolu-demo-gmail.log"
    sleep 0.5
done
printf '    mailbox stub pid %s\n' "$GMAIL_PID"

# --------------------------------------------------------------------- the stack

log "starting the stack with the demo provider"
# The Gmail credentials point the adapter at the stub on the host. They are what makes the app offer the
# channel at all: without them the deployment has no mailbox and the invoice has nowhere to go.
DEMO_STUB_PORT="$DEMO_STUB_PORT" \
    GMAIL_CLIENT_ID=stub-client \
    GMAIL_CLIENT_SECRET=stub-secret \
    GMAIL_API_BASE="http://host.docker.internal:${GMAIL_PORT}" \
    GMAIL_AUTH_ENDPOINT="http://localhost:${GMAIL_PORT}/auth" \
    GMAIL_TOKEN_ENDPOINT="http://host.docker.internal:${GMAIL_PORT}/token" \
    "${COMPOSE[@]}" up -d --no-build api agent-worker integration-worker >/dev/null

for _ in $(seq 1 90); do
    curl -fsS "http://localhost:${API_PORT}/health" >/dev/null 2>&1 && break
    sleep 1
done
curl -fsS "http://localhost:${API_PORT}/health" >/dev/null 2>&1 ||
    fail "the API never became ready; see 'docker compose logs api'"

wait_ready() {
    local deadline=$((SECONDS + 60))
    while (( SECONDS < deadline )); do
        if curl -fsS "http://localhost:${API_PORT}/ready" | grep -q '"status":"ok"'; then
            return 0
        fi
        sleep 1
    done

    return 1
}
wait_ready || fail "the API is up but not ready; see 'docker compose logs api'"

log "creating the demo workspace"
EMAIL="$DEMO_EMAIL" PASSWORD="$DEMO_PASSWORD" WORKSPACE="$DEMO_WORKSPACE" \
    API="http://localhost:${API_PORT}" bash -s <<'SEED'
set -euo pipefail

api() { printf '%s/api' "$API"; }

# The account is created once. A second register answers 409, which is expected and not an error: the
# sections below are what make this idempotent.
register() {
    curl -sS -o /dev/null -w '%{http_code}' -X POST "$(api)/auth/register" \
        -H 'Content-Type: application/json' \
        -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}"
}

code=$(register)
if [[ "$code" == "201" || "$code" == "200" ]]; then
    # The verification link goes to the API's log, because MAIL_DRIVER is `log` locally. That is the
    # same path the smoke uses, and the same one a person would take by opening the link.
    token=""
    for _ in $(seq 1 20); do
        token=$(docker logs bolu-api 2>&1 | grep -o 'token=[A-Za-z0-9_-]*' | tail -1 | cut -d= -f2 || true)
        [[ -n "$token" ]] && break
        sleep 1
    done
    [[ -n "$token" ]] || { echo "no verification token in the API log" >&2; exit 1; }
    curl -fsS -X POST "$(api)/auth/verify-email" -H 'Content-Type: application/json' \
        -d "{\"token\":\"${token}\"}" >/dev/null
    printf '    account created and verified: %s\n' "$EMAIL"
else
    printf '    account already exists: %s\n' "$EMAIL"
fi

access=$(curl -fsS -X POST "$(api)/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])')

workspaces=$(curl -fsS "$(api)/workspaces" -H "Authorization: Bearer ${access}")
ws=$(echo "$workspaces" | python3 -c '
import json, sys
data = json.load(sys.stdin)["data"]
rows = data if isinstance(data, list) else data.get("workspaces", [])
print(next((w["id"] for w in rows if w["name"] == "'"$WORKSPACE"'"), ""))')

if [[ -z "$ws" ]]; then
    ws=$(curl -fsS -X POST "$(api)/workspaces/onboard" -H "Authorization: Bearer ${access}" \
        -H 'Content-Type: application/json' \
        -d "{\"name\":\"${WORKSPACE}\",\"business_field\":\"F&B\",\"timezone\":\"Asia/Jakarta\"}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["workspace"]["id"])')
    printf '    workspace created: %s\n' "$WORKSPACE"
else
    printf '    workspace already there: %s\n' "$WORKSPACE"
fi

auth=(-H "Authorization: Bearer ${access}" -H "X-Workspace-Id: ${ws}")

# A Bolu from the sales template, which is the one that carries the order-to-invoice flow.
agents=$(curl -fsS "$(api)/agents" "${auth[@]}" |
    python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')
if [[ "$agents" == "0" ]]; then
    team=$(curl -fsS "$(api)/teams" "${auth[@]}" |
        python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print([t for t in d if t["kind"]=="bolu"][0]["id"])')
    curl -fsS -X POST "$(api)/agents" "${auth[@]}" -H 'Content-Type: application/json' \
        -d "{\"team_id\":\"${team}\",\"name\":\"Oren\",\"template_key\":\"oren\"}" >/dev/null
    printf '    Bolu created: Oren (template oren)\n'
else
    printf '    Bolu already there: %s\n' "$agents"
fi

# The catalogue. A price is what every later quote is built on, so it is typed in the way a person
# types it on the screen — through the API the screen itself calls.
products=$(curl -fsS "$(api)/orders/products" "${auth[@]}" |
    python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')
if [[ "$products" == "0" ]]; then
    for product in \
        '{"sku":"KOPI-1","name":"Kopi Arabika","price_idr":15000,"unit":"kg"}' \
        '{"sku":"SUSU-1","name":"Kopi Susu","price_idr":20000,"unit":"pcs"}' \
        '{"sku":"GULA-1","name":"Gula Aren","price_idr":18000,"unit":"kg"}' \
        '{"sku":"TEH-1","name":"Teh Melati","price_idr":12000,"unit":"box"}' \
        '{"sku":"ROTI-1","name":"Roti Sisir","price_idr":9000,"unit":"pcs"}'; do
        curl -fsS -X POST "$(api)/orders/products" "${auth[@]}" \
            -H 'Content-Type: application/json' -d "$product" >/dev/null
    done
    printf '    catalogue filled: 5 products\n'
else
    printf '    catalogue already has %s products\n' "$products"
fi

# The mailbox, connected the way a person connects it: start the flow, follow the provider's consent
# screen, and let the callback store the token. Seeding the row directly would leave `status = connected`
# with no credential, which is exactly the state the executor refuses.
connected=$(curl -fsS "$(api)/integrations" "${auth[@]}" |
    python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(next((c["integration_id"] for c in d if c.get("app")=="gmail" and c["status"]=="connected"), ""))')
if [[ -z "$connected" ]]; then
    consent=$(curl -sS "$(api)/integrations/gmail/connect?return_to=/dashboard/integrations" "${auth[@]}" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["url"])')
    callback=$(curl -sS -o /dev/null -w '%{redirect_url}' "$consent")
    if [[ -n "$callback" ]]; then
        curl -sS "$callback" >/dev/null || true
        connected=$(curl -fsS "$(api)/integrations" "${auth[@]}" |
            python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(next((c["integration_id"] for c in d if c.get("app")=="gmail" and c["status"]=="connected"), ""))')
    fi
fi

if [[ -n "$connected" ]]; then
    agent=$(curl -fsS "$(api)/agents" "${auth[@]}" |
        python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d[0]["id"] if d else "")')
    if [[ -n "$agent" ]]; then
        curl -fsS -X PUT "$(api)/agents/${agent}/grants/${connected}" "${auth[@]}" \
            -H 'Content-Type: application/json' -d '{"permission":"read_write"}' >/dev/null
        printf '    mailbox connected and granted to the Bolu\n'
    fi
else
    printf '    mailbox NOT connected: the invoice will reach the draft and go no further\n'
fi

# What a person would put in the workspace memory: the name, the greeting, and the hours. The template's
# persona reads them from here, which is what makes the Bolu answer as this business.
memories=$(curl -fsS "$(api)/memories" "${auth[@]}" |
    python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))')
if [[ "$memories" == "0" ]]; then
    for fact in \
        'Nama usaha: Toko Demo. Sapaan pelanggan: "Bu" atau "Pak" diikuti namanya. Jam buka 08.00-17.00 WIB.' \
        'Format tagihan: sebut kode pesanan, daftar barisnya, dan totalnya, lalu minta konfirmasi.'; do
        curl -fsS -X POST "$(api)/memories" "${auth[@]}" -H 'Content-Type: application/json' \
            -d "$(python3 -c 'import json,sys; print(json.dumps({"content": sys.argv[1]}))' "$fact")" >/dev/null
    done
    printf '    main memory written: 2 facts\n'
fi
SEED

cat <<INFO

============================================================
  Stack siap dipakai.

  Dasbor   http://localhost:3000
  Login    ${DEMO_EMAIL}
  Sandi    ${DEMO_PASSWORD}
  Workspace ${DEMO_WORKSPACE}

  Yang bisa dicoba, dan apa yang harus terlihat:

  1. Kantor / Tim Bolu -> ada satu Bolu bernama Oren (template penjualan).
  2. Integrasi -> sambungkan Gmail lewat tombolnya kalau ingin tagihan benar-benar
     "terkirim". Tanpa itu, drafnya tetap muncul sebagai kartu, dan menyetujuinya
     akan gagal dengan alasan "unknown tool: gmail.send" — yang memang perilaku
     yang diminta: tidak ada yang ditandai terkirim kalau tidak benar-benar keluar.
  3. Obrolan -> pilih Oren, lalu kirim pesan seperti:
        Saya mau pesan 2 kg Kopi Arabika ya. Nama saya Sari, nomor saya 628111222333.
     Oren akan mencari harga di katalog, lalu mencatat pesanannya.
  4. Kirim lagi:
        Tolong susun tagihannya ya.
     Muncul kartu draf dengan tombol Setujui / Minta revisi / Batalkan.
  5. Coba juga yang harus DITANYAKAN, bukan ditebak:
        Saya mau pesan Teh Melati ya.           (jumlahnya belum jelas: akan ditanya)
        Pesan Kopi Luwak dong.                  (tidak dijual: katalog yang menjawab,
                                                 beserta kandidat yang mirip)
  6. Dasbor -> lihat statistiknya; Pengaturan -> Data (kebijakan retensi & hapus);
     Pemakaian ada di sidebar untuk token dan biaya.

  Memeriksa sendiri:
    Setelah menekan Setujui pada kartu tagihan, pesan yang benar-benar keluar ada di
      ${DEMO_SENT_LOG}
    Kalau berkas itu kosong, tidak ada yang terkirim — dan itu memang janjinya.

  Menghentikan:
    kill ${STUB_PID} ${GMAIL_PID}         # stub model dan mailbox di host
    docker compose stop api agent-worker integration-worker

  Catatan: stack ini memakai provider stub, jadi jawaban Bolu mengikuti alur yang
  diprogram, bukan model sungguhan. Katalog, draf, persetujuan, dan seluruh sisanya
  adalah kode produksi.
============================================================
INFO
