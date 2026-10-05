// Package health keeps the services around the library answering, and restarts a Jellyfin that stops.
//
// Every run probes, all at once and each within `timeout`: Plex /identity, Jellyfin /System/Info/Public and an
// authenticated /Items?Limit=1, qBittorrent's API, the Cloudflare public hostnames (status compared, redirects not
// followed), and Docker's view of the Jellyfin and qBittorrent containers. A service that fails `fail_threshold`
// runs in a row raises an alert on every run until it passes. Its container is restarted through the Docker Engine
// API only when all of these hold:
//   - the container is in the `restartable` allow-list (default: none);
//   - every failure is one a restart can cure: no answer, a timeout, HTTP 5xx, Docker "unhealthy". A refused key,
//     an unexpected status or a stopped container is alerted, never "fixed" by a restart;
//   - nobody is playing on Jellyfin: /Sessions counts the sessions, and an unread count holds the restart, unless
//     Jellyfin answered none of this run's requests (a server that answers nothing streams to no one);
//   - this stage has not restarted it within `restart_cooldown` (at least 1 h).
//
// A restart is a stop with a 120 s grace, a start, then a wait of up to 5 min until the service answers again; a
// service that does not is alerted as a failed restart. It is recorded before it is sent, so it is resumable: when
// projectionist dies between the stop and the start, the next run starts the container it stopped.
//
// Plex is a DSM package and the hostnames belong to Cloudflare: their failures only alert. Plex and Jellyfin are
// sinks: these probes read no data from them, they only prove the apps answer, and /Sessions is the viewer check.
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
	name = "health"
	// Docker's wait between SIGTERM and SIGKILL. `docker restart`'s 10 s SIGKILLed a Jellyfin still cancelling a scan
	// (2026-10-05), and its start half never ran.
	stopGrace    = 120 * time.Second
	callTimeout  = 5 * time.Minute // one Docker stop past its grace, or one start: a busybox restart took 111 s on the loaded NAS
	readyTimeout = 5 * time.Minute // from the start until the service answers again
	readyPoll    = 5 * time.Second
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
		Timeout:              30 * time.Second, // a Cloudflare hostname took 9.8 s on the loaded NAS; a hung Jellyfin never answers
		FailThreshold:        3,
		RestartCooldown:      time.Hour,
		DockerSocket:         "/var/run/docker.sock",
		JellyfinContainer:    "jellyfin",
		QBittorrentContainer: "qbittorrent",
		QBittorrentURL:       "http://127.0.0.1:8080",
	}
}

// fault classifies a probe's outcome.
type fault string

// A restart may cure unanswered and down; it never cures wrong.
const (
	passed     fault = ""
	unanswered fault = "unanswered" // no HTTP status at all: refused, reset, timed out
	down       fault = "down"       // it answered, but HTTP 5xx or Docker "unhealthy"
	wrong      fault = "wrong"      // the wrong answer (refused key, unexpected status, container not running)
)

// faultOf classifies a client error; anything that is neither a 5xx nor a transport failure (401, unreadable key
// file) is wrong.
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
		return unanswered
	}
	return wrong
}

type probe struct {
	name, service string
	docker        bool // Docker's view of the service's container, not an answer from the service itself
	check         func(ctx context.Context) (detail string, f fault)
}

type result struct {
	probe, service, detail string
	docker                 bool
	fault                  fault
	latency                time.Duration
}

// State is kept in the store between runs.
type State struct {
	Streak      map[string]int       `json:"streak"`       // service → consecutive failed runs
	LastRestart map[string]time.Time `json:"last_restart"` // container → when this stage last restarted it
}

type Stage struct {
	Sleep func(ctx context.Context, d time.Duration) error // nil: real time; tests move the harness clock instead
}

func (Stage) Name() string { return name }

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if opt.Timeout <= 0 || opt.FailThreshold < 1 || opt.RestartCooldown < time.Hour {
		return fmt.Errorf("stages.health: need timeout > 0, fail_threshold >= 1, restart_cooldown >= 1h; got %s, %d, %s",
			opt.Timeout, opt.FailThreshold, opt.RestartCooldown)
	}
	st := State{Streak: map[string]int{}, LastRestart: map[string]time.Time{}}
	if _, err := env.Store.GetJSON(ctx, name, "state", &st); err != nil {
		return err
	}
	dk := newDocker(opt.DockerSocket)
	ps := probes(env, opt, dk)
	containers := map[string]string{"jellyfin": opt.JellyfinContainer, "qbittorrent": opt.QBittorrentContainer}
	for _, svc := range slices.Sorted(maps.Keys(containers)) {
		if c := containers[svc]; slices.Contains(opt.Restartable, c) {
			if err := s.resume(ctx, env, dk, st, c, named(ps, svc), opt.Timeout, rep); err != nil {
				return err
			}
		}
	}
	results := runProbes(ctx, opt.Timeout, ps)
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
		answered := slices.ContainsFunc(results, func(r result) bool {
			return r.service == svc && !r.docker && r.fault != unanswered
		})
		v := decide(restartCase{
			streak: st.Streak[svc], threshold: opt.FailThreshold, faults: faults,
			container: c, allowed: slices.Contains(opt.Restartable, c),
			sinceRestart: env.Now().Sub(st.LastRestart[c]), cooldown: opt.RestartCooldown, playing: playing, silent: !answered,
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
			if err := s.restart(ctx, env, dk, &st, svc, c, named(ps, svc), opt.Timeout, describe(failed[svc]), rep); err != nil {
				return err
			}
		}
	}
	rep.Summary = summarize(results)
	env.Log.Info("probes done", "event", "health_checked", "probes", len(results), "failed", len(results)-countPassed(results))
	return env.Store.PutJSON(context.WithoutCancel(ctx), name, "state", st)
}

// restart records the restart, stops c with a long grace, then brings it back. A shutdown never cuts it between the
// stop and the start.
func (s Stage) restart(ctx context.Context, env *stage.Env, dk *docker, st *State, svc, c string, ready probe,
	timeout time.Duration, why string, rep *stage.Report) error {
	asked := env.Now() // this restart's own time: one before it in the same run can take minutes
	st.Streak[svc] = 0 // the next restart needs a fresh run of failures
	st.LastRestart[c] = asked
	// Recorded before acting: a crash in between can only skip a restart, never repeat one inside the cooldown, and
	// one cut off after the stop is finished by resume.
	if err := env.Store.PutJSON(context.WithoutCancel(ctx), name, "state", st); err != nil {
		return err
	}
	return act(ctx, env, rep, "restart", c, why, func() error {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopGrace+callTimeout)
		defer cancel()
		if err := dk.stop(stopCtx, c); err != nil {
			return fmt.Errorf("stop: %w", err) // nothing started: a container this did stop is resumed by the next run
		}
		return s.bringBack(ctx, env, dk, c, asked, ready, timeout)
	})
}

// resume starts a container that a restart of ours stopped and nothing started again (projectionist died between
// the stop and the start): it last started before we asked, and its stop ended inside the stop request's bound.
// A container the owner stops later is left stopped: it started after our restart, or stopped after that bound.
func (s Stage) resume(ctx context.Context, env *stage.Env, dk *docker, st State, c string, ready probe,
	timeout time.Duration, rep *stage.Report) error {
	asked, ok := st.LastRestart[c]
	if !ok {
		return nil
	}
	inspectCtx, cancel := context.WithTimeout(ctx, timeout) // a wedged dockerd must not hang this and every later run
	cs, err := dk.inspect(inspectCtx, c)
	cancel()
	ours := !cs.FinishedAt.Before(asked) && cs.FinishedAt.Before(asked.Add(stopGrace+callTimeout))
	if err != nil || cs.Status != "exited" || !cs.StartedAt.Before(asked) || !ours {
		return nil // an inspect error is the probe's to report
	}
	rep.Alert("container %s stopped during this stage's restart at %s and never started again; starting it",
		c, asked.Format(time.RFC3339))
	return act(ctx, env, rep, "resume", c, "restart cut off after the stop", func() error {
		return s.bringBack(ctx, env, dk, c, asked, ready, timeout)
	})
}

// bringBack starts c and waits up to readyTimeout for ready to pass. No answer from Docker is proof: while ready
// fails, a container still exited and not started since `since` is started again. A shutdown cuts only the wait.
func (s Stage) bringBack(ctx context.Context, env *stage.Env, dk *docker, c string, since time.Time, ready probe,
	timeout time.Duration) error {
	bounded := func(d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), d)
	}
	start := func() error {
		startCtx, cancel := bounded(callTimeout)
		defer cancel()
		return dk.start(startCtx, c)
	}
	startErr, state := start(), "not inspected"
	deadline := env.Now().Add(readyTimeout)
	for {
		readyCtx, cancel := bounded(timeout)
		detail, f := ready.check(readyCtx)
		cancel()
		switch {
		case f == passed:
			return nil
		case !env.Now().Before(deadline):
			msg := fmt.Sprintf("%s did not answer within %s of the start (%s); container %s", ready.name, readyTimeout, detail, state)
			if startErr != nil {
				msg += "; last start: " + startErr.Error()
			}
			return errors.New(msg)
		}
		if err := s.sleep(ctx, readyPoll); err != nil {
			return fmt.Errorf("shut down before %s answered again: %w", ready.name, err)
		}
		inspectCtx, cancel := bounded(timeout)
		cs, err := dk.inspect(inspectCtx, c)
		cancel()
		if err != nil {
			state = err.Error()
			continue
		}
		state, _ = cs.judge()
		if cs.Status == "exited" && cs.StartedAt.Before(since) {
			env.Log.Warn("start again", "event", "container_start_retry", "container", c, "last_start_err", startErr)
			startErr = start()
		}
	}
}

func (s Stage) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// act runs one container action to its end, then alerts a failure, logs, counts and records it.
func act(ctx context.Context, env *stage.Env, rep *stage.Report, action, c, why string, call func() error) error {
	start := time.Now()
	err := call()
	outcome, level := "ok", slog.LevelWarn
	if err != nil {
		outcome, level = "failed", slog.LevelError
		rep.Alert("%s of container %s FAILED: %v", action, c, err)
	}
	env.Log.Log(ctx, level, "container "+action, "event", "container_action", "action", action, "container", c,
		"outcome", outcome, "duration_ms", time.Since(start).Milliseconds(), "err", err)
	env.Metrics.Add("projectionist_health_container_actions_total", 1, "container", c, "action", action, "outcome", outcome)
	return env.Store.AddEvent(context.WithoutCancel(ctx), env.Now(), name, c, action+"_"+outcome, map[string]any{"why": why})
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
	silent            bool                // the service answered none of its own HTTP probes this run
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
		switch {
		// A server that answered nothing this run, /Sessions included, streams to no one. Any answer, a 5xx too,
		// means it is up and may be streaming, so an unread count is a busy one.
		case err != nil && !(c.silent && faultOf(err) == unanswered):
			return verdict{alert: true, hold: "cannot tell who is playing: " + err.Error()}
		case n > 0:
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
	get := func(name, service, url string, want ...int) probe {
		return probe{name: name, service: service, check: func(ctx context.Context) (string, fault) {
			return httpCheck(ctx, web, url, want)
		}}
	}
	client := func(name, service string, call func(context.Context) error) probe {
		return probe{name: name, service: service, check: func(ctx context.Context) (string, fault) {
			if err := call(ctx); err != nil {
				return err.Error(), faultOf(err)
			}
			return "200", passed
		}}
	}
	inspect := func(service, container string) probe {
		return probe{name: "docker:" + container, service: service, docker: true, check: func(ctx context.Context) (string, fault) {
			s, err := dk.inspect(ctx, container)
			if err != nil {
				return err.Error(), wrong
			}
			return s.judge()
		}}
	}
	ps := []probe{
		client("plex", "plex", func(ctx context.Context) error { return env.Plex.Do(ctx, http.MethodGet, "/identity") }),
		client("jellyfin", "jellyfin", func(ctx context.Context) error {
			return env.Jellyfin.GetJSON(ctx, "/System/Info/Public", nil)
		}),
		client("jellyfin-items", "jellyfin", func(ctx context.Context) error {
			return env.Jellyfin.GetJSON(ctx, "/Items?Limit=1", nil)
		}),
		inspect("jellyfin", opt.JellyfinContainer),
		// 403 is up: the WebUI answers and wants a login (its localhost auth bypass is off).
		get("qbittorrent", "qbittorrent", opt.QBittorrentURL+"/api/v2/app/version", 200, 403),
		inspect("qbittorrent", opt.QBittorrentContainer),
	}
	for _, u := range slices.Sorted(maps.Keys(opt.Public)) {
		ps = append(ps, get(u, "cloudflare", u, opt.Public[u]))
	}
	return ps
}

// named is the probe called n: each service's own HTTP probe is the one a restart waits on.
func named(ps []probe, n string) probe {
	return ps[slices.IndexFunc(ps, func(p probe) bool { return p.name == n })]
}

func httpCheck(ctx context.Context, c *http.Client, url string, want []int) (string, fault) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error(), wrong
	}
	resp, err := c.Do(req)
	if err != nil {
		return err.Error(), unanswered
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
			out[i] = result{probe: p.name, service: p.service, docker: p.docker, detail: detail, fault: f, latency: time.Since(start)}
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
