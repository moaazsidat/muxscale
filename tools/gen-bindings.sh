#!/usr/bin/env bash
# Generates the Swift and Kotlin bindings the mobile apps import.
#
# The generated code is committed so a fresh clone builds without a Rust
# toolchain, and so a change to the FFI surface shows up as a reviewable diff
# rather than appearing silently at build time. CI regenerates and fails on a
# diff, the same rule the protobuf codegen follows.
set -euo pipefail

cd "$(dirname "$0")/../rust"
OUT="../app/modules/muxscale-core/generated"

cargo build --release --lib

LIB="target/release/libmuxscale_core.dylib"
[ -f "$LIB" ] || LIB="target/release/libmuxscale_core.so"
[ -f "$LIB" ] || { echo "no built library found; is the cdylib crate-type set?" >&2; exit 1; }

rm -rf "$OUT"
mkdir -p "$OUT/swift" "$OUT/kotlin"

cargo run --release --bin uniffi-bindgen -- generate \
  --library "$LIB" --language swift --out-dir "$OUT/swift" --no-format
cargo run --release --bin uniffi-bindgen -- generate \
  --library "$LIB" --language kotlin --out-dir "$OUT/kotlin" --no-format

echo "generated:"
find "$OUT" -type f | sed 's|^|  |'
