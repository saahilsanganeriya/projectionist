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
	"testing/synctest"
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

// held is a run of the guard stage that raises an alert at once when told to, then lasts until release is closed.
type held struct {
	alert   bool
	release chan struct{}
}

func (held) Name() string { return "guard" }
func (h held) Run(_ context.Context, _ *stage.Env, rep *stage.Report) error {
	if h.alert {
		rep.Alert("Plex is transcoding on the CPU")
	}
	<-h.release
	return nil
}

// serve returns a runner on the clock time.Now gives (a synctest bubble's, inside one) and its /health status code.
func serve(t *testing.T) (*scheduler.Runner, func() int) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runner := &scheduler.Runner{
		Store: st, Log: obs.NewLogger(io.Discard, "text", "error"), Metrics: obs.NewMetrics(),
		Now:    func() time.Time { return time.Now().UTC() },
		NewEnv: func(stage.Stage, *slog.Logger) *stage.Env { return &stage.Env{} },
	}
	h := Handler(runner, st, runner.Metrics)
	return runner, func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		return rec.Code
	}
}

// Catches: the owner's only alert path going silent — the DSM health task e-mails only on a non-2xx /health, so an
// alerting or failed run must turn it 503 and the next clean run must clear it — and a run a shutdown cut off
// changing it either way: it reports the last finished run's outcome.
func TestHealthFollowsLatestRun(t *testing.T) {
	runner, code := serve(t)
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

// Catches an old alert holding /health at 503 through a long clean run (live: the owner e-mailed every 15 min for
// 12 h while the subtitle backlog drained), a run clearing it before it has gone 30 min without an alert, and a run
// that lasted over 30 min clearing its own alert as it ends.
func TestCleanRunInProgressSupersedesOldAlert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, code := serve(t)
		alerting := held{alert: true, release: make(chan struct{})}
		go runner.RunOnce(context.Background(), alerting)
		time.Sleep(time.Hour)
		close(alerting.release)
		synctest.Wait()
		if got := code(); got != 503 {
			t.Fatalf("an hour-long run just ended in an alert, /health = %d, want 503", got)
		}
		run := held{release: make(chan struct{})}
		defer close(run.release)
		go runner.RunOnce(context.Background(), run)
		time.Sleep(29 * time.Minute)
		if got := code(); got != 503 {
			t.Fatalf("29 min into a clean run after an alerting one, /health = %d, want 503", got)
		}
		time.Sleep(time.Minute)
		if got := code(); got != 200 {
			t.Fatalf("30 min into a clean run after an alerting one, /health = %d, want 200", got)
		}
	})
}

// Catches an alert a long run raises reaching /health only when that run ends, hours later.
func TestAlertInRunInProgressDegradesAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, code := serve(t)
		runner.RunOnce(context.Background(), guard("ok"))
		run := held{alert: true, release: make(chan struct{})}
		defer close(run.release)
		go runner.RunOnce(context.Background(), run)
		synctest.Wait()
		if got := code(); got != 503 {
			t.Fatalf("a run in progress raised an alert after a clean one, /health = %d, want 503", got)
		}
	})
}
