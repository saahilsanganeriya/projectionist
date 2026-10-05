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
// nothing, and a restart when the config names no restartable container.
func TestRunHealsJellyfin(t *testing.T) {
	h := fake.NewEnv(t, nil)
	jellyfinAnswers := func(info, items int) {
		h.Jellyfin.HandleFunc("GET", "/System/Info/Public", reply(info))
		h.Jellyfin.HandleFunc("GET", "/Items", reply(items))
	}
	h.Plex.Handle("GET", "/identity", 200, `<MediaContainer machineIdentifier="m"/>`)
	qbit := httptest.NewServer(reply(403))
	t.Cleanup(qbit.Close)
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/web/", http.StatusFound) // following it would read 200, not 302
		}
	}))
	t.Cleanup(tunnel.Close)

	var restarts []time.Time
	dockerMux := http.NewServeMux()
	dockerMux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"State":{"Status":"running"}}`))
	})
	dockerMux.HandleFunc("POST /containers/jellyfin/restart", func(w http.ResponseWriter, r *http.Request) {
		var st State
		h.Env.Store.GetJSON(r.Context(), name, "state", &st)
		restarts = append(restarts, st.LastRestart["jellyfin"])
		w.WriteHeader(http.StatusNoContent)
	})
	opts := "fail_threshold = 2\ndocker_socket = \"" + serveUnix(t, dockerMux) + "\"\nqbittorrent_url = \"" + qbit.URL + "\"\n"
	public := "[public]\n\"" + tunnel.URL + "/\" = 302\n"
	h.SetOptions(opts + "restartable = [\"jellyfin\"]\n" + public)

	run := func() stage.Report {
		var rep stage.Report
		if err := (Stage{}).Run(context.Background(), h.Env, &rep); err != nil {
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
}

// Catches: a restart cut off between Docker's stop and its start (projectionist killed mid-restart) leaving
// Jellyfin stopped for good, and the opposite: starting a container the owner stopped, one Docker's restart policy
// is already bringing back, or one outside restartable.
func TestResumesACutOffRestart(t *testing.T) {
	asked := time.Date(2026, 10, 5, 5, 0, 0, 0, time.UTC) // this stage's recorded restart of jellyfin
	cases := []struct {
		name, status      string
		started, finished time.Duration // the container's StartedAt and FinishedAt, relative to asked
		restartable       string
		starts            int
	}{
		{"stopped by our restart, never started again", "exited", -time.Hour, time.Minute, `["jellyfin"]`, 1},
		{"stopped by the owner after our restart", "exited", time.Minute, time.Hour, `["jellyfin"]`, 0},
		{"stopped before we asked", "exited", -time.Hour, -time.Minute, `["jellyfin"]`, 0},
		{"Docker's restart policy has it", "restarting", -time.Hour, time.Minute, `["jellyfin"]`, 0},
		{"not in restartable", "exited", -time.Hour, time.Minute, `[]`, 0},
	}
	for _, tc := range cases {
		h := fake.NewEnv(t, nil)
		h.Env.Store.PutJSON(context.Background(), name, "state", State{Streak: map[string]int{}, LastRestart: map[string]time.Time{"jellyfin": asked}})
		starts := 0
		mux := http.NewServeMux()
		mux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"State":{"Status":%q,"StartedAt":%q,"FinishedAt":%q}}`,
				tc.status, asked.Add(tc.started).Format(time.RFC3339Nano), asked.Add(tc.finished).Format(time.RFC3339Nano))
		})
		mux.HandleFunc("POST /containers/jellyfin/start", func(w http.ResponseWriter, _ *http.Request) {
			starts++
			w.WriteHeader(http.StatusNoContent)
		})
		h.SetOptions(fmt.Sprintf("docker_socket = %q\nrestartable = %s\nqbittorrent_url = \"http://127.0.0.1:1\"", serveUnix(t, mux), tc.restartable))
		var rep stage.Report
		if err := (Stage{}).Run(context.Background(), h.Env, &rep); err != nil {
			t.Fatal(err)
		}
		alerted := strings.Contains(strings.Join(rep.Alerts, " | "), "never started again; starting it")
		if starts != tc.starts || alerted != (tc.starts > 0) {
			t.Errorf("%s: %d start(s), alerts %q; want %d", tc.name, starts, rep.Alerts, tc.starts)
		}
	}
}

// Catches: options that break the restart bounds accepted: fail_threshold 0 restarts a healthy Jellyfin once per
// cooldown, a cooldown under 1 h restarts it more than once an hour, a zero timeout fails every probe.
func TestRefusesUnsafeOptions(t *testing.T) {
	h := fake.NewEnv(t, nil)
	for _, opts := range []string{`fail_threshold = 0`, `restart_cooldown = "59m"`, `timeout = "0s"`} {
		h.SetOptions(opts)
		if err := (Stage{}).Run(context.Background(), h.Env, &stage.Report{}); err == nil || !strings.Contains(err.Error(), "restart_cooldown >= 1h") {
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

// Catches: a shutdown abandoning a restart request; Docker 24 stops and starts on the request's context, so an
// abandoned restart can leave Jellyfin stopped, which this stage then never restarts.
func TestRestartOutlivesShutdown(t *testing.T) {
	h := fake.NewEnv(t, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/jellyfin/restart", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st, rep := State{Streak: map[string]int{}, LastRestart: map[string]time.Time{}}, stage.Report{}
	if err := restart(ctx, h.Env, newDocker(serveUnix(t, mux)), &st, "jellyfin", "jellyfin", h.Now, "test", &rep); err != nil || len(rep.Alerts) > 0 {
		t.Fatalf("err = %v, alerts %q; want the restart sent despite the shutdown", err, rep.Alerts)
	}
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
