#!/usr/bin/env bash
# Verifies a prepared release directory:
#   * every required distributable asset is present (binaries,
#     installer, config, docs, exactly one SBOM);
#   * SHA256SUMS covers exactly the asset set (no missing entries, no
#     extras, never the manifest itself or the signature bundle) and
#     `sha256sum -c` passes;
#   * the SBOM is valid SPDX JSON produced by Syft, names this
#     release, records source traceability, and records the Go module
#     inventory actually embedded in the shipped binaries (per their
#     Go build info) at the build versions; no module pinned in
#     go.mod may appear in the SBOM at a different version. (go.mod
#     can pin test-only modules that are never linked into a shipped
#     binary and so can never appear in a binary-derived SBOM.)
#
# Usage: scripts/verify-release.sh [dist-dir] [version] [expected-commit]
# The expected source commit may also be given as EXPECTED_SOURCE_COMMIT.
# When the version and the expected commit are both provided, the SBOM
# namespace must equal
#   https://github.com/ali-shortcuts/nexaroute/releases/<version>/sbom-<commit>
# exactly — no substring matching, so an attacker-controlled host, a
# wrong release version, a well-formed but unrelated SHA, or "unknown"
# are all rejected. The release workflow always passes both.
set -euo pipefail
cd "$(dirname "$0")/.."
dist="${1:-dist}"
version="${2:-}"
expected_commit="${3:-${EXPECTED_SOURCE_COMMIT:-}}"
[[ -d "$dist" ]] || { echo "verify-release: no such directory: $dist" >&2; exit 1; }

required=(
  nexaroute-linux-amd64
  nexaroute-linux-arm64
  nexaroute-videogen-linux-amd64
  nexaroute-videogen-linux-arm64
  install.sh
  video.example.json
  VIDEO_GATEWAY.md
  RELEASE_NOTES.md
  SHA256SUMS
)
for f in "${required[@]}"; do
  [[ -s "$dist/$f" ]] || { echo "verify-release: missing release asset: $dist/$f" >&2; exit 1; }
done
sboms=( "$dist"/*-sbom.spdx.json )
[[ -e "${sboms[0]}" ]] || { echo "verify-release: no SBOM (*-sbom.spdx.json) in $dist" >&2; exit 1; }
[[ ${#sboms[@]} -eq 1 ]] || { echo "verify-release: multiple SBOM files in $dist" >&2; exit 1; }
sbom="$(basename "${sboms[0]}")"
assets=()
for f in "${required[@]}"; do
  [[ "$f" == "SHA256SUMS" ]] || assets+=( "$f" )
done
assets+=( "$sbom" )

# Manifest coverage: exact set equality with the assets on disk.
if grep -Eq '(^|[[:space:]])(\./)?(SHA256SUMS|SHA256SUMS\.cosign\.bundle)([[:space:]]|$)' "$dist/SHA256SUMS"; then
  echo "verify-release: SHA256SUMS must not list itself or the signature bundle" >&2
  exit 1
fi
listed="$(awk '{print $2}' "$dist/SHA256SUMS" | sed 's|^\./||' | sort)"
expected="$(printf '%s\n' "${assets[@]}" | sort)"
if [[ "$listed" != "$expected" ]]; then
  echo "verify-release: SHA256SUMS coverage mismatch" >&2
  diff <(printf '%s\n' "$expected") <(printf '%s\n' "$listed") >&2 || true
  exit 1
fi
ondisk="$(cd "$dist" && ls -1 | grep -v -e '^SHA256SUMS$' -e '^SHA256SUMS\.cosign\.bundle$' | sort)"
if [[ "$ondisk" != "$expected" ]]; then
  echo "verify-release: unexpected or missing files in $dist" >&2
  diff <(printf '%s\n' "$expected") <(printf '%s\n' "$ondisk") >&2 || true
  exit 1
fi
(cd "$dist" && sha256sum -c SHA256SUMS >/dev/null) || {
  echo "verify-release: sha256sum -c failed in $dist" >&2
  exit 1
}

python3 - "${sboms[0]}" "$version" "$expected_commit" <<'PYEOF'
import json, re, sys

path, version, expected = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    with open(path) as fh:
        doc = json.load(fh)
except Exception as exc:
    sys.exit(f"verify-release: SBOM is not valid JSON: {exc}")
if doc.get("spdxVersion") != "SPDX-2.3":
    sys.exit(f"verify-release: SBOM spdxVersion is {doc.get('spdxVersion')!r}, want SPDX-2.3")
creators = " ".join(doc.get("creationInfo", {}).get("creators", []))
if "Tool: syft-" not in creators:
    sys.exit(f"verify-release: SBOM was not produced by Syft (creators: {creators!r})")
if version and version not in doc.get("name", ""):
    sys.exit(f"verify-release: SBOM document name {doc.get('name')!r} does not name {version}")
ns = doc.get("documentNamespace", "")
if expected:
    if not re.fullmatch(r"[0-9a-f]{40}", expected):
        sys.exit(f"verify-release: expected source commit is not a 40-char hex SHA: {expected!r}")
    if version:
        want_ns = f"https://github.com/ali-shortcuts/nexaroute/releases/{version}/sbom-{expected}"
        if ns != want_ns:
            sys.exit(f"verify-release: SBOM namespace is not the exact expected release namespace {want_ns}: {ns!r}")
    elif "github.com/ali-shortcuts/nexaroute" not in ns or not ns.endswith(f"sbom-{expected}"):
        sys.exit(f"verify-release: SBOM namespace does not record the expected source commit {expected}: {ns!r}")
elif version:
    if "github.com/ali-shortcuts/nexaroute" not in ns or not re.search(r"sbom-[0-9a-f]{40}$", ns):
        sys.exit(f"verify-release: SBOM namespace lacks repository/commit traceability: {ns!r}")

# The Go module inventory pinned in go.mod must appear in the SBOM at
# the same versions: the shipped binaries embed exactly this graph.
gomod = {}
main_module = ""
in_require = False
for line in open("go.mod"):
    line = line.strip()
    module_match = re.match(r"^module\s+(\S+)\s*$", line)
    if module_match:
        main_module = module_match.group(1)
    if line.startswith("require ("):
        in_require = True
        continue
    if in_require and line == ")":
        in_require = False
        continue
    m = re.match(r"^(require\s+)?(\S+)\s+(v\S+)(\s+//.*)?$", line)
    if m and (in_require or m.group(1)):
        gomod[m.group(2)] = m.group(3)
packages = {p.get("name"): p.get("versionInfo") for p in doc.get("packages", [])}

# Syft can identify Go modules through SPDX purl external references.
# Reject any such module that is not pinned by this repository's go.mod.
# The main module and the Go standard library are expected exceptions.
# This is deliberately limited to pkg:golang references so unrelated
# package-manager ecosystems in a valid SPDX document remain unaffected.
for package in doc.get("packages", []):
    for ref in package.get("externalRefs") or []:
        if ref.get("referenceType") != "purl":
            continue
        locator = ref.get("referenceLocator", "")
        if not isinstance(locator, str) or not locator.startswith("pkg:golang/"):
            continue
        name = package.get("name", "")
        if name not in gomod and name not in (main_module, "stdlib"):
            sys.exit(
                f"verify-release: SBOM lists Go module {name!r} that go.mod does not pin"
            )

# The modules that must appear in the SBOM are the ones actually
# embedded in the shipped binaries (their Go build info). go.mod can
# legitimately pin test-only modules (imported solely by *_test.go),
# which are never linked into a shipped binary and can never appear
# in a binary-derived SBOM; requiring them made this check fail on a
# correct SBOM once the module graph grew test-only pins. When the
# dist files are fixtures (not real Go binaries) the embedded set is
# empty and only the drift check below applies.
import os, subprocess
dist_dir = os.path.dirname(path) or "."
embedded = {}
for bin_name in ("nexaroute-linux-amd64", "nexaroute-linux-arm64",
                 "nexaroute-videogen-linux-amd64", "nexaroute-videogen-linux-arm64"):
    try:
        out = subprocess.run(["go", "version", "-m", os.path.join(dist_dir, bin_name)],
                             capture_output=True, text=True, timeout=30)
    except Exception:
        continue
    for ln in out.stdout.splitlines():
        parts = ln.split()
        if len(parts) >= 3 and parts[0] == "dep":
            embedded[parts[1]] = parts[2]
missing = {m: v for m, v in embedded.items() if packages.get(m) != v}
if missing:
    sys.exit(f"verify-release: SBOM does not record the shipped binaries' embedded modules at their build versions: {missing}")
drift = {m: (packages[m], v) for m, v in gomod.items() if m in packages and packages[m] != v}
if drift:
    sys.exit(f"verify-release: SBOM Go module inventory mismatch vs go.mod: {drift}")
shipped = {"nexaroute-linux-amd64", "nexaroute-linux-arm64",
           "nexaroute-videogen-linux-amd64", "nexaroute-videogen-linux-arm64"}
seen_files = {f.get("fileName", "").lstrip("./") for f in doc.get("files", [])}
if not shipped <= seen_files:
    sys.exit(f"verify-release: SBOM does not describe all shipped binaries; missing {sorted(shipped - seen_files)}")
print(f"verify-release: SBOM valid SPDX-2.3 by Syft; {len(embedded)} embedded modules verified in SBOM, {len(gomod)} go.mod pins drift-checked; "
      f"{len(doc.get('packages', []))} packages, {len(doc.get('files', []))} files total")
PYEOF
echo "verify-release: PASS ($dist)"
