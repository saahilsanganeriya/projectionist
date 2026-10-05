package health

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// Catches: an optimize sent outside the night window (its ends included), inside the 24 h cooldown, while a guest
// watches, or while the viewer count cannot be read.
func TestOptimizeHold(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	window, _ := config.ParseWindow("03:00-06:00")
	at := func(hh, mm int) time.Time { return time.Date(2026, 10, 6, hh, mm, 0, 0, la) }
	nobody := func() (int, error) { return 0, nil }
	cases := []struct {
		name     string
		now      time.Time
		ago      time.Duration // since the last optimize; 0 = never
		watching func() (int, error)
		hold     string
	}{
		{"before the window", at(2, 59), 0, nobody, "outside"},
		{"the window opens", at(3, 0), 0, nobody, ""},
		{"its last minute", at(5, 59), 0, nobody, ""},
		{"the window closed", at(6, 0), 0, nobody, "outside"},
		{"inside the cooldown", at(3, 30), 23*time.Hour + 59*time.Minute, nobody, "at most once per 24h"},
		{"cooldown over", at(3, 30), 24 * time.Hour, nobody, ""},
		{"a guest watching", at(3, 30), 0, func() (int, error) { return 1, nil }, "1 guest session(s)"},
		{"count unread", at(3, 30), 0, func() (int, error) { return 0, errors.New("plex: HTTP 503") }, "cannot tell who is watching"},
	}
	for _, c := range cases {
		var last time.Time
		if c.ago > 0 {
			last = c.now.Add(-c.ago)
		}
		if got := optimizeHold(c.now, la, window, last, c.watching); (got == "") != (c.hold == "") || !strings.Contains(got, c.hold) {
			t.Errorf("%s: hold %q; want ~%q", c.name, got, c.hold)
		}
	}
}

// Catches: an optimize after fewer than 3 slow runs, at a load equal to plex_slow, after a streak a fast run broke,
// for 500 changes (the bar is more than 500), or while Plex fails its probe (nothing is timed then); and no optimize
// after 3 slow runs, after 501 changes, or for a hub answering 5xx (the tab does not load). A hub answering 4xx (a
// wrong plex_movies_section) alerts instead of counting as slow.
func TestPlexOptimizeTriggers(t *testing.T) {
	cases := []struct {
		name     string
		loads    []time.Duration // the Movies hub's load on each run (Home takes 10 ms)
		hubCode  int             // the Movies hub's answer
		identity int             // the plex probe's answer
		changes  int
		sends    int
		alert    string // on the last run
	}{
		{"fast", []time.Duration{time.Second, time.Second, time.Second}, 200, 200, 0, 0, ""},
		{"slow on 2 runs", []time.Duration{6 * time.Second, 6 * time.Second}, 200, 200, 0, 0, ""},
		{"slow on 3 runs in a row", []time.Duration{6 * time.Second, 6 * time.Second, 6 * time.Second}, 200, 200, 0, 1, ""},
		{"exactly plex_slow", []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}, 200, 200, 0, 0, ""},
		{"a fast run breaks the streak", []time.Duration{6 * time.Second, 6 * time.Second, time.Second, 6 * time.Second, 6 * time.Second}, 200, 200, 0, 0, ""},
		{"5xx on 3 runs", []time.Duration{0, 0, 0}, 503, 200, 0, 1, ""},
		{"wrong section", []time.Duration{0}, 404, 200, 0, 0, "plex hub refused, which no optimize cures: plex GET /hubs/sections/4?count=12: HTTP 404"},
		{"plex down", []time.Duration{6 * time.Second}, 200, 503, 501, 0, ""},
		{"500 changes", []time.Duration{time.Second}, 200, 200, 500, 0, ""},
		{"501 changes", []time.Duration{time.Second}, 200, 200, 501, 1, ""},
	}
	for _, c := range cases {
		p := newPlexEnv(t, time.Date(2026, 10, 6, 10, 30, 0, 0, time.UTC)) // 03:30 Pacific
		p.Plex.Handle("GET", "/identity", c.identity, "")
		p.Env.Store.AddLibraryChanges(context.Background(), "rename", c.changes)
		var rep stage.Report
		for _, load := range c.loads {
			p.clk.hub(load, c.hubCode)
			rep = p.run(t)
			p.clk.add(5 * time.Minute)
		}
		got := strings.Join(rep.Alerts, " | ")
		if sends := len(p.Plex.Calls("PUT", "/library/optimize")); sends != c.sends || (c.alert == "") != (got == "") || !strings.Contains(got, c.alert) {
			t.Errorf("%s: %d optimize(s), alerts %q; want %d, alert ~%q", c.name, sends, got, c.sends, c.alert)
		}
		if hubs := len(p.Plex.Calls("GET", "/hubs")); c.identity != 200 && hubs != 0 {
			t.Errorf("%s: %d hub loads while Plex fails its probe", c.name, hubs)
		}
	}
}

// Catches, over one optimize's life: a due optimize sent outside the window or under a guest, a refused request
// spending the day's optimize, the optimize sent before it is recorded, the hubs timed (or a second optimize sent)
// while Plex still lists it, an unrelated activity taken for it, no re-timing once it ends, a still-slow Plex alerted
// on one run only (DSM polls /health every 15 min) or after a fast load, the changes counted before an optimize
// making the next one due again, the 24 h cooldown ignored, and an optimize Plex lists for over an hour not alerted.
func TestPlexOptimizeLifecycle(t *testing.T) {
	p := newPlexEnv(t, time.Date(2026, 10, 5, 21, 0, 0, 0, time.UTC)) // 14:00 Pacific
	ctx := context.Background()
	optimizing := `{"MediaContainer":{"size":2,"Activity":[{"uuid":"a","type":"library.update.section","title":"Scanning Movies"},` +
		`{"uuid":"b","title":"Optimizing database","progress":40}]}}`
	scanOnly := `{"MediaContainer":{"size":1,"Activity":[{"uuid":"a","type":"library.update.section","title":"Scanning Movies"}]}}`
	var sentWhileRecorded []bool
	putCode := http.StatusInternalServerError
	p.Plex.HandleFunc("PUT", "/library/optimize", func(w http.ResponseWriter, _ *http.Request) {
		var st State
		p.Env.Store.GetJSON(ctx, name, "state", &st)
		sentWhileRecorded = append(sentWhileRecorded, st.Plex.Running && st.Plex.LastOptimize.Equal(p.clk.now()))
		w.WriteHeader(putCode)
	})
	step := func(what string, sends int, alert string) stage.Report {
		t.Helper()
		rep := p.run(t)
		got := strings.Join(rep.Alerts, " | ")
		if n := len(p.Plex.Calls("PUT", "/library/optimize")); n != sends || (alert == "") != (got == "") || !strings.Contains(got, alert) {
			t.Fatalf("%s: %d optimize request(s), alerts %q; want %d, alert ~%q", what, n, got, sends, alert)
		}
		p.clk.add(5 * time.Minute)
		return rep
	}

	p.Env.Store.AddLibraryChanges(ctx, "rename", 600)
	step("due at 14:00", 0, "")
	p.clk.set(time.Date(2026, 10, 6, 10, 10, 0, 0, time.UTC)) // 03:10 Pacific
	p.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="1"><Video type="movie" title="Heat"><User title="guest"/></Video></MediaContainer>`)
	step("a guest watching", 0, "")
	p.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	step("Plex refuses the request", 1, "Plex Optimize Database (due: 600 library changes since the last optimize) failed")
	putCode = http.StatusOK
	step("sent", 2, "")
	if len(sentWhileRecorded) != 2 || !sentWhileRecorded[0] || !sentWhileRecorded[1] {
		t.Fatalf("recorded as running at each request: %v; want [true true]", sentWhileRecorded)
	}
	p.Plex.Handle("GET", "/activities", 200, optimizing)
	hubs := len(p.Plex.Calls("GET", "/hubs"))
	step("still optimizing", 2, "")
	if n := len(p.Plex.Calls("GET", "/hubs")); n != hubs {
		t.Fatalf("hubs loaded %d time(s) while Plex optimizes", n-hubs)
	}
	p.Plex.Handle("GET", "/activities", 200, scanOnly)
	p.clk.hub(6*time.Second, 200)
	step("ended, still slow", 2, "still slow after its database optimize: 10ms before, 6s now")
	step("still slow", 2, "still slow after its database optimize")
	p.clk.hub(17*time.Millisecond, 200)
	step("fast again", 2, "")
	var metrics bytes.Buffer
	p.Env.Metrics.WriteText(&metrics)
	if !strings.Contains(metrics.String(), `projectionist_health_plex_hub_seconds{hub="movies"} 0.017`) {
		t.Fatalf("metrics:\n%s", metrics.String())
	}

	p.Env.Store.AddLibraryChanges(ctx, "collections", 500)
	step("500 changes since the optimize", 2, "")
	p.Env.Store.AddLibraryChanges(ctx, "details", 1)
	step("501, inside the cooldown", 2, "")
	p.clk.set(time.Date(2026, 10, 7, 10, 30, 0, 0, time.UTC)) // the next night, 24 h 10 min after the send
	step("501, the next night", 3, "")
	p.Plex.Handle("GET", "/activities", 200, optimizing)
	p.clk.add(55 * time.Minute)
	step("optimizing for an hour", 3, "still runs after 1h0m0s")
}

// plexEnv is a harness on which every probe but Plex's passes. Its clock moves only when a hub is loaded, by that
// load's latency.
type plexEnv struct {
	*fake.Harness
	clk *clock
}

func newPlexEnv(t *testing.T, at time.Time) *plexEnv {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	p := &plexEnv{Harness: fake.NewEnv(t, nil), clk: &clock{t: at, movies: 10 * time.Millisecond, code: 200}}
	p.Env.Now, p.Env.Loc = p.clk.now, la
	p.Plex.Handle("GET", "/identity", 200, "")
	p.Plex.Handle("GET", "/activities", 200, `{"MediaContainer":{"size":0}}`)
	p.Plex.Handle("PUT", "/library/optimize", 200, "")
	p.Plex.HandleFunc("GET", "/hubs/sections/4", func(w http.ResponseWriter, _ *http.Request) {
		p.clk.mu.Lock()
		defer p.clk.mu.Unlock()
		p.clk.t = p.clk.t.Add(p.clk.movies)
		w.WriteHeader(p.clk.code)
	})
	p.Plex.HandleFunc("GET", "/hubs", func(http.ResponseWriter, *http.Request) { p.clk.add(10 * time.Millisecond) })
	p.Jellyfin.Handle("GET", "/System/Info/Public", 200, "")
	p.Jellyfin.Handle("GET", "/Items", 200, "")
	qbit := httptest.NewServer(reply(200))
	t.Cleanup(qbit.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{name}/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"State":{"Status":"running"}}`))
	})
	p.SetOptions("docker_socket = \"" + serveUnix(t, mux) + "\"\nqbittorrent_url = \"" + qbit.URL + "\"\n")
	return p
}

func (p *plexEnv) run(t *testing.T) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), p.Env, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// clock is the stage's Env.Now, moved from the fake servers' handlers too, and the Movies hub's next answer.
type clock struct {
	mu     sync.Mutex
	t      time.Time
	movies time.Duration // the Movies hub's load; Home takes 10 ms
	code   int           // its status
}

func (c *clock) hub(load time.Duration, code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.movies, c.code = load, code
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time)     { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }
