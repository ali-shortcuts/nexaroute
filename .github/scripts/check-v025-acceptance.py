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


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON field: {key}")
        result[key] = value
    return result


def get(repo, endpoint, paginate=False):
    args = ["gh", "api"]
    if paginate:
        # Older gh versions do not provide --slurp; --paginate emits one JSON
        # array per page, concatenated. Decode every page, including the last.
        args.append("--paginate")
    args.append(f"repos/{repo}/{endpoint}")
    raw = subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=30)
    if not paginate:
        return json.loads(raw, object_pairs_hook=unique_object)
    decoder = json.JSONDecoder(object_pairs_hook=unique_object)
    pages = []
    pos = 0
    while pos < len(raw):
        while pos < len(raw) and raw[pos].isspace():
            pos += 1
        if pos == len(raw):
            break
        page, pos = decoder.raw_decode(raw, pos)
        if not isinstance(page, list):
            raise ValueError("invalid comments page")
        pages.append(page)
    if not pages:
        raise ValueError("empty comments response")
    return pages


def verify(repo, issue, version, sha):
    if version != "v0.25.0" or not SHA.fullmatch(sha):
        raise ValueError("missing or invalid exact candidate SHA")
    issue_data = get(repo, f"issues/{issue}")
    if not isinstance(issue_data, dict) or type(issue_data.get("number")) is not int \
            or issue_data["number"] != issue or type(issue_data.get("comments")) is not int \
            or issue_data["comments"] < 0:
        raise ValueError("invalid issue identity or comment count")
    pages = get(repo, f"issues/{issue}/comments?per_page=100", paginate=True)
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("invalid or incomplete comments response")
    comments = [item for page in pages for item in page]
    if len(comments) != issue_data["comments"]:
        raise ValueError("incomplete or changed paginated issue comments")
    if any(not isinstance(c, dict) or type(c.get("id")) is not int or c["id"] <= 0
           or not isinstance(c.get("created_at"), str) or not re.fullmatch(
               r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", c["created_at"])
           or not isinstance(c.get("body"), str) or not isinstance(c.get("author_association"), str)
           for c in comments):
        raise ValueError("malformed issue comment response")
    if len({c["id"] for c in comments}) != len(comments):
        raise ValueError("duplicate issue comment ID")
    records = [c for c in comments if isinstance(c.get("body"), str) and c["body"].startswith(MARKER)
               and c.get("author_association") in ("OWNER", "MEMBER")]
    if not records:
        raise ValueError("no authorized structured acceptance record")
    records.sort(key=lambda c: (c.get("created_at") or "", c.get("id") or 0))
    record = json.loads(records[-1]["body"][len(MARKER):], object_pairs_hook=unique_object)
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
