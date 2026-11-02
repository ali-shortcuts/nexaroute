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
from urllib.parse import parse_qs, urlsplit

MARKER = "NEXAROUTE_ACCEPTANCE_V1\n"
SHA = re.compile(r"^[0-9a-f]{40}$")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON field: {key}")
        result[key] = value
    return result


def get(repo, endpoint):
    path = f"repos/{repo}" + (f"/{endpoint}" if endpoint else "")
    raw = subprocess.check_output(["gh", "api", path], text=True, stderr=subprocess.DEVNULL, timeout=30)
    return json.loads(raw, object_pairs_hook=unique_object)


def comment_pages(repo, issue, repo_id, page_size=100):
    """Follow and validate GitHub's Link chain without trusting link destinations."""
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) or type(repo_id) is not int or repo_id <= 0 \
            or type(issue) is not int or issue <= 0:
        raise ValueError("invalid repository identity")
    if type(page_size) is not int or not 1 <= page_size <= 100:
        raise ValueError("invalid page size")
    page, last_page, pages = 1, None, []
    while True:
        if page > 1000:
            raise ValueError("pagination limit exceeded")
        path = f"repos/{repo}/issues/{issue}/comments?per_page={page_size}&page={page}"
        raw = subprocess.check_output(["gh", "api", "-i", path], text=True, stderr=subprocess.DEVNULL, timeout=30)
        head, separator, body = raw.partition("\n\n")
        if not separator:
            raise ValueError("missing response headers")
        lines = head.splitlines()
        if not lines or not re.fullmatch(r"HTTP/\d(?:\.\d)? 200(?: .*)?", lines[0]):
            raise ValueError("invalid HTTP status")
        values = []
        for line in lines[1:]:
            header = re.fullmatch(r"([!#$%&'*+.^_`|~0-9A-Za-z-]+):[ \t]*(.*)", line)
            if not header:
                raise ValueError("malformed HTTP response header")
            if header.group(1).lower() == "link":
                values.append(header.group(2))
        if len(values) > 1:
            raise ValueError("duplicate Link headers")
        links = {}
        if values:
            for entry in values[0].split(","):
                match = re.fullmatch(r'\s*<([^<>]+)>;\s*rel="(next|last|prev|first)"\s*', entry)
                if not match:
                    raise ValueError("malformed pagination link")
                url, relation = match.groups()
                if relation in links:
                    raise ValueError("duplicate pagination relation")
                parsed = urlsplit(url)
                expected_paths = {f"/repositories/{repo_id}/issues/{issue}/comments", f"/repos/{repo}/issues/{issue}/comments"}
                query = parse_qs(parsed.query, keep_blank_values=True, strict_parsing=True)
                if parsed.scheme != "https" or parsed.netloc != "api.github.com" or parsed.path not in expected_paths \
                        or parsed.fragment or set(query) != {"per_page", "page"} \
                        or query["per_page"] != [str(page_size)] or len(query["page"]) != 1 \
                        or not re.fullmatch(r"[1-9][0-9]*", query["page"][0]):
                    raise ValueError("pagination link is not bound to this repository and issue")
                links[relation] = int(query["page"][0])
        if page == 1 and ("prev" in links or "first" in links):
            raise ValueError("unexpected previous or first page")
        if ("prev" in links and links["prev"] != page - 1) or ("first" in links and links["first"] != 1):
            raise ValueError("invalid previous or first page")
        if page > 1 and (links.get("prev") != page - 1 or links.get("first") != 1):
            raise ValueError("missing previous or first page")
        if "next" in links:
            if links["next"] != page + 1 or "last" not in links or links["last"] < page + 1:
                raise ValueError("invalid next or last page")
            if last_page is not None and last_page != links["last"]:
                raise ValueError("last page changed during traversal")
            last_page = links["last"]
        elif (last_page is not None and page != last_page) or "last" in links:
            raise ValueError("pagination ended before the last page")
        rows = json.loads(body, object_pairs_hook=unique_object)
        if not isinstance(rows, list) or len(rows) > page_size or (page > 1 and not rows) \
                or ("next" in links and len(rows) != page_size):
            raise ValueError("invalid comments page")
        pages.append(rows)
        if "next" not in links:
            return pages
        page += 1


def verify(repo, issue, version, sha):
    if version != "v0.25.0" or not SHA.fullmatch(sha):
        raise ValueError("missing or invalid exact candidate SHA")
    repo_data = get(repo, "")
    if not isinstance(repo_data, dict) or not isinstance(repo_data.get("full_name"), str) \
            or repo_data["full_name"].lower() != repo.lower() \
            or type(repo_data.get("id")) is not int or repo_data["id"] <= 0:
        raise ValueError("invalid repository identity response")
    issue_data = get(repo, f"issues/{issue}")
    if not isinstance(issue_data, dict) or type(issue_data.get("number")) is not int \
            or issue_data["number"] != issue or type(issue_data.get("comments")) is not int \
            or issue_data["comments"] < 0 or not isinstance(issue_data.get("updated_at"), str) \
            or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", issue_data["updated_at"]):
        raise ValueError("invalid issue identity or comment count")
    pages = comment_pages(repo, issue, repo_data["id"])
    issue_after = get(repo, f"issues/{issue}")
    if not isinstance(issue_after, dict) or type(issue_after.get("number")) is not int \
            or issue_after["number"] != issue or type(issue_after.get("comments")) is not int \
            or issue_after["comments"] != issue_data["comments"] \
            or issue_after.get("updated_at") != issue_data["updated_at"]:
        raise ValueError("issue changed during comment collection")
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
