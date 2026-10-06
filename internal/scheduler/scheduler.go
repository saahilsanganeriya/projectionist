// Package scheduler runs each stage on its own cadence, never two copies of one stage at once, and records every
// run in the store so a restart neither repeats a stage early nor forgets an alert.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/obs"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// Job is a stage with its schedule.
type Job struct {
	Stage  stage.Stage
	Every  time.Duration
	Window config.Window
}

// Status is what /health reports per stage.
type Status struct {
	Running    bool      `json:"running"`
	LastStart  time.Time `json:"last_start,omitzero"`
	LastFinish time.Time `json:"last_finish,omitzero"`
	Outcome    string    `json:"outcome,omitempty"`
	Alerts     []string  `json:"alerts,omitempty"`
	Error      string    `json:"error,omitempty"`
	Summary    string    `json:"summary,omitempty"` // the latest run's Report.Summary
	NextRun    time.Time `json:"next_run,omitzero"`
}

// Runner executes one run of a stage: store bookkeeping, panic recovery, logs, metrics, health status.
type Runner struct {
	Store   *store.Store
	Log     *slog.Logger
	Metrics *obs.Metrics
	NewEnv  func(st stage.Stage, log *slog.Logger) *stage.Env
	Now     func() time.Time

	mu     sync.Mutex
	status map[string]*Status
}

func (r *Runner) set(name string, f func(*Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == nil {
		r.status = map[string]*Status{}
	}
	if r.status[name] == nil {
		r.status[name] = &Status{}
	}
	f(r.status[name])
}

// Snapshot copies the current per-stage status.
func (r *Runner) Snapshot() map[string]Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Status, len(r.status))
	for k, v := range r.status {
		out[k] = *v
	}
	return out
}

// RunOnce runs st to completion and returns its report and outcome.
func (r *Runner) RunOnce(ctx context.Context, st stage.Stage) (rep stage.Report, outcome stage.Outcome) {
	name := st.Name()
	log := r.Log.With("stage", name)
	started := r.Now()
	r.set(name, func(s *Status) { s.Running, s.LastStart = true, started })
	r.Metrics.Set("projectionist_stage_running", 1, "stage", name)
	runID, err := r.Store.StartRun(ctx, name, started)
	if err != nil {
		log.Error("store start run failed", "event", "store_error", "err", err)
	}
	log.Info("run started", "event", "run_start", "run_id", runID)

	err = func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		return st.Run(ctx, r.NewEnv(st, log), &rep)
	}()

	finished := r.Now()
	outcome = rep.Outcome(err)
	// Whatever it failed with (context canceled, a killed ffmpeg), a shutdown did it.
	interrupted := err != nil && ctx.Err() != nil
	if interrupted {
		outcome = stage.Interrupted
		if len(rep.Alerts) > 0 { // some alerts are raised once (a stored "told" marker): they reach /health now or never
			outcome = stage.Alert
		}
	}
	for _, a := range rep.Alerts {
		log.Warn(a, "event", "alert")
	}
	summary := rep.Summary
	if err != nil {
		summary = strings.TrimSpace(summary + " error: " + err.Error())
	}
	if runID != 0 {
		if serr := r.Store.FinishRun(context.WithoutCancel(ctx), runID, finished, string(outcome), summary); serr != nil {
			log.Error("store finish run failed", "event", "store_error", "err", serr)
		}
	}
	attrs := []any{"event", "run_end", "run_id", runID, "outcome", outcome, "duration_ms", finished.Sub(started).Milliseconds(), "alerts", len(rep.Alerts)}
	for k, v := range rep.Counts {
		attrs = append(attrs, "count_"+k, v)
		r.Metrics.Add("projectionist_stage_items_total", float64(v), "stage", name, "kind", k)
	}
	switch {
	case interrupted:
		log.Info("run interrupted", append(attrs, "err", err)...)
	case err != nil:
		log.Error("run failed", append(attrs, "err", err)...)
	default:
		log.Info("run finished", attrs...)
	}
	r.Metrics.Add("projectionist_stage_runs_total", 1, "stage", name, "outcome", string(outcome))
	r.Metrics.Add("projectionist_stage_alerts_total", float64(len(rep.Alerts)), "stage", name)
	r.Metrics.Set("projectionist_stage_running", 0, "stage", name)
	r.Metrics.Set("projectionist_stage_last_run_timestamp_seconds", float64(finished.Unix()), "stage", name)
	r.Metrics.Set("projectionist_stage_last_duration_seconds", finished.Sub(started).Seconds(), "stage", name)
	r.set(name, func(s *Status) {
		if s.Running = false; outcome == stage.Interrupted {
			return // the last finished run's outcome stands
		}
		s.LastFinish, s.Outcome, s.Alerts, s.Error, s.Summary = finished, string(outcome), rep.Alerts, "", rep.Summary
		if err != nil {
			s.Error = err.Error()
		}
	})
	return rep, outcome
}

// NextRun is when a job whose previous run started at last should run next: one cadence later, but never in the
// past, and pushed to the window's next opening when outside it. A zero last means "never ran": run now.
func NextRun(last time.Time, every time.Duration, w config.Window, loc *time.Location, now time.Time) time.Time {
	next := now
	if !last.IsZero() && last.Add(every).After(now) {
		next = last.Add(every)
	}
	if w.Contains(next, loc) {
		return next
	}
	lt := next.In(loc)
	open := time.Date(lt.Year(), lt.Month(), lt.Day(), int(w.Start/time.Hour), int(w.Start%time.Hour/time.Minute), 0, 0, loc)
	if !open.After(next) {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

// Scheduler drives every job until ctx ends; each job has its own goroutine, so a long encode delays only itself.
type Scheduler struct {
	Jobs   []Job
	Runner *Runner
	Loc    *time.Location

	mu   sync.Mutex
	wake map[string]chan struct{} // per job; one slot, so triggers coalesce
}

// Trigger asks for a run of the named stage now, or at its window's next opening. Any number of triggers while the
// stage runs or waits yield exactly one extra run. False when the stage is not scheduled here.
func (s *Scheduler) Trigger(name string) bool {
	s.mu.Lock()
	ch, ok := s.wake[name]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- struct{}{}:
		default: // a run is already pending
		}
	}
	return ok
}

func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.wake = make(map[string]chan struct{}, len(s.Jobs))
	for _, job := range s.Jobs {
		s.wake[job.Stage.Name()] = make(chan struct{}, 1)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, job := range s.Jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, job, s.wake[job.Stage.Name()])
		}()
	}
	<-ctx.Done() // with no jobs enabled the service must still stay up (and keep serving /health)
	wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context, job Job, wake <-chan struct{}) {
	name := job.Stage.Name()
	var last time.Time
	if run, ok, err := s.Runner.Store.LastRun(ctx, name); err != nil {
		s.Runner.Log.Error("store last run failed", "event", "store_error", "stage", name, "err", err)
	} else if ok {
		last = run.StartedAt
		// A run the process stopped in (never finished, or recorded interrupted) has no outcome: the one before stands.
		done, _, err := s.Runner.Store.LastFinishedRun(ctx, name)
		if err != nil {
			s.Runner.Log.Error("store last run failed", "event", "store_error", "stage", name, "err", err)
		}
		s.Runner.set(name, func(st *Status) {
			st.LastStart, st.LastFinish, st.Outcome = run.StartedAt, done.FinishedAt, done.Outcome
		})
	}
	triggered := false
	for {
		next := NextRun(last, job.Every, job.Window, s.Loc, s.Runner.Now())
		if triggered {
			next = NextRun(time.Time{}, job.Every, job.Window, s.Loc, s.Runner.Now()) // due now, inside the window
		}
		s.Runner.set(name, func(st *Status) { st.NextRun = next })
		select {
		case <-ctx.Done():
			return
		case <-wake:
			triggered = true
			continue
		case <-time.After(time.Until(next)):
		}
		triggered = false
		last = s.Runner.Now()
		s.Runner.RunOnce(ctx, job.Stage)
	}
}
