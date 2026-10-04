package api

import (
	"context"
	"net/http"
	"time"
)

const readyTimeout = 2 * time.Second

// live answers whether the process is up and serving HTTP. It touches no
// dependencies, so a database outage never gets the app restarted.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready answers whether the app can serve real traffic. It runs a query on
// the database and fails closed: any error, or no answer within readyTimeout,
// is a 503.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	var one int
	if err := s.db.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		logFrom(r.Context()).Error("readiness: postgres", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "postgres": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "postgres": "ok"})
}
