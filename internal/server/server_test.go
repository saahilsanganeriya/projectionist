package server

import (
	"context"
	"errors"
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

// guard is a stage whose run ends as told: "alert", "error", "interrupted" (a shutdown cuts it off) or "ok".
type guard string

func (guard) Name() string { return "guard" }
func (g guard) Run(_ context.Context, _ *stage.Env, rep *stage.Report) error {
	switch g {
	case "alert":
		rep.Alert("Plex is transcoding on the CPU")
	case "error", "interrupted":
		return errors.New("jellyfin down")
	}
	return nil
}

// Catches: the owner's only alert path going silent — the DSM health task e-mails only on a non-2xx /health, so an
// alerting or failed run must turn it 503 and the next clean run must clear it — and a run a shutdown cut off
// changing it either way: it reports the last finished run's outcome.
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
	shutdown, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		run  guard
		want int
	}{{"alert", 503}, {"interrupted", 503}, {"ok", 200}, {"interrupted", 200}, {"error", 503}} {
		ctx := context.Background()
		if c.run == "interrupted" {
			ctx = shutdown
		}
		runner.RunOnce(ctx, c.run)
		if got := code(); got != c.want {
			t.Fatalf("after a run ending %s, /health = %d, want %d", c.run, got, c.want)
		}
	}
}
