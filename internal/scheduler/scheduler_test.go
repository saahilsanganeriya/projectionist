package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
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
	run  func(ctx context.Context, env *stage.Env, rep *stage.Report) error
	name string
}

func (f fakeStage) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}
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

// Catches: triggers during a run piling up into several reruns (or being lost), an idle trigger waiting for the
// cadence instead of running now, and a trigger running a windowed stage outside its window.
func TestTrigger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) { // fake clock: starts 2000-01-01 00:00 UTC
		st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var mu sync.Mutex // runs append from the scheduler's goroutines; fake-time sleeps order nothing for -race
		var busyRuns, nightRuns []time.Time
		runs := func() (busy, night []time.Time) {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(busyRuns), slices.Clone(nightRuns)
		}
		release := make(chan struct{})
		night0200, _ := config.ParseWindow("02:00-08:00")
		s := &Scheduler{Loc: time.UTC, Runner: &Runner{
			Store: st, Log: obs.NewLogger(io.Discard, "text", "error"), Metrics: obs.NewMetrics(),
			Now:    func() time.Time { return time.Now().UTC() },
			NewEnv: func(stage.Stage, *slog.Logger) *stage.Env { return &stage.Env{} },
		}}
		s.Jobs = []Job{
			{Stage: fakeStage{name: "busy", run: func(ctx context.Context, _ *stage.Env, _ *stage.Report) error {
				mu.Lock()
				busyRuns = append(busyRuns, time.Now())
				mu.Unlock()
				select {
				case <-release:
				case <-ctx.Done():
				}
				return nil
			}}, Every: 24 * time.Hour},
			{Stage: fakeStage{name: "night", run: func(context.Context, *stage.Env, *stage.Report) error {
				mu.Lock()
				nightRuns = append(nightRuns, time.Now())
				mu.Unlock()
				return nil
			}}, Every: 48 * time.Hour, Window: night0200},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // a failed check must still stop the scheduler, or the bubble deadlocks over the message
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()
		synctest.Wait() // "busy" has never run, so it runs at once and holds until released
		for range 3 {
			s.Trigger("busy")
		}
		release <- struct{}{}
		release <- struct{}{}
		synctest.Wait()
		if busy, _ := runs(); len(busy) != 2 || !busy[1].Equal(busy[0]) {
			t.Fatalf("three triggers during a run gave runs at %v, want exactly one immediate rerun", busy)
		}
		time.Sleep(9 * time.Hour) // night ran at 02:00 (first run); now 09:00, outside its window
		s.Trigger("busy")
		s.Trigger("night")
		synctest.Wait()
		if busy, _ := runs(); len(busy) != 3 || !busy[2].Equal(time.Now()) {
			t.Fatalf("an idle trigger did not run at once: runs %v, now %v", busy, time.Now())
		}
		release <- struct{}{}
		if _, night := runs(); len(night) != 1 {
			t.Fatalf("a trigger ran a windowed stage outside its window: %v", night)
		}
		time.Sleep(18 * time.Hour) // next day 03:00; the cadence alone would wait for day 3
		if _, night := runs(); len(night) != 2 || !night[1].Equal(time.Date(2000, 1, 2, 2, 0, 0, 0, time.UTC)) {
			t.Fatalf("triggered windowed stage ran at %v, want once more at 2000-01-02 02:00", night)
		}
		if s.Trigger("absent") {
			t.Error("Trigger of an unscheduled stage reported true")
		}
		cancel()
		<-done
	})
}
