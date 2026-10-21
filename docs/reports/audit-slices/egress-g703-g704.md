# Audit slice 2: G703/G704 egress triage

- Parent audit: [#593](https://github.com/ali-shortcuts/nexaroute/issues/593) (kept open)
- Slice issue: [#603](https://github.com/ali-shortcuts/nexaroute/issues/603)
- Audited `main`: `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`
- Report date: 2026-10-21
- Scanner: pinned `gosec v2.29.0` (`GOBIN=.artifacts/tools go install github.com/securego/gosec/v2/cmd/gosec@v2.29.0`)
- Command: `.artifacts/tools/gosec -quiet -fmt=json -out=.artifacts/gosec-current.json ./...`
- Baseline result: **98 findings**, matching the last recorded audit slice: **G304 54, G703 28, G204 12, G704 4**.

This slice only triages and, where confirmed, fixes G703 and G704. It does not
touch G304 or command-execution findings, `Config.Validate`, or the shared
audit-final reports. Gosec exit status is `1` whenever findings exist; that is
not a test failure.

## Summary

| Rule | Count | Confirmed defect | Fixed finding locations | Accepted / false positive |
| --- | ---: | ---: | ---: | ---: |
| G704 SSRF via taint analysis | 4 | 1 redirect defect | 2 `Do` sinks | 2 request-construction locations on the same operator-CLI boundary |
| G703 path traversal via taint analysis | 28 | 0 | 0 | 28 local/operator path uses |
| **Total** | **32** | **1 defect** | **2 findings** | **30 findings** |

The confirmed defect is in `cmd/videogen`: both the JSON API client and the
asset-download client followed HTTP redirects without any origin boundary. A
malicious/compromised Video Gateway, or an intermediary able to answer the
operator-supplied base URL, could redirect the CLI to an unrelated host or to
plain HTTP. That is an SSRF/redirect boundary and, on a same-host HTTPS-to-HTTP
downgrade, can expose the bearer token. The fix applies the existing
`internal/egress.RefuseCrossOriginRedirects` contract to both clients while
preserving any caller-supplied redirect policy.

After the fix gosec still reports **G703 28 / G704 4**, because the same sinks
exist and now sit behind an explicit redirect guard. The count is unchanged by
design; hiding them with suppressions would not improve the security posture.

## G704 finding-by-finding

The four locations are two request/response pairs. The request target is built
from the operator-supplied `--base-url` plus fixed or URL-escaped segments; no
client/model/upstream response selects the host. The CLI is a trusted operator
client, so the base URL itself is an accepted local boundary, not remote SSRF.
The redirect *follow* behaviour was the defect.

| ID | Location at audited SHA | Caller / source boundary | Root cause / classification | Result |
| --- | --- | --- | --- | --- |
| G704-01 | `cmd/videogen/client.go:199` (`http.NewRequestWithContext` in `request`) | Operator `--base-url`; fixed API path | Operator-configured CLI target. Paired with G704-02; no untrusted caller input selects the host. | Accepted boundary; redirect defect fixed at the client. |
| G704-02 | `cmd/videogen/client.go:208` (`c.http.Do`) | Same JSON API client | **Confirmed**: `http.Client` had no `CheckRedirect`, so a 3xx from the gateway was followed cross-origin (or to plain HTTP). | **Fixed** via `applyRedirectGuard`; regression tests below. |
| G704-03 | `cmd/videogen/client.go:110` (`http.NewRequestWithContext` in `DownloadAsset`) | Operator `--base-url`; `jobID`/`assetID` are `url.PathEscape`d | Operator-configured CLI target. Paired with G704-04. | Accepted boundary; redirect defect fixed at the client. |
| G704-04 | `cmd/videogen/client.go:120` (`client.Do` for download) | Asset download client | **Confirmed**: download client had no `CheckRedirect`; a redirect could fetch an arbitrary internal target and change the destination of an authenticated request. | **Fixed** via `applyRedirectGuard`; regression tests below. |

### Fix

`cmd/videogen/client.go` now clones both clients and composes
`egress.RefuseCrossOriginRedirects(10, service)` with any existing
`CheckRedirect` policy. Same-scheme/same-host redirects remain valid; scheme,
host, or port changes are refused before the request is sent. A supplied
client's own policy still runs first and cannot be silently replaced.

Regression coverage in `cmd/videogen/client_test.go`:

- `TestAPIClientRefusesCrossOriginRedirects` — both `Providers` and
  `DownloadAsset` refuse a redirect to a second server; the destination is
  never hit and a refused download leaves no output file.
- `TestAPIClientFollowsSameOriginRedirects` — a same-origin redirect still
  works.
- `TestAPIClientPreservesCallerRedirectPolicy` — a caller's redirect policy is
  preserved.

Before the fix, the new cross-origin test failed with:

```text
--- FAIL: TestAPIClientRefusesCrossOriginRedirects (0.00s)
    client_test.go:235: providers redirect error=<nil>, want cross-origin refusal
```

After the fix the whole `cmd/videogen` package passes (evidence below).

## G703 finding-by-finding

All 28 G703 locations are local filesystem paths, not URLs or request targets.
None accepts a network/upstream value as the path. They fall into four trust
boundaries: operator CLI/config/env paths, fixed internal bundle entry names, a
private staging directory created with `os.MkdirTemp`, and operator-selected
font paths. A local attacker who can replace those paths already controls the
config/key directory; this is not a remote traversal boundary. These are
classified as accepted local-boundary uses / false positives for a network
trust decision, not as blanket "safe because configurable" claims.

| ID | Location at audited SHA | Source and function | Classification / reason |
| --- | --- | --- | --- |
| G703-01 | `internal/video/render/render.go:94` | `Renderer.FontFile` (operator config) or `NEXAROUTE_VIDEO_FONT`/fixed system font paths | Operator-selected local font; validated regular file. Video plan data does not supply it. False positive for traversal. |
| G703-02 | `internal/secrets/secrets.go:52` | `Key`; `configPath` from gateway `--config`, or `NEXAROUTE_MASTER_KEY_FILE` | Operator/local key path. Not remote input. False positive. |
| G703-03 | `internal/secrets/secrets.go:54` | `Key`; `os.Stat` on the same key path | Same local key path; checks 0600/length. False positive. |
| G703-04 | `internal/secrets/secrets.go:83` | `Key`; creates the directory of the key path | Derived from operator config/key path, mode 0700. False positive. |
| G703-05 | `internal/secrets/secrets.go:90` | `Key`; creates the key file `O_CREATE|O_EXCL` 0600 | Operator config directory; exclusive create. False positive. |
| G703-06 | `internal/secrets/secrets.go:102` | `Key`; removes partial key file | Same operator key path after write failure. False positive. |
| G703-07 | `internal/secrets/secrets.go:105` | `Key`; syncs the key directory | Derived from operator key path. False positive. |
| G703-08 | `internal/secrets/secrets.go:135` | `recoverPendingRotation`; promotes `<config>.key.next` to `<config>.key` | Local key-rotation recovery for operator-owned files. False positive. |
| G703-09 | `internal/secrets/secrets.go:158` | `recoverMissingRotationKey`; same staged-key rename | Local key-rotation recovery. False positive. |
| G703-10 | `internal/secrets/secrets.go:166` | `readRotationKey`; reads `<config>.key.next` | Local key file. False positive. |
| G703-11 | `internal/secrets/secrets.go:170` | `readRotationKey`; stats same staged key | Local key file permission/length check. False positive. |
| G703-12 | `internal/secrets/secrets.go:181` | `syncDirectory`; opens the key directory | Local operator directory. False positive. |
| G703-13 | `cmd/videogen/main.go:79` | `render`; reads operator `--file` manifest | Operator CLI input path. False positive. |
| G703-14 | `cmd/videogen/main.go:140` | `compose`; reads operator `--file` manifest | Operator CLI input path. False positive. |
| G703-15 | `cmd/videogen/main.go:199` | `create`; reads operator `--file` request JSON | Operator CLI input path. False positive. |
| G703-16 | `cmd/videogen/client.go:101` | `DownloadAsset`; stats output directory | Operator CLI `--out` path. False positive. |
| G703-17 | `cmd/videogen/client.go:104` | `DownloadAsset`; stats output file before network | Operator CLI `--out`; refuses existing files. False positive. |
| G703-18 | `cmd/videogen/client.go:161` | `DownloadAsset`; re-stats output before publishing | Operator CLI `--out`; prevents overwrite races. False positive. |
| G703-19 | `cmd/videogen/client.go:166` | `DownloadAsset`; renames temp file into output | Temp and destination are in the operator-chosen output directory. False positive. |
| G703-20 | `cmd/gateway/secrets.go:33` | `secrets status`; reads operator `--config` | Operator CLI config path. False positive. |
| G703-21 | `cmd/gateway/secrets.go:83` | `secrets decrypt`; reads operator `--config` | Operator CLI config path. False positive. |
| G703-22 | `cmd/gateway/secrets.go:101` | `secrets rotate`; reads operator `--config` | Operator CLI config path. False positive. |
| G703-23 | `cmd/gateway/secrets.go:136` | rotation; promotes `<config>.key.next` | Operator-owned key path. False positive. |
| G703-24 | `cmd/gateway/secrets.go:169` | `writeRotateFile`; renames temp to operator path | Destination derived from the operator config path. False positive. |
| G703-25 | `cmd/gateway/secrets.go:177` | `syncRotateDirectory`; syncs operator key directory | Operator-owned directory. False positive. |
| G703-26 | `cmd/gateway/restore_verify.go:215` | `verifyConfigDecryption`; stages host `<config>.key` into private stage | Source is operator config path; destination is `os.MkdirTemp` staging. False positive. |
| G703-27 | `cmd/gateway/restore_verify.go:261` | `availableMasterKeyBytes`; reads `NEXAROUTE_MASTER_KEY_FILE` | Operator environment path. False positive. |
| G703-28 | `cmd/gateway/backup.go:196` | `createBackup.add`; writes staged bundle entry | `name` is one of the fixed `backupEntry*` constants; `stage` is a private `os.MkdirTemp` directory. No caller-controlled name reaches this join. False positive. |

No G703 fix is warranted in this slice. Any future hardening of local
key/config filesystem races is a separate local-privilege threat model, not a
G703 egress/SSRF change.

## Verification

Commands and real results for the code/test checkpoint
`02f1ddedcee8163be4609bb9e64e2aa0d00e581d`:

| Command | Result |
| --- | --- |
| `go test ./cmd/videogen -run 'TestAPIClient(RefusesCrossOriginRedirects|FollowsSameOriginRedirects|PreservesCallerRedirectPolicy)$' -count=1` before the fix | **FAIL** (`providers redirect error=<nil>`) — reproduced the defect |
| `go test ./cmd/videogen -count=1` after the fix | **PASS** |
| `go test ./cmd/videogen ./internal/egress -count=1` | **PASS** |
| `go vet ./cmd/videogen ./internal/egress` | **PASS** |
| `go test -race ./cmd/videogen ./internal/egress -count=1` | **PASS** |
| `.artifacts/tools/gosec -quiet -fmt=json -out=.artifacts/gosec-after-fix.json ./...` | Exit 1, findings only: **G703 28, G704 4**; same sinks, G704 now guarded |
| `./scripts/verify.sh` | **EXIT 0**, `VERIFY PASS` (full log: `.artifacts/issue603/verify.log`); unit/integration pass, `go vet`, mandatory race pass, release/SBOM checks, short fuzz, linux amd64/arm64 builds all passed |
| Browser acceptance inside `verify.sh` | **SKIPPED** with the script's printed warning: Chromium + Python Playwright unavailable in this runner. Not claimed as run. |
| `git diff --check` / `gofmt -l .` (inside verify) | **PASS** |

CI/Security results for the opened PR must be read from the PR checks; this
report does not claim those checks are green. The code/test checkpoint is
`02f1ddedcee8163be4609bb9e64e2aa0d00e581d`; this report is added in a
report-only commit after that verification, so the tested code tree is
unchanged.

## Remaining risk / blockers

- **Operator-supplied videogen base URL**: a malicious `--base-url` is still a
  trusted local choice and is not sanitized as an SSRF target. That is the CLI
  contract; the redirect boundary is now closed. UNVERIFIED against a real
  remote Video Gateway deployment (no live service in this slice).
- **G703 local filesystem boundaries**: not changed. A local attacker with
  write access to the config/key/output directories is outside the network
  egress threat model. No external verification was attempted.
- **Scanner findings**: G703/G704 remain intentionally visible. No blanket
  suppression or baseline was added.
- **G304, G204, and other rules**: out of scope for this slice and untouched.
