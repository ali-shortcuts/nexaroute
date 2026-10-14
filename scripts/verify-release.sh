#!/usr/bin/env bash
# Verifies a prepared release directory:
#   * every required distributable asset is present (binaries,
#     installer, config, docs, exactly one SBOM);
#   * SHA256SUMS covers exactly the asset set (no missing entries, no
#     extras, never the manifest itself or the signature bundle) and
#     `sha256sum -c` passes;
#   * the SBOM is valid SPDX JSON produced by Syft, names this
#     release, records source traceability, and lists the Go module
#     inventory at the exact versions pinned in go.mod (the shipped
#     binaries embed that inventory).
#
# Usage: scripts/verify-release.sh [dist-dir] [version]
set -euo pipefail
cd "$(dirname "$0")/.."
dist="${1:-dist}"
version="${2:-}"
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

python3 - "${sboms[0]}" "$version" <<'PYEOF'
import json, re, sys

path, version = sys.argv[1], sys.argv[2]
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
if version:
    if version not in doc.get("name", ""):
        sys.exit(f"verify-release: SBOM document name {doc.get('name')!r} does not name {version}")
    ns = doc.get("documentNamespace", "")
    if "github.com/ali-shortcuts/nexaroute" not in ns or not re.search(r"sbom-[0-9a-f]{40}$", ns):
        sys.exit(f"verify-release: SBOM namespace lacks repository/commit traceability: {ns!r}")

# The Go module inventory pinned in go.mod must appear in the SBOM at
# the same versions: the shipped binaries embed exactly this graph.
gomod = {}
in_require = False
for line in open("go.mod"):
    line = line.strip()
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
missing = {m: v for m, v in gomod.items() if packages.get(m) != v}
if missing:
    sys.exit(f"verify-release: SBOM Go module inventory mismatch vs go.mod: {missing}")
shipped = {"nexaroute-linux-amd64", "nexaroute-linux-arm64",
           "nexaroute-videogen-linux-amd64", "nexaroute-videogen-linux-arm64"}
seen_files = {f.get("fileName", "").lstrip("./") for f in doc.get("files", [])}
if not shipped <= seen_files:
    sys.exit(f"verify-release: SBOM does not describe all shipped binaries; missing {sorted(shipped - seen_files)}")
print(f"verify-release: SBOM valid SPDX-2.3 by Syft; {len(gomod)} go.mod modules present at pinned versions; "
      f"{len(doc.get('packages', []))} packages, {len(doc.get('files', []))} files total")
PYEOF
echo "verify-release: PASS ($dist)"
