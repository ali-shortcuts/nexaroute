#!/usr/bin/env python3
"""Validate a NexaRoute acceptance report (issue #646).

Checks one JSON report produced by an existing harness -- never a new
harness, and never proof of a live run by itself:

  * conformance canary:  schema "conformance.live-canary.v1"
    (internal/providers/livecanary, `conformance canary`)
  * provider-accept:     schema "nexaroute.provider-acceptance.v1"
    (cmd/gateway, `nexaroute provider-accept`)
  * external-accept:     schema "nexaroute.external-acceptance.v1"
    (cmd/gateway, `nexaroute external-accept`)

Every rule below mirrors a rule already in the Go code that writes the
report (scenario status sets, outcome derivation, totals arithmetic,
the auth_failure 400/401/403 rule, the stream_usage both-tokens rule,
the error_envelope 4xx rule, budget ceilings). A rule the writer does
not enforce is reported as a warning, not an error, so the validator
cannot fail a report the harness itself would legitimately produce.

What this validator does NOT do, by design:

  * It does not prove a run was live. A loopback fixture produces a
    canary report with the same schema as a live run; only the
    base_url / evidence_class tells them apart. With --require-live
    it rejects loopback-class reports outright instead of certifying
    them.
  * It does not check a commit SHA: none of the three report schemas
    carries one. The SHA belongs in the operator's signed run record
    next to the report, and the run record is what a reviewer checks
    against the report's generated_at date.
  * It does not read credentials. It scans the report text for
    credential-shaped material (AWS access-key ids, GitHub tokens,
    PEM private keys, live/project secret keys, raw Bearer tokens)
    and fails the report if any is found, because a sanitized
    report must never contain them. Ordinary fixture labels such
    as "canary-test-secret-1234" do not match those shapes.

Usage:
    python3 scripts/check-acceptance-report.py --report <path> [--kind auto]
    python3 scripts/check-acceptance-report.py --report <path> --require-live

Exit codes: 0 = valid (warnings allowed), 1 = invalid, 2 = usage error.
"""

import argparse
import json
import re
import sys
from datetime import datetime

CANARY_SCHEMA = "conformance.live-canary.v1"
PROVIDER_ACCEPT_SCHEMA = "nexaroute.provider-acceptance.v1"
EXTERNAL_ACCEPT_SCHEMA = "nexaroute.external-acceptance.v1"

CANARY_DIALECTS = {
    "openai_compatible",
    "anthropic_compatible",
    "gemini",
    "azure_openai",
    "vertex",
    "bedrock",
    "openai_responses",
}
CANARY_SCENARIOS = {
    "text",
    "stream",
    "stream_usage",
    "tools",
    "tool_result",
    "structured_output",
    "images",
    "parallel_tools",
    "reasoning",
    "auth_failure",
    "error_envelope",
}
CANARY_STATUSES = {"PASS", "FAIL", "SKIP", "CANCELED"}
PROVIDER_STATUSES = {"passed", "degraded", "failed", "configured", "unsupported"}
MODEL_RESULTS = {"passed", "failed"}
EXTERNAL_STATUSES = {"PASS", "FAIL", "BLOCKED", "NOT_RUN"}
EXTERNAL_CLASSES = {"mixed", "external", "loopback", "none"}

SECRET_PATTERNS = [
    (r"AKIA[0-9A-Z]{16}", "AWS access-key id shape"),
    (r"ghp_[A-Za-z0-9]{36}", "GitHub personal token shape"),
    (r"gho_[A-Za-z0-9]{36}", "GitHub OAuth token shape"),
    (r"github_pat_[A-Za-z0-9_]{22,}", "GitHub fine-grained token shape"),
    (r"-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----", "PEM private key"),
    (r"sk-live-[A-Za-z0-9]{16,}", "live secret-key shape"),
    (r"sk-proj-[A-Za-z0-9]{16,}", "project secret-key shape"),
    (r"Bearer [A-Za-z0-9._~+/=-]{20,}", "raw Bearer token"),
]


class Checker:
    def __init__(self):
        self.errors = []
        self.warnings = []

    def error(self, msg):
        self.errors.append(msg)

    def warn(self, msg):
        self.warnings.append(msg)

    def require(self, cond, msg):
        if not cond:
            self.error(msg)

    def is_int(self, v):
        return isinstance(v, int) and not isinstance(v, bool)

    def is_num(self, v):
        return (isinstance(v, (int, float)) and not isinstance(v, bool)
                and v == v)  # NaN guard

    def check_generated_at(self, report):
        raw = report.get("generated_at")
        if not isinstance(raw, str) or not raw:
            self.error("generated_at is missing or not a string")
            return
        try:
            datetime.fromisoformat(raw.replace("Z", "+00:00"))
        except ValueError:
            self.error("generated_at %r is not RFC3339" % raw)

    def check_secrets(self, raw_text):
        for pattern, label in SECRET_PATTERNS:
            if re.search(pattern, raw_text):
                self.error("report contains credential-shaped material (%s)" % label)

    def is_loopback_url(self, url):
        if not isinstance(url, str):
            return False
        host = url.lower()
        for prefix in ("http://", "https://"):
            if host.startswith(prefix):
                host = host[len(prefix):]
                break
        host = host.split("/")[0].split(":")[0].strip("[]")
        return (host == "localhost" or host == "::1"
                or host.startswith("127.") or host.endswith(".localhost"))


def check_canary(c, report, require_live):
    c.require(report.get("schema") == CANARY_SCHEMA,
              "schema=%r, want %r" % (report.get("schema"), CANARY_SCHEMA))
    c.check_generated_at(report)
    provider = report.get("provider")
    if not isinstance(provider, dict):
        c.error("provider is missing or not an object")
        return
    for field in ("id", "dialect", "base_url", "model"):
        c.require(isinstance(provider.get(field), str) and provider[field],
                  "provider.%s is missing or empty" % field)
    dialect = provider.get("dialect")
    if dialect not in CANARY_DIALECTS:
        c.error("provider.dialect=%r is not one of the #646 dialects "
                "(extend this validator explicitly if a new dialect is "
                "accepted)" % (dialect,))
    scenarios = report.get("scenarios")
    if not isinstance(scenarios, list) or not scenarios:
        c.error("scenarios is missing or empty")
        return
    seen, passes, fails, canceled = set(), 0, 0, 0
    pass_dims, fail_dims = set(), set()
    total_calls = 0
    for sc in scenarios:
        if not isinstance(sc, dict):
            c.error("scenario entry is not an object: %r" % (sc,))
            continue
        sid = sc.get("id")
        if sid in seen:
            c.error("duplicate scenario id %r" % (sid,))
        seen.add(sid)
        if sid not in CANARY_SCENARIOS:
            c.error("unknown scenario id %r" % (sid,))
        status = sc.get("status")
        if status not in CANARY_STATUSES:
            c.error("scenario %r status=%r, want one of %s"
                    % (sid, status, sorted(CANARY_STATUSES)))
            continue
        if status == "PASS":
            passes += 1
            pass_dims.update(sc.get("dimensions") or [])
        elif status == "FAIL":
            fails += 1
            fail_dims.update(sc.get("dimensions") or [])
        elif status == "CANCELED":
            canceled += 1
        calls = sc.get("calls", 0)
        if c.is_int(calls) and calls >= 0:
            total_calls += calls
        else:
            c.error("scenario %r calls=%r is not a non-negative integer" % (sid, calls))
        http_status = sc.get("http_status")
        if status == "PASS":
            if not c.is_int(http_status):
                c.error("scenario %r PASS has no http_status" % (sid,))
            elif sid == "auth_failure" and http_status not in (400, 401, 403):
                c.error("scenario auth_failure PASS with http %d, want 400/401/403"
                        % http_status)
            elif sid == "error_envelope" and not (400 <= http_status < 500):
                c.error("scenario error_envelope PASS with http %d, want 4xx"
                        % http_status)
            elif sid not in ("auth_failure", "error_envelope") and not (200 <= http_status < 300):
                c.error("scenario %r PASS with http %d, want 2xx" % (sid, http_status))
        if sid == "stream_usage" and status == "PASS":
            if not (c.is_int(sc.get("input_tokens")) and sc["input_tokens"] > 0
                    and c.is_int(sc.get("output_tokens")) and sc["output_tokens"] > 0):
                c.error("scenario stream_usage PASS without input_tokens>0 and "
                        "output_tokens>0 (a stream without usage is not a pass)")
    outcome = report.get("outcome")
    want = None
    if canceled > 0:
        want = "CANCELED"
    elif fails > 0 and passes > 0:
        want = "PARTIAL"
    elif fails > 0:
        want = "FAILED"
    elif passes > 0:
        lims = report.get("limitations") or []
        if any("did not finish" in str(x) for x in lims):
            want = "PARTIAL"
        else:
            want = "PASS"
    else:
        want = "NO-EVIDENCE"
    if outcome != want:
        c.error("outcome=%r, want %r from %d PASS / %d FAIL / %d CANCELED "
                "scenarios" % (outcome, want, passes, fails, canceled))
    limits = report.get("limits")
    if not isinstance(limits, dict):
        c.error("limits is missing or not an object")
    else:
        max_calls = limits.get("max_calls")
        calls_used = limits.get("calls_used")
        if c.is_int(max_calls) and c.is_int(calls_used):
            if max_calls <= 0:
                c.error("limits.max_calls=%r, want > 0" % (max_calls,))
            if calls_used > max_calls:
                c.error("limits.calls_used=%d exceeds max_calls=%d"
                        % (calls_used, max_calls))
            if calls_used != total_calls:
                c.warn("limits.calls_used=%d != sum of scenario calls=%d "
                       "(soft check: the harness attributes every call to a "
                       "scenario today, but this is not a writer-enforced rule)"
                       % (calls_used, total_calls))
        observed = limits.get("observed_cost_usd")
        max_cost = limits.get("max_cost_usd")
        if c.is_num(observed) and c.is_num(max_cost) and max_cost > 0 and observed > max_cost:
            c.error("limits.observed_cost_usd=%r exceeds max_cost_usd=%r"
                    % (observed, max_cost))
    verified = report.get("verified_dimensions") or []
    failed_dims_report = report.get("failed_dimensions") or []
    if not set(verified) <= pass_dims:
        c.error("verified_dimensions %r includes dimensions no PASS scenario "
                "verified" % (verified,))
    if set(verified) & set(failed_dims_report):
        c.error("a dimension is both verified and failed: %r"
                % (sorted(set(verified) & set(failed_dims_report)),))
    if not set(failed_dims_report) <= fail_dims:
        c.warn("failed_dimensions %r includes dimensions no FAIL scenario "
               "reported" % (failed_dims_report,))
    if require_live and c.is_loopback_url(provider.get("base_url")):
        c.error("--require-live: provider.base_url %r is loopback; a loopback "
                "report is fixture evidence, not live evidence"
                % (provider.get("base_url"),))
    print("class: %s (provider base_url %r)"
          % ("loopback" if c.is_loopback_url(provider.get("base_url")) else "external",
             provider.get("base_url")))


def check_provider_accept(c, report, require_live):
    c.require(report.get("schema") == PROVIDER_ACCEPT_SCHEMA,
              "schema=%r, want %r" % (report.get("schema"), PROVIDER_ACCEPT_SCHEMA))
    c.check_generated_at(report)
    if report.get("mode") not in ("discover", "test", "discover+test"):
        c.error("mode=%r, want discover, test, or discover+test" % (report.get("mode"),))
    bounds = report.get("bounds")
    if not isinstance(bounds, dict):
        c.error("bounds is missing or not an object")
    else:
        up, mx = bounds.get("upstream_calls"), bounds.get("max_upstream_calls")
        if c.is_int(up) and c.is_int(mx) and up > mx:
            c.error("bounds.upstream_calls=%d exceeds max_upstream_calls=%d" % (up, mx))
    providers = report.get("providers")
    if not isinstance(providers, list):
        c.error("providers is missing or not a list")
        return
    if not providers:
        c.warn("report lists no providers")
    counts = {"passed": 0, "degraded": 0, "failed": 0, "configured": 0, "unsupported": 0}
    disc_att, disc_ok, disc_fail, disc_unsup = 0, 0, 0, 0
    models_tested, models_passed, models_failed, insufficient = 0, 0, 0, 0
    all_loopback = True
    for p in providers:
        if not isinstance(p, dict):
            c.error("provider entry is not an object")
            continue
        for field in ("provider", "protocol", "auth_mode", "base_url"):
            c.require(isinstance(p.get(field), str) and p[field],
                      "provider %r: %s is missing or empty" % (p.get("provider"), field))
        status = p.get("status")
        if status not in PROVIDER_STATUSES:
            c.error("provider %r status=%r, want one of %s"
                    % (p.get("provider"), status, sorted(PROVIDER_STATUSES)))
        else:
            counts[status] += 1
        if not c.is_loopback_url(p.get("base_url")):
            all_loopback = False
        disc = p.get("discovery") or {}
        if disc.get("supported"):
            if disc.get("attempted"):
                disc_att += 1
                if disc.get("ok"):
                    disc_ok += 1
                else:
                    disc_fail += 1
        else:
            disc_unsup += 1
        for m in p.get("models") or []:
            if not isinstance(m, dict):
                c.error("provider %r: model entry is not an object" % (p.get("provider"),))
                continue
            if m.get("result") == "passed":
                models_tested += 1
                models_passed += 1
            elif m.get("result") == "failed":
                models_tested += 1
                models_failed += 1
            else:
                c.error("provider %r model %r result=%r, want passed or failed "
                        "(the writer counts only those two)"
                        % (p.get("provider"), m.get("model"), m.get("result")))
        if p.get("auth_mode") != "none" and not p.get("credential_configured"):
            insufficient += 1
    totals = report.get("totals")
    if not isinstance(totals, dict):
        c.error("totals is missing or not an object")
        return
    checks = [
        ("providers", len(providers)),
        ("passed", counts["passed"]),
        ("degraded", counts["degraded"]),
        ("failed", counts["failed"]),
        ("configured", counts["configured"]),
        ("unsupported", counts["unsupported"]),
        ("discovery_attempted", disc_att),
        ("discovery_succeeded", disc_ok),
        ("discovery_failed", disc_fail),
        ("discovery_unsupported", disc_unsup),
        ("models_tested", models_tested),
        ("models_passed", models_passed),
        ("models_failed", models_failed),
        ("insufficient_credential", insufficient),
    ]
    for field, want in checks:
        if totals.get(field) != want:
            c.error("totals.%s=%r, want %d from the provider entries"
                    % (field, totals.get(field), want))
    if report.get("ok") != (totals.get("failed", 1) == 0 and totals.get("degraded", 1) == 0):
        c.error("ok=%r contradicts totals failed=%r degraded=%r"
                % (report.get("ok"), totals.get("failed"), totals.get("degraded")))
    if require_live and providers and all_loopback:
        c.error("--require-live: every provider base_url is loopback; this is "
                "fixture evidence, not live evidence")


def check_external_accept(c, report, require_live):
    c.require(report.get("schema") == EXTERNAL_ACCEPT_SCHEMA,
              "schema=%r, want %r" % (report.get("schema"), EXTERNAL_ACCEPT_SCHEMA))
    c.check_generated_at(report)
    if report.get("mode") != "live":
        c.error("mode=%r, want live (the writer always sets live; note that "
                "mode alone proves nothing about the targets)" % (report.get("mode"),))
    ev_class = report.get("evidence_class")
    if ev_class not in EXTERNAL_CLASSES:
        c.error("evidence_class=%r, want one of %s"
                % (ev_class, sorted(EXTERNAL_CLASSES)))
    bounds = report.get("bounds")
    if isinstance(bounds, dict):
        up, mx = bounds.get("upstream_calls"), bounds.get("max_upstream_calls")
        if c.is_int(up) and c.is_int(mx) and up > mx:
            c.error("bounds.upstream_calls=%d exceeds max_upstream_calls=%d" % (up, mx))
    checks = report.get("checks")
    if not isinstance(checks, list):
        c.error("checks is missing or not a list")
        return
    if not checks:
        c.warn("report lists no checks")
    counts = {"PASS": 0, "FAIL": 0, "BLOCKED": 0, "NOT_RUN": 0}
    kind_counts, selected, ext_targets, loop_targets = {"provider": 0, "oidc": 0}, [], 0, 0
    has_selected_key = any(isinstance(ch, dict) and "selected" in ch for ch in checks)
    for ch in checks:
        if not isinstance(ch, dict):
            c.error("check entry is not an object")
            continue
        status = ch.get("status")
        if status not in EXTERNAL_STATUSES:
            c.error("check %r status=%r, want one of %s"
                    % (ch.get("id"), status, sorted(EXTERNAL_STATUSES)))
        else:
            counts[status] += 1
        if ch.get("kind") in kind_counts:
            kind_counts[ch["kind"]] += 1
        if has_selected_key:
            is_selected = bool(ch.get("selected"))
        else:
            is_selected = True
        if is_selected:
            selected.append(status)
            if ch.get("target_class") == "external":
                ext_targets += 1
            elif ch.get("target_class") == "loopback":
                loop_targets += 1
    if selected and any(s not in EXTERNAL_STATUSES for s in selected):
        pass  # already reported per check above
    if any(s == "FAIL" for s in selected):
        want_outcome = "FAIL"
    elif any(s == "BLOCKED" for s in selected):
        want_outcome = "BLOCKED"
    elif not selected or all(s == "NOT_RUN" for s in selected):
        want_outcome = "NOT_RUN"
    else:
        want_outcome = "PASS"
    if report.get("outcome") != want_outcome:
        c.error("outcome=%r, want %r from the selected check statuses"
                % (report.get("outcome"), want_outcome))
    if ext_targets > 0 and loop_targets > 0:
        want_class = "mixed"
    elif ext_targets > 0:
        want_class = "external"
    elif loop_targets > 0:
        want_class = "loopback"
    else:
        want_class = "none"
    if has_selected_key and ev_class != want_class:
        c.error("evidence_class=%r, want %r from the selected target classes"
                % (ev_class, want_class))
    totals = report.get("totals")
    if not isinstance(totals, dict):
        c.error("totals is missing or not an object")
        return
    for field, want in [
        ("checks", len(checks)),
        ("passed", counts["PASS"]),
        ("failed", counts["FAIL"]),
        ("blocked", counts["BLOCKED"]),
        ("not_run", counts["NOT_RUN"]),
        ("providers_tested", kind_counts["provider"]),
        ("oidc_checks", kind_counts["oidc"]),
    ]:
        if totals.get(field) != want:
            c.error("totals.%s=%r, want %d from the check entries"
                    % (field, totals.get(field), want))
    if require_live and ev_class in ("loopback", "none"):
        c.error("--require-live: evidence_class=%r; a loopback/none report is "
                "fixture evidence, not live evidence" % (ev_class,))


SCHEMAS = {
    CANARY_SCHEMA: ("canary", check_canary),
    PROVIDER_ACCEPT_SCHEMA: ("provider-accept", check_provider_accept),
    EXTERNAL_ACCEPT_SCHEMA: ("external-accept", check_external_accept),
}


def main(argv):
    ap = argparse.ArgumentParser(description="Validate a NexaRoute acceptance report.")
    ap.add_argument("--report", required=True, help="path to the report JSON")
    ap.add_argument("--kind", default="auto",
                    choices=["auto", "canary", "provider-accept", "external-accept"],
                    help="report kind; auto detects it from the schema field")
    ap.add_argument("--require-live", action="store_true",
                    help="fail loopback-class reports instead of just naming the class")
    args = ap.parse_args(argv)

    try:
        with open(args.report, "r", encoding="utf-8") as fh:
            raw = fh.read()
    except OSError as exc:
        print("error: cannot read %s: %s" % (args.report, exc), file=sys.stderr)
        return 2
    try:
        report = json.loads(raw)
    except json.JSONDecodeError as exc:
        print("error: %s is not valid JSON: %s" % (args.report, exc), file=sys.stderr)
        return 1
    if not isinstance(report, dict):
        print("error: report root is not a JSON object", file=sys.stderr)
        return 1

    schema = report.get("schema")
    if args.kind == "auto":
        if schema not in SCHEMAS:
            print("error: unknown schema %r; pass --kind explicitly" % (schema,),
                  file=sys.stderr)
            return 2
        kind, fn = SCHEMAS[schema]
    else:
        fn = {"canary": check_canary,
              "provider-accept": check_provider_accept,
              "external-accept": check_external_accept}[args.kind]
        kind = args.kind

    c = Checker()
    c.check_secrets(raw)
    fn(c, report, args.require_live)
    for w in c.warnings:
        print("warning: %s" % w)
    if c.errors:
        for e in c.errors:
            print("error: %s" % e)
        print("INVALID: %s report %s (%d errors, %d warnings)"
              % (kind, args.report, len(c.errors), len(c.warnings)))
        return 1
    print("VALID: %s report %s (%d warnings)" % (kind, args.report, len(c.warnings)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
