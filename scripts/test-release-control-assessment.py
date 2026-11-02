#!/usr/bin/env python3
"""Fixture regressions for offline assessment, not proof of live controls."""
import copy
import importlib.util
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("assessment", ROOT / "scripts/release-control-assessment.py")
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)
SHA = "a" * 40


def snapshot():
    return {"release_app_id": 42, "trusted_workflow_sha": SHA, "repository_secrets_reviewed": True,
            "repository_publication_secret_count": 0, "trusted_audience": "fixture-audience",
            "environment": {"name": a.ENVIRONMENT, "required_reviewer_count": 1, "prevent_self_review": True,
                            "admin_bypass_allowed": False, "custom_branch_policies": True, "protected_branches": False,
                            "allowed_refs": [a.TRUSTED_REF]},
            "tag_rules": {"enforcement": "active", "pattern": "v*", "restrict_creation": True,
                          "restrict_update": True, "restrict_deletion": True, "bypass_app_ids": [42]},
            "oidc": {"repository": a.TRUSTED_REPO, "ref": a.TRUSTED_REF, "environment": a.ENVIRONMENT,
                     "workflow_sha": SHA, "workflow_path": ".github/workflows/release-controller.yml", "audience": "fixture-audience"},
            "controller": {"workflow_ref": a.TRUSTED_REF, "event": "workflow_dispatch", "authorized_actor": True,
                           "candidate_sha": SHA, "current_main_sha": SHA,
                           "acceptance_reports_independently_reviewed": True, "owner_approved": True}}


class Tests(unittest.TestCase):
    def test_snapshot_is_only_a_fixture(self):
        self.assertEqual(a.policy_findings(snapshot()), [])

    def test_untrusted_or_incomplete_settings_fail(self):
        cases = [("repository_publication_secret_count", 1), ("repository_secrets_reviewed", False),
                 ("trusted_workflow_sha", "main")]
        for key, value in cases:
            s = snapshot(); s[key] = value
            with self.subTest(key=key): self.assertTrue(a.policy_findings(s))
        cases = [("environment", "required_reviewer_count", 0), ("environment", "prevent_self_review", False),
                 ("environment", "admin_bypass_allowed", True), ("environment", "allowed_refs", ["refs/tags/*"]),
                 ("tag_rules", "restrict_creation", False), ("tag_rules", "restrict_update", False),
                 ("tag_rules", "restrict_deletion", False), ("tag_rules", "bypass_app_ids", [42, 5]),
                 ("oidc", "ref", "refs/tags/*"), ("oidc", "repository", "other/repo"),
                 ("oidc", "workflow_sha", "main"), ("oidc", "environment", ""), ("oidc", "audience", "*"),
                 ("oidc", "workflow_path", "other.yml"),
                 ("controller", "event", "push"), ("controller", "workflow_ref", "refs/tags/v0.25.0"),
                 ("controller", "authorized_actor", False), ("controller", "candidate_sha", "b" * 40),
                 ("controller", "owner_approved", False), ("controller", "acceptance_reports_independently_reviewed", False)]
        for section, key, value in cases:
            s = copy.deepcopy(snapshot()); s[section][key] = value
            with self.subTest(section=section, key=key): self.assertTrue(a.policy_findings(s))

    def test_privileged_workflow_patterns(self):
        text = '''on:\n  workflow_dispatch:\n  push:\n    tags: ["v*"]\npermissions:\n  contents: write\n  packages: write\n  id-token: write\nsteps:\n  - run: gh release create "$tag"\n  - uses: org/repo/.github/workflows/publish.yml@main\n  - run: docker push example\n  - env: {KEY: "${{ secrets.KEY }}"}\n'''
        findings = a.workflow_findings(text, "refs/tags/v0.24.0")
        self.assertTrue({"write-token", "other-write-token", "oidc-token", "historical-tag-privilege",
                         "manual-dispatch-privilege", "direct-release", "unpinned-reusable-workflow",
                         "push-or-package", "external-secret-reference"} <= findings)
        self.assertNotIn("unpinned-reusable-workflow", a.workflow_findings(
            "  uses: org/repo/.github/workflows/publish.yml@" + SHA, a.TRUSTED_REF))

    def test_actual_historical_tags_are_flagged(self):
        tags = subprocess.check_output(["git", "tag", "-l", "v*"], cwd=ROOT, text=True).splitlines()
        if not tags:
            self.skipTest("historical tag refs unavailable; live history assessment remains blocked")
        for tag in tags:
            text = subprocess.check_output(["git", "show", f"{tag}:.github/workflows/release.yml"], cwd=ROOT, text=True)
            with self.subTest(tag=tag):
                self.assertIn("historical-tag-privilege", a.workflow_findings(text, f"refs/tags/{tag}"))
                self.assertIn("direct-release", a.workflow_findings(text, f"refs/tags/{tag}"))


if __name__ == "__main__":
    unittest.main()
