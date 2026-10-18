package httpapi

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// Phase 19 eval gate (#510 item 4): in apply mode, a gated task
// pool may only be claimed when a deployment in it has passing
// eval evidence for the gated suite. Without evidence the request
// routes on the route default and the trace shows eval_gated.

func seedEvalRun(t *testing.T, srv *Server, runID, suite, dep string, counts map[eval.Verdict]int) {
	t.Helper()
	err := srv.evaluationSnapshot().Store().Save(eval.Result{
		RunID: runID, SuiteID: suite, SuiteVersion: "v1", DeploymentID: dep,
		Scoreable: true, Counts: counts,
		StartedAt: time.Now(), FinishedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed eval run: %v", err)
	}
}

func gatedServer(t *testing.T) (*Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var codeCalls, ctxCalls atomic.Int64
	codeUp := taskIntelUpstream(t, &codeCalls, "code-answer")
	t.Cleanup(codeUp.Close)
	ctxUp := taskIntelUpstream(t, &ctxCalls, "ctx-answer")
	t.Cleanup(ctxUp.Close)
	cfg := taskIntelTestConfig(codeUp.URL, ctxUp.URL, "apply", "")
	cfg.RouteProfiles[0].TaskEvalGates = map[string]string{"coding": "coding-suite"}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()
	return srv, &codeCalls, &ctxCalls
}

func TestTaskIntelEvalGate_NoEvidenceRoutesDefault(t *testing.T) {
	srv, _, _ := gatedServer(t)
	rr := taskIntelPost(t, srv, "gate-none", taskIntelCodingBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	tev := taskClassifiedEvent(t, srv, "gate-none")
	if tev.TaskIntelOutcome != "eval_gated" {
		t.Fatalf("outcome=%q want eval_gated (summary %q)", tev.TaskIntelOutcome, tev.Message)
	}
	if tev.TaskIntelPool != "code-pool" {
		t.Fatalf("target pool=%q want code-pool (the gated target stays visible)", tev.TaskIntelPool)
	}
	if rev := routeResolvedEvent(t, srv, "gate-none"); rev.Pool != "default-pool" {
		t.Fatalf("route pool=%q want default-pool", rev.Pool)
	}
	if !strings.Contains(tev.Message, "eval gate") {
		t.Fatalf("trace summary must name the gate, got %q", tev.Message)
	}
}

func TestTaskIntelEvalGate_PassingEvidenceApplies(t *testing.T) {
	srv, _, _ := gatedServer(t)
	seedEvalRun(t, srv, "run-pass", "coding-suite", "codeprov/m", map[eval.Verdict]int{eval.VerdictPass: 3})
	rr := taskIntelPost(t, srv, "gate-pass", taskIntelCodingBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	tev := taskClassifiedEvent(t, srv, "gate-pass")
	if tev.TaskIntelOutcome != "applied" {
		t.Fatalf("outcome=%q want applied", tev.TaskIntelOutcome)
	}
	if rev := routeResolvedEvent(t, srv, "gate-pass"); rev.Pool != "code-pool" {
		t.Fatalf("route pool=%q want code-pool", rev.Pool)
	}
}

func TestTaskIntelEvalGate_WrongDeploymentStillGated(t *testing.T) {
	srv, _, _ := gatedServer(t)
	// Passing evidence exists — but for the ctx deployment, which is
	// not in the code pool. The gate must not be satisfied by it.
	seedEvalRun(t, srv, "run-wrong", "coding-suite", "ctxprov/m", map[eval.Verdict]int{eval.VerdictPass: 3})
	rr := taskIntelPost(t, srv, "gate-wrong", taskIntelCodingBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if tev := taskClassifiedEvent(t, srv, "gate-wrong"); tev.TaskIntelOutcome != "eval_gated" {
		t.Fatalf("outcome=%q want eval_gated", tev.TaskIntelOutcome)
	}
}

func TestTaskIntelEvalGate_LatestFailingRunRevokes(t *testing.T) {
	srv, _, _ := gatedServer(t)
	seedEvalRun(t, srv, "run-pass2", "coding-suite", "codeprov/m", map[eval.Verdict]int{eval.VerdictPass: 3})
	seedEvalRun(t, srv, "run-fail2", "coding-suite", "codeprov/m", map[eval.Verdict]int{eval.VerdictPass: 1, eval.VerdictFail: 1})
	rr := taskIntelPost(t, srv, "gate-revoked", taskIntelCodingBody(t), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if tev := taskClassifiedEvent(t, srv, "gate-revoked"); tev.TaskIntelOutcome != "eval_gated" {
		t.Fatalf("outcome=%q want eval_gated after a failing latest run", tev.TaskIntelOutcome)
	}
}
