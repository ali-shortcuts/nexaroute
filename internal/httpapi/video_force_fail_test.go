package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type forceFailControlStub struct {
	calls     int
	jobID     string
	attempted bool
	confirmed bool
}

func (s *forceFailControlStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	errorJSON(w, http.StatusNotFound, "not found")
}

func (s *forceFailControlStub) ForceFail(_ context.Context, jobID string) (bool, bool, error) {
	s.calls++
	s.jobID = jobID
	return s.attempted, s.confirmed, nil
}

func serveForceFailTestRequest(t *testing.T, srv *Server, role authz.Role, body string) *httptest.ResponseRecorder {
	t.Helper()
	cookieValue := installTestOIDCSession(t, srv, role)
	csrf := strings.Repeat("a", 43)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/video/jobs/job-123/force-fail", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:19001"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://gateway")
	req.Header.Set("X-NexaRoute-CSRF", csrf)
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookieValue})
	req.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: csrf})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestAdminVideoForceFailAuthorizationConfirmationAndAudit(t *testing.T) {
	t.Run("operator is audited and cancellation outcome is explicit", func(t *testing.T) {
		srv := testGateway(t, config.Default())
		control := &forceFailControlStub{attempted: true, confirmed: false}
		srv.AttachVideoHandler(control)
		w := serveForceFailTestRequest(t, srv, authz.RoleOperator, `{"confirm_job_id":"job-123"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("force-fail status=%d body=%s", w.Code, w.Body.String())
		}
		if control.calls != 1 || control.jobID != "job-123" {
			t.Fatalf("force control calls/job=%d/%q", control.calls, control.jobID)
		}
		var response struct {
			State       string `json:"state"`
			Attempted   bool   `json:"provider_cancellation_attempted"`
			Confirmed   bool   `json:"provider_cancellation_confirmed"`
			Message     string `json:"message"`
			ForceFailed bool   `json:"operator_force_failed"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.State != "failed" || !response.ForceFailed || !response.Attempted || response.Confirmed || !strings.Contains(response.Message, "remote work may continue") {
			t.Fatalf("force-fail response is incomplete or misleading: %+v", response)
		}
		events, err := srv.securityDB.AuditRecords(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		var authorized, completed bool
		for _, event := range events {
			if event.Path != "/admin/api/video/jobs/job-123/force-fail" || event.Actor != "user-123" || event.Permission != string(authz.ManageVideo) {
				continue
			}
			if event.Action == "admin.operation.authorized" && event.Outcome == "attempt" {
				authorized = true
			}
			if event.Action == "admin.operation.completed" && event.Outcome == "success" && event.Status == http.StatusOK {
				completed = true
			}
		}
		if !authorized || !completed {
			t.Fatalf("durable force-fail audit missing authorized/completed records: authorized=%v completed=%v events=%+v", authorized, completed, events)
		}
	})

	t.Run("viewer is denied without mutation", func(t *testing.T) {
		srv := testGateway(t, config.Default())
		control := &forceFailControlStub{attempted: true, confirmed: true}
		srv.AttachVideoHandler(control)
		w := serveForceFailTestRequest(t, srv, authz.RoleViewer, `{"confirm_job_id":"job-123"}`)
		if w.Code != http.StatusForbidden || control.calls != 0 {
			t.Fatalf("unauthorized force-fail status/calls=%d/%d body=%s", w.Code, control.calls, w.Body.String())
		}
	})

	t.Run("confirmation must match exact job ID", func(t *testing.T) {
		srv := testGateway(t, config.Default())
		control := &forceFailControlStub{}
		srv.AttachVideoHandler(control)
		w := serveForceFailTestRequest(t, srv, authz.RoleOperator, `{"confirm_job_id":"different-job"}`)
		if w.Code != http.StatusBadRequest || control.calls != 0 {
			t.Fatalf("wrong confirmation status/calls=%d/%d body=%s", w.Code, control.calls, w.Body.String())
		}
	})
}
