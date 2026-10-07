package httpapi

import (
	"encoding/csv"
	"net/http"
	"strconv"
)

func (s *Server) adminIdentityUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows := s.identityUsage.Snapshot()
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=nexaroute-identity-usage.csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"identity", "tenant_id", "project_id", "team_id", "requests", "input_tokens", "output_tokens"})
		for _, row := range rows {
			_ = cw.Write([]string{row.Identity, row.TenantID, row.ProjectID, row.TeamID, strconv.FormatInt(row.Requests, 10), strconv.FormatInt(row.Input, 10), strconv.FormatInt(row.Output, 10)})
		}
		cw.Flush()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}
