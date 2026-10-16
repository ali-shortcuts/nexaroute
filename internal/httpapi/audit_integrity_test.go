package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Issue #428 (audit F4): the audit read path must surface chain integrity.
// Before the fix, GET /admin/api/audit served records with no verification
// at all — a record rewritten below the store (actor "mallory") looked
// clean in the admin UI until someone ran the CLI verifier.

type brokenChainBackend struct {
	securitystore.Backend
}

func (b brokenChainBackend) VerifyAuditChain(context.Context) (securitystore.ChainReport, error) {
	return securitystore.ChainReport{
		TotalRecords: 2, ChainedRecords: 2, Verified: false,
		Problems: []string{"audit record seq 1 hash mismatch"},
	}, nil
}

func serveAudit(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	srv.adminAuditRecords(w, httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/audit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("audit read status=%d body=%s", w.Code, w.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestAdminAuditReadReportsIntegrity(t *testing.T) {
	srv := testGateway(t, config.Default())
	if err := srv.securityDB.AppendAudit(context.Background(), securitystore.AuditEvent{
		Timestamp: time.Now().UTC(), Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("healthy chain reports ok", func(t *testing.T) {
		payload := serveAudit(t, srv)
		integrity, ok := payload["integrity"].(map[string]any)
		if !ok {
			t.Fatalf("response has no integrity block: %v", payload)
		}
		if integrity["status"] != "ok" || integrity["verified"] != true {
			t.Fatalf("healthy chain must report ok/verified, got %v", integrity)
		}
		if events, ok := payload["events"].([]any); !ok || len(events) != 1 {
			t.Fatalf("events missing: %v", payload["events"])
		}
	})

	t.Run("tampered chain reports failed, not silent data", func(t *testing.T) {
		orig := srv.securityDB
		srv.securityDB = brokenChainBackend{orig}
		defer func() { srv.securityDB = orig }()
		payload := serveAudit(t, srv)
		integrity, ok := payload["integrity"].(map[string]any)
		if !ok {
			t.Fatalf("response has no integrity block: %v", payload)
		}
		if integrity["status"] != "failed" || integrity["verified"] != false {
			t.Fatalf("tampered chain must report failed, got %v", integrity)
		}
		problems, ok := integrity["problems"].([]any)
		if !ok || len(problems) == 0 {
			t.Fatalf("failed integrity must carry the problems, got %v", integrity)
		}
	})
}
