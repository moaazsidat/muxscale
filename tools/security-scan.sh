#!/usr/bin/env bash
# Dependency vulnerability scan across both toolchains.
#
# Part of M5's security work, and the part that can be run rather than
# commissioned. A third-party audit reviews design and implementation; this
# catches the far more common case of a dependency picking up a known CVE
# between releases.
#
# Exit codes: 0 clean, 1 something needs attention.
set -uo pipefail

cd "$(dirname "$0")/.."
status=0

# `go install` and `cargo install` put binaries in these directories, which are
# frequently not on PATH -- notably under CI and non-login shells. Looking there
# avoids telling someone to install a tool they already have.
PATH="$PATH:$(go env GOPATH 2>/dev/null || echo "$HOME/go")/bin:${CARGO_HOME:-$HOME/.cargo}/bin"
export PATH

echo "=== Go: govulncheck ==="
if ! command -v govulncheck >/dev/null 2>&1; then
  echo "  govulncheck not installed:"
  echo "    go install golang.org/x/vuln/cmd/govulncheck@latest"
  status=1
else
  # govulncheck distinguishes vulnerabilities your code actually calls from
  # ones merely present in the module graph. Only the former should fail a
  # build; the latter are worth knowing about but are not exploitable here.
  if (cd go && govulncheck ./...); then
    echo "  no reachable vulnerabilities"
  else
    echo "  REACHABLE VULNERABILITY -- fix before shipping"
    status=1
  fi
fi

echo
echo "=== Rust: cargo audit ==="
if ! command -v cargo-audit >/dev/null 2>&1; then
  echo "  cargo-audit not installed:"
  echo "    cargo install cargo-audit --locked"
  status=1
else
  if (cd rust && cargo audit); then
    echo "  no advisories"
  else
    echo "  ADVISORY FOUND -- fix before shipping"
    status=1
  fi
fi

echo
echo "=== Go: vet ==="
if (cd go && go vet ./...); then
  echo "  clean"
else
  status=1
fi

echo
echo "=== Rust: clippy ==="
# Output goes to a file rather than through `| tail`, because piping makes the
# `if` test tail's exit status instead of clippy's -- which reported "clean"
# unconditionally and would have let a -D warnings failure through unnoticed.
clippy_log=$(mktemp)
if (cd rust && cargo clippy --all-targets --all-features -- -D warnings) \
     >"$clippy_log" 2>&1; then
  echo "  clean"
else
  echo "  CLIPPY FAILED:"
  tail -30 "$clippy_log" | sed 's|^|    |'
  status=1
fi
rm -f "$clippy_log"

echo
echo "=== npm: audit (app) ==="
# The app has by far the largest dependency graph in the repo, and until it
# existed there was nothing here to scan. Only production dependencies are
# considered: a vulnerability in a test runner is not a vulnerability in a
# shipped app.
#
# Advisories that are genuinely unreachable are listed in
# tools/npm-audit-allow.txt with a reason and a date, rather than the threshold
# being lowered or the whole scan skipped -- the same rule the secret sweep
# follows with its `notasecret` markers. An advisory not on that list fails.
if [ ! -d app/node_modules ]; then
  echo "  skipped: app/node_modules is absent (run npm install in app/)"
else
  allow=tools/npm-audit-allow.txt
  audit_json=$(cd app && npm audit --omit=dev --json 2>/dev/null || true)

  # The reviewer exits 1 when it finds something *or* when it cannot make sense
  # of the input, so a broken check fails loudly instead of reporting clean.
  if unexpected=$(printf '%s' "$audit_json" | python3 tools/npm-audit-review.py "$allow"); then
    echo "  no unreviewed advisories"
  else
    echo "  UNREVIEWED ADVISORY:"
    printf '%s\n' "$unexpected" | sed 's|^|    |'
    echo "  Fix it, or add the GHSA id to $allow with a reason if it is unreachable."
    status=1
  fi
fi

echo
echo "=== secrets: nothing credential-shaped in tracked files ==="
# A narrow, high-signal sweep. Broad entropy scanning belongs in a dedicated
# tool; this exists to catch the specific mistake of committing a real key while
# wiring up APNs or Tailscale.
#
# Tests legitimately need credential-shaped strings as canaries -- proving no
# plaintext reaches the relay requires something recognisable to look for. Those
# lines carry a `notasecret` marker rather than the check being weakened or
# whole files excluded, so a real key committed to a test file is still caught.
hits=$(git grep -nIE \
  -e 'BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY' \
  -e 'sk-ant-[A-Za-z0-9]{20,}' \
  -e 'tskey-auth-[A-Za-z0-9]{10,}' \
  -e 'AKIA[0-9A-Z]{16}' \
  -- . ':(exclude)tools/security-scan.sh' 2>/dev/null | grep -v 'notasecret' || true)

if [ -n "$hits" ]; then
  echo "  SOMETHING CREDENTIAL-SHAPED IS COMMITTED:"
  echo "$hits" | sed 's|^|    |'
  echo "  If it is a deliberate test canary, add a 'notasecret' comment on the line."
  status=1
else
  echo "  clean"
fi

echo
if [ "$status" -eq 0 ]; then
  echo "All scans clean."
else
  echo "Scans found something. See above."
fi
exit "$status"
