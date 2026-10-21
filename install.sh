#!/usr/bin/env sh
# muxscale installer.
#
#   curl -fsSL https://muxscale.com/install.sh | sh
#
# Deliberately POSIX sh, not bash: this runs on whatever the user has, and
# `curl | bash` on a machine without bash is an avoidable first impression.
#
# Rules this script holds to, because piping a script from the internet into a
# shell is a thing people are right to be wary of:
#
#   - No sudo, ever. Installs under $HOME.
#   - Verifies a signature before running anything, and says so.
#   - Starts nothing, modifies no shell profile, installs no service. Starting
#     muxscale is `muxscale up`, a step the user takes.
#   - Idempotent: re-running upgrades in place and keeps config and state.
#   - Fails loudly and early rather than half-installing.
set -eu

MUXSCALE_VERSION="${MUXSCALE_VERSION:-stable}"
MUXSCALE_INSTALL_DIR="${MUXSCALE_INSTALL_DIR:-$HOME/.local/bin}"
MUXSCALE_BASE_URL="${MUXSCALE_BASE_URL:-https://dl.muxscale.com}"
MUXSCALE_NO_VERIFY="${MUXSCALE_NO_VERIFY:-}"

# The release signing key, baked in so signature verification does not depend
# on the user already having a keyring or on fetching the key over the same
# channel as the artifact.
MUXSCALE_MINISIGN_PUBKEY="${MUXSCALE_MINISIGN_PUBKEY:-RWRnXdUsAipjClORVQYdxYQEuh5OC6dmCrnoDIYGBYVuiIuSocfyNfDg}"

say() { printf '%s\n' "$*"; }
err() { printf 'muxscale: %s\n' "$*" >&2; }

die() {
  err "$*"
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "this installer needs $1, which is not on your PATH"
}

detect_platform() {
  os=$(uname -s)
  arch=$(uname -m)

  case "$os" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *)
      die "muxscale does not support $os yet.
muxscale runs on macOS and Linux. On Windows, install inside WSL2."
      ;;
  esac

  case "$arch" in
    x86_64 | amd64)  arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    armv7l)          arch=armv7 ;;
    *)
      die "muxscale has no build for $arch yet.
Tell us which machine you're on: hello@muxscale.com"
      ;;
  esac

  printf '%s-%s' "$os" "$arch"
}

resolve_version() {
  if [ "$MUXSCALE_VERSION" != "stable" ] && [ "$MUXSCALE_VERSION" != "beta" ]; then
    printf '%s' "$MUXSCALE_VERSION"
    return
  fi
  ver=$(curl -fsSL "$MUXSCALE_BASE_URL/channels/$MUXSCALE_VERSION" 2>/dev/null) || {
    die "could not reach $MUXSCALE_BASE_URL to resolve the '$MUXSCALE_VERSION' channel.
Check your connection, or pin a version:
    MUXSCALE_VERSION=0.1.0 curl -fsSL https://muxscale.com/install.sh | sh"
  }
  ver=$(printf '%s' "$ver" | tr -d ' \t\n\r')
  [ -n "$ver" ] || die "the '$MUXSCALE_VERSION' channel is empty; try pinning MUXSCALE_VERSION"
  printf '%s' "$ver"
}

verify_checksum() {
  # tarball, expected_sha256
  actual=""
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$1" | cut -d' ' -f1)
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$1" | cut -d' ' -f1)
  else
    die "no sha256 tool found (looked for sha256sum and shasum); refusing to install unverified"
  fi

  [ "$actual" = "$2" ] || die "checksum mismatch.
  expected $2
  got      $actual
This download was corrupted or tampered with. Nothing was installed."
}

verify_signature() {
  # tarball, sigfile
  if [ -n "$MUXSCALE_NO_VERIFY" ]; then
    err "WARNING: signature verification disabled by MUXSCALE_NO_VERIFY."
    return 0
  fi
  if ! command -v minisign >/dev/null 2>&1; then
    # The checksum is still verified against a value fetched over TLS. Say
    # exactly what is and is not being checked rather than implying full
    # verification happened.
    say "  note: minisign not installed, so the signature was not checked."
    say "        the SHA-256 checksum was verified. For full verification:"
    say "          brew install minisign   # or: apt install minisign"
    return 0
  fi
  minisign -Vm "$1" -x "$2" -P "$MUXSCALE_MINISIGN_PUBKEY" >/dev/null 2>&1 ||
    die "signature verification FAILED.
This artifact was not signed by the muxscale release key. Nothing was installed.
If you are certain this is expected, re-run with MUXSCALE_NO_VERIFY=1."
  say "  signature verified"
}

main() {
  need curl
  need tar
  need mkdir

  platform=$(detect_platform)
  version=$(resolve_version)

  say "muxscale $version for $platform"

  tarball="muxscale_${version}_${platform}.tar.gz"
  url="$MUXSCALE_BASE_URL/$version/$tarball"

  tmp=$(mktemp -d 2>/dev/null || mktemp -d -t muxscale)
  # Clean up on every exit path, including failure: a half-downloaded tarball
  # left in /tmp is the kind of thing that confuses the next attempt.
  trap 'rm -rf "$tmp"' EXIT INT TERM

  say "  downloading $tarball"
  curl -fsSL "$url" -o "$tmp/$tarball" ||
    die "could not download $url
Is $version a real release? Leave MUXSCALE_VERSION unset to get the current one."

  curl -fsSL "$url.sha256" -o "$tmp/sha256" ||
    die "could not download the checksum for $tarball; refusing to install unverified"
  expected=$(cut -d' ' -f1 <"$tmp/sha256" | tr -d ' \t\n\r')
  verify_checksum "$tmp/$tarball" "$expected"
  say "  checksum verified"

  if curl -fsSL "$url.minisig" -o "$tmp/sig" 2>/dev/null; then
    verify_signature "$tmp/$tarball" "$tmp/sig"
  else
    say "  note: no signature published for this build"
  fi

  tar -xzf "$tmp/$tarball" -C "$tmp" || die "the archive could not be extracted"
  [ -f "$tmp/muxscale" ] || die "the archive did not contain a muxscale binary"

  mkdir -p "$MUXSCALE_INSTALL_DIR" ||
    die "could not create $MUXSCALE_INSTALL_DIR"

  # Write to a temporary name in the destination directory and rename, so an
  # interrupted install cannot leave a truncated binary on PATH, and so
  # upgrading works while the daemon is running.
  staged="$MUXSCALE_INSTALL_DIR/.muxscale.new.$$"
  cp "$tmp/muxscale" "$staged" || die "could not write to $MUXSCALE_INSTALL_DIR"
  chmod 0755 "$staged"
  mv -f "$staged" "$MUXSCALE_INSTALL_DIR/muxscale" ||
    die "could not install into $MUXSCALE_INSTALL_DIR"

  say "  installed to $MUXSCALE_INSTALL_DIR/muxscale"
  say ""

  case ":$PATH:" in
    *":$MUXSCALE_INSTALL_DIR:"*)
      say "Next:"
      say "    muxscale up        start muxscale now and at every login"
      say "    muxscale connect   serve it to your phone"
      ;;
    *)
      # Never edit a shell profile without being asked. Print the line and let
      # the user decide.
      say "$MUXSCALE_INSTALL_DIR is not on your PATH. Add this line to your shell profile:"
      say ""
      say "    export PATH=\"$MUXSCALE_INSTALL_DIR:\$PATH\""
      say ""
      say "Then run: muxscale up"
      ;;
  esac
}

main "$@"
