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
if (cd rust && cargo clippy --all-targets -- -D warnings 2>&1 | tail -5); then
  echo "  clean"
else
  echo "  clippy warnings"
  status=1
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
