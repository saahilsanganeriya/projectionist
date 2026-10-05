// Package stage defines the one interface every pipeline step implements, and what a step is handed when it runs.
package stage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
	"projectionist/internal/probe"
	"projectionist/internal/store"
	"projectionist/internal/viewers"
)

// Stage is one step of the pipeline. The scheduler never runs two copies of the same stage at once, so Run may
// assume it is alone; it must be idempotent (a rerun after a crash finishes the work, never repeats a side effect)
// and must return promptly when ctx is cancelled.
type Stage interface {
	Name() string
	Run(ctx context.Context, env *Env, rep *Report) error
}

// Env is everything a stage may use. Clients read their secrets from files on every call, never from env vars.
type Env struct {
	Log      *slog.Logger // already tagged stage=<name>
	Store    *store.Store
	Plex     *plex.Client
	Jellyfin *jellyfin.Client
	Viewers  *viewers.Gate
	Probe    *probe.Prober
	FFmpeg   config.FFmpeg
	Now      func() time.Time // UTC
	// Options decodes this stage's own keys from its [stages.<name>] table (unknown keys are reported at startup).
	Options func(v any) error
}

// Report is filled in by Run. Alerts survive a returned error, so a stage can raise alerts and then fail.
type Report struct {
	Alerts  []string
	Counts  map[string]int
	Summary string
}

// Alert records a condition the owner must hear about: the run's outcome becomes "alert" and /health turns 503
// until this stage's next run raises none.
func (r *Report) Alert(format string, args ...any) {
	r.Alerts = append(r.Alerts, fmt.Sprintf(format, args...))
}

// Count adds n to a per-run counter, exported as projectionist_stage_items_total{stage,kind}.
func (r *Report) Count(kind string, n int) {
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	r.Counts[kind] += n
}

// Outcome names a finished run for the store, logs and metrics.
type Outcome string

const (
	OK    Outcome = "ok"
	Alert Outcome = "alert"
	Error Outcome = "error"
)

func (r *Report) Outcome(err error) Outcome {
	switch {
	case err != nil:
		return Error
	case len(r.Alerts) > 0:
		return Alert
	default:
		return OK
	}
}
