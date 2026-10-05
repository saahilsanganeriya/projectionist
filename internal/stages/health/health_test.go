package health

import (
	"bytes"
	"context"
	"errors"
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
// container outside the allow-list, two restarts inside the cooldown, a restart under a viewer, and a restart
// while the viewer count is unknown.
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
		{"sessions unanswered", func(c *restartCase) { c.playing = sessions(0, timeout) }, true, true, ""},
		{"sessions refused", func(c *restartCase) { c.playing = sessions(0, &jellyfin.StatusError{Code: 401}) }, true, false, "cannot tell"},
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
		{containerState{"running", &healthState{"unhealthy"}}, down},
		{containerState{"running", &healthState{"starting"}}, passed},
		{containerState{"running", nil}, passed},
		{containerState{"exited", nil}, wrong},
		{containerState{"restarting", &healthState{"unhealthy"}}, wrong},
	}
	for _, c := range cases {
		if detail, f := c.state.judge(); f != c.want {
			t.Errorf("%s: fault %q, want %q", detail, f, c.want)
		}
	}
}

// Catches: no restart after K failed runs on a 5xx, the streak not reset by a restart, a second restart inside the
// cooldown or under a viewer, an idle connected client counted as a viewer (it would hold every restart), the
// restart not recorded before it is sent (a crash could then repeat it), a
// followed redirect read as the hostname's answer, results missing from /health and /metrics, and a restart once
// the container is dropped from restartable.
func TestRunHealsJellyfin(t *testing.T) {
	h := fake.NewEnv(t, nil)
	jfStatus := http.StatusServiceUnavailable
	for _, p := range []string{"/System/Info/Public", "/Items"} {
		h.Jellyfin.HandleFunc("GET", p, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(jfStatus) })
	}
	h.Plex.Handle("GET", "/identity", 200, `<MediaContainer machineIdentifier="m"/>`)
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }))
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
	opts := "docker_socket = \"" + serveUnix(t, dockerMux) + "\"\nqbittorrent_url = \"" + qbit.URL + "\"\n"
	public := "[public]\n\"" + tunnel.URL + "/\" = 302\n"
	h.SetOptions(opts + public)

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

	expect("1st failure", run(), 0, "")
	expect("2nd failure", run(), 0, "")
	rep := run()
	expect("3rd failure", rep, 1, "restarting container jellyfin")
	if !restarts[0].Equal(h.Now) || !strings.Contains(rep.Summary, "jellyfin-items FAIL (jellyfin GET /Items?Limit=1: HTTP 503)") {
		t.Fatalf("restart recorded at %v before acting (want %v); summary %q", restarts[0], h.Now, rep.Summary)
	}
	expect("streak restarts after the restart", run(), 1, "")
	h.Now = h.Now.Add(30 * time.Minute)
	run()
	expect("inside the cooldown", run(), 1, "cooldown 1h0m0s")
	h.Now = h.Now.Add(31 * time.Minute)
	h.Jellyfin.Handle("GET", "/Sessions", 200, `[{"Id":"s","UserName":"guest","NowPlayingItem":{"Name":"Heat"}}]`)
	expect("viewer playing", run(), 1, "1 session(s) playing")
	h.Jellyfin.Handle("GET", "/Sessions", 200, `[{"Id":"s","UserName":"guest"}]`) // connected, nothing playing
	expect("viewer gone", run(), 2, "restarting container jellyfin")
	jfStatus = http.StatusOK
	expect("recovered", run(), 2, "")

	var metrics bytes.Buffer
	h.Env.Metrics.WriteText(&metrics)
	if !strings.Contains(metrics.String(), `projectionist_health_restarts_total{container="jellyfin",outcome="restarted"} 2`) ||
		!strings.Contains(metrics.String(), `projectionist_health_up{probe="jellyfin"} 1`) {
		t.Fatalf("metrics:\n%s", metrics.String())
	}

	h.SetOptions(opts + "restartable = []\n" + public)
	h.Now, jfStatus = h.Now.Add(2*time.Hour), http.StatusBadGateway
	run()
	run()
	expect("dropped from restartable", run(), 2, "the container is not in restartable")
}

// Catches: fail_threshold = 0 accepted; every run would then restart a healthy Jellyfin once per cooldown.
func TestRefusesZeroThreshold(t *testing.T) {
	h := fake.NewEnv(t, nil)
	h.SetOptions(`fail_threshold = 0`)
	if err := (Stage{}).Run(context.Background(), h.Env, &stage.Report{}); err == nil || !strings.Contains(err.Error(), "fail_threshold >= 1") {
		t.Fatalf("err = %v, want the fail_threshold refusal", err)
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
	hung := probe{"hung", "jellyfin", func(ctx context.Context) (string, fault) { <-ctx.Done(); return ctx.Err().Error(), down }}
	if r := runProbes(context.Background(), time.Millisecond, []probe{hung}); r[0].fault != down {
		t.Fatalf("result %+v", r[0])
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
