#!/usr/bin/env python3
"""Print the rounds a task recorded, one per line.

Reads the steps endpoint's response on stdin. It is a separate file rather than
an inline one-liner because the smoke script is shell: keeping the JSON handling
in Python here is what keeps the shell readable and the quoting honest.
"""

import json
import sys


def main() -> int:
    payload = json.load(sys.stdin)
    for step in payload["data"]:
        tool = step.get("tool_name", "")
        tokens = ""
        if step.get("input_tokens") or step.get("output_tokens"):
            tokens = f"  ({step.get('input_tokens', 0)} in / {step.get('output_tokens', 0)} out)"
        print(f"  {step['seq']}. {step['kind']:<12} {tool}{tokens}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
