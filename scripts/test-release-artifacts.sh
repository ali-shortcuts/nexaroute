#!/usr/bin/env bash
# Fixture tests for the release artifact scripts (no network, no real
# binaries): a synthetic dist must verify, and each tampering must be
# rejected — modified asset, missing SBOM, manifest entry dropped,
# manifest self-reference, SBOM module-version drift vs go.mod.
set -euo pipefail
cd "$(dirname "$0")/.."

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

make_fixture() {
  local dir="$1"
  mkdir -p "$dir"
  for f in nexaroute-linux-amd64 nexaroute-linux-arm64 nexaroute-videogen-linux-amd64 \
           nexaroute-videogen-linux-arm64 install.sh video.example.json \
           VIDEO_GATEWAY.md RELEASE_NOTES.md; do
    printf 'fixture %s\n' "$f" > "$dir/$f"
  done
  python3 - "$dir/nexaroute-v9.9.9-sbom.spdx.json" <<'PYEOF'
import json, re, sys
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
doc = {
    "spdxVersion": "SPDX-2.3",
    "name": "nexaroute-v9.9.9",
    "documentNamespace": "https://github.com/ali-shortcuts/nexaroute/releases/v9.9.9/sbom-0123456789abcdef0123456789abcdef01234567",
    "creationInfo": {"creators": ["Tool: syft-1.54.1"]},
    "packages": [{"name": n, "versionInfo": v} for n, v in sorted(gomod.items())],
    "files": [{"fileName": f} for f in (
        "nexaroute-linux-amd64", "nexaroute-linux-arm64",
        "nexaroute-videogen-linux-amd64", "nexaroute-videogen-linux-arm64")],
}
with open(sys.argv[1], "w") as fh:
    json.dump(doc, fh, indent=2)
PYEOF
  ./scripts/write-checksums.sh "$dir" >/dev/null
}

expect_fail() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "test-release-artifacts: FAIL — $name was accepted" >&2
    exit 1
  fi
  echo "test-release-artifacts: rejected as required — $name"
}

good="$work/good"
make_fixture "$good"
./scripts/verify-release.sh "$good" v9.9.9 >/dev/null
echo "test-release-artifacts: intact fixture verifies"

# Exact source-commit traceability: the fixture SBOM records commit
# 0123456789abcdef0123456789abcdef01234567 in its namespace.
fixture_commit="0123456789abcdef0123456789abcdef01234567"
EXPECTED_SOURCE_COMMIT="$fixture_commit" ./scripts/verify-release.sh "$good" v9.9.9 >/dev/null
./scripts/verify-release.sh "$good" v9.9.9 "$fixture_commit" >/dev/null
echo "test-release-artifacts: correct expected source commit verifies (env and positional)"
expect_fail "wrong expected source commit" env EXPECTED_SOURCE_COMMIT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ./scripts/verify-release.sh "$good" v9.9.9
expect_fail "malformed expected source commit" env EXPECTED_SOURCE_COMMIT="unknown" ./scripts/verify-release.sh "$good" v9.9.9

set_ns() {
  python3 - "$1" "$2" <<'PYEOF'
import glob, json, sys
p = glob.glob(sys.argv[1] + "/*-sbom.spdx.json")[0]
doc = json.load(open(p))
doc["documentNamespace"] = sys.argv[2]
json.dump(doc, open(p, "w"), indent=2)
PYEOF
  ./scripts/write-checksums.sh "$1" >/dev/null
}

t="$work/unknownns"; make_fixture "$t"
set_ns "$t" "https://github.com/ali-shortcuts/nexaroute/releases/v9.9.9/sbom-unknown"
expect_fail "SBOM namespace stamped unknown with an expected commit" env EXPECTED_SOURCE_COMMIT="$fixture_commit" ./scripts/verify-release.sh "$t" v9.9.9

# Exact-namespace enforcement: with a version and an expected commit,
# the namespace must equal the canonical release namespace exactly.
t="$work/evilhost"; make_fixture "$t"
set_ns "$t" "https://evil.example/?github.com/ali-shortcuts/nexaroute/releases/v9.9.9/sbom-$fixture_commit"
expect_fail "attacker-controlled namespace host (substring bait)" env EXPECTED_SOURCE_COMMIT="$fixture_commit" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/wrongver"; make_fixture "$t"
set_ns "$t" "https://github.com/ali-shortcuts/nexaroute/releases/v9.9.8/sbom-$fixture_commit"
expect_fail "namespace release version mismatch" env EXPECTED_SOURCE_COMMIT="$fixture_commit" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/badns"; make_fixture "$t"
set_ns "$t" "not-a-release-namespace"
expect_fail "malformed SBOM namespace" env EXPECTED_SOURCE_COMMIT="$fixture_commit" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/tampered"; make_fixture "$t"; echo 'evil' >> "$t/nexaroute-linux-amd64"
expect_fail "modified asset" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/nosbom"; make_fixture "$t"; rm "$t"/*-sbom.spdx.json
./scripts/write-checksums.sh "$t" >/dev/null 2>&1 || true
expect_fail "missing SBOM" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/dropped"; make_fixture "$t"; grep -v 'install.sh' "$t/SHA256SUMS" > "$t/SHA256SUMS.tmp" && mv "$t/SHA256SUMS.tmp" "$t/SHA256SUMS"
expect_fail "manifest entry dropped" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/selfref"; make_fixture "$t"; (cd "$t" && sha256sum SHA256SUMS >> SHA256SUMS)
expect_fail "manifest self-reference" ./scripts/verify-release.sh "$t" v9.9.9

t="$work/drift"; make_fixture "$t"
python3 - "$t" <<'PYEOF'
import glob, json, sys
p = glob.glob(sys.argv[1] + "/*-sbom.spdx.json")[0]
doc = json.load(open(p))
doc["packages"][0]["versionInfo"] = "v0.0.0-drifted"
json.dump(doc, open(p, "w"), indent=2)
PYEOF
./scripts/write-checksums.sh "$t" >/dev/null
expect_fail "SBOM module version drift vs go.mod" ./scripts/verify-release.sh "$t" v9.9.9

echo "test-release-artifacts: PASS"
