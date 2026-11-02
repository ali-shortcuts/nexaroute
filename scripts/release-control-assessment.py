#!/usr/bin/env python3
"""Conservative OFFLINE release-control assessment; never authorizes release.

Input is an operator-supplied, non-secret policy snapshot. A passing fixture
or snapshot is not evidence of active GitHub/cloud configuration. Historical
workflow inspection is heuristic, not a parser, and cannot prove absence of an
indirect publication path. Live administrative verification is mandatory.
"""
import argparse
import json
import re
import subprocess
import sys

SHA = re.compile(r"^[0-9a-f]{40}$")
TRUSTED_REPO = "ali-shortcuts/nexaroute"
TRUSTED_REF = "refs/heads/main"
ENVIRONMENT = "release-publication"


def workflow_findings(text, ref):
    """Flag sensitive constructs even when surrounding YAML claims to guard them."""
    found = set()
    if re.search(r"(?m)^\s*contents:\s*write\s*(?:#.*)?$", text):
        found.add("write-token")
    if re.search(r"(?m)^\s*(?:packages|deployments|actions):\s*write\b", text):
        found.add("other-write-token")
    if re.search(r"(?m)^\s*id-token:\s*write\b", text):
        found.add("oidc-token")
    if re.search(r"\bgh\s+release\s+(?:create|edit|upload|delete)\b", text):
        found.add("direct-release")
    if re.search(r"\bgit\s+push\b|\b(?:docker|podman)\s+push\b|\bnpm\s+publish\b", text):
        found.add("push-or-package")
    if re.search(r"\$\{\{\s*secrets\.(?!GITHUB_TOKEN\b)", text, re.I):
        found.add("external-secret-reference")
    if ref.startswith("refs/tags/") and ("write-token" in found or "oidc-token" in found):
        found.add("historical-tag-privilege")
    if re.search(r"(?m)^\s*workflow_dispatch:\s*(?:#.*)?$", text) and ("write-token" in found or "oidc-token" in found):
        found.add("manual-dispatch-privilege")
    # Reusable workflows are evaluated separately from ordinary actions.
    for target in re.findall(r"(?m)^\s*(?:-\s*)?uses:\s*([^\s#]+)", text):
        if "/.github/workflows/" in target:
            _, sep, revision = target.rpartition("@")
            if not sep or not SHA.fullmatch(revision):
                found.add("unpinned-reusable-workflow")
    return found


def policy_findings(data):
    """Evaluate a proposed snapshot shape; not a live policy attestation."""
    failures = []
    def require(condition, message):
        if not condition:
            failures.append(message)
    if not isinstance(data, dict):
        return ["snapshot must be an object"]
    env = data.get("environment") if isinstance(data.get("environment"), dict) else {}
    tags = data.get("tag_rules") if isinstance(data.get("tag_rules"), dict) else {}
    oidc = data.get("oidc") if isinstance(data.get("oidc"), dict) else {}
    control = data.get("controller") if isinstance(data.get("controller"), dict) else {}
    app_id = data.get("release_app_id")
    require(type(app_id) is int and app_id > 0, "release app identity missing")
    require(data.get("repository_secrets_reviewed") is True and data.get("repository_publication_secret_count") == 0
            and type(data.get("repository_publication_secret_count")) is int,
            "repository-level publication secret inventory not verified empty")
    require(env.get("name") == ENVIRONMENT and type(env.get("required_reviewer_count")) is int
            and env["required_reviewer_count"] > 0 and env.get("prevent_self_review") is True
            and env.get("admin_bypass_allowed") is False, "environment approval protection missing")
    require(env.get("custom_branch_policies") is True and env.get("protected_branches") is False
            and env.get("allowed_refs") == [TRUSTED_REF], "environment refs not restricted to trusted controller")
    require(tags.get("enforcement") == "active" and tags.get("pattern") == "v*"
            and tags.get("restrict_creation") is True and tags.get("restrict_update") is True
            and tags.get("restrict_deletion") is True and type(app_id) is int
            and tags.get("bypass_app_ids") == [app_id], "tag rules/bypass not tightly restricted")
    workflow_sha = data.get("trusted_workflow_sha")
    require(isinstance(workflow_sha, str) and SHA.fullmatch(workflow_sha) is not None,
            "immutable trusted workflow SHA missing")
    audience = data.get("trusted_audience")
    require(oidc.get("repository") == TRUSTED_REPO and oidc.get("ref") == TRUSTED_REF
            and oidc.get("environment") == ENVIRONMENT and oidc.get("workflow_sha") == workflow_sha
            and oidc.get("workflow_path") == ".github/workflows/release-controller.yml"
            and isinstance(audience, str) and bool(audience.strip()) and "*" not in audience
            and oidc.get("audience") == audience, "OIDC trust is missing or broad")
    candidate = control.get("candidate_sha")
    require(control.get("workflow_ref") == TRUSTED_REF and control.get("event") == "workflow_dispatch"
            and control.get("authorized_actor") is True, "controller ref, event or actor is untrusted")
    require(isinstance(candidate, str) and SHA.fullmatch(candidate) is not None
            and candidate == control.get("current_main_sha"), "candidate is missing or stale")
    require(control.get("acceptance_reports_independently_reviewed") is True
            and control.get("owner_approved") is True, "independent acceptance or owner approval missing")
    return failures


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("ambiguous duplicate snapshot key")
        result[key] = value
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True, help="non-secret, operator-supplied policy snapshot JSON")
    args = parser.parse_args()
    try:
        with open(args.snapshot, encoding="utf-8") as file:
            data = json.load(file, object_pairs_hook=unique_object)
        errors = policy_findings(data)
        tags = subprocess.check_output(["git", "tag", "-l", "v*"], text=True).splitlines()
        if not tags:
            errors.append("no local historical release tags were inspected")
        for tag in tags:
            content = subprocess.check_output(["git", "show", f"{tag}:.github/workflows/release.yml"], text=True, stderr=subprocess.DEVNULL)
            findings = workflow_findings(content, f"refs/tags/{tag}")
            if findings:
                print(f"{tag}: {', '.join(sorted(findings))}")
                errors.append(f"{tag}: historical privileged workflow requires central mitigation")
        for error in errors:
            print(f"BLOCKED: {error}")
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f"BLOCKED: audit input or Git history unreadable: {exc}", file=sys.stderr)
        return 1
    print("OFFLINE ONLY: this assessment does not verify GitHub/cloud settings or authorize publication.")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
