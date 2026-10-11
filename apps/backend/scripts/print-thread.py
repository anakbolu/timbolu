#!/usr/bin/env python3
"""Print a conversation's messages, so the smoke shows where the answer landed.

Reads the messages endpoint's response on stdin.
"""

import json
import sys


def main() -> int:
    payload = json.load(sys.stdin)
    data = payload["data"]
    messages = data["messages"] if isinstance(data, dict) else data

    for message in messages:
        who = "user" if message.get("user_id") else "bolu"
        text = " ".join(
            block.get("markdown", "")
            for block in message.get("blocks", [])
            if block.get("type") == "text"
        )
        task = message.get("task_id") or ""
        suffix = f"  [task {task}]" if task else ""
        print(f"  {who}: {message['status']}: {text}{suffix}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
