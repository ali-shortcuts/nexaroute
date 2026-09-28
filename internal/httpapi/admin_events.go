package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/ali-shortcuts/nexaroute/internal/events"
)

const (
	defaultAdminEventSnapshot = 64
	maxAdminEventSnapshot     = 256
)

// adminEventStream exposes a bounded, authenticated SSE feed for operator
// diagnostics. The event bus itself remains bounded and producers never block
// on slow dashboard connections.
func (s *Server) adminEventStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := defaultAdminEventSnapshot
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxAdminEventSnapshot {
			errorJSON(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxAdminEventSnapshot))
			return
		}
		limit = parsed
	}
	snapshot, live, cancel, ok := s.bus.SubscribeSnapshot(limit)
	if !ok {
		errorJSON(w, http.StatusServiceUnavailable, "admin event subscriber capacity reached")
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	// Commit headers even when there is no historical activity. This lets a
	// dashboard establish an idle live stream without waiting for the first
	// gateway event.
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
	emit := func(e events.Event) bool {
		payload, err := json.Marshal(e)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: event\ndata: %s\n\n", payload); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for _, e := range snapshot {
		if !emit(e) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-live:
			if !open || !emit(e) {
				return
			}
		}
	}
}
