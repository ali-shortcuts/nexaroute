#!/usr/bin/env python3
"""Fail-closed metadata check for v0.25.0 issue acceptance records.

A structured issue comment links a non-PR Actions run and its unexpired
artifact to the candidate SHA. Metadata cannot prove that an artifact's
contents meet an issue's acceptance criteria: independent human review of the
actual report and topology is still mandatory.
"""
import argparse
import json
import re
import subprocess
import sys

MARKER = "NEXAROUTE_ACCEPTANCE_V1\n"
SHA = re.compile(r"^[0-9a-f]{40}$")


def get(repo, endpoint, paginate=False):
    args = ["gh", "api"]
    if paginate:
        args += ["--paginate", "--slurp"]
    args.append(f"repos/{repo}/{endpoint}")
    raw = subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=30)
    return json.loads(raw)


def verify(repo, issue, version, sha):
    if version != "v0.25.0" or not SHA.fullmatch(sha):
        raise ValueError("missing or invalid exact candidate SHA")
    pages = get(repo, f"issues/{issue}/comments?per_page=100", paginate=True)
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("invalid or incomplete comments response")
    comments = [item for page in pages for item in page if isinstance(item, dict)]
    records = [c for c in comments if isinstance(c.get("body"), str) and c["body"].startswith(MARKER)
               and c.get("author_association") in ("OWNER", "MEMBER")]
    if not records:
        raise ValueError("no authorized structured acceptance record")
    records.sort(key=lambda c: (c.get("created_at") or "", c.get("id") or 0))
    record = json.loads(records[-1]["body"][len(MARKER):])
    if not isinstance(record, dict) or type(record.get("issue")) is not int or record["issue"] != issue \
            or record.get("release") != version or record.get("commit") != sha or record.get("result") != "PASS":
        raise ValueError("missing, stale, mismatched or non-passing acceptance record")
    if not isinstance(record.get("summary"), str) or not record["summary"].strip():
        raise ValueError("acceptance summary is missing")
    run_id, artifact_id = record.get("run_id"), record.get("artifact_id")
    if type(run_id) is not int or run_id <= 0 or type(artifact_id) is not int or artifact_id <= 0:
        raise ValueError("invalid run or artifact ID")
    run = get(repo, f"actions/runs/{run_id}")
    if not isinstance(run, dict) or run.get("id") != run_id or run.get("head_sha") != sha \
            or run.get("event") != "workflow_dispatch" or run.get("status") != "completed" \
            or run.get("conclusion") != "success":
        raise ValueError("run is not a successful exact-SHA manual acceptance run")
    artifact = get(repo, f"actions/artifacts/{artifact_id}")
    workflow_run = artifact.get("workflow_run") if isinstance(artifact, dict) else None
    if not isinstance(artifact, dict) or artifact.get("id") != artifact_id or artifact.get("expired") is not False \
            or not isinstance(workflow_run, dict) or workflow_run.get("id") != run_id \
            or workflow_run.get("head_sha") != sha:
        raise ValueError("artifact is missing, expired or does not belong to the exact-SHA run")
    print(f"Acceptance metadata # {issue}: run {run_id}, artifact {artifact_id}; human content review required")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", required=True)
    parser.add_argument("--issue", required=True, type=int)
    parser.add_argument("--version", required=True)
    parser.add_argument("--sha", required=True)
    args = parser.parse_args()
    try:
        verify(args.repo, args.issue, args.version, args.sha)
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired, ValueError, TypeError, KeyError) as exc:
        print(f"::error::Acceptance evidence for #{args.issue} cannot be verified: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
