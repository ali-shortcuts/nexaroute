# Audit slice 3 — gosec command-execution findings (G204)

Parent audit: issue 593. Slice issue: 604.
Audited SHA: `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d` (main at triage time).
Scanner: gosec v2.29.0 (the version pinned by `.github/workflows/audit.yml`),
run exactly as CI runs it: `gosec -quiet ./...` (text + JSON).

## Fresh baseline

| Rule | Before | After this slice | Note |
| --- | --- | --- | --- |
| G204 | 12 | 0 | this slice |
| G115 | 0 | 0 | checkpoint re-verified, see below |
| G304 | 54 | 54 | untouched — separate slice |
| G703 | 28 | 28 | untouched — separate slice |
| G704 | 4 | 4 | untouched — separate slice |
| Total | 98 | 86 | |

## Method

Every `exec.Command`/`exec.CommandContext` call site in production code was
read at source and every argument was traced to its origin. Two facts shape
the whole slice:

1. **No shell is ever invoked.** There is no `sh`, `bash`, `cmd`, or `-c`
   invocation anywhere in production Go code; every subprocess is started
   with `os/exec` and a discrete argv vector.
2. **No executable name is request- or job-controlled.** Every binary name
   comes from exactly one of: the operator's gateway config file
   (`video.piper_binary`, `video.piper_model_path`), an operator environment
   variable (`NEXAROUTE_FFMPEG`, `NEXAROUTE_KEY_PROVIDER_COMMAND`,
   `NEXAROUTE_KEY_PROVIDER_REF`), a flag of the operator's own `videogen`
   CLI (`--piper-bin`), a compile-time default (`ffmpeg`, `ffprobe`,
   `piper`, `secret-tool`), a literal, or the fixed `BrowserCommands`
   allowlist. Job/API payloads supply only text, paths, and dimensions,
   which are validated before use and passed as individual argv elements,
   on stdin, or through private temp files — never concatenated into a
   command string.

## Per-finding triage (all 12 G204 findings)

Disposition key: **FP** = false positive / safe by construction; action taken
is an inline `#nosec G204` justification in the established repo style (same
pattern as the existing `#nosec G115` annotations), stating the invariant.
No behavior was changed and no scanner rule or whole file was silenced.

| # | Location | Executable source | Variable data & how it travels | Disposition |
| --- | --- | --- | --- | --- |
| 1 | `internal/video/composer/ffmpeg.go:30` | `exec.LookPath` resolution of operator FFmpeg binary (default `ffmpeg`) | none — fixed `-version` flag | FP, annotated |
| 2 | `internal/video/composer/ffmpeg.go:33` | same LookPath-resolved path | none — fixed `-hide_banner -encoders` | FP, annotated |
| 3 | `internal/video/composer/ffmpeg.go:58` | `Runner.Binary`: gateway config / `NEXAROUTE_FFMPEG` / `ffmpeg` | args built only by this package's `Normalize`/`Concat` helpers from `%d`-formatted dimensions and workspace paths; concat list file quote-escapes entries | FP, annotated |
| 4 | `internal/video/audio/piper.go:111` | `PiperOptions.Binary`: `videogen` CLI flag, default `piper` | model path validated as absolute regular file; output is a `os.CreateTemp` private file; narration text on stdin, never argv | FP, annotated |
| 5 | `internal/secrets/keyring_provider_linux.go:19` | literal `secret-tool` | `ref` is a single argv element from operator config (`NEXAROUTE_KEY_PROVIDER_REF`) | FP, annotated |
| 6 | `internal/secrets/keyring_provider_linux.go:34` | literal `secret-tool` | same `ref`; the key itself travels on stdin, never argv | FP, annotated |
| 7 | `internal/desktop/desktop.go:63` | `BrowserCommands` fixed allowlist — the only production caller is `OpenBrowser` in the same file | `url` is the locally constructed dashboard URL, one argv element | FP, annotated |
| 8 | `internal/video/tts/piper.go:105` | `p.Binary`: absolute path resolved by `exec.LookPath` in `NewPiper` from operator config | model path validated as an existing regular file in `NewPiper`; narration text via a private temp input file, never argv | FP, annotated |
| 9 | `internal/video/render/render.go:65` | `Renderer.ffmpeg()`: `Renderer.FFmpeg` / `NEXAROUTE_FFMPEG` / `ffmpeg` | none — fixed `-hide_banner -filters` | FP, annotated |
| 10 | `internal/video/render/render.go:208` | same | args built from a validated render plan; numeric fields formatted internally (`fnum`, `%d`); scene/caption text passed to FFmpeg via `textfile=` files, not filter-string interpolation | FP, annotated |
| 11 | `internal/video/render/render.go:340` | `Renderer.ffprobe()`: config / default `ffprobe` | fixed flags; `path` is one argv element for a file stat-checked (exists, non-empty) at the top of `Probe` | FP, annotated |
| 12 | `internal/video/render/render.go:380` | `Renderer.ffmpeg()` | seek position formatted internally by `fnum`; `path` is one argv element | FP, annotated |

### Manually audited, not flagged by gosec

`internal/secrets/key_provider.go:44` (`CommandProvider`) does not appear in
the fresh gosec v2.29.0 report (verified: zero matches in the JSON output),
so no annotation was added. It was still audited by hand: the command is the
operator-configured `NEXAROUTE_KEY_PROVIDER_COMMAND` (a documented operator
contract — the operator deliberately installs a KMS/helper executable), it
is executed with no arguments and no shell, the `get`/`put` operation and the
key reference travel on stdin, and secrets never appear in argv or error
messages. The existing type comment already records this contract.

## G115 checkpoint

The issue asked to confirm whether any G115 integer-boundary findings remain
on the fresh report. **None do**: the fresh v2.29.0 run reports zero G115
findings before and after this slice (the only historical sites, in
`internal/video/tts/audio.go`, already carry justified `#nosec G115`
annotations). Nothing to fix.

## Confirmed defects

None. No shell injection, unsafe executable selection, unsafe argument
construction, environment/working-directory trust, or error-path defect was
confirmed in this slice, so no code behavior changed and no new regression
test was required. The validations that the dispositions rely on are already
covered by tests, all passing with assertions executing:

- `TestNewPiperRequiresModelAndSidecar` (internal/video/tts) — startup
  validation of the Piper binary/model contract.
- `TestPreflightRejectsMissingFFmpegExecutable` (internal/video/composer)
  and `TestPreflightMissingFFmpeg` (internal/video/render) — fail-closed
  `LookPath` behavior.
- `TestOpenBrowserPreferenceAndFallback` (internal/desktop) — browser
  allowlist order/fallback via a fake runner.
- `TestCommandProviderProtocolAndInvalidOutput` (internal/secrets) — the
  stdin protocol and fail-closed output validation of the command provider.
- `TestSynthesizePiperRejectsInvalidInputs` (internal/video/audio) — input
  validation before any Piper invocation.

## Commands and results

- `gosec -quiet -fmt=json ./...` (v2.29.0) on `cbdf159c`: 98 issues
  (G204: 12, G304: 54, G703: 28, G704: 4, G115: 0).
- After annotations: 86 issues, **G204: 0, G115: 0**; G304/G703/G704 counts
  unchanged (other slices' scope untouched).
- `gofmt -l` on all touched packages: clean.
- `go test -race -count=1` on `internal/desktop`, `internal/secrets`,
  `internal/video/composer`, `internal/video/audio`, `internal/video/tts`,
  `internal/video/render`: all `ok`.
- Full `scripts/verify.sh` and CI/Security results for the exact PR head are
  recorded in the PR for this slice.

## Residual risk

- Executable selection is an operator trust boundary by design: an operator
  who can edit the gateway config, environment, or CLI flags can already
  execute arbitrary programs as the service account, so pointing
  `piper_binary`/`NEXAROUTE_FFMPEG` at another binary grants no new
  privilege. This boundary is documented at each annotated call site.
- Bare binary names resolve through the operator's `PATH` (standard
  `os/exec` semantics); deployments that need a stricter boundary can set
  absolute paths in config/env, which all call sites accept.
- Argument-level option injection (a data value beginning with `-`) is not
  reachable for the flagged sites: paths are server-generated workspace
  paths or validated absolute files, and user text never enters argv.
