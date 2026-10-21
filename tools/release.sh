#!/bin/sh
# Builds, signs and publishes a muxscale release to dl.muxscale.com, in the
# layout install.sh reads:
#
#   channels/<channel>                          the version the channel is on
#   <version>/muxscale_<version>_<os>-<arch>.tar.gz
#   <version>/....tar.gz.sha256                 "<sha256>  <file>"
#   <version>/....tar.gz.minisig                minisign signature
#
#   tools/release.sh 0.1.0              build, sign, upload, move stable to it
#   tools/release.sh 0.1.1 --no-promote build, sign, upload; stable stays put
#   tools/release.sh 0.1.0 --dry-run    build and sign into dist/release only
#
# The signing key is ~/.minisign/muxscale.key; its password is in the macOS
# Keychain as "muxscale-release-minisign". The public half is
# tools/minisign.pub, and must be the key baked into install.sh -- checked
# below, because a release signed by a key the installer does not trust fails
# every install.
set -eu

die() {
  printf 'release: %s\n' "$*" >&2
  exit 1
}

version="${1:-}"
[ -n "$version" ] || die "usage: tools/release.sh <version> [--no-promote] [--dry-run] [--channel NAME]"
shift
case "$version" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) die "version must look like 1.2.3, got $version" ;;
esac

promote=1
dry=0
channel=stable
while [ $# -gt 0 ]; do
  case "$1" in
    --no-promote) promote=0 ;;
    --dry-run) dry=1 ;;
    --channel) shift; channel="${1:?--channel needs a name}" ;;
    *) die "unknown option $1" ;;
  esac
  shift
done

root=$(cd "$(dirname "$0")/.." && pwd)
key="$HOME/.minisign/muxscale.key"
pub="$root/tools/minisign.pub"
bucket=muxscale-dl
out="$root/dist/release/$version"

[ -f "$key" ] || die "no signing key at $key"
pubkey=$(tail -n 1 "$pub")
grep -q "$pubkey" "$root/install.sh" ||
  die "install.sh does not carry the key in tools/minisign.pub; installs would fail verification"
# muxscale.com serves its own copy; point MUXSCALE_SERVED_INSTALL at it to
# check the two match.
served="${MUXSCALE_SERVED_INSTALL:-}"
if [ -n "$served" ]; then
  cmp -s "$root/install.sh" "$served" ||
    die "$served differs from install.sh; copy it over and redeploy the site"
fi
command -v minisign >/dev/null || die "minisign is not installed (brew install minisign)"

if [ "$dry" = 0 ] && [ -n "$(git -C "$root" status --porcelain -- go)" ]; then
  die "go/ has uncommitted changes; a release is built from what is committed"
fi

password=$(security find-generic-password -s muxscale-release-minisign -w) ||
  die "could not read the signing key's password from the Keychain"

rm -rf "$out"
mkdir -p "$out"
commit=$(git -C "$root" rev-parse --short HEAD)

# os-arch as install.sh names them, and the Go target for each.
for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 linux-armv7; do
  os=${target%-*}
  arch=${target#*-}
  goarch=$arch
  goarm=
  if [ "$arch" = armv7 ]; then
    goarch=arm
    goarm=7
  fi

  stage=$(mktemp -d)
  printf '  building %s\n' "$target"
  (cd "$root/go" && CGO_ENABLED=0 GOOS=$os GOARCH=$goarch GOARM=$goarm \
    go build -trimpath -ldflags "-s -w -X main.version=$version" \
    -o "$stage/muxscale" ./cmd/muxscale)

  tarball="muxscale_${version}_${target}.tar.gz"
  # Fixed owner and no extended attributes, so the archive does not carry
  # this workstation's user or macOS metadata.
  COPYFILE_DISABLE=1 tar -czf "$out/$tarball" -C "$stage" --uid 0 --gid 0 --uname root --gname root muxscale
  rm -rf "$stage"

  (cd "$out" && shasum -a 256 "$tarball" >"$tarball.sha256")
  printf '%s\n' "$password" | minisign -S -s "$key" -m "$out/$tarball" \
    -t "muxscale $version $target ($commit)" >/dev/null
  minisign -Vm "$out/$tarball" -p "$pub" >/dev/null || die "signature check failed for $tarball"
done
unset password

printf '%s\n' "$version" >"$out/$channel"

if [ "$dry" = 1 ]; then
  printf 'dry run: built and signed into %s\n' "$out"
  exit 0
fi

wrangler() { (cd "$root/site" && npx wrangler "$@"); }

for f in "$out"/muxscale_*; do
  name=$(basename "$f")
  case "$name" in
    *.tar.gz) type=application/gzip ;;
    *) type=text/plain ;;
  esac
  printf '  uploading %s\n' "$name"
  # A version's files never change once published, so they may be cached
  # for good; re-releasing a version is a mistake this does not help with.
  wrangler r2 object put "$bucket/$version/$name" --file "$f" --remote \
    --content-type "$type" --cache-control "public, max-age=31536000, immutable" >/dev/null
done

if [ "$promote" = 1 ]; then
  # The channel moves, so nothing may cache it for long.
  wrangler r2 object put "$bucket/channels/$channel" --file "$out/$channel" --remote \
    --content-type text/plain --cache-control "no-cache" >/dev/null
  printf 'released %s; %s now points to it\n' "$version" "$channel"
else
  printf 'uploaded %s; %s unchanged\n' "$version" "$channel"
fi
