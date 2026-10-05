package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/plex"
	"projectionist/internal/stage"
)

const (
	slowChecks       = 3              // slow runs in a row that make an optimize due
	optimizeCooldown = 24 * time.Hour // at most one optimize a day
	optimizeTimeout  = time.Hour      // an optimize Plex still lists after this alerts; the one on 2026-10-05 took 4 min 21 s
)

// plexState is the Plex upkeep's memory between runs.
type plexState struct {
	SlowStreak   int           `json:"slow_streak"`   // runs in a row whose slower hub took longer than plex_slow
	Due          string        `json:"due,omitempty"` // why an optimize is due; "" while none is
	Changes      int           `json:"changes"`       // store.LibraryChanges when the last optimize was sent
	LastOptimize time.Time     `json:"last_optimize,omitzero"`
	Running      bool          `json:"running,omitempty"` // the optimize sent at LastOptimize has not been seen to end
	Before       time.Duration `json:"before,omitempty"`  // the slower hub's load when it was sent
	Uncured      bool          `json:"uncured,omitempty"` // the hubs stayed slow after it: alert until a fast load
}

// upkeepPlex times Plex's hubs and sends Plex's Optimize Database once it is due and allowed (optimizeHold). A sent
// optimize is followed on the next runs through /activities; the first run after it ends times the hubs again and
// compares. plexUp: the plex probe passed this run. Only a store failure is returned.
func upkeepPlex(ctx context.Context, env *stage.Env, opt Options, window config.Window, st *State, plexUp bool, rep *stage.Report) error {
	ps := &st.Plex
	ended := false
	if ps.Running {
		listed, err := optimizing(ctx, env, opt.Timeout)
		switch {
		case err == nil && !listed:
			ps.Running, ended = false, true
		case env.Now().Sub(ps.LastOptimize) >= optimizeTimeout:
			rep.Alert("Plex's database optimize, sent %s, still runs after %s (activities: %v)",
				ps.LastOptimize.Format(time.RFC3339), optimizeTimeout, err)
			return nil
		default:
			rep.Summary += "; plex: optimizing since " + ps.LastOptimize.Format(time.RFC3339)
			return nil // timing the hubs now would time the optimize
		}
	}
	if !plexUp {
		return nil // the plex probe reports it
	}
	took, timed, refused, detail := timeHubs(ctx, env, opt)
	if ctx.Err() != nil {
		return nil // cut short by a shutdown: says nothing about Plex
	}
	rep.Summary += "; plex hubs: " + detail
	for _, r := range refused {
		rep.Alert("plex hub refused, which no optimize cures: %s", r)
	}
	slow := timed && took > opt.PlexSlow
	switch {
	case slow:
		ps.SlowStreak++
	case timed:
		ps.SlowStreak, ps.Uncured = 0, false
	}
	if ended && timed {
		ps.Uncured = slow
		env.Log.Warn("plex optimized", "event", "plex_optimized", "sent_at", ps.LastOptimize.Format(time.RFC3339),
			"before_ms", ps.Before.Milliseconds(), "after_ms", took.Milliseconds())
	}
	if ps.Uncured && slow {
		rep.Alert("Plex's hubs are still slow after its database optimize: %s before, %s now (plex_slow %s)",
			ps.Before.Round(time.Millisecond), took.Round(time.Millisecond), opt.PlexSlow)
	}

	changes, err := env.Store.LibraryChanges(ctx)
	if err != nil {
		return err
	}
	switch {
	case ps.Due != "":
	case ps.SlowStreak >= slowChecks:
		ps.Due = fmt.Sprintf("hubs slower than %s on %d runs in a row (last %s)", opt.PlexSlow, ps.SlowStreak, took.Round(time.Millisecond))
	case changes-ps.Changes > opt.PlexBatch:
		ps.Due = fmt.Sprintf("%d library changes since the last optimize", changes-ps.Changes)
	}
	if ps.Due == "" {
		return nil
	}
	watching := func() (int, error) {
		callCtx, cancel := context.WithTimeout(ctx, opt.Timeout)
		defer cancel()
		return env.Viewers.Count(callCtx)
	}
	now := env.Now()
	if hold := optimizeHold(now, env.Loc, window, ps.LastOptimize, watching); hold != "" {
		env.Log.Info("plex optimize held", "event", "plex_optimize_held", "due", ps.Due, "reason", hold)
		return nil
	}

	prev := *ps
	ps.Running, ps.LastOptimize, ps.Before, ps.Changes, ps.Due = true, now, took, changes, ""
	// Recorded before it is sent: a crash in between can only skip an optimize, never repeat one inside the cooldown.
	if err := env.Store.PutJSON(context.WithoutCancel(ctx), name, "state", st); err != nil {
		return err
	}
	if err := env.Plex.WithTimeout(opt.Timeout).Do(ctx, http.MethodPut, "/library/optimize?async=1"); err != nil {
		*ps = prev // nothing started: the next run inside the window tries again
		rep.Alert("Plex Optimize Database (due: %s) failed: %v", prev.Due, err)
		return nil
	}
	env.Log.Warn("plex optimize sent", "event", "plex_optimize_sent", "due", prev.Due, "before_ms", took.Milliseconds(),
		"changes", changes-prev.Changes)
	return nil
}

// optimizeHold says why a due optimize waits, or "" to send it now. watching counts the sessions of anyone but the
// owners on Plex and Jellyfin; a count it cannot read is a busy one.
func optimizeHold(now time.Time, loc *time.Location, window config.Window, last time.Time, watching func() (int, error)) string {
	switch {
	case !window.Contains(now, loc):
		return "outside plex_optimize_window"
	case now.Sub(last) < optimizeCooldown:
		return fmt.Sprintf("optimized %s ago, at most once per %s", now.Sub(last).Round(time.Minute), optimizeCooldown)
	}
	switch n, err := watching(); {
	case err != nil:
		return "cannot tell who is watching: " + err.Error()
	case n > 0:
		return fmt.Sprintf("%d guest session(s) watching", n)
	}
	return ""
}

// timeHubs loads the Movies tab's hubs, then the Home screen's, one after the other as a viewer opening them would;
// the first load of a run is the cold one (a second within the minute took 16 ms where the first took 728 ms). took
// is the slower load. A hub that does not answer within the timeout, or answers 5xx, took the whole timeout; one
// answering 4xx (a wrong plex_movies_section, a refused token) is not timed: it is returned in refused.
func timeHubs(ctx context.Context, env *stage.Env, opt Options) (took time.Duration, timed bool, refused []string, detail string) {
	px := env.Plex.WithTimeout(opt.Timeout) // a 26.8 s load must be timed, not cut at the shared client's 20 s
	var parts []string
	for _, hub := range []struct{ label, path string }{
		{"movies", fmt.Sprintf("/hubs/sections/%d?count=12", opt.PlexMoviesSection)},
		{"home", "/hubs?count=12"},
	} {
		start := env.Now()
		err := px.Do(ctx, http.MethodGet, hub.path)
		d := env.Now().Sub(start)
		var status *plex.StatusError
		switch {
		case errors.As(err, &status) && status.Code < 500:
			refused = append(refused, err.Error())
			parts = append(parts, hub.label+" refused")
			continue
		case err != nil:
			d = max(d, opt.Timeout)
		}
		level := slog.LevelDebug
		if d > opt.PlexSlow {
			level = slog.LevelWarn
		}
		env.Log.Log(ctx, level, "plex hub", "event", "plex_hub", "hub", hub.label, "latency_ms", d.Milliseconds(), "err", err)
		env.Metrics.Set("projectionist_health_plex_hub_seconds", d.Seconds(), "hub", hub.label)
		parts = append(parts, fmt.Sprintf("%s %dms", hub.label, d.Milliseconds()))
		took, timed = max(took, d), true
	}
	return took, timed, refused, strings.Join(parts, ", ")
}

// optimizing reports whether Plex lists its database optimize among its activities. Plex 1.43 titles it "Optimizing
// database" (its log, 2026-10-05); the type is undocumented, so either field naming it matches.
func optimizing(ctx context.Context, env *stage.Env, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var mc struct {
		Activity []struct{ Type, Title string }
	}
	if err := env.Plex.GetJSON(ctx, "/activities", &mc); err != nil {
		return false, err
	}
	for _, a := range mc.Activity {
		if strings.Contains(strings.ToLower(a.Type+" "+a.Title), "optimiz") {
			return true, nil
		}
	}
	return false, nil
}
