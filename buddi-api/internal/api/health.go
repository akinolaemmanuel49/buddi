package api

import (
	"context"
	"net/http"
	"time"
)

type healthResponse struct {
	Status string `json:"status"`
	Uptime string `json:"uptime,omitempty"`
}

var startedAt = time.Now()

// handleHealth is the liveness probe: it answers as long as the process can
// serve requests, without touching dependencies.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, http.StatusOK, healthResponse{
		Status: "ok",
		Uptime: time.Since(startedAt).Round(time.Second).String(),
	})
}

// handleReady is the readiness probe: it fails when the database is unreachable
// so the orchestrator stops routing traffic to this instance.
func (a *API) handleReady(w http.ResponseWriter, r *http.Request) {
	if a.db == nil {
		WriteJSON(w, r, http.StatusServiceUnavailable, healthResponse{Status: "no database configured"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := a.db.Ping(ctx); err != nil {
		a.log.WarnContext(r.Context(), "readiness check failed", "error", err)
		WriteJSON(w, r, http.StatusServiceUnavailable, healthResponse{Status: "database unavailable"})

		return
	}

	WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ready"})
}
