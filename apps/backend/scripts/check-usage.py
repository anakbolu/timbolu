#!/usr/bin/env python3
"""Check the usage screen's reads against the rows the same run produced.

Usage: check-usage.py <report-json> <task-json> <csv> <attention-json> <temporal-ui-base>

The epic's verification is a reconciliation rather than a smoke test: the numbers a
dashboard shows and the rows a query returns are two views of one fact, and a
release is where they drift apart. So this compares the report — and the export,
and the attention list — against the task the smoke just ran.

What it does *not* do is assert an exact total. The database behind the run is
shared with whatever else is on the compose stack, so "the report equals this
run's tokens" would fail for a reason that has nothing to do with the code. What
it asserts is that the task's own figures are *inside* the report's, and that the
three reads agree with each other.
"""

import csv
import io
import json
import sys


def main():
    if len(sys.argv) != 6:
        raise SystemExit(__doc__)

    report = json.loads(sys.argv[1])["data"]
    task = json.loads(sys.argv[2])["data"]
    csv_body = sys.argv[3]
    attention = json.loads(sys.argv[4])["data"]
    ui_base = sys.argv[5].strip()

    # --- the totals cover the task the run just finished ------------------------------
    if report["totals"]["input_tokens"] < task["input_tokens"]:
        raise SystemExit(
            f"the report's input tokens ({report['totals']['input_tokens']}) are less than "
            f"the task's own ({task['input_tokens']})")

    if report["totals"]["calls"] < task["step_count"]:
        raise SystemExit(
            f"the report counts {report['totals']['calls']} calls, fewer than the task's "
            f"{task['step_count']} rounds")

    # --- the breakdowns agree with the total, and with each other ---------------------
    per_agent = sum(row["cost_micros"] for row in report["by_agent"])
    per_task = sum(row["cost_micros"] for row in report["by_task"])
    if per_agent > report["totals"]["cost_micros"]:
        raise SystemExit("the per-Bolu rows add up to more than the total")
    if per_task > report["totals"]["cost_micros"]:
        raise SystemExit("the per-task rows add up to more than the total")

    # --- the export is the same read -------------------------------------------------
    rows = list(csv.reader(io.StringIO(csv_body)))
    if not rows:
        raise SystemExit("the export is empty")

    header = rows[0]
    if header[0] != "bagian" or "biaya_mikro_rupiah" not in header:
        raise SystemExit(f"the export's header is not the report's schema: {header}")

    sections = {}
    for row in rows[1:]:
        if len(row) != len(header):
            raise SystemExit(f"a row carries {len(row)} columns and the header {len(header)}: {row}")
        sections[row[0]] = row

    for section in ("total", "per_bolu", "per_tugas"):
        if section not in sections:
            raise SystemExit(f"the export has no {section} row")

    exported = int(sections["total"][header.index("biaya_mikro_rupiah")])
    if exported != report["totals"]["cost_micros"]:
        raise SystemExit(
            f"the export says {exported} micro-rupiah and the screen {report['totals']['cost_micros']}")

    # --- the attention list carries the failure this run produced --------------------
    failed = [task_row for task_row in attention if task_row["task_id"] == task["task_id"]]
    if task["status"] == "failed" and not failed:
        raise SystemExit("the failed task is not on the attention list")
    if task["status"] == "failed" and failed[0]["attention"] != "failed":
        raise SystemExit(f"the failed task is listed as {failed[0]['attention']!r}")

    # The Temporal link is offered when the deployment named a UI and the task reached
    # the engine, and it must point at *this* workflow rather than at a plausible one.
    if failed and ui_base:
        link = failed[0].get("temporal_url", "")
        if not link:
            raise SystemExit("the deployment named a Temporal UI and the task has no link")
        if not link.startswith(ui_base.rstrip("/")):
            raise SystemExit(f"the link {link} does not start at the configured {ui_base}")
        if task["workflow_id"] not in link:
            raise SystemExit(f"the link {link} does not name the task's workflow")
        if task.get("temporal_run_id") and task["temporal_run_id"] not in link:
            raise SystemExit(f"the link {link} does not name the task's run")

    print(f"  usage agrees with the rows: {report['totals']['calls']} calls, "
          f"{report['totals']['total_tokens']} tokens, {report['totals']['cost_idr']:.2f} IDR, "
          f"{len(report['by_agent'])} Bolu, {len(report['by_task'])} tugas, "
          f"{len(attention)} perlu perhatian")


if __name__ == "__main__":
    main()
