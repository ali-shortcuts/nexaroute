package httpapi

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// securitySnapshotMaxBytes bounds one live security-store snapshot
// served by this endpoint. It mirrors the backup bundle's per-entry
// limit (cmd/gateway/backup.go maxBackupEntryBytes, 2 GiB): a
// snapshot the backup CLI could not bundle must never be streamed
// as if it were usable. Real audit stores are orders of magnitude
// smaller (audit events are capped by config), so hitting this bound
// means a corrupt or hostile store, not a large legitimate one.
const securitySnapshotMaxBytes = 1 << 31

// snapshotBoundedBuffer fails rather than buffering past its limit,
// so a snapshot that exceeds the bound is refused whole instead of
// being streamed truncated (a truncated bbolt file would look like a
// corrupt store to restore, not like a refused snapshot).
type snapshotBoundedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (w *snapshotBoundedBuffer) Write(p []byte) (int, error) {
	if int64(w.buf.Len())+int64(len(p)) > w.limit {
		return 0, fmt.Errorf("security snapshot exceeds the %d-byte limit", w.limit)
	}
	return w.buf.Write(p)
}

// adminSecuritySnapshot serves GET /admin/api/security-snapshot: a
// consistent, point-in-time copy of the security/audit store taken
// from the gateway's own already-open store handle. It is the ONLY
// supported live-snapshot path for the file-backed store.
//
// Why in-process: a second process opening the bbolt file read-only
// while the gateway runs fails with a lock timeout — the gateway
// holds the exclusive flock for the whole process lifetime — so the
// old `nexaroute backup` (separate process) cannot snapshot a live
// gateway at all. This handler instead calls BackupTo on the live
// handle (a bbolt read transaction), which is consistent even while
// writes proceed, and verifies the audit chain BEFORE streaming, so
// a tampered store is refused whole (503) rather than exported.
//
// Authorization is exactly the existing admin machinery: the
// middleware authenticates (OIDC session or emergency admin key)
// and requires audit.read for GET on this path (see
// adminPermissionForRequest); this handler adds no credential
// mechanism and logs nothing about the snapshot contents. A broken
// audit chain has NO override over HTTP — an operator who must back
// up a broken-chain store stops the gateway and uses the CLI's
// --allow-broken-audit-chain override on the stopped path. Backends
// without a snapshot capability (PostgreSQL, Redis) answer 501:
// their state must be dumped with backend-native tooling, never by
// copying files. The response is marked no-store so no proxy caches
// a copy of the security database.
func (s *Server) adminSecuritySnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.securityErr != nil || s.securityDB == nil {
		errorJSON(w, http.StatusServiceUnavailable, "admin security store unavailable")
		return
	}
	snap, ok := s.securityDB.(securitystore.Snapshotter)
	if !ok {
		errorJSON(w, http.StatusNotImplemented, "security store backend does not support live snapshots")
		return
	}
	report, err := s.securityDB.VerifyAuditChain(r.Context())
	if err != nil {
		errorJSON(w, http.StatusServiceUnavailable, "audit chain verification unavailable")
		return
	}
	if !report.Verified {
		problem := "unknown problem"
		if len(report.Problems) > 0 {
			problem = report.Problems[0]
		}
		errorJSON(w, http.StatusServiceUnavailable, fmt.Sprintf("audit chain verification failed (%s); live snapshot refused", problem))
		return
	}
	buf := &snapshotBoundedBuffer{limit: securitySnapshotMaxBytes}
	if err := snap.BackupTo(r.Context(), buf); err != nil {
		errorJSON(w, http.StatusServiceUnavailable, fmt.Sprintf("security snapshot failed: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.buf.Bytes())
}
