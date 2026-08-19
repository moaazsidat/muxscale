#!/usr/bin/env bash
# Run every test suite in the repo.
#
# Three toolchains, one entry point. The reason this exists rather than a note
# in the README saying "run go test, cargo test and npm test" is the protocol:
# the routing header is implemented three times and the fixtures that prove the
# implementations agree are only checked when all three suites run. A change
# that passes Go and breaks TypeScript is exactly the failure this is for.
#
# Every suite runs even if an earlier one fails -- a red Go suite should not
# hide a red Rust one -- and the exit status is 1 if any failed.
set -uo pipefail

cd "$(dirname "$0")/.."
status=0
failed=()

# A subshell rather than `env -C`: the -C flag is GNU coreutils and recent BSD
# only, and this has to run on whatever a contributor's macOS ships.
run() {
  local name=$1 dir=$2
  shift 2
  echo
  echo "=== $name ==="
  if (cd "$dir" && "$@"); then
    echo "  ok"
  else
    echo "  FAILED"
    failed+=("$name")
    status=1
  fi
}

run "Go" go go test ./...
run "Rust" rust cargo test --all-features
run "TypeScript (cloud)" cloud node --test 'test/*.test.ts'
run "TypeScript (app)" app node --test 'src/**/*.test.ts'

echo
if [ "$status" -eq 0 ]; then
  echo "All suites passed."
else
  echo "Failed: ${failed[*]}"
fi
exit "$status"
