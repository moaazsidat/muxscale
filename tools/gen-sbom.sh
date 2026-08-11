#!/usr/bin/env bash
# Generates a CycloneDX software bill of materials for both toolchains.
#
# An SBOM is what lets someone answer "are we affected?" on the day a CVE lands
# in a transitive dependency, without cloning the repo and reconstructing the
# graph. It ships alongside each release for that reason, not for a compliance
# checkbox.
#
# Built from `go list -m all` and `cargo metadata` rather than a third-party
# scanner, so it needs no extra tooling and cannot drift from what actually
# builds.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="${1:-sbom.cdx.json}"
VERSION="${MUXSCALE_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

python3 - "$OUT" "$VERSION" <<'PY'
import json, subprocess, sys, uuid, datetime, pathlib

out_path, version = sys.argv[1], sys.argv[2]
components, seen = [], set()

def add(name, ver, purl, ctype="library", scope="required"):
    key = (name, ver)
    if key in seen or not ver:
        return
    seen.add(key)
    components.append({
        "type": ctype,
        "bom-ref": purl,
        "name": name,
        "version": ver,
        "purl": purl,
        "scope": scope,
    })

# ---- Go ----
# `go list -m all` includes modules pruned from the build. `go list -deps` on
# the actual binary is what genuinely ships, so the two are distinguished:
# reporting a pruned module as shipped is how an SBOM produces false alarms.
shipped = set()
try:
    deps = subprocess.run(
        ["go", "list", "-deps", "-f", "{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}",
         "./cmd/muxscale"],
        cwd="go", capture_output=True, text=True, check=True).stdout
    shipped = {line for line in deps.splitlines() if line.strip()}
except Exception as e:
    print(f"warning: could not determine shipped Go deps ({e})", file=sys.stderr)

try:
    mods = subprocess.run(["go", "list", "-m", "-json", "all"],
                          cwd="go", capture_output=True, text=True, check=True).stdout
    dec = json.JSONDecoder()
    idx = 0
    while idx < len(mods):
        while idx < len(mods) and mods[idx] in " \t\r\n":
            idx += 1
        if idx >= len(mods):
            break
        obj, idx = dec.raw_decode(mods, idx)
        path, ver = obj.get("Path"), obj.get("Version")
        if not ver:
            continue
        scope = "required" if f"{path}@{ver}" in shipped else "optional"
        add(path, ver, f"pkg:golang/{path}@{ver}", scope=scope)
except Exception as e:
    print(f"warning: Go modules unavailable ({e})", file=sys.stderr)

# ---- Rust ----
# `cargo tree -e normal` is used rather than `cargo metadata`, because
# metadata's resolve graph includes optional dependencies whether or not their
# feature is enabled. That reported the bindgen toolchain as shipping inside the
# mobile library, which was both wrong and -- once checked -- a real problem
# worth fixing rather than reporting around.
try:
    shipped_crates = set()
    tree = subprocess.run(
        ["cargo", "tree", "-e", "normal", "--prefix", "none", "--format", "{p}",
         "-p", "muxscale-core"],
        cwd="rust", capture_output=True, text=True, check=True).stdout
    for line in tree.splitlines():
        parts = line.strip().split()
        if len(parts) >= 2 and parts[1].startswith("v"):
            shipped_crates.add((parts[0], parts[1][1:]))

    meta = json.loads(subprocess.run(
        ["cargo", "metadata", "--format-version", "1"],
        cwd="rust", capture_output=True, text=True, check=True).stdout)

    def is_proc_macro(pkg):
        # Proc macros execute during compilation and are not linked into the
        # artifact, so listing them as shipped overstates the runtime surface.
        return any("proc-macro" in t.get("kind", []) for t in pkg.get("targets", []))

    for pkg in meta.get("packages", []):
        name, ver = pkg["name"], pkg["version"]
        if name == "muxscale-core":
            continue
        shipped_here = (name, ver) in shipped_crates and not is_proc_macro(pkg)
        add(name, ver, f"pkg:cargo/{name}@{ver}",
            scope="required" if shipped_here else "optional")
except Exception as e:
    print(f"warning: cargo dependency graph unavailable ({e})", file=sys.stderr)

components.sort(key=lambda c: (c["purl"]))

bom = {
    "bomFormat": "CycloneDX",
    "specVersion": "1.5",
    "serialNumber": f"urn:uuid:{uuid.uuid4()}",
    "version": 1,
    "metadata": {
        "timestamp": datetime.datetime.now(datetime.timezone.utc)
            .strftime("%Y-%m-%dT%H:%M:%SZ"),
        "tools": [{"vendor": "muxscale", "name": "gen-sbom.sh", "version": "1"}],
        "component": {
            "type": "application",
            "bom-ref": f"pkg:generic/muxscale@{version}",
            "name": "muxscale",
            "version": version,
            "description": "Monitor and unblock your coding agents from your phone",
        },
    },
    "components": components,
}

pathlib.Path(out_path).write_text(json.dumps(bom, indent=2) + "\n")

required = sum(1 for c in components if c["scope"] == "required")
print(f"wrote {out_path}")
print(f"  {len(components)} components "
      f"({required} shipped in the binary, {len(components) - required} build-only)")
PY
