#!/usr/bin/env python3
"""Reduce `npm audit --json` to advisories nobody has reviewed yet.

Reads the audit JSON on stdin and the allowlist path as argv[1]. Prints one
line per unreviewed advisory and exits 1 if there are any, 0 if there are none.

A separate file rather than a shell one-liner because the previous inline
version had a quoting bug that made Python exit non-zero before printing
anything -- and the shell read "no output" as "nothing wrong". A check that
reports clean when it is broken is worse than no check.
"""

import json
import os
import re
import sys


def main() -> int:
    raw = sys.stdin.read()
    if not raw.strip():
        print("npm audit produced no output")
        return 1
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"could not parse npm audit output: {e}")
        return 1

    allowed = set()
    allow_path = sys.argv[1] if len(sys.argv) > 1 else ""
    if allow_path and os.path.exists(allow_path):
        with open(allow_path) as f:
            for line in f:
                line = line.split("#", 1)[0].strip()
                if line:
                    allowed.add(line)

    # Report root advisories rather than every package that depends on one: a
    # single unfixable transitive dependency otherwise shows up as a dozen
    # findings that all need the same decision.
    seen = set()
    rows = []
    for name, vuln in sorted(data.get("vulnerabilities", {}).items()):
        for via in vuln.get("via", []):
            if not isinstance(via, dict):
                continue
            ghsa = re.search(r"GHSA-[\w-]+", via.get("url", ""))
            ident = ghsa.group(0) if ghsa else "{}:{}".format(name, via.get("title", ""))
            if ident in seen or ident in allowed:
                continue
            seen.add(ident)
            rows.append(
                "{}\t{}\t{}\t{}".format(
                    via.get("severity", "?"), ident, name, via.get("title", "")
                )
            )

    for row in rows:
        print(row)
    return 1 if rows else 0


if __name__ == "__main__":
    sys.exit(main())
