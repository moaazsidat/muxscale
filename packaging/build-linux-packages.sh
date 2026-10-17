#!/usr/bin/env bash
# Builds .deb and .rpm packages from a compiled muxscale binary.
#
# Hand-rolled rather than using fpm or nfpm, for the same reason the SBOM is:
# the packages are simple, and a build step that needs a Ruby or Go toolchain
# installed first is a build step that breaks on someone else's machine.
#
# Both packages deliberately install *only* the binary, a man page and a
# licence. No systemd unit, no postinst that starts anything. `muxscale up`
# installs the service when the user asks for it, and a package manager is the wrong place to decide that something should start
# watching your coding agents.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${MUXSCALE_VERSION:-0.1.0}"
ARCH="${MUXSCALE_ARCH:-amd64}"
OUT="${MUXSCALE_OUT:-dist}"

case "$ARCH" in
  amd64) rpm_arch=x86_64; go_arch=amd64 ;;
  arm64) rpm_arch=aarch64; go_arch=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

mkdir -p "$OUT"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "building muxscale $VERSION for linux/$go_arch"
(cd go && GOOS=linux GOARCH="$go_arch" CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$work/muxscale" ./cmd/muxscale)

# ---- shared payload ----
root="$work/root"
mkdir -p "$root/usr/bin" "$root/usr/share/doc/muxscale" "$root/usr/share/man/man1"
install -m 0755 "$work/muxscale" "$root/usr/bin/muxscale"
# Not conditional: the .rpm spec lists copyright under %license, so a missing
# LICENSE would produce a package that claims MIT and carries no licence text.
install -m 0644 LICENSE "$root/usr/share/doc/muxscale/copyright"

# man(7) wants a date in .TH, and mandoc lints an empty one. Honour
# SOURCE_DATE_EPOCH so a rebuild of the same release produces a byte-identical
# package rather than one that differs only by the second it was built. That
# matters here because the release is signed: a user who rebuilds from source
# should be able to reach the same hash as the artifact that was signed.
EPOCH="${SOURCE_DATE_EPOCH:-}"
if [ -n "$EPOCH" ]; then
  # BSD date takes -r, GNU date takes -d @; try both rather than assume a host.
  fmt_date() { date -u -r "$EPOCH" "$1" 2>/dev/null || date -u -d "@$EPOCH" "$1"; }
  MAN_DATE=$(fmt_date +%Y-%m-%d)
  STAMP=$(fmt_date +%Y%m%d%H%M.%S)
else
  MAN_DATE=$(date -u +%Y-%m-%d)
  STAMP=""
fi

cat > "$root/usr/share/man/man1/muxscale.1" <<MAN
.TH MUXSCALE 1 "$MAN_DATE" "muxscale $VERSION" "User Commands"
.SH NAME
muxscale \- monitor and unblock your coding agents
.SH SYNOPSIS
.B muxscale
[\fICOMMAND\fR]
.SH DESCRIPTION
Watches coding agent sessions on this machine, detects when one is blocked
waiting for a human, and lets you answer from elsewhere.
.PP
Installing this package does not start anything. Run
.B muxscale up
to start muxscale now and at every login, and
.B muxscale connect
to use it from your phone.
.SH COMMANDS
.TP
.B up
Start muxscale now and at every login. With \-\-shims, also wrap agents so
sessions you start accept replies.
.TP
.B connect
Serve muxscale to your phone over Tailscale and print the code to scan.
.B connect off
stops it.
.TP
.B status
Say what is running and reachable, and how to fix what is not.
.TP
.B down
Stop muxscale and remove everything it set up.
.TP
.B ls
List agent sessions and pending approvals. With \-w, follow events.
.TP
.B approve \fIID\fR, deny \fIID\fR
Answer a pending approval.
.TP
.B send \fIID\fR \fITEXT\fR
Reply to a session.
.SH FILES
.TP
.I ~/.config/muxscale/config.toml
Configuration.
.TP
.I ~/.local/state/muxscale/state.db
Sessions, transcripts and the audit log.
.SH SEE ALSO
https://muxscale.com/docs
MAN
gzip -9nf "$root/usr/share/man/man1/muxscale.1"

installed_size=$(du -sk "$root" | cut -f1)

# ---- .deb ----
deb="$work/deb"
mkdir -p "$deb/DEBIAN"
cp -a "$root/." "$deb/"

cat > "$deb/DEBIAN/control" <<CONTROL
Package: muxscale
Version: $VERSION
Section: devel
Priority: optional
Architecture: $ARCH
Maintainer: muxscale <dev@muxscale.com>
Installed-Size: $installed_size
Homepage: https://muxscale.com
Description: Monitor and unblock your coding agents
 muxscale watches the coding agent sessions running on your machine, detects
 when one is blocked waiting for a human, and lets you answer from your phone.
 .
 Installing this package does not start a daemon or modify any agent's
 configuration. Run "muxscale up" to start it, then "muxscale connect".
CONTROL

# Checksums let dpkg detect local modification; a package without them is
# accepted but reports nothing useful from `dpkg -V`.
#
# `find -printf` and `md5sum` are both GNU-only, and this script has to run on
# the macOS dev machine as well as a Linux build host, so neither is assumed.
if command -v md5sum >/dev/null 2>&1; then
  md5_of() { md5sum "$1" | cut -d' ' -f1; }
elif command -v md5 >/dev/null 2>&1; then
  md5_of() { md5 -q "$1"; }
else
  die() { echo "$*" >&2; exit 1; }
  die "no md5 tool found (looked for md5sum and md5); cannot write DEBIAN/md5sums"
fi

: > "$deb/DEBIAN/md5sums"
(
  cd "$deb"
  find . -type f ! -path './DEBIAN/*' | sed 's|^\./||' | LC_ALL=C sort |
    while IFS= read -r f; do
      printf '%s  %s\n' "$(md5_of "$f")" "$f" >> DEBIAN/md5sums
    done
)

# Every file is stamped identically so the mtimes tar records do not vary
# between builds. Done last, after the control file and md5sums are written.
if [ -n "$STAMP" ]; then
  find "$deb" -exec touch -t "$STAMP" {} +
fi

deb_file="$OUT/muxscale_${VERSION}_${ARCH}.deb"
if command -v dpkg-deb >/dev/null 2>&1; then
  dpkg-deb --build --root-owner-group "$deb" "$deb_file" >/dev/null
else
  # dpkg-deb is not present on macOS. A .deb is an ar archive of three members
  # in a fixed order, so it can be assembled directly -- which keeps this
  # script usable for producing and inspecting packages during development.
  echo "  dpkg-deb not found; assembling the ar archive directly"
  # ustar rather than the gnu format: bsdtar spells that format 'gnutar' and
  # GNU tar spells it 'gnu', while both accept 'ustar'. Every path here is far
  # under ustar's 100-character limit, so nothing is lost by taking the format
  # both tools agree on.
  #
  # Ownership has to be forced to root:root. Without it the archive records
  # whoever ran the build -- uid 501 on a Mac -- and dpkg faithfully installs
  # /usr/bin/muxscale owned by uid 501 on the target machine. This is what
  # dpkg-deb --root-owner-group does on the other branch. The two tars spell
  # the flags differently, so pick by what is actually installed.
  if tar --version 2>&1 | grep -qi bsdtar; then
    own="--uid 0 --gid 0 --uname root --gname root"
  else
    own="--owner=root --group=root --numeric-owner"
  fi
  # Compressed with a separate `gzip -n` rather than tar's -z, because
  # libarchive's gzip filter writes the current time into the gzip header --
  # four bytes that change every second and defeat reproducibility no matter
  # how carefully the tar payload is stamped. -n omits it.
  # shellcheck disable=SC2086  # $own is a deliberate multi-flag word split
  tar --format=ustar $own -cf "$work/control.tar" -C "$deb/DEBIAN" .
  # shellcheck disable=SC2086
  ( cd "$deb" && tar --format=ustar $own -cf "$work/data.tar" --exclude ./DEBIAN . )
  gzip -9nf "$work/control.tar" "$work/data.tar"
  printf '2.0\n' > "$work/debian-binary"

  # Written byte by byte rather than shelled out to `ar`. macOS ar runs ranlib
  # over the members, warns that a gzip stream is not a Mach-O object, and
  # emits a truncated archive; and dpkg requires the three members in exactly
  # this order, which no ar guarantees. The format is a fixed 60-byte header
  # per member, so producing it directly is both shorter and portable.
  ar_member() {
    # name, path
    size=$(wc -c < "$2" | tr -d ' ')
    printf '%-16s%-12s%-6s%-6s%-8s%-10s\140\n' "$1" 0 0 0 100644 "$size"
    cat "$2"
    [ $((size % 2)) -eq 0 ] || printf '\n'   # members are 2-byte aligned
  }

  {
    printf '!<arch>\n'
    ar_member debian-binary  "$work/debian-binary"
    ar_member control.tar.gz "$work/control.tar.gz"
    ar_member data.tar.gz    "$work/data.tar.gz"
  } > "$deb_file"
fi
echo "  $deb_file"

# ---- .rpm ----
# rpmbuild is rarely present outside a Linux build host, so the spec is emitted
# for a packaging job to consume rather than silently skipping.
spec="$OUT/muxscale.spec"
cat > "$spec" <<SPEC
Name:           muxscale
Version:        $VERSION
Release:        1%{?dist}
Summary:        Monitor and unblock your coding agents
License:        MIT
URL:            https://muxscale.com
BuildArch:      $rpm_arch

%description
muxscale watches the coding agent sessions running on your machine, detects
when one is blocked waiting for a human, and lets you answer from your phone.

Installing this package does not start a daemon or modify any agent's
configuration. Run "muxscale up" to start it, then "muxscale connect".

%files
%{_bindir}/muxscale
%{_mandir}/man1/muxscale.1*
%license %{_docdir}/muxscale/copyright

%changelog
SPEC
echo "  $spec"

if command -v rpmbuild >/dev/null 2>&1; then
  rpmbuild -bb "$spec" \
    --define "_topdir $work/rpm" \
    --buildroot "$root" >/dev/null 2>&1 &&
    find "$work/rpm/RPMS" -name '*.rpm' -exec cp {} "$OUT/" \; &&
    echo "  built rpm"
else
  echo "  rpmbuild not found; the spec is emitted for a Linux packaging job"
fi

echo
echo "packages in $OUT:"
ls -la "$OUT" | tail -n +2 | sed 's|^|  |'
