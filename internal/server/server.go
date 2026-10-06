// Package server exposes /health and /metrics.
//
// /health answers 503 while any stage's latest finished run raised an alert or failed, so a one-line DSM task
// (curl -fsS http://127.0.0.1:8097/health) keeps DSM's "email me when a task fails" as the alert channel. A run a
// shutdown or restart cut off is not finished: the one before it stands (stage.Interrupted).
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"projectionist/internal/obs"
	"projectionist/internal/scheduler"
	"projectionist/internal/store"
)

type health struct {
	Status string                      `json:"status"` // ok | degraded
	Stages map[string]scheduler.Status `json:"stages"`
	Store  string                      `json:"store,omitempty"`
}

func Handler(runner *scheduler.Runner, st *store.Store, metrics *obs.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		h := health{Status: "ok", Stages: runner.Snapshot()}
		for _, s := range h.Stages {
			if s.Outcome == "alert" || s.Outcome == "error" {
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
