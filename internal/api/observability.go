package api

import (
	"fmt"
	"net/http"
	"strconv"
)

func (s *Server) handleAudit(writer http.ResponseWriter, request *http.Request) {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	entries, err := s.store.ListAuditEntries(request.Context(), limit)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "audit", "could not load audit entries")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"audit_entries": entries})
}

func (s *Server) handleMetrics(writer http.ResponseWriter, request *http.Request) {
	diagnostics, err := s.store.Diagnostics(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "metrics", "metrics unavailable")
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(writer, "# HELP veqri_uptime_seconds Core process uptime.\n# TYPE veqri_uptime_seconds gauge\nveqri_uptime_seconds %.0f\n", s.timeSinceStart().Seconds())
	for _, metric := range []struct {
		name  string
		value int
	}{
		{"veqri_tasks_total", diagnostics.Counts["tasks"]},
		{"veqri_events_total", diagnostics.Counts["events"]},
		{"veqri_pending_tasks", diagnostics.PendingTasks},
		{"veqri_pending_approvals", diagnostics.PendingApprovals},
		{"veqri_failed_deliveries", diagnostics.FailedDeliveries},
	} {
		_, _ = fmt.Fprintf(writer, "# TYPE %s gauge\n%s %d\n", metric.name, metric.name, metric.value)
	}
}

func (s *Server) handleDiagnostics(writer http.ResponseWriter, request *http.Request) {
	diagnostics, err := s.store.Diagnostics(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "diagnostics", "diagnostics failed")
		return
	}
	writeJSON(writer, http.StatusOK, diagnostics)
}
