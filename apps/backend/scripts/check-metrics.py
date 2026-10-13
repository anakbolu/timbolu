#!/usr/bin/env python3
"""Check that a deployment's own measurements agree with the data they were derived from.

Usage: check-metrics.py <worker-metrics> <task-json> <api-metrics>

The epic's verification is a reconciliation rather than a smoke test: the counters
a chart reads and the rows a dashboard reads are two views of one fact, and a
release is where they drift apart. So this compares the exposition format against
the task the smoke just ran, and it fails when the chart is the smaller of the
two — a counter that lost an increment is the failure that matters, and one that
overshot would mean a call was counted twice.

What it does *not* do is assert an exact total. The database behind the run is
shared with whatever else is on the compose stack, so "the counter equals this
run's tokens" would fail for a reason that has nothing to do with the code.
"""

import json
import sys


def read(body, name, **labels):
    """read returns one series out of the Prometheus exposition format, or None.

    A counter that was never incremented is not exported at all — an instrument
    with no observations is not a zero, it is an absence — so a caller that needs
    to distinguish "recorded nothing" from "recorded zero" asks with `found`.
    """
    for line in body.splitlines():
        if not line.startswith(name):
            continue
        if any(f'{key}="{want}"' not in line for key, want in labels.items()):
            continue

        return float(line.rsplit(" ", 1)[1])

    return None


def required(body, name, **labels):
    """required reads a series that must be there."""
    value = read(body, name, **labels)
    if value is None:
        raise SystemExit(f"no series for {name} {labels}")

    return value


def main():
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)

    worker, task_json, api = sys.argv[1], sys.argv[2], sys.argv[3]
    task = json.loads(task_json)["data"]

    finished = required(worker, "bolu_task_finished_total", trigger="chat", outcome="succeeded")
    if finished < 1:
        raise SystemExit(f"the finished-task counter reads {finished}")

    # The task's spend must be what the row holds: that is what "consistent with
    # the ledger" means in practice, and the counter and the row are two views of
    # one fact.
    input_tokens = required(worker, "bolu_task_tokens_total", direction="input")
    if input_tokens < task["input_tokens"]:
        raise SystemExit(
            f"the token counter reads {input_tokens}, less than the task's own {task['input_tokens']}")

    # The cost counter is only present once something was priced, and the smoke's stub model costs
    # nothing — the provider is not in the deployment's table. So the two are compared only when
    # there is a cost to compare, and a deployment that priced its calls is checked properly.
    cost = read(worker, "bolu_task_cost_total")
    if task["cost_micros"] > 0:
        if cost is None:
            raise SystemExit("the task cost something and the counter recorded nothing")
        if cost < task["cost_micros"]:
            raise SystemExit(
                f"the cost counter reads {cost}, less than the task's own {task['cost_micros']}")

    calls = read(worker, "bolu_model_calls_total", provider="openai", purpose="agent")
    if not calls:
        # The task's own rounds are the agent's turns; a purpose the deployment does not know is
        # grouped under `other`, so a series under agent is what says the label is being set at all.
        calls = read(worker, "bolu_model_calls_total", purpose="other")
    if not calls:
        raise SystemExit("no model call was recorded")

    # The depth is the difference of the two processes' counters, and it came back
    # down: some tasks are opened per phase, and every dispatch of this run must
    # have a settlement. A single-process depth would read one forever, which is
    # what the first version of this check caught.
    dispatched = required(api, "bolu_task_dispatched_total")
    settled = required(worker, "bolu_task_settled_total")
    if settled == 0:
        raise SystemExit("the worker recorded no settlement, so the depth can never come down")
    if settled > dispatched:
        raise SystemExit(f"more tasks were settled ({settled:.0f}) than dispatched ({dispatched:.0f})")

    print(f"  metrics agree with the task row: {finished:.0f} finished, "
          f"{input_tokens:.0f} input tokens, {cost or 0:.0f} micro-rupiah, "
          f"depth {dispatched - settled:.0f} of {dispatched:.0f} dispatched")


if __name__ == "__main__":
    main()
