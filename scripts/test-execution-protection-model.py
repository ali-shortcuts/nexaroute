#!/usr/bin/env python3
"""Illustrative negative cases; NOT a GitHub policy emulator or enforcement test."""
import subprocess
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[1]
LEGACY = ".github/workflows/release.yml"
CONTROLLER = ".github/workflows/release-controller.yml"  # Proposed, not implemented.
RELEASE_APP = "fixture-release-app"
OPERATOR = "fixture-operator"


def hypothetical_execution(workflow, event, actor):
    # Illustrative policy: quarantine the legacy path to workflow_call and an
    # approved actor; a separate proposed controller is dispatch-only.
    # Actual Actions policies are not configured or tested here. Event/actor
    # rules alone do not restrict refs or direct REST API release operations.
    if workflow == LEGACY:
        return event == "workflow_call" and actor == OPERATOR
    if workflow == CONTROLLER:
        return event == "workflow_dispatch" and actor == OPERATOR
    return True  # Ordinary CI deliberately outside this example's scope.


def hypothetical_tag_operation(actor, operation):
    # Illustrates a later, explicitly approved app bypass, not the initial
    # proposed empty bypass list or any actual ruleset.
    return operation in ("create", "update", "delete") and actor == RELEASE_APP


def hypothetical_environment_secret(job_environment):
    return job_environment == "release-publication"


def hypothetical_cloud_trust(repo, ref, environment, workflow_sha):
    return (repo, ref, environment, workflow_sha) == (
        "ali-shortcuts/nexaroute", "refs/heads/main", "release-publication", "a" * 40)


class ModelTests(unittest.TestCase):
    def test_historical_publisher_push_blocked_by_proposed_policy(self):
        tags = subprocess.check_output(["git", "tag", "-l", "v*"], cwd=ROOT, text=True).splitlines()
        if not tags:
            self.skipTest("no historical tags; live or historical behavior not established")
        for tag in tags:
            workflow = subprocess.check_output(["git", "show", f"{tag}:{LEGACY}"], cwd=ROOT, text=True)
            with self.subTest(tag=tag):
                self.assertIn("contents: write", workflow)
                self.assertIn("gh release create", workflow)
                self.assertFalse(hypothetical_execution(LEGACY, "push", OPERATOR))
                self.assertFalse(hypothetical_execution(LEGACY, "push", "unapproved-actor"))

    def test_actor_event_and_tag_negatives(self):
        self.assertFalse(hypothetical_execution(LEGACY, "workflow_dispatch", "unapproved-actor"))
        self.assertFalse(hypothetical_execution(LEGACY, "workflow_dispatch", OPERATOR))
        # The quarantine still allows workflow_call if an authorized future
        # definition adds it; this is why the model is not a security boundary.
        self.assertTrue(hypothetical_execution(LEGACY, "workflow_call", OPERATOR))
        self.assertFalse(hypothetical_execution(CONTROLLER, "workflow_dispatch", "unapproved-actor"))
        self.assertFalse(hypothetical_tag_operation("unapproved-actor", "create"))
        self.assertFalse(hypothetical_tag_operation("unapproved-actor", "update"))
        self.assertFalse(hypothetical_tag_operation("unapproved-actor", "delete"))
        self.assertTrue(hypothetical_execution(".github/workflows/ci.yml", "push", "contributor"))

    def test_environment_and_oidc(self):
        self.assertFalse(hypothetical_environment_secret(None))
        self.assertFalse(hypothetical_environment_secret("other"))
        self.assertFalse(hypothetical_cloud_trust("ali-shortcuts/nexaroute", "refs/tags/v0.24.0", "release-publication", "a" * 40))
        self.assertFalse(hypothetical_cloud_trust("ali-shortcuts/nexaroute", "refs/heads/main", "release-publication", "b" * 40))

    def test_policy_does_not_block_direct_rest_release_or_filter_refs(self):
        # Execution policies govern workflow runs, not direct API publication;
        # an allowed manual dispatcher may still select an untrusted ref.
        self.assertTrue(hypothetical_execution("external-rest-api", "release_create", "write-user"))
        self.assertTrue(hypothetical_execution(CONTROLLER, "workflow_dispatch", OPERATOR))
        # Separate controller/ref, tag and identity controls are mandatory.


if __name__ == "__main__":
    unittest.main()
