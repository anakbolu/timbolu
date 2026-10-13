#!/usr/bin/env bash
#
# The numbers Gerbang 1 is decided on, read from the database in one go.
#
# E14.6 asks for a decision backed by data: drafts approved without a revision, cost per task, tasks
# failing per integration, and the token spend behind all of it. Those facts live in `tasks`,
# `task_steps`, `drafts`, `draft_revisions`, `orders`, and `usage_ledger` — and a person deciding
# whether to open the next phase should not be writing four queries by hand each time, because the
# fourth version of a hand-written query is a different number from the first.
#
# It reads the database rather than the API on purpose. The API's report answers "what did this
# workspace spend"; this answers "is the flow good enough to build on", which is a question about the
# rows themselves — including the ones a workspace filter would hide.
#
# Usage:
#   scripts/gate-report.sh                       # the compose Postgres
#   DATABASE_URL=postgres://… scripts/gate-report.sh
#
# The thresholds printed beside each number are the ones `docs/e2e-mvp.md` records. They are printed
# rather than asserted: this is a report for a decision, not a gate that passes or fails.
set -euo pipefail

cd "$(dirname "$0")/.."

DB="${DATABASE_URL:-postgres://employeebot:employeebot_secret@localhost:5432/employeebot?sslmode=disable}"

# Inside the compose network the database is `postgres`, not `localhost`, so the default above is
# rewritten for the container rather than asking the reader to know which address applies.
CONTAINER_DB="$(printf '%s' "$DB" | sed -E 's#@(localhost|127\.0\.0\.1):#@postgres:#')"

if docker exec bolu-postgres pg_isready -U employeebot >/dev/null 2>&1; then
    psql() { docker exec -i bolu-postgres psql -U employeebot -d employeebot "$@"; }
    where="the compose Postgres"
else
    psql() { command psql "$CONTAINER_DB" "$@"; }
    where="$CONTAINER_DB"
fi

printf '\n==> Gerbang 1: bukti dari %s\n' "$where"

echo
echo "Tugas"
psql -q -t -A -F'|' -c "
SELECT
    format('  total %s  ·  selesai %s  ·  gagal %s  ·  dibatalkan %s  ·  masih hidup %s',
           count(*),
           count(*) FILTER (WHERE status = 'succeeded'),
           count(*) FILTER (WHERE status = 'failed'),
           count(*) FILTER (WHERE status = 'canceled'),
           count(*) FILTER (WHERE status IN ('queued','running','waiting_approval')))
FROM tasks;"

echo
echo "Tugas gagal per integrasi  (ambang: ≤ 5% dari panggilan alatnya)"
psql -q -t -A -F'|' -c "
SELECT format('  %-10s %s panggilan  ·  %s gagal  ·  %s%%  ->  %s',
              split_part(tool_name, '.', 1),
              count(*),
              count(*) FILTER (WHERE output ? 'error'),
              round(100.0 * count(*) FILTER (WHERE output ? 'error') / count(*), 1),
              CASE WHEN 100.0 * count(*) FILTER (WHERE output ? 'error') / count(*) <= 5 THEN 'lolos' ELSE 'di atas ambang' END)
FROM task_steps
WHERE kind = 'tool_result' AND tool_name <> ''
GROUP BY split_part(tool_name, '.', 1)
ORDER BY count(*) DESC;"

echo
echo "Draf disetujui tanpa revisi  (ambang: ≥ 70% dari yang sudah diputuskan)"
psql -q -t -A -F'|' -c "
SELECT format('  diputuskan %s  ·  bersih %s  ·  revisi %s  ·  %s%%  ->  %s',
              count(*),
              count(*) FILTER (WHERE status IN ('approved','sent') AND revision = 0),
              coalesce(sum((SELECT count(*) FROM draft_revisions r WHERE r.draft_id = d.id)), 0),
              CASE WHEN count(*) = 0 THEN 0
                   ELSE round(100.0 * count(*) FILTER (WHERE status IN ('approved','sent') AND revision = 0) / count(*), 1) END,
              CASE WHEN count(*) = 0 THEN 'belum ada yang diputuskan'
                   WHEN 100.0 * count(*) FILTER (WHERE status IN ('approved','sent') AND revision = 0) / count(*) >= 70 THEN 'lolos'
                   ELSE 'di bawah ambang' END)
FROM drafts d
WHERE status IN ('approved','sent','revise','canceled');"

echo
echo "Biaya & token"
psql -q -t -A -F'|' -c "
SELECT format('  ledger: %s panggilan (%s agen, %s lain)  ·  %s token masuk  ·  %s token keluar  ·  %s micro-rupiah',
              count(*),
              count(*) FILTER (WHERE purpose = 'agent'),
              count(*) FILTER (WHERE purpose <> 'agent'),
              coalesce(sum(input_tokens), 0),
              coalesce(sum(output_tokens), 0),
              coalesce(sum(cost_micros), 0))
FROM usage_ledger;"

psql -q -t -A -F'|' -c "
SELECT format('  per tugas rata-rata: %s panggilan  ·  %s token  ·  %s micro-rupiah',
              coalesce(round(avg(step_count), 1), 0),
              coalesce(round(avg(input_tokens + output_tokens)), 0),
              coalesce(round(avg(cost_micros)), 0))
FROM tasks WHERE status = 'succeeded';"

psql -q -t -A -F'|' -c "
SELECT format('  per pesanan tercatat: %s pesanan  ·  %s token  ·  %s micro-rupiah',
              count(o.id),
              coalesce(sum(t.input_tokens + t.output_tokens), 0),
              coalesce(sum(t.cost_micros), 0))
FROM orders o JOIN tasks t ON t.id = o.task_id;"

echo
echo "Pesanan"
psql -q -t -A -F'|' -c "
SELECT format('  %s pesanan  ·  %s pelanggan  ·  total %s rupiah',
              (SELECT count(*) FROM orders),
              (SELECT count(*) FROM customers),
              -- sum(bigint) is numeric in Postgres, so the division has to be cast back: without it a
              -- rupiah total prints with a tail of zeros and reads as a float nobody meant to store.
              (SELECT coalesce((sum(total_micros) / 1000000)::bigint, 0) FROM orders));"

echo
echo "Catatan: angka rupiah di atas hanya berarti bila deployment-nya memakai provider sungguhan."
echo "Dengan stub, kolom biayanya nyaris nol — yang bisa dibaca adalah bentuk alurnya (panggilan"
echo "dan token per tugas), bukan harganya. Lihat docs/e2e-mvp.md."
echo
