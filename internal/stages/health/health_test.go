package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/jellyfin"
	"projectionist/internal/stage"
)

// Catches: a restart before K failures, a restart a refused key or a stopped container cannot cure, a restart of a
// container outside the allow-list, two restarts inside the cooldown, a restart under a viewer, and a restart while
// the viewer count is unread although Jellyfin answers (a 5xx from /Sessions, or another probe answered).
func TestDecide(t *testing.T) {
	sessions := func(n int, err error) func() (int, error) { return func() (int, error) { return n, err } }
	timeout := &url.Error{Op: "Get", URL: "http://jf/Sessions", Err: context.DeadlineExceeded}
	cases := []struct {
		name          string
		edit          func(*restartCase)
		alert, reboot bool
		hold          string
	}{
		{"below threshold", func(c *restartCase) { c.streak = 2 }, false, false, ""},
		{"at threshold, all clear", func(*restartCase) {}, true, true, ""},
		{"nothing to restart (Plex)", func(c *restartCase) { c.container = "" }, true, false, ""},
		{"refused key beside the outage", func(c *restartCase) { c.faults = []fault{down, wrong} }, true, false, "cannot cure"},
		{"not in restartable", func(c *restartCase) { c.allowed = false }, true, false, "not in restartable"},
		{"inside the cooldown", func(c *restartCase) { c.sinceRestart = 59 * time.Minute }, true, false, "cooldown"},
		{"cooldown just over", func(c *restartCase) { c.sinceRestart = time.Hour }, true, true, ""},
		{"someone playing", func(c *restartCase) { c.playing = sessions(1, nil) }, true, false, "1 session(s) playing"},
		{"silent server, sessions unanswered", func(c *restartCase) { c.silent, c.playing = true, sessions(0, timeout) }, true, true, ""},
		{"server answering, sessions unanswered", func(c *restartCase) { c.playing = sessions(0, timeout) }, true, false, "cannot tell"},
		{"silent server, sessions 5xx", func(c *restartCase) {
			c.silent, c.playing = true, sessions(0, &jellyfin.StatusError{Code: 500})
		}, true, false, "cannot tell"},
	}
	for _, tc := range cases {
		c := restartCase{streak: 3, threshold: 3, faults: []fault{down}, container: "jellyfin", allowed: true,
			sinceRestart: 2 * time.Hour, cooldown: time.Hour, playing: sessions(0, nil)}
		tc.edit(&c)
		v := decide(c)
		if v.alert != tc.alert || v.restart != tc.reboot || !strings.Contains(v.hold, tc.hold) || (tc.hold == "") != (v.hold == "") {
			t.Errorf("%s: got %+v; want alert=%v restart=%v hold~%q", tc.name, v, tc.alert, tc.reboot, tc.hold)
		}
	}
}

// Catches: starting a container the owner stopped (or racing Docker's restart policy), and missing "unhealthy".
func TestDockerJudge(t *testing.T) {
	cases := []struct {
		state containerState
		want  fault
	}{
		{containerState{Status: "running", Health: &healthState{"unhealthy"}}, down},
		{containerState{Status: "running", Health: &healthState{"starting"}}, passed},
		{containerState{Status: "running"}, passed},
		{containerState{Status: "exited"}, wrong},
		{containerState{Status: "restarting", Health: &healthState{"unhealthy"}}, wrong},
	}
	for _, c := range cases {
		if detail, f := c.state.judge(); f != c.want {
			t.Errorf("%s: fault %q, want %q", detail, f, c.want)
		}
	}
}

// Catches, over whole runs: no restart after K failed runs on a 5xx, the streak not reset by a restart, a second
// restart inside the cooldown or under a viewer, an idle connected client counted as a viewer, the restart not
// recorded before it is sent, a followed redirect read as the hostname's answer, results missing from /health and
// /metrics, a restart while Jellyfin answers /System/Info/Public but not /Sessions (a library scan can stall one and
// not the other under a guest's stream), Docker's "running" taken for an answer from a Jellyfin that answers
// nothing, a restart when the config names no restartable container, and a restart that did not cure Jellyfin
// resetting the streak, which turned /health back to ok for the next K-1 runs while Jellyfin was still down.
func TestRunHealsJellyfin(t *testing.T) {
	h := fake.NewEnv(t, nil)
	jellyfinAnswers := func(info, items int) {
		h.Jellyfin.HandleFunc("GET", "/System/Info/Public", reply(info))
		h.Jellyfin.HandleFunc("GET", "/Items", reply(items))
	}
	h.Plex.Handle("GET", "/identity", 200, `<MediaContainer machineIdentifier="m"/>`)
	h.Plex.Handle("GET", "/hubs/sections/4", 200, "")
	h.Plex.Handle("GET", "/hubs", 200, "")
	qbit := httptest.NewServer(reply(403))
	t.Cleanup(qbit.Close)
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/web/", http.StatusFound) // following it would read 200, not 302
		}
	}))
	t.Cleanup(tunnel.Close)

	var restarts []time.Time
	cures := true
	dockerMux := http.NewServeMux()
	dockerMux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"State":{"Status":"running"}}`))
	})
	dockerMux.HandleFunc("POST /containers/jellyfin/stop", func(w http.ResponseWriter, r *http.Request) {
		var st State
		h.Env.Store.GetJSON(r.Context(), name, "state", &st)
		restarts = append(restarts, st.LastRestart["jellyfin"])
		w.WriteHeader(http.StatusNoContent)
	})
	dockerMux.HandleFunc("POST /containers/jellyfin/start", func(w http.ResponseWriter, _ *http.Request) {
		if cures {
			jellyfinAnswers(200, 200)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	opts := "fail_threshold = 2\ndocker_socket = \"" + serveUnix(t, dockerMux) + "\"\nqbittorrent_url = \"" + qbit.URL + "\"\n"
	public := "[public]\n\"" + tunnel.URL + "/\" = 302\n"
	h.SetOptions(opts + "restartable = [\"jellyfin\"]\n" + public)

	advance := func(ctx context.Context, d time.Duration) error { h.Now = h.Now.Add(d); return ctx.Err() }
	run := func() stage.Report {
		var rep stage.Report
		if err := (Stage{Sleep: advance}).Run(context.Background(), h.Env, &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	expect := func(step string, rep stage.Report, restartCount int, alert string) {
		t.Helper()
		got := strings.Join(rep.Alerts, " | ")
		if len(restarts) != restartCount || (alert == "") != (got == "") || !strings.Contains(got, alert) {
			t.Fatalf("%s: restarts=%d alerts=%q; want %d restarts, alert ~%q", step, len(restarts), got, restartCount, alert)
		}
	}

	jellyfinAnswers(503, 503)
	expect("1st failure", run(), 0, "")
	rep := run()
	expect("2nd failure", rep, 1, "restarting container jellyfin")
	if !restarts[0].Equal(h.Now) || !strings.Contains(rep.Summary, "jellyfin-items FAIL (jellyfin GET /Items?Limit=1: HTTP 503)") {
		t.Fatalf("restart recorded at %v before acting (want %v); summary %q", restarts[0], h.Now, rep.Summary)
	}
	jellyfinAnswers(503, 503)
	expect("streak restarts after the restart", run(), 1, "")
	h.Now = h.Now.Add(30 * time.Minute)
	expect("inside the cooldown", run(), 1, "cooldown 1h0m0s")
	h.Now = h.Now.Add(31 * time.Minute)
	h.Jellyfin.Handle("GET", "/Sessions", 200, `[{"Id":"s","UserName":"guest","NowPlayingItem":{"Name":"Heat"}}]`)
	expect("viewer playing", run(), 1, "1 session(s) playing")
	h.Jellyfin.Handle("GET", "/Sessions", 200, `[{"Id":"s","UserName":"guest"}]`) // connected, nothing playing
	expect("viewer gone", run(), 2, "restarting container jellyfin")

	h.Now = h.Now.Add(2 * time.Hour)
	jellyfinAnswers(200, 503)
	h.Jellyfin.HandleFunc("GET", "/Sessions", reply(0))
	run()
	expect("Jellyfin answers, /Sessions does not", run(), 2, "cannot tell who is playing")
	jellyfinAnswers(0, 0)
	expect("Jellyfin answers nothing", run(), 3, "restarting container jellyfin")
	jellyfinAnswers(200, 200)
	expect("recovered", run(), 3, "")

	var metrics bytes.Buffer
	h.Env.Metrics.WriteText(&metrics)
	if !strings.Contains(metrics.String(), `projectionist_health_container_actions_total{container="jellyfin",action="restart",outcome="ok"} 3`) ||
		!strings.Contains(metrics.String(), `projectionist_health_up{probe="jellyfin"} 1`) {
		t.Fatalf("metrics:\n%s", metrics.String())
	}

	h.SetOptions(opts + public)
	h.Now = h.Now.Add(2 * time.Hour)
	jellyfinAnswers(502, 502)
	run()
	expect("restartable left out", run(), 3, "the container is not in restartable")

	h.SetOptions(opts + "restartable = [\"jellyfin\"]\n" + public)
	h.Now, cures = h.Now.Add(2*time.Hour), false
	h.Jellyfin.Handle("GET", "/Sessions", 200, `[]`)
	expect("restart that does not cure", run(), 4, "restart of container jellyfin FAILED: jellyfin did not answer within 5m0s")
	expect("still down after the failed restart", run(), 4, "failed 4 checks in a row")
}

// Catches: a restart cut off between Docker's stop and its start (projectionist killed mid-restart, or the stop
// request timing out, after which dockerd kills the container) leaving Jellyfin stopped for good, and the opposite: starting a container the owner stopped (also after a restart of ours
// that never took effect), one Docker's restart policy is already bringing back, or one outside restartable. And a
// dockerd that accepts the connection and never answers hanging the run, and with it every later run.
func TestResumesACutOffRestart(t *testing.T) {
	asked := time.Date(2026, 10, 5, 5, 0, 0, 0, time.UTC) // this stage's recorded restart of jellyfin
	cases := []struct {
		name, status      string
		started, finished time.Duration // the container's StartedAt and FinishedAt, relative to asked
		restartable       string
		starts            int
		hang              bool // inspect never answers
	}{
		{"stopped by our restart, never started again", "exited", -time.Hour, time.Minute, `["jellyfin"]`, 1, false},
		{"killed when our stop's client gave up at its deadline", "exited", -time.Hour, stopBound + 5*time.Second, `["jellyfin"]`, 1, false},
		{"stopped by the owner minutes after our restart", "exited", time.Minute, 5 * time.Minute, `["jellyfin"]`, 0, false},
		{"stopped before we asked", "exited", -time.Hour, -time.Minute, `["jellyfin"]`, 0, false},
		{"stopped by the owner, our restart having done nothing", "exited", -time.Hour, time.Hour, `["jellyfin"]`, 0, false},
		{"Docker's restart policy has it", "restarting", -time.Hour, time.Minute, `["jellyfin"]`, 0, false},
		{"not in restartable", "exited", -time.Hour, time.Minute, `[]`, 0, false},
		{"dockerd never answers", "exited", -time.Hour, time.Minute, `["jellyfin"]`, 0, true},
	}
	for _, tc := range cases {
		h := fake.NewEnv(t, nil)
		h.Jellyfin.Handle("GET", "/System/Info/Public", 200, "")
		h.Env.Store.PutJSON(context.Background(), name, "state", State{Streak: map[string]int{}, LastRestart: map[string]time.Time{"jellyfin": asked}})
		starts := 0
		mux := http.NewServeMux()
		mux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, r *http.Request) {
			if tc.hang {
				<-r.Context().Done()
				return
			}
			fmt.Fprintf(w, `{"State":{"Status":%q,"StartedAt":%q,"FinishedAt":%q}}`,
				tc.status, asked.Add(tc.started).Format(time.RFC3339Nano), asked.Add(tc.finished).Format(time.RFC3339Nano))
		})
		mux.HandleFunc("POST /containers/jellyfin/start", func(w http.ResponseWriter, _ *http.Request) {
			starts++
			w.WriteHeader(http.StatusNoContent)
		})
		opts := fmt.Sprintf("docker_socket = %q\nrestartable = %s\nqbittorrent_url = \"http://127.0.0.1:1\"\n", serveUnix(t, mux), tc.restartable)
		if tc.hang {
			opts += `timeout = "200ms"`
		}
		h.SetOptions(opts)
		var rep stage.Report
		done := make(chan error, 1)
		go func() { done <- (Stage{}).Run(context.Background(), h.Env, &rep) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second): // a watchdog, reached only when the bound is missing
			t.Fatalf("%s: Run still blocked 10 s after start with a 200ms timeout", tc.name)
		}
		alerted := strings.Contains(strings.Join(rep.Alerts, " | "), "never started again; starting it")
		if starts != tc.starts || alerted != (tc.starts > 0) {
			t.Errorf("%s: %d start(s), alerts %q; want %d", tc.name, starts, rep.Alerts, tc.starts)
		}
	}
}

// Catches: options that break the restart bounds accepted: fail_threshold 0 restarts a healthy Jellyfin once per
// cooldown, a cooldown under 1 h restarts it more than once an hour, a zero timeout fails every probe; and an empty
// plex_optimize_window, which would optimize Plex at prime time.
func TestRefusesUnsafeOptions(t *testing.T) {
	h := fake.NewEnv(t, nil)
	for opts, want := range map[string]string{`fail_threshold = 0`: "restart_cooldown >= 1h", `restart_cooldown = "59m"`: "restart_cooldown >= 1h",
		`timeout = "0s"`: "restart_cooldown >= 1h", `plex_optimize_window = ""`: "plex_optimize_window HH:MM-HH:MM"} {
		h.SetOptions(opts)
		if err := (Stage{}).Run(context.Background(), h.Env, &stage.Report{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want the options refusal", opts, err)
		}
	}
}

// Catches: a shutdown mid-run recorded as every service failing, which brings the next restart forward.
func TestShutdownMidRunIsNotAFailure(t *testing.T) {
	h := fake.NewEnv(t, nil)
	h.SetOptions(`docker_socket = "/nonexistent.sock"
qbittorrent_url = "http://127.0.0.1:1"`)
	ctx, cancel := context.WithCancel(context.Background())
	h.Jellyfin.HandleFunc("GET", "/System/Info/Public", func(http.ResponseWriter, *http.Request) { cancel() }) // SIGTERM lands
	err := (Stage{}).Run(ctx, h.Env, &stage.Report{})
	if found, _ := h.Env.Store.GetJSON(context.Background(), name, "state", &State{}); !errors.Is(err, context.Canceled) || found {
		t.Fatalf("err = %v, state stored = %v; want context.Canceled and nothing stored", err, found)
	}
}

// Catches: a service that accepts the connection and never answers hanging the stage, and with it every later
// health run (the scheduler never starts a second copy of a stage).
func TestHungProbeTimesOut(t *testing.T) {
	hung := probe{name: "hung", check: func(ctx context.Context) (string, fault) { <-ctx.Done(); return ctx.Err().Error(), unanswered }}
	if r := runProbes(context.Background(), time.Millisecond, []probe{hung}); r[0].fault != unanswered {
		t.Fatalf("result %+v", r[0])
	}
}

// Catches: a shutdown landing during the viewer check of a Jellyfin that answered no probe read as "/Sessions got no
// answer either, so nobody is watching", which would start a restart without knowing who is watching.
func TestShutdownDuringViewerCheckHoldsTheRestart(t *testing.T) {
	h := fake.NewEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	h.Jellyfin.HandleFunc("GET", "/Sessions", func(w http.ResponseWriter, _ *http.Request) { cancel(); w.Write([]byte(`[]`)) })
	c := restartCase{streak: 3, threshold: 3, faults: []fault{down}, container: "jellyfin", allowed: true,
		sinceRestart: 2 * time.Hour, cooldown: time.Hour, playing: jellyfinPlaying(ctx, h.Env, time.Minute), silent: true}
	if v := decide(c); v.restart || !strings.Contains(v.hold, "cannot tell who is playing") {
		t.Fatalf("verdict %+v; want the restart held", v)
	}
}

// Catches the 2026-10-05 outage and its kin: a stop grace under 120 s (`docker restart`'s 10 s SIGKILLed a Jellyfin
// cancelling a scan), no start sent right after the stop, a start Docker acknowledged but never ran left alone, a
// restart given up at its first failed poll or waiting past 5 min, a service that never comes back not alerted, a
// running container started again, a start after a refused stop, a shutdown abandoning the stop or the start, and
// the stop request given up inside the grace (Docker 24 SIGKILLs the moment the stop's client goes).
func TestRestartSequence(t *testing.T) {
	notBack := "restart of container jellyfin FAILED: jellyfin did not answer within 5m0s of the start (jellyfin GET /System/Info/Public: HTTP 503); container "
	polls := int(readyTimeout / readyPoll)
	cases := []struct {
		name          string
		stopCode      int  // the stop's answer
		ignoredStarts int  // starts Docker answers 204 and never runs; -1: all of them
		failedPolls   int  // polls Jellyfin fails once running; -1: all of them
		shutdown      bool // SIGTERM landed before the restart
		calls, alert  string
	}{
		{"answers on the fourth poll", 204, 0, 3, false, "stop t=120 start poll" + strings.Repeat(" inspect poll", 3), ""},
		{"stop returned but the start never ran", 204, 1, 0, false, "stop t=120 start poll inspect start poll", ""},
		{"no start ever runs", 204, -1, 0, false, "stop t=120 start poll" + strings.Repeat(" inspect start poll", polls), notBack + "exited, no healthcheck"},
		{"running, never answers", 204, 0, -1, false, "stop t=120 start poll" + strings.Repeat(" inspect poll", polls), notBack + "running, no healthcheck"},
		{"stop refused", 500, 0, 0, false, "stop t=120", "FAILED: stop: docker POST /containers/jellyfin/stop?t=120: HTTP 500"},
		{"shutdown before the restart", 204, 0, 0, true, "stop t=120 start poll", ""},
	}
	for _, tc := range cases {
		h := fake.NewEnv(t, nil)
		asked := h.Now
		var mu sync.Mutex
		var calls []string
		running, startedAt, ignored, failed := true, asked.Add(-time.Hour), 0, 0
		// record logs a call and holds the lock until the handler returns: `defer record("x")()`.
		record := func(call string) func() { mu.Lock(); calls = append(calls, call); return mu.Unlock }
		h.Jellyfin.HandleFunc("GET", "/System/Info/Public", func(w http.ResponseWriter, _ *http.Request) {
			defer record("poll")()
			fresh := running && !startedAt.Before(asked) // hung before the restart, starting up after it
			if !fresh || tc.failedPolls < 0 || failed < tc.failedPolls {
				failed += btoi(fresh)
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
		mux := http.NewServeMux()
		mux.HandleFunc("GET /containers/jellyfin/json", func(w http.ResponseWriter, _ *http.Request) {
			defer record("inspect")()
			status := "exited"
			if running {
				status = "running"
			}
			fmt.Fprintf(w, `{"State":{"Status":%q,"StartedAt":%q}}`, status, startedAt.Format(time.RFC3339Nano))
		})
		mux.HandleFunc("POST /containers/jellyfin/stop", func(w http.ResponseWriter, r *http.Request) {
			defer record("stop t=" + r.URL.Query().Get("t"))()
			running = running && tc.stopCode != http.StatusNoContent
			w.WriteHeader(tc.stopCode)
		})
		mux.HandleFunc("POST /containers/jellyfin/start", func(w http.ResponseWriter, _ *http.Request) {
			defer record("start")()
			if tc.ignoredStarts >= 0 && ignored >= tc.ignoredStarts {
				running, startedAt = true, asked.Add(time.Hour)
			} else {
				ignored++
			}
			w.WriteHeader(http.StatusNoContent)
		})
		dk := newDocker(serveUnix(t, mux))
		var stopOpen time.Duration // how long after sending it the client would still wait for the stop's answer
		unix := dk.http.Transport
		dk.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if d, ok := r.Context().Deadline(); ok && strings.HasSuffix(r.URL.Path, "/stop") {
				stopOpen = time.Until(d)
			}
			return unix.RoundTrip(r)
		})
		ctx, cancel := context.WithCancel(context.Background())
		if tc.shutdown {
			cancel()
		}
		advance := func(ctx context.Context, d time.Duration) error { h.Now = h.Now.Add(d); return ctx.Err() }
		st, rep := State{Streak: map[string]int{}, LastRestart: map[string]time.Time{}}, stage.Report{}
		err := (Stage{Sleep: advance}).restart(ctx, h.Env, dk, &st, "jellyfin", "jellyfin",
			named(probes(h.Env, defaults(), dk), "jellyfin"), time.Minute, "test", &rep)
		cancel()
		got := strings.Join(rep.Alerts, " | ")
		if err != nil || strings.Join(calls, " ") != tc.calls || (tc.alert == "") != (got == "") || !strings.Contains(got, tc.alert) {
			t.Errorf("%s: err %v, calls %q, alerts %q; want calls %q, alert ~%q", tc.name, err, strings.Join(calls, " "), got, tc.calls, tc.alert)
		}
		if stopOpen <= stopGrace {
			t.Errorf("%s: the stop request is given up %s after it is sent, inside the %s grace", tc.name, stopOpen.Round(time.Second), stopGrace)
		}
	}
}

// Catches: a second restart in one run recorded at the run's start instead of at its own request. The restart
// before it can take minutes, so resume's window, which opens at the recorded time, could miss the stop it caused.
func TestSecondRestartRecordsItsOwnTime(t *testing.T) {
	h := fake.NewEnv(t, nil)
	t0 := h.Now
	h.Plex.Handle("GET", "/identity", 200, "")
	h.Jellyfin.HandleFunc("GET", "/Items", reply(503))
	var mu sync.Mutex
	calls := map[string]int{}
	answer := func(name string, failures int) http.HandlerFunc { // fails its first `failures` calls, then answers 200
		return func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if calls[name]++; calls[name] <= failures {
				w.WriteHeader(http.StatusBadGateway)
			}
		}
	}
	h.Jellyfin.HandleFunc("GET", "/System/Info/Public", answer("jellyfin", 2)) // the probe, then the first poll after the start
	qbit := httptest.NewServer(answer("qbittorrent", 1))
	t.Cleanup(qbit.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"State":{"Status":"running"}}`))
	})
	mux.HandleFunc("POST /containers/{name}/{action}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h.SetOptions(fmt.Sprintf("fail_threshold = 1\nrestartable = [\"jellyfin\", \"qbittorrent\"]\ndocker_socket = %q\nqbittorrent_url = %q", serveUnix(t, mux), qbit.URL))
	advance := func(ctx context.Context, d time.Duration) error { h.Now = h.Now.Add(d); return ctx.Err() }
	if err := (Stage{Sleep: advance}).Run(context.Background(), h.Env, &stage.Report{}); err != nil {
		t.Fatal(err)
	}
	var st State
	h.Env.Store.GetJSON(context.Background(), name, "state", &st)
	if want := t0.Add(readyPoll); !st.LastRestart["jellyfin"].Equal(t0) || !st.LastRestart["qbittorrent"].Equal(want) {
		t.Fatalf("restarts recorded at %v; want jellyfin at %v, qbittorrent at %v (after Jellyfin's wait)", st.LastRestart, t0, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// reply answers with code, or hangs up without answering when code is 0.
func reply(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if code == 0 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(code)
	}
}

// serveUnix serves h on a unix socket, as dockerd does, and returns the socket path.
func serveUnix(t *testing.T, h http.Handler) string {
	dir, err := os.MkdirTemp("", "hc") // short path: macOS caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.RemoveAll(dir) })
	return sock
}
