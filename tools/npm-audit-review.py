#!/usr/bin/env python3
"""Reduce an npm/pnpm audit report to advisories nobody has reviewed yet.

Reads the audit JSON on stdin and the allowlist path as argv[1]. Prints one
line per unreviewed advisory and exits 1 if there are any, 0 if there are none.

Two report shapes exist and both are handled:

- npm 7+ writes {"vulnerabilities": {package: {via: [...]}}}, where a package
  appears once per path that reaches it.
- pnpm (and npm 6) write {"advisories": {id: {...}}}, one entry per advisory.

Anything else exits 1 rather than reporting nothing found. That distinction is
the whole point of this file: an earlier inline version died on a SyntaxError,
printed nothing, and the shell read the silence as "clean". A check that passes
when it is broken is worse than no check, and switching package managers is
exactly the sort of change that would otherwise trip it.
"""

import json
import os
import re
import sys

GHSA = re.compile(r"GHSA-[\w-]+")


def load_allowlist(path: str) -> set[str]:
    allowed: set[str] = set()
    if path and os.path.exists(path):
        with open(path) as f:
            for line in f:
                line = line.split("#", 1)[0].strip()
                if line:
                    allowed.add(line)
    return allowed


def identify(text: str, fallback: str) -> str:
    """The GHSA id if there is one, so the allowlist is package-manager agnostic."""
    found = GHSA.search(text or "")
    return found.group(0) if found else fallback


def from_npm(data: dict) -> list[tuple[str, str, str, str]]:
    """npm 7+: one entry per affected package, each listing the advisories via it."""
    rows = []
    for name, vuln in sorted(data.get("vulnerabilities", {}).items()):
        for via in vuln.get("via", []):
            # A string `via` means "vulnerable because of another package", not
            # an advisory of its own; the advisory is reported under that one.
            if not isinstance(via, dict):
                continue
            title = via.get("title", "")
            rows.append(
                (
                    via.get("severity", "?"),
                    identify(via.get("url", ""), f"{name}:{title}"),
                    name,
                    title,
                )
            )
    return rows


def from_pnpm(data: dict) -> list[tuple[str, str, str, str]]:
    """pnpm and npm 6: one entry per advisory, keyed by its numeric id."""
    rows = []
    for key, adv in sorted(data.get("advisories", {}).items()):
        title = adv.get("title", "")
        name = adv.get("module_name", "?")
        ident = identify(
            adv.get("github_advisory_id") or adv.get("url") or adv.get("references", ""),
            f"{name}:{title}",
        )
        rows.append((adv.get("severity", "?"), ident, name, title))
    return rows


def main() -> int:
    raw = sys.stdin.read()
    if not raw.strip():
        print("the audit produced no output")
        return 1
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"could not parse the audit output: {e}")
        return 1
    if not isinstance(data, dict):
        print("the audit output was not an object")
        return 1

    if "vulnerabilities" in data:
        rows = from_npm(data)
    elif "advisories" in data:
        rows = from_pnpm(data)
    else:
        # Neither shape. Refuse rather than conclude there is nothing wrong: a
        # report this does not understand has not been reviewed.
        print(
            "unrecognised audit format (no `vulnerabilities` or `advisories` key); "
            f"top-level keys were: {sorted(data)}"
        )
        return 1

    allowed = load_allowlist(sys.argv[1] if len(sys.argv) > 1 else "")

    # Report each advisory once. A single unfixable transitive dependency
    # otherwise shows up as a dozen findings that all need the same decision.
    seen: set[str] = set()
    out = []
    for severity, ident, name, title in rows:
        if ident in seen or ident in allowed:
            continue
        seen.add(ident)
        out.append(f"{severity}\t{ident}\t{name}\t{title}")

    for line in out:
        print(line)
    return 1 if out else 0


if __name__ == "__main__":
    sys.exit(main())
