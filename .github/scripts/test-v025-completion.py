#!/usr/bin/env python3
"""Offline completion-policy regressions; fixtures are not real acceptance."""
import json
import os
import shutil
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
GATE = ROOT / ".github/scripts/check-final-release-gate.sh"
SHA = "0123456789abcdef0123456789abcdef01234567"
FAKE_GH = r'''#!/usr/bin/env python3
import json, os, re, sys
if "--slurp" in sys.argv or "--paginate" in sys.argv: sys.exit(2)
path = sys.argv[-1]
mode = os.environ.get("MODE", "pass")
sha = os.environ["GITHUB_SHA"]
if path == "repos/example/nexaroute":
    print(json.dumps({"id": 100, "full_name": "example/nexaroute"})); sys.exit(0)
if path.endswith("/git/ref/heads/main"):
    if mode == "main-error": sys.exit(1)
    print(json.dumps({"object": {"type": "commit", "sha": "b" * 40 if mode == "main-stale" else sha}})); sys.exit(0)
issue_match = re.search(r"/issues/(\d+)(/comments\?per_page=100&page=(\d+))?$", path)
if issue_match:
    issue = int(issue_match.group(1))
    if mode == "api-error" and issue == 650: sys.exit(1)
    if not issue_match.group(2):
        number = issue + 1 if mode == "wrong-issue" and issue == 646 else issue
        state = "open" if mode == "open" and issue == 646 else "closed"
        reason = "not_planned" if mode == "not-planned" and issue == 649 else "completed"
        closed_at = "2026-11-02T00:00:00Z" if issue == 654 else "2026-11-01T00:00:00Z"
        if mode == "umbrella-first" and issue == 654: closed_at = "2026-10-31T00:00:00Z"
        if mode == "missing-date" and issue == 646: closed_at = None
        count = "1" if mode == "bad-comment-count" and issue == 646 else (101 if mode == "multiple-pages" and issue == 646 else (2 if mode == "duplicate-comment" and issue == 646 else 1))
        updated = "2026-11-01T00:00:00Z"
        if mode in ("count-change", "date-change") and issue == 646:
            file = ".calls-" + mode
            try: n = int(open(file).read())
            except FileNotFoundError: n = 0
            open(file,"w").write(str(n + 1))
            if n >= 2:
                if mode == "count-change": count = 2
                else: updated = "2026-11-02T00:00:00Z"
        print(json.dumps({"number": number, "state": state, "state_reason": reason, "closed_at": closed_at, "comments": count, "updated_at": updated})); sys.exit(0)
    if "-i" not in sys.argv: sys.exit(2)
    page = int(issue_match.group(3))
    if mode == "comment-error" and issue == 646: sys.exit(1)
    rec = {"issue": issue, "release": "v0.25.0", "commit": sha, "result": "PASS", "run_id": 100000 + issue, "artifact_id": 200000 + issue, "summary": "fixture"}
    if mode == "stale" and issue == 646: rec["commit"] = "a" * 40
    if mode == "result-fail" and issue == 646: rec["result"] = "FAIL"
    comment = {"id": issue, "created_at": "2026-11-01T00:00:00Z", "author_association": "OWNER", "body": "NEXAROUTE_ACCEPTANCE_V1\n" + json.dumps(rec)}
    if mode == "unauthorized" and issue == 646: comment["author_association"] = "NONE"
    if mode == "duplicate-record" and issue == 646: comment["body"] = 'NEXAROUTE_ACCEPTANCE_V1\n' + json.dumps(rec)[:-1] + ',"result":"PASS"}'
    link = ""
    if issue == 646 and mode in ("multiple-pages", "malformed-link", "cross-repo-link", "skipped-page", "missing-last", "truncated-chain"):
        base = "https://api.github.com/repositories/100/issues/646/comments?per_page=100&page="
        if mode == "cross-repo-link": base = base.replace("/100/", "/999/")
        if page == 1:
            next_page = 3 if mode == "skipped-page" else 2
            link = f'Link: <{base}{next_page}>; rel="next"'
            if mode != "missing-last": link += f', <{base}2>; rel="last"'
            if mode == "malformed-link": link = "Link: invalid"
        else:
            link = f'Link: <{base}1>; rel="prev", <{base}1>; rel="first"'
        if mode == "truncated-chain": link = f'Link: <{base}2>; rel="next", <{base}3>; rel="last"' if page == 1 else f'Link: <{base}1>; rel="prev", <{base}1>; rel="first"'
    print("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n" + (link + "\r\n" if link else "") + "\r\n")
    if mode == "partial-page-error" and issue == 646: print(json.dumps([comment])); sys.exit(1)
    if mode == "bad-page" and issue == 646: print('{invalid'); sys.exit(0)
    if mode == "bad-comment" and issue == 646: print('[null]'); sys.exit(0)
    if mode in ("missing", "incomplete-pages") and issue == 646: print("[]"); sys.exit(0)
    if mode == "duplicate-comment" and issue == 646: print(json.dumps([comment, comment])); sys.exit(0)
    if mode == "multiple-pages" and issue == 646:
        if page == 1:
            print(json.dumps([comment] + [dict(comment, id=10000 + n, body="ordinary comment") for n in range(99)])); sys.exit(0)
        comment = dict(comment, id=9999, body="ordinary comment")
    print(json.dumps([comment])); sys.exit(0)
run = re.search(r"/actions/runs/(\d+)$", path)
if run:
    n = int(run.group(1))
    if mode == "run-error" and n == 100646: sys.exit(1)
    print(json.dumps({"id": n, "head_sha": ("b" * 40 if mode == "run-sha" and n == 100646 else sha), "event": ("pull_request" if mode == "pr-run" and n == 100646 else "workflow_dispatch"), "status": "completed", "conclusion": "success"})); sys.exit(0)
artifact = re.search(r"/actions/artifacts/(\d+)$", path)
if artifact:
    n = int(artifact.group(1))
    print(json.dumps({"id": n, "expired": mode == "expired" and n == 200646, "workflow_run": {"id": n - 100000 + (1 if mode == "wrong-run" and n == 200646 else 0), "head_sha": sha}})); sys.exit(0)
sys.exit(1)
'''


def main():
    with tempfile.TemporaryDirectory(prefix="v025-gate-test-") as tmp:
        base = Path(tmp)
        (base / "bin").mkdir()
        (base / "bin/gh").write_text(FAKE_GH)
        (base / "bin/gh").chmod(0o755)
        jq = base / "bin/jq"
        jq.write_text("#!/usr/bin/env bash\nif [[ \"${MODE:-}\" == jq-error ]]; then echo true; exit 42; fi\nexec \"$REAL_JQ\" \"$@\"\n")
        jq.chmod(0o755)
        releases = base / "work/docs/releases"
        releases.mkdir(parents=True)
        manifest = releases / "v0.25.0-exclusions.json"
        env = dict(os.environ, PATH=str(base / "bin") + os.pathsep + os.environ["PATH"],
                   GITHUB_REPOSITORY="example/nexaroute", GH_TOKEN="fixture", GITHUB_SHA=SHA,
                   REAL_JQ=shutil.which("jq"))
        env.pop("GITHUB_REF_NAME", None)
        env.pop("GITHUB_REF", None)

        def run(mode="pass", body=None, sha=SHA):
            if body is None:
                manifest.unlink(missing_ok=True)
            else:
                manifest.write_text(body)
            p = subprocess.run(["bash", str(GATE), "--version", "v0.25.0"], cwd=base / "work",
                               env=dict(env, MODE=mode, GITHUB_SHA=sha), text=True, capture_output=True)
            return p.returncode, p.stdout + p.stderr

        good_empty = json.dumps({"release": "v0.25.0", "exclusions": []})
        rc, out = run(body=good_empty)
        assert rc == 0, f"valid fixture should pass: {out}"
        rc, out = run(mode="multiple-pages", body=good_empty)
        assert rc == 0, f"paginated fixture should pass: {out}"
        entry = {"issue": 649, "reason": "fixture", "approved_by": "owner", "approved_at": "2026-11-01"}
        for label, value in [("signed", entry), ("unsigned", dict(entry, approved_by="", approved_at=""))]:
            rc, _ = run(mode="not-planned", body=json.dumps({"release": "v0.25.0", "exclusions": [value]}))
            assert rc != 0, f"{label} exclusion passed"
        rc, _ = run(body=json.dumps({"release": "v0.25.0", "exclusions": [entry]}))
        assert rc != 0, "signed exclusion passed even with closed issues"
        for mode in ("open", "not-planned", "api-error", "wrong-issue", "umbrella-first", "missing-date", "jq-error", "missing", "stale", "result-fail",
                     "unauthorized", "bad-page", "bad-comment", "incomplete-pages", "count-change", "date-change", "malformed-link", "cross-repo-link", "skipped-page", "missing-last", "truncated-chain", "bad-comment-count", "duplicate-comment", "partial-page-error", "duplicate-record", "comment-error", "main-error", "main-stale", "run-error", "run-sha", "pr-run", "expired", "wrong-run"):
            rc, _ = run(mode=mode, body=good_empty)
            assert rc != 0, f"{mode} passed"
        for body in ('{', '{"release":"v0.24.0","exclusions":[]}', '{"release":"v0.25.0"}', good_empty + good_empty,
                     '{"release":"v0.25.0","exclusions":[{}],"exclusions":[]}'):
            rc, _ = run(body=body)
            assert rc != 0, "invalid manifest passed"
        p = subprocess.run(["bash", str(GATE), "--version", "v9.9.9"], cwd=base / "work",
                           env=dict(env, GITHUB_REF_NAME="v0.25.0"), text=True, capture_output=True)
        assert p.returncode != 0, "actual v0.25.0 tag was overridden"
        for tag_env in ({"GITHUB_REF": "refs/tags/v0.25.0"},
                        {"GITHUB_REF": "refs/tags/v0.25.0", "GITHUB_REF_NAME": "v9.9.9"}):
            p = subprocess.run(["bash", str(GATE), "--version", "v9.9.9"], cwd=base / "work",
                               env=dict(env, **tag_env), text=True, capture_output=True)
            assert p.returncode != 0, "actual tag ref bypassed strict policy"
        rc, _ = run(body=good_empty, sha="short")
        assert rc != 0, "invalid candidate SHA passed"
    print("v0.25.0 completion-only evidence policy PASS (offline fixtures)")


if __name__ == "__main__":
    main()
