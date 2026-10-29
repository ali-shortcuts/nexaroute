package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// brokenChainStore wraps a real store but reports a broken chain, so
// the handler's chain gate can be tested without tampering with bytes.
type brokenChainStore struct {
	*securitystore.Store
}

func (b brokenChainStore) VerifyAuditChain(context.Context) (securitystore.ChainReport, error) {
	return securitystore.ChainReport{TotalRecords: 1, ChainedRecords: 1, Verified: false, Problems: []string{"audit record seq 0 hash mismatch"}}, nil
}

// noSnapshotBackend embeds only the Backend interface, so it has no
// BackupTo method: the shape of the PostgreSQL/Redis adapters for the
// handler's type assertion.
type noSnapshotBackend struct {
	securitystore.Backend
}

func serveSecuritySnapshot(t *testing.T, srv *Server, method string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.adminSecuritySnapshot(w, httptest.NewRequest(method, "http://gateway/admin/api/security-snapshot", nil))
	return w
}

func openSnapshot(t *testing.T, body []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "security")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	ro, err := securitystore.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("snapshot does not open read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()
	report, err := ro.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("snapshot chain not verified: %+v", report)
	}
}

func TestAdminSecuritySnapshotServesVerifiableStore(t *testing.T) {
	srv := testGateway(t, config.Default())
	if err := srv.securityDB.AppendAudit(context.Background(), securitystore.AuditEvent{
		Timestamp: time.Now().UTC(), Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type %q, want application/octet-stream", ct)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot response must be no-store, got %q", w.Header().Get("Cache-Control"))
	}
	if len(w.Body.Bytes()) == 0 {
		t.Fatal("empty snapshot body")
	}
	openSnapshot(t, w.Body.Bytes())
}

func TestAdminSecuritySnapshotConcurrentWritesStayVerifiable(t *testing.T) {
	srv := testGateway(t, config.Default())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = srv.securityDB.AppendAudit(context.Background(), securitystore.AuditEvent{
				Timestamp: time.Now().UTC(), Actor: "writer", Action: "concurrent", Outcome: "success", Status: 200,
			})
		}
	}()
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	wg.Wait()
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot during concurrent writes: status=%d body=%s", w.Code, w.Body.String())
	}
	openSnapshot(t, w.Body.Bytes())
}

func TestAdminSecuritySnapshotRefusesBrokenChain(t *testing.T) {
	srv := testGateway(t, config.Default())
	orig, ok := srv.securityDB.(*securitystore.Store)
	if !ok {
		t.Fatalf("test gateway store is %T, want *securitystore.Store", srv.securityDB)
	}
	srv.securityDB = brokenChainStore{Store: orig}
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken chain snapshot: status=%d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "audit chain") {
		t.Fatalf("broken chain refusal must name the audit chain, got %s", w.Body.String())
	}
}

func TestAdminSecuritySnapshotUnsupportedBackendIs501(t *testing.T) {
	srv := testGateway(t, config.Default())
	srv.securityDB = noSnapshotBackend{Backend: srv.securityDB}
	w := serveSecuritySnapshot(t, srv, http.MethodGet)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("non-snapshot backend: status=%d, want 501", w.Code)
	}
	if strings.Contains(w.Body.String(), "security.db") {
		t.Fatalf("501 body must not leak store paths, got %s", w.Body.String())
	}
}

// Through the real Handler() middleware stack, not the handler
// function directly: this is what proves the route's permission
// mapping (audit.read) and the existing admin authentication actually
// gate the endpoint. The direct-handler tests above cannot catch a
// missing or wrong matrix entry.
func TestAdminSecuritySnapshotThroughHandlerAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "snapshot-test-key"
	cfg.Admin.EmergencyAccessEnabled = true
	srv := testGateway(t, cfg)
	if err := srv.securityDB.AppendAudit(context.Background(), securitystore.AuditEvent{
		Timestamp: time.Now().UTC(), Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}
	serve := func(key, remote string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/security-snapshot", nil)
		req.RemoteAddr = remote
		if key != "" {
			req.Header.Set("x-admin-key", key)
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	// Emergency key from loopback: 200 with a verifiable store.
	w := serve("snapshot-test-key", "127.0.0.1:10001")
	if w.Code != http.StatusOK {
		t.Fatalf("emergency-key snapshot through handler: status=%d body=%s", w.Code, w.Body.String())
	}
	openSnapshot(t, w.Body.Bytes())
	// No key and wrong key: 401, never the snapshot bytes.
	if w := serve("", "127.0.0.2:10001"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no-key snapshot through handler: status=%d, want 401", w.Code)
	}
	if w := serve("wrong-key", "127.0.0.3:10001"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key snapshot through handler: status=%d, want 401", w.Code)
	}
}

func TestAdminSecuritySnapshotViewerSessionForbiddenThroughHandler(t *testing.T) {
	srv := testGateway(t, config.Default())
	cookieValue := installTestOIDCSession(t, srv, authz.RoleViewer)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/security-snapshot", nil)
	req.RemoteAddr = "127.0.0.1:10001"
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookieValue})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer session snapshot through handler: status=%d, want 403", rr.Code)
	}
}

func TestAdminSecuritySnapshotRejectsNonGet(t *testing.T) {
	srv := testGateway(t, config.Default())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		w := serveSecuritySnapshot(t, srv, method)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s snapshot: status=%d, want 405", method, w.Code)
		}
	}
}
