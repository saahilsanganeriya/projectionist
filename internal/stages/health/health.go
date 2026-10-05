// Package health keeps the services around the library answering, and restarts a Jellyfin that stops.
//
// Every run probes, all at once and each within `timeout`: Plex /identity, Jellyfin /System/Info/Public and an
// authenticated /Items?Limit=1, qBittorrent's API, the Cloudflare public hostnames (status compared, redirects not
// followed), and Docker's view of the Jellyfin and qBittorrent containers. A service that fails `fail_threshold`
// runs in a row raises an alert on every run until it passes. Its container is restarted through the Docker Engine
// API only when all of these hold:
//   - the container is in the `restartable` allow-list;
//   - every failure is one a restart can cure: no answer, a timeout, HTTP 5xx, Docker "unhealthy". A refused key,
//     an unexpected status or a stopped container is alerted, never "fixed" by a restart;
//   - nobody is playing on Jellyfin;
//   - this stage has not restarted it within `restart_cooldown`.
//
// Plex is a DSM package and the hostnames belong to Cloudflare: their failures only alert.
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/stage"
	"projectionist/internal/viewers"
)

const (
	name           = "health"
	restartTimeout = 10 * time.Minute // a busybox restart took 111 s on the loaded NAS (30 s of it the stop grace)
)

type Options struct {
	Timeout              time.Duration  `toml:"timeout"`        // per probe
	FailThreshold        int            `toml:"fail_threshold"` // consecutive failed runs before an alert or a restart
	RestartCooldown      time.Duration  `toml:"restart_cooldown"`
	Restartable          []string       `toml:"restartable"` // the only containers this stage may restart
	DockerSocket         string         `toml:"docker_socket"`
	JellyfinContainer    string         `toml:"jellyfin_container"`
	QBittorrentContainer string         `toml:"qbittorrent_container"`
	QBittorrentURL       string         `toml:"qbittorrent_url"`
	Public               map[string]int `toml:"public"` // URL → the status it must answer; no default (a map would merge)
}

func defaults() Options {
	return Options{
		Timeout:              15 * time.Second, // the Cloudflare hostnames took up to 6.4 s under NAS I/O load 14
		FailThreshold:        3,
		RestartCooldown:      time.Hour,
		Restartable:          []string{"jellyfin"},
		DockerSocket:         "/var/run/docker.sock",
		JellyfinContainer:    "jellyfin",
		QBittorrentContainer: "qbittorrent",
		QBittorrentURL:       "http://127.0.0.1:8080",
	}
}

// fault classifies a probe's outcome.
type fault string

const (
	passed fault = ""
	down   fault = "down"  // no answer, timeout, HTTP 5xx, Docker "unhealthy": a restart may cure it
	wrong  fault = "wrong" // an answer, but the wrong one (refused key, unexpected status, container not running)
)

// faultOf classifies a client error: no answer or a 5xx is down, anything else (401, unreadable key file) is wrong.
func faultOf(err error) fault {
	var status *jellyfin.StatusError
	if errors.As(err, &status) {
		if status.Code >= 500 {
			return down
		}
		return wrong
	}
	var netErr net.Error // *url.Error is one: every transport failure, timeouts included
	if errors.As(err, &netErr) {
		return down
	}
	return wrong
}

type probe struct {
	name, service string
	check         func(ctx context.Context) (detail string, f fault)
}

type result struct {
	probe, service, detail string
	fault                  fault
	latency                time.Duration
}

// State is kept in the store between runs.
type State struct {
	Streak      map[string]int       `json:"streak"`       // service → consecutive failed runs
	LastRestart map[string]time.Time `json:"last_restart"` // container → when this stage last restarted it
}

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if opt.Timeout <= 0 || opt.FailThreshold < 1 {
		return errors.New("stages.health: timeout must be > 0 and fail_threshold >= 1")
	}
	st := State{Streak: map[string]int{}, LastRestart: map[string]time.Time{}}
	if _, err := env.Store.GetJSON(ctx, name, "state", &st); err != nil {
		return err
	}
	dk := newDocker(opt.DockerSocket)
	results := runProbes(ctx, opt.Timeout, probes(env, opt, dk))
	if err := ctx.Err(); err != nil {
		return err // probes cut short by shutdown are not failures
	}

	failed := map[string][]result{}
	var services []string
	for _, r := range results {
		if !slices.Contains(services, r.service) {
			services = append(services, r.service)
		}
		up, level := 1.0, slog.LevelDebug
		if r.fault != passed {
			failed[r.service] = append(failed[r.service], r)
			up, level = 0, slog.LevelWarn
		}
		env.Log.Log(ctx, level, "probe", "event", "probe", "probe", r.probe, "service", r.service,
			"fault", string(r.fault), "detail", r.detail, "latency_ms", r.latency.Milliseconds())
		env.Metrics.Set("projectionist_health_up", up, "probe", r.probe)
		env.Metrics.Set("projectionist_health_latency_seconds", r.latency.Seconds(), "probe", r.probe)
	}

	now := env.Now()
	containers := map[string]string{"jellyfin": opt.JellyfinContainer, "qbittorrent": opt.QBittorrentContainer}
	for _, svc := range services {
		if len(failed[svc]) == 0 {
			st.Streak[svc] = 0
		} else {
			st.Streak[svc]++
		}
		c := containers[svc]
		var faults []fault
		for _, r := range failed[svc] {
			faults = append(faults, r.fault)
		}
		var playing func() (int, error)
		if svc == "jellyfin" {
			playing = jellyfinPlaying(ctx, env, opt.Timeout)
		}
		v := decide(restartCase{
			streak: st.Streak[svc], threshold: opt.FailThreshold, faults: faults,
			container: c, allowed: slices.Contains(opt.Restartable, c),
			sinceRestart: now.Sub(st.LastRestart[c]), cooldown: opt.RestartCooldown, playing: playing,
		})
		env.Metrics.Set("projectionist_health_failed_checks", float64(st.Streak[svc]), "service", svc)
		if !v.alert {
			continue
		}
		msg := fmt.Sprintf("%s failed %d checks in a row: %s", svc, st.Streak[svc], describe(failed[svc]))
		switch {
		case v.hold != "":
			rep.Alert("%s; not restarting %s: %s", msg, c, v.hold)
			env.Log.Info("restart held", "event", "restart_held", "container", c, "reason", v.hold)
		case !v.restart:
			rep.Alert("%s", msg)
		default:
			rep.Alert("%s; restarting container %s", msg, c)
			if err := restart(ctx, env, dk, &st, svc, c, now, describe(failed[svc]), rep); err != nil {
				return err
			}
		}
	}
	rep.Summary = summarize(results)
	env.Log.Info("probes done", "event", "health_checked", "probes", len(results), "failed", len(results)-countPassed(results))
	return env.Store.PutJSON(context.WithoutCancel(ctx), name, "state", st)
}

func restart(ctx context.Context, env *stage.Env, dk *docker, st *State, svc, c string, now time.Time, why string, rep *stage.Report) error {
	st.Streak[svc] = 0 // the next restart needs a fresh run of failures
	st.LastRestart[c] = now
	// Recorded before acting: a crash in between can only skip a restart, never repeat one inside the cooldown.
	if err := env.Store.PutJSON(context.WithoutCancel(ctx), name, "state", st); err != nil {
		return err
	}
	// Docker 24 runs the stop and then the start on the request's context: a request we abandon can leave the
	// container stopped, so a shutdown never cancels it and the timeout is far above a slow restart.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restartTimeout)
	start := time.Now()
	err := dk.restart(rctx, c)
	cancel()
	outcome := "restarted"
	if err != nil {
		outcome = "restart_failed"
		rep.Alert("restart of container %s failed: %v", c, err)
	}
	env.Log.Warn("container restart", "event", "container_restart", "container", c, "service", svc, "outcome", outcome,
		"duration_ms", time.Since(start).Milliseconds(), "err", err)
	env.Metrics.Add("projectionist_health_restarts_total", 1, "container", c, "outcome", outcome)
	return env.Store.AddEvent(context.WithoutCancel(ctx), now, name, c, outcome, map[string]any{"failed": why})
}

// restartCase is everything decide weighs for one service this run.
type restartCase struct {
	streak, threshold int
	faults            []fault // this run's failed probes
	container         string  // "" when there is nothing to restart (Plex, Cloudflare)
	allowed           bool    // container is in restartable
	sinceRestart      time.Duration
	cooldown          time.Duration
	playing           func() (int, error) // sessions playing on the service; nil when it has no viewers
}

type verdict struct {
	alert, restart bool
	hold           string // why a due restart is not happening
}

func decide(c restartCase) verdict {
	switch {
	case c.streak < c.threshold:
		return verdict{}
	case c.container == "":
		return verdict{alert: true}
	case slices.Contains(c.faults, wrong):
		return verdict{alert: true, hold: "a restart cannot cure this failure"}
	case !c.allowed:
		return verdict{alert: true, hold: "the container is not in restartable"}
	case c.sinceRestart < c.cooldown:
		return verdict{alert: true, hold: fmt.Sprintf("restarted %s ago, cooldown %s", c.sinceRestart.Round(time.Minute), c.cooldown)}
	}
	if c.playing != nil {
		n, err := c.playing()
		// A server that does not answer at all cannot be streaming to anyone; any other error leaves it unknown.
		if err != nil && faultOf(err) != down {
			return verdict{alert: true, hold: "cannot tell who is playing: " + err.Error()}
		}
		if n > 0 {
			return verdict{alert: true, hold: fmt.Sprintf("%d session(s) playing", n)}
		}
	}
	return verdict{alert: true, restart: true}
}

// jellyfinPlaying counts every playing session, the owner's included: a restart cuts his stream too.
func jellyfinPlaying(ctx context.Context, env *stage.Env, timeout time.Duration) func() (int, error) {
	return func() (int, error) {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		sessions, err := env.Jellyfin.Sessions(callCtx, viewers.JellyfinActiveWithin)
		if ctx.Err() != nil {
			return 0, ctx.Err() // shutting down: an unanswered call says nothing about who is watching
		}
		n := 0
		for _, s := range sessions {
			if s.NowPlayingItem != nil {
				n++
			}
		}
		return n, err
	}
}

func probes(env *stage.Env, opt Options, dk *docker) []probe {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true // every check makes a fresh connection, as a new viewer would
	web := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // a 302 is the answer
	}
	get := func(url string, want ...int) func(context.Context) (string, fault) {
		return func(ctx context.Context) (string, fault) { return httpCheck(ctx, web, url, want) }
	}
	client := func(call func(context.Context) error) func(context.Context) (string, fault) {
		return func(ctx context.Context) (string, fault) {
			if err := call(ctx); err != nil {
				return err.Error(), faultOf(err)
			}
			return "200", passed
		}
	}
	inspect := func(container string) func(context.Context) (string, fault) {
		return func(ctx context.Context) (string, fault) {
			s, err := dk.inspect(ctx, container)
			if err != nil {
				return err.Error(), wrong
			}
			return s.judge()
		}
	}
	ps := []probe{
		{"plex", "plex", client(func(ctx context.Context) error { return env.Plex.Do(ctx, http.MethodGet, "/identity") })},
		{"jellyfin", "jellyfin", client(func(ctx context.Context) error {
			return env.Jellyfin.GetJSON(ctx, "/System/Info/Public", nil)
		})},
		{"jellyfin-items", "jellyfin", client(func(ctx context.Context) error {
			return env.Jellyfin.GetJSON(ctx, "/Items?Limit=1", nil)
		})},
		{"docker:" + opt.JellyfinContainer, "jellyfin", inspect(opt.JellyfinContainer)},
		// 403 is up: the WebUI answers and wants a login (its localhost auth bypass is off).
		{"qbittorrent", "qbittorrent", get(opt.QBittorrentURL+"/api/v2/app/version", 200, 403)},
		{"docker:" + opt.QBittorrentContainer, "qbittorrent", inspect(opt.QBittorrentContainer)},
	}
	for _, u := range slices.Sorted(maps.Keys(opt.Public)) {
		ps = append(ps, probe{u, "cloudflare", get(u, opt.Public[u])})
	}
	return ps
}

func httpCheck(ctx context.Context, c *http.Client, url string, want []int) (string, fault) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error(), wrong
	}
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), down
	}
	resp.Body.Close()
	switch {
	case slices.Contains(want, resp.StatusCode):
		return strconv.Itoa(resp.StatusCode), passed
	case resp.StatusCode >= 500:
		return fmt.Sprintf("HTTP %d", resp.StatusCode), down
	default:
		return fmt.Sprintf("HTTP %d, want %v", resp.StatusCode, want), wrong
	}
}

// runProbes runs every probe at once, each within timeout; the config fixes their number at a handful.
func runProbes(ctx context.Context, timeout time.Duration, ps []probe) []result {
	out := make([]result, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			detail, f := p.check(ctx)
			out[i] = result{probe: p.name, service: p.service, detail: detail, fault: f, latency: time.Since(start)}
		})
	}
	wg.Wait()
	return out
}

func describe(rs []result) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.probe + ": " + r.detail
	}
	return strings.Join(parts, "; ")
}

// summarize is the one line /health shows for this stage: every probe, its answer and latency.
func summarize(rs []result) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		status := "ok"
		if r.fault != passed {
			status = "FAIL"
		}
		parts[i] = fmt.Sprintf("%s %s (%s) %dms", r.probe, status, r.detail, r.latency.Milliseconds())
	}
	return fmt.Sprintf("%d/%d ok: %s", countPassed(rs), len(rs), strings.Join(parts, "; "))
}

func countPassed(rs []result) int {
	n := 0
	for _, r := range rs {
		if r.fault == passed {
			n++
		}
	}
	return n
}
