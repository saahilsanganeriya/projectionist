// Package stage defines the one interface every pipeline step implements, and what a step is handed when it runs.
package stage

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fanart"
	"projectionist/internal/jellyfin"
	"projectionist/internal/llm"
	"projectionist/internal/obs"
	"projectionist/internal/plex"
	"projectionist/internal/probe"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
	"projectionist/internal/viewers"
)

// Stage is one step of the pipeline. The scheduler never runs two copies of the same stage at once, so Run may
// assume it is alone; it must be idempotent (a rerun after a crash finishes the work, never repeats a side effect)
// and must return promptly when ctx is cancelled.
type Stage interface {
	Name() string
	Run(ctx context.Context, env *Env, rep *Report) error
}

// Hooker is a stage that also takes requests on the service's HTTP listener (e.g. Plex webhooks). Webhook handlers
// only record what arrived, and the stage's next run acts on it; the subtitles stage's align-worker API instead serves
// the run in progress (its queue's titles, leased to a worker on another machine). Mounted by `serve` for enabled
// stages.
type Hooker interface {
	Hooks(env *Env) map[string]http.Handler // pattern ("POST /hooks/<name>") → handler
}

// Env is everything a stage may use. Clients read their secrets from files on every call, never from env vars.
type Env struct {
	Log      *slog.Logger // already tagged stage=<name>
	Store    *store.Store
	Plex     *plex.Client
	Jellyfin *jellyfin.Client
	Viewers  *viewers.Gate
	Probe    *probe.Prober
	TMDB     *tmdb.Client   // the only source of identity, episodes, collections and external ids; one per process
	Fanart   *fanart.Client // community artwork, keyed by TMDB/IMDb id (films) or TVDB id (shows)
	LLM      *llm.Client    // every model call, through the AI gateway; one per process, so its bound on calls in flight holds
	FFmpeg   config.FFmpeg
	Metrics  *obs.Metrics     // the /metrics registry, for a stage's own gauges and counters
	Now      func() time.Time // UTC
	Loc      *time.Location   // config timezone: only for text the owner reads (status files)
	// Options decodes this stage's own keys from its [stages.<name>] table into v (call it at the top of Run).
	Options func(v any) error
	// PeerOptions decodes another stage's keys, for a mapping that stage owns (tonight reads jfsync's users). An absent
	// table, or a nil PeerOptions (a one-off tool), leaves v as it was.
	PeerOptions func(stage string, v any) error
	// Trigger asks the scheduler to run the named stage soon (inside its window); repeated triggers coalesce into
	// one run. False when that stage is not scheduled (disabled, or a one-off `projectionist run`).
	Trigger func(stage string) bool
}

// Report is filled in by Run. Alerts survive a returned error, so a stage can raise alerts and then fail.
type Report struct {
	Alerts  []string
	Counts  map[string]int
	Summary string
	// Wake asks the scheduler to run the stage again no later than this, inside its window, though the cadence would
	// wait longer. Not kept across a restart: the next run on the cadence asks again.
	Wake time.Time
	// OnAlert, set by the scheduler, is called by Alert with each alert as it is raised, so /health has it before the
	// run ends. An alert appended to Alerts directly reaches /health only when the run ends.
	OnAlert func(alert string)
}

// Alert records a condition the owner must hear about: the run's outcome becomes "alert" and /health turns 503 at
// once, until a later run of this stage ends without one or has run 30 min without one.
func (r *Report) Alert(format string, args ...any) {
	a := fmt.Sprintf(format, args...)
	r.Alerts = append(r.Alerts, a)
	if r.OnAlert != nil {
		r.OnAlert(a)
	}
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
	// Interrupted is a run a shutdown or restart cut off (its context was cancelled) before it raised an alert: no
	// verdict on the stage, which resumes on its next run, so /health keeps showing the last finished run's outcome.
	// A cut-off run that raised alerts is recorded Alert.
	Interrupted Outcome = "interrupted"
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
