package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

type videoForceFailer interface {
	ForceFail(context.Context, string) (attempted, confirmed bool, err error)
}

func (s *Server) adminVideoForceFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	jobID := r.PathValue("id")
	if jobID == "" || strings.TrimSpace(jobID) != jobID || strings.ContainsAny(jobID, "/\\") {
		errorJSON(w, http.StatusBadRequest, "invalid video job ID")
		return
	}
	defer r.Body.Close()
	var in struct {
		ConfirmJobID string `json:"confirm_job_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || in.ConfirmJobID != jobID {
		errorJSON(w, http.StatusBadRequest, "confirm_job_id must exactly match the video job ID")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		errorJSON(w, http.StatusBadRequest, "request body must contain one JSON object")
		return
	}
	force, ok := s.videoHandler.(videoForceFailer)
	if !ok {
		errorJSON(w, http.StatusServiceUnavailable, "video force-fail control is unavailable")
		return
	}
	attempted, confirmed, err := force.ForceFail(r.Context(), jobID)
	if err != nil {
		switch {
		case errors.Is(err, queue.ErrNotFound):
			errorJSON(w, http.StatusNotFound, "video job not found")
		case errors.Is(err, orchestrator.ErrForceFailNotActive), errors.Is(err, queue.ErrTerminalJob):
			errorJSON(w, http.StatusConflict, "video job is no longer active")
		default:
			errorJSON(w, http.StatusServiceUnavailable, "video job could not be force-failed")
		}
		return
	}
	message := "Job was marked failed and its reservation was released."
	if attempted && confirmed {
		message += " The provider acknowledged cancellation."
	} else if attempted {
		message += " Provider cancellation was not confirmed; remote work may continue and costs may accrue."
	} else {
		message += " No provider cancellation could be attempted; if remote work was already submitted, it may continue and costs may accrue."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id": jobID, "state": "failed", "operator_force_failed": true,
		"provider_cancellation_attempted": attempted,
		"provider_cancellation_confirmed": confirmed,
		"message":                         message,
	})
}
