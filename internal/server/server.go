// Package server exposes /health and /metrics.
//
// /health answers 503 while, for any stage, the run in progress has raised an alert, or the latest finished run
// raised an alert or failed and no later run has been running for cleanRunSupersedes without an alert. So a one-line
// DSM task (curl -fsS http://127.0.0.1:8097/health) keeps DSM's "email me when a task fails" as the alert channel,
// without e-mailing for hours about an old outcome while a long run goes clean. A run a shutdown or restart cut off is
// not finished: the one before it stands (stage.Interrupted) until a new run supersedes it.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"projectionist/internal/obs"
	"projectionist/internal/scheduler"
	"projectionist/internal/store"
)

// cleanRunSupersedes is how long a run in progress must go without an alert before the latest finished run's alert or
// failure stops counting.
const cleanRunSupersedes = 30 * time.Minute

type health struct {
	Status string                      `json:"status"` // ok | degraded
	Stages map[string]scheduler.Status `json:"stages"`
	Store  string                      `json:"store,omitempty"`
}

func Handler(runner *scheduler.Runner, st *store.Store, metrics *obs.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		h := health{Status: "ok", Stages: runner.Snapshot()}
		now := runner.Now()
		for _, s := range h.Stages {
			superseded := s.Running && now.Sub(s.LastStart) >= cleanRunSupersedes
			if len(s.RunAlerts) > 0 || (s.Outcome == "alert" || s.Outcome == "error") && !superseded {
				h.Status = "degraded"
			}
		}
		if err := st.DB().PingContext(r.Context()); err != nil {
			h.Status, h.Store = "degraded", err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		if h.Status != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(h)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		metrics.WriteText(w)
	})
	return mux
}

// Serve runs the HTTP server until ctx ends.
func Serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.WithoutCancel(ctx))
	}()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
