package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/obs"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// Catches: a restart rerunning a stage early, a missed run never catching up, and windowed stages running outside
// their window (or skipping a day).
func TestNextRun(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	early, _ := config.ParseWindow("02:00-08:00")
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	now := at("2026-10-05T06:00:00Z") // 23:00 PDT
	cases := []struct {
		name string
		last time.Time
		w    config.Window
		want time.Time
	}{
		{"never ran", time.Time{}, config.Window{}, now},
		{"ran 5 min ago", now.Add(-5 * time.Minute), config.Window{}, now.Add(10 * time.Minute)},
		{"overdue runs now", now.Add(-time.Hour), config.Window{}, now},
		{"outside window waits for 02:00 PDT", now.Add(-time.Hour), early, at("2026-10-05T09:00:00Z")},
		{"inside window", at("2026-10-05T09:30:00Z"), early, at("2026-10-05T09:45:00Z")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := now
			if c.name == "inside window" {
				n = at("2026-10-05T09:40:00Z")
			}
			if got := NextRun(c.last, 15*time.Minute, c.w, la, n); !got.Equal(c.want) {
				t.Fatalf("NextRun = %s, want %s", got, c.want)
			}
		})
	}
}

type fakeStage struct {
	run func(ctx context.Context, env *stage.Env, rep *stage.Report) error
}

func (fakeStage) Name() string { return "fake" }
func (f fakeStage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	return f.run(ctx, env, rep)
}

// Catches: outcomes mis-recorded — alerts raised before an error lost, a panic killing the service instead of
// failing the run, or the store not reflecting what /health shows.
func TestRunOnceOutcomes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Runner{
		Store: st, Log: obs.NewLogger(io.Discard, "text", "error"), Metrics: obs.NewMetrics(),
		Now:    func() time.Time { return time.Now().UTC() },
		NewEnv: func(stage.Stage, *slog.Logger) *stage.Env { return &stage.Env{} },
	}
	cases := []struct {
		name string
		run  func(context.Context, *stage.Env, *stage.Report) error
		want stage.Outcome
	}{
		{"ok", func(context.Context, *stage.Env, *stage.Report) error { return nil }, stage.OK},
		{"alert", func(_ context.Context, _ *stage.Env, rep *stage.Report) error { rep.Alert("cpu transcode"); return nil }, stage.Alert},
		{"alert then error", func(_ context.Context, _ *stage.Env, rep *stage.Report) error {
			rep.Alert("drift")
			return errors.New("jellyfin down")
		}, stage.Error},
		{"panic", func(context.Context, *stage.Env, *stage.Report) error { panic("boom") }, stage.Error},
	}
	for _, c := range cases {
		rep, got := r.RunOnce(context.Background(), fakeStage{run: c.run})
		if got != c.want {
			t.Errorf("%s: outcome %s, want %s", c.name, got, c.want)
		}
		run, _, _ := st.LastRun(context.Background(), "fake")
		if run.Outcome != string(c.want) || run.FinishedAt.IsZero() {
			t.Errorf("%s: stored run %+v", c.name, run)
		}
		if c.name == "alert then error" && len(rep.Alerts) != 1 {
			t.Errorf("alert raised before the error was lost: %v", rep.Alerts)
		}
	}
}
