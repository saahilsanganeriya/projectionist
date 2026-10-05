package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"projectionist/internal/obs"
	"projectionist/internal/scheduler"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

type alerting struct{ alert bool }

func (alerting) Name() string { return "guard" }
func (a alerting) Run(_ context.Context, _ *stage.Env, rep *stage.Report) error {
	if a.alert {
		rep.Alert("Plex is transcoding on the CPU")
	}
	return nil
}

// Catches: the owner's only alert path going silent — the DSM health task e-mails only on a non-2xx /health,
// so an alerting run must turn it 503 and the next clean run must clear it.
func TestHealthFollowsLatestRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := &scheduler.Runner{
		Store: st, Log: obs.NewLogger(io.Discard, "text", "error"), Metrics: obs.NewMetrics(),
		Now:    func() time.Time { return time.Now().UTC() },
		NewEnv: func(stage.Stage, *slog.Logger) *stage.Env { return &stage.Env{} },
	}
	h := Handler(runner, st, runner.Metrics)
	code := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		return rec.Code
	}
	runner.RunOnce(context.Background(), alerting{alert: true})
	if got := code(); got != http.StatusServiceUnavailable {
		t.Fatalf("after an alert /health = %d, want 503", got)
	}
	runner.RunOnce(context.Background(), alerting{})
	if got := code(); got != http.StatusOK {
		t.Fatalf("after a clean run /health = %d, want 200", got)
	}
}
