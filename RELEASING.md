# Releasing

`tools/release.sh 0.1.0` builds every platform, signs each tarball with the release key, uploads
them to the `muxscale-dl` R2 bucket behind dl.muxscale.com, and moves the `stable` channel to the
new version, which is what `install.sh` (served at muxscale.com/install.sh) installs. `--no-promote` uploads
without moving the channel; `--dry-run` only builds and signs into `dist/release/`.

The signing key is `~/.minisign/muxscale.key`, its password in the macOS Keychain as
`muxscale-release-minisign`. The public key is `tools/minisign.pub` and is baked into `install.sh`;
the script refuses to release if the two differ. Lose the private key and existing installs can no
longer verify new releases, so keep a backup of both the key file and its password.
