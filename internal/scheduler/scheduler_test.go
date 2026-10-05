package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
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
// failing the run, a run a shutdown cut off (context canceled, or a child process killed with it) recorded as an
// error, alerts a cut-off run raised dropped with it (some are raised once, behind a stored marker, so a restart
// lost them for good), or the store not reflecting what /health shows.
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
		name     string
		run      func(context.Context, *stage.Env, *stage.Report) error
		want     stage.Outcome
		shutdown bool // the run's context is cancelled while it runs
	}{
		{"ok", func(context.Context, *stage.Env, *stage.Report) error { return nil }, stage.OK, false},
		{"alert", func(_ context.Context, _ *stage.Env, rep *stage.Report) error { rep.Alert("cpu transcode"); return nil }, stage.Alert, false},
		{"alert then error", func(_ context.Context, _ *stage.Env, rep *stage.Report) error {
			rep.Alert("drift")
			return errors.New("jellyfin down")
		}, stage.Error, false},
		{"panic", func(context.Context, *stage.Env, *stage.Report) error { panic("boom") }, stage.Error, false},
		{"shutdown", func(ctx context.Context, _ *stage.Env, _ *stage.Report) error { return ctx.Err() }, stage.Interrupted, true},
		{"child killed by the shutdown", func(context.Context, *stage.Env, *stage.Report) error {
			return errors.New("ffmpeg: signal: killed")
		}, stage.Interrupted, true},
		{"ok though shut down", func(context.Context, *stage.Env, *stage.Report) error { return nil }, stage.OK, true},
		{"alert then shutdown", func(ctx context.Context, _ *stage.Env, rep *stage.Report) error {
			rep.Alert("Happiest Season's poster is unreadable")
			return ctx.Err()
		}, stage.Alert, true},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		fn := c.run
		if c.shutdown {
			fn = func(ctx context.Context, env *stage.Env, rep *stage.Report) error {
				cancel()
				return c.run(ctx, env, rep)
			}
		}
		rep, got := r.RunOnce(ctx, fakeStage{run: fn})
		cancel()
		if got != c.want {
			t.Errorf("%s: outcome %s, want %s", c.name, got, c.want)
		}
		run, _, _ := st.LastRun(context.Background(), "fake")
		if run.Outcome != string(c.want) || run.FinishedAt.IsZero() {
			t.Errorf("%s: stored run %+v", c.name, run)
		}
		if strings.HasPrefix(c.name, "alert then") && len(rep.Alerts) != 1 {
			t.Errorf("%s: alert raised before it was lost: %v", c.name, rep.Alerts)
		}
		if s := r.Snapshot()["fake"]; c.want != stage.Interrupted && (s.Outcome != string(c.want) || len(s.Alerts) != len(rep.Alerts)) {
			t.Errorf("%s: /health shows outcome %q alerts %v, want %s and the run's %d", c.name, s.Outcome, s.Alerts, c.want, len(rep.Alerts))
		}
	}
}

// Catches: a restart reading the run it stopped in (never finished, or recorded interrupted) as the stage's outcome,
// which either hid an error the run before it ended in or, recorded as an error, held /health at 503 for a shutdown.
func TestRestartKeepsLastFinishedOutcome(t *testing.T) {
	for _, c := range []struct {
		name string
		runs []string // outcomes oldest first; "" = never finished
		want string
	}{
		{"ok, then stopped mid-run", []string{"ok", ""}, "ok"},
		{"error, then interrupted", []string{"error", "interrupted"}, "error"},
		{"only interrupted", []string{"interrupted"}, ""},
	} {
		synctest.Test(t, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			for _, o := range c.runs {
				id, _ := st.StartRun(context.Background(), "fake", time.Now().Add(-time.Hour))
				if o != "" {
					st.FinishRun(context.Background(), id, time.Now().Add(-time.Hour), o, "")
				}
			}
			s := &Scheduler{Loc: time.UTC, Runner: &Runner{Store: st, Log: obs.NewLogger(io.Discard, "text", "error"), Metrics: obs.NewMetrics(),
				Now: func() time.Time { return time.Now().UTC() }, NewEnv: func(stage.Stage, *slog.Logger) *stage.Env { return &stage.Env{} }},
				Jobs: []Job{{Stage: fakeStage{run: func(context.Context, *stage.Env, *stage.Report) error { return nil }}, Every: 24 * time.Hour}}}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { s.Run(ctx); close(done) }()
			synctest.Wait()
			if got := s.Runner.Snapshot()["fake"].Outcome; got != c.want {
				t.Errorf("%s: /health shows outcome %q after a restart, want %q", c.name, got, c.want)
			}
			cancel()
			<-done
		})
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
