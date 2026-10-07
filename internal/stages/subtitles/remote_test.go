package subtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectionist/internal/stage"
)

// remote gives f an align-worker API on a test listener, with a clock the test moves (b.now).
type remote struct {
	b     *broker
	srv   *httptest.Server
	clock atomic.Int64 // added to the wall clock, in ns
}

func (f *fixture) remote(t *testing.T, lanes int) *remote {
	rm := &remote{b: newBroker()}
	rm.b.now = func() time.Time { return time.Now().Add(time.Duration(rm.clock.Load())) }
	rm.b.wait, rm.b.tick = 50*time.Millisecond, 5*time.Millisecond
	f.st.remote = rm.b
	f.options(fmt.Sprintf("providers = [\"opensubtitles\"]\nalign_remote_lanes = %d", lanes))
	mux := http.NewServeMux()
	for pattern, h := range f.st.Hooks(f.h.Env) {
		mux.Handle(pattern, h)
	}
	rm.srv = httptest.NewServer(mux)
	t.Cleanup(rm.srv.Close)
	// The NAS's ffmpeg streams a lease's tracks; the worker's (the same script) reads them as it would the video.
	ff := f.h.Env.FFmpeg.Dir
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
case "$*" in *matroska*) printf media;; *framecrc*) cat "`+ff+`/ran.framecrc";; *s16le*) cat "`+ff+`/speech.pcm";; *) exit 1;; esac
`)
	return rm
}

// worker runs the real align worker against rm until the test ends (through via, when given); it returns once the NAS
// has heard from it.
func (rm *remote) worker(t *testing.T, f *fixture, via ...string) {
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	done := make(chan struct{})
	go func() {
		err = Work(ctx, WorkerOptions{Server: rm.srv.URL, Via: via, Lanes: 2, FFmpegDir: f.h.Env.FFmpeg.Dir,
			Alass: filepath.Join(f.h.Env.FFmpeg.Dir, "alass"), Work: t.TempDir(), Log: slog.New(slog.DiscardHandler)})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		if <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	})
	for !rm.b.connected() {
		select {
		case <-done:
			t.Fatalf("the worker ended before the NAS heard from it: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
}

func (rm *remote) call(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, rm.srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// lease starts a run of f in the background and leases its first queued title by hand.
func (rm *remote) lease(t *testing.T, f *fixture) (job, <-chan stage.Report) {
	t.Helper()
	rm.call(t, "GET", "/align/ping", nil)
	reports := make(chan stage.Report, 1)
	go func() {
		var rep stage.Report
		if err := f.st.Run(context.Background(), f.h.Env, &rep); err != nil {
			t.Errorf("run: %v", err)
		}
		reports <- rep
	}()
	for {
		code, body := rm.call(t, "POST", "/align/next", nil)
		if code == http.StatusOK {
			var j job
			json.Unmarshal(body, &j)
			return j, reports
		}
	}
}

// history is each title's events in order (status and fields), with the library and each video's file key written
// alike, and without the bytes read: what an alignment recorded, wherever it ran.
func (f *fixture) history() map[string][]string {
	out := map[string][]string{}
	rows, _ := f.h.Env.Store.DB().Query(`SELECT item, status, fields FROM events WHERE stage=? ORDER BY id`, name)
	defer rows.Close()
	for rows.Next() {
		var item, status, raw string
		rows.Scan(&item, &status, &raw)
		var fields map[string]any
		json.Unmarshal([]byte(raw), &fields)
		delete(fields, "video_bytes")
		delete(fields, "disk_bytes")
		if st, err := os.Stat(item); err == nil && fields["file"] == fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano()) {
			fields["file"] = "<this file>"
		}
		b, _ := json.Marshal(fields)
		rel := strings.TrimPrefix(item, f.lib)
		out[rel] = append(out[rel], status+" "+string(b))
	}
	return out
}

// Catches a worker's alignment differing from the NAS's own: another sidecar, outcome or history (an attempt lost on
// the way back, a candidate keyed to another file or provider, so the NAS would try a refused one again), the worker
// not running the stage's alignment on the tracks streamed to it (a PGS track renumbered wrong, the speech audio not
// the one hear picks), the NAS still reading the audio or captions itself, or reading a video no candidate arrived for.
func TestRemoteAlignmentMatchesLocal(t *testing.T) {
	for _, dead := range []bool{false, true} { // dead: every download fails, so the history holds the attempts
		local := newFixture(t)
		local.dead = dead
		lrep := local.run(t)
		f := newFixture(t)
		f.dead = dead
		rm := f.remote(t, 2)
		rm.worker(t, f)
		rep := f.run(t)
		for _, rel := range []string{ran, clerks} {
			got, _ := os.ReadFile(strings.TrimSuffix(f.path(rel), ".mkv") + ".en.srt")
			want, _ := os.ReadFile(strings.TrimSuffix(local.path(rel), ".mkv") + ".en.srt")
			if string(got) != string(want) {
				t.Errorf("dead %v, %s: the worker's sidecar differs from the NAS's own:\n%s\nwant\n%s", dead, rel, got, want)
			}
		}
		if got, want := relative(f.outcomes(), f.lib), relative(local.outcomes(), local.lib); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("dead %v: outcomes %v; want %v", dead, got, want)
		}
		got, want := f.history(), local.history()
		for rel := range want {
			if !slices.Equal(got[rel], want[rel]) {
				t.Errorf("dead %v, %s: history\n%s\nwant\n%s", dead, rel, strings.Join(got[rel], "\n"), strings.Join(want[rel], "\n"))
			}
		}
		calls, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "helper.calls"))
		for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
			if strings.Contains(line, f.lib) && !strings.Contains(line, "-c copy -f matroska -") {
				t.Errorf("dead %v: the NAS read more than a stream copy: %s", dead, line)
			}
		}
		streamedRan := strings.Contains(string(calls), "-map 0:1 -map 0:2 -c copy -f matroska -")
		if rep.Counts["remote_leased"] != 2 || streamedRan == dead || rep.Counts["cap_used"] != lrep.Counts["cap_used"] {
			t.Errorf("dead %v: %d titles leased, Ran's audio and PGS track streamed %v, %d videos read (here %d); want 2, %v, the same",
				dead, rep.Counts["remote_leased"], streamedRan, rep.Counts["cap_used"], lrep.Counts["cap_used"], !dead)
		}
	}
}

func relative(m map[string]string, lib string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[strings.TrimPrefix(k, lib)] = v
	}
	return out
}

// Catches the worker unable to reach the NAS through a relay (the NAS's sshd refuses port forwarding): requests or a
// media stream cut short, or a connection the relay does not close.
func TestRemoteWorkerThroughARelay(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 for the relay")
	}
	f := newFixture(t)
	rm := f.remote(t, 2)
	rm.worker(t, f, "sh", "-c")
	if f.run(t); f.outcomes()[f.path(ran)] != "retimed" || f.outcomes()[f.path(clerks)] != "retimed" {
		t.Errorf("through the relay: Ran %q, Clerks %q; want both retimed", f.outcomes()[f.path(ran)], f.outcomes()[f.path(clerks)])
	}
}

// Catches a lease that never answers losing its title (recorded, or out of the queue), holding the run past the lease
// time, or its title not leased again on the next run.
func TestRemoteLeaseRunsOut(t *testing.T) {
	f := newFixture(t)
	rm := f.remote(t, 1)
	j, reports := rm.lease(t, f)
	if j.Item != f.path(ran) {
		t.Fatalf("leased %s first; want Ran (no English audio)", j.Item)
	}
	rm.clock.Add(int64(leaseFor + time.Minute))
	select {
	case <-reports:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end once the lease ran out")
	}
	if _, err := os.Stat(strings.TrimSuffix(f.path(ran), ".mkv") + ".en.srt"); err == nil || f.outcomes()[f.path(ran)] != "queued" ||
		f.queued()[0] != f.path(ran) {
		t.Fatalf("after its lease ran out, Ran: sidecar %v, recorded %q, queue %v; want none, still queued", err == nil,
			f.outcomes()[f.path(ran)], f.queued())
	}
	rm.clock.Store(0)
	rm.worker(t, f)
	if f.run(t); f.outcomes()[f.path(ran)] != "retimed" {
		t.Errorf("next run: Ran %q; want leased again and retimed", f.outcomes()[f.path(ran)])
	}
}

// Catches a worker's result the NAS takes without its own checks: a subtitle that does not fit the video's length
// written (it must leave the title queued, with an alert), or a sidecar that appeared meanwhile overwritten.
func TestRemoteResultChecks(t *testing.T) {
	f := newFixture(t)
	rm := f.remote(t, 1)
	j, reports := rm.lease(t, f)
	short := srt(captionTimes()[:10], 0)
	if code, _ := rm.call(t, "POST", "/align/result/"+j.Lease, result{Result: "kept", Text: short, Fields: map[string]any{}}); code != http.StatusUnprocessableEntity {
		t.Errorf("a 10-cue subtitle answered %d; want 422", code)
	}
	rm.clock.Add(int64(2 * connectedFor)) // the rest aligns here
	rep := <-reports
	if f.outcomes()[f.path(ran)] != "queued" || f.queued()[0] != f.path(ran) || len(rep.Alerts) != 1 {
		t.Fatalf("Ran %q, queue %v, alerts %v; want still queued, one alert", f.outcomes()[f.path(ran)], f.queued(), rep.Alerts)
	}
	rm.clock.Store(0)
	j, reports = rm.lease(t, f)
	base := strings.TrimSuffix(f.path(ran), ".mkv")
	for _, ext := range []string{".en.srt", ".eng.srt"} {
		os.WriteFile(base+ext, []byte("theirs"), 0o644)
	}
	if code, body := rm.call(t, "POST", "/align/result/"+j.Lease, result{Result: "kept", Text: srt(f.ts, 0), Fields: map[string]any{}}); code != http.StatusNoContent {
		t.Fatalf("result answered %d %s", code, body)
	}
	rm.clock.Add(int64(2 * connectedFor))
	<-reports
	en, _ := os.ReadFile(base + ".en.srt")
	eng, _ := os.ReadFile(base + ".eng.srt")
	if f.outcomes()[f.path(ran)] != "not_written" || string(en) != "theirs" || string(eng) != "theirs" {
		t.Errorf("Ran %q, .en.srt %q, .eng.srt %q; want not_written, both untouched", f.outcomes()[f.path(ran)], en, eng)
	}
}

// Catches the unauthenticated align-worker API answering a caller on another machine.
func TestRemoteRefusesOtherMachines(t *testing.T) {
	f := newFixture(t)
	f.options(`align_remote_lanes = 1`)
	mux := http.NewServeMux()
	for pattern, h := range f.st.Hooks(f.h.Env) {
		mux.Handle(pattern, h)
	}
	for _, c := range []struct {
		method, path, from string
		want               int
	}{
		{"GET", "/align/ping", "127.0.0.1:50000", http.StatusNoContent},
		{"GET", "/align/ping", "[::1]:50000", http.StatusNoContent},
		{"GET", "/align/ping", "192.168.4.30:50000", http.StatusForbidden},
		{"POST", "/align/next", "100.79.1.2:50000", http.StatusForbidden},
		{"GET", "/align/media/x", "192.168.4.30:50000", http.StatusForbidden},
		{"GET", "/align/candidate/x/0", "192.168.4.30:50000", http.StatusForbidden},
		{"POST", "/align/result/x", "192.168.4.30:50000", http.StatusForbidden},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.RemoteAddr = c.from
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s from %s: %d; want %d", c.method, c.path, c.from, rec.Code, c.want)
		}
	}
}

// Catches the queue waiting for a worker that never called, or one that went quiet, instead of aligning here.
func TestRemoteFallsBackToLocal(t *testing.T) {
	for _, quiet := range []time.Duration{0, connectedFor + time.Second} {
		f := newFixture(t)
		rm := f.remote(t, 2)
		if quiet > 0 {
			rm.call(t, "GET", "/align/ping", nil)
			rm.clock.Add(int64(quiet))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var rep stage.Report
		err := f.st.Run(ctx, f.h.Env, &rep)
		cancel()
		if o := f.outcomes(); err != nil || o[f.path(ran)] != "retimed" || o[f.path(clerks)] != "retimed" || rep.Counts["remote_leased"] != 0 {
			t.Errorf("worker quiet for %v: run %v, Ran %q, Clerks %q, %d leased; want both aligned here", quiet, err,
				o[f.path(ran)], o[f.path(clerks)], rep.Counts["remote_leased"])
		}
	}
}

// Catches Plex never hearing of a sidecar (its own watcher does not add one beside a video it holds): each written
// sidecar's folder, and once each folder of the sidecars written before, gets one partial scan in the section holding
// it, with the path escaped as Plex reads it; a folder Plex did not take is kept for the next run, and never fails one.
func TestPlexScansEachNewSidecarsFolder(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(f.lib, "Movies", "Gone (2001)", "Gone (2001).mkv")
	for _, item := range []string{f.path(dune), gone} {
		f.h.Env.Store.AddEvent(context.Background(), f.h.Now, name, item, "retimed", map[string]any{"sidecar": "x.en.srt"})
	}
	f.h.Plex.Handle("GET", "/library/sections", 200, fmt.Sprintf(`{"MediaContainer":{"Directory":[`+
		`{"key":"4","Location":[{"path":%q}]},{"key":"2","Location":[{"path":%q}]}]}}`, f.path("Movies"), f.path("TV")))
	var refusing atomic.Bool
	refusing.Store(true)
	f.h.Plex.HandleFunc("GET", "/library/sections/4/refresh", func(w http.ResponseWriter, _ *http.Request) {
		if refusing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	scanned := func(from int) []string {
		var out []string
		for _, c := range f.h.Plex.Calls("GET", "/library/sections/4/refresh")[from:] {
			q, _ := url.ParseQuery(c.RawQuery)
			if strings.Contains(c.RawQuery, "+") {
				t.Errorf("refresh query %q: a space written as +, which Plex reads as a plus sign", c.RawQuery)
			}
			out = append(out, q.Get("path"))
		}
		slices.Sort(out)
		return out
	}
	want := []string{filepath.Dir(f.path(clerks)), filepath.Dir(f.path(dune)), filepath.Dir(f.path(ran))}
	var pending []string
	rep := f.run(t)
	f.h.Env.Store.GetJSON(context.Background(), name, "plex_pending", &pending)
	if slices.Sort(pending); len(rep.Alerts) > 0 || len(scanned(0)) == 0 || !slices.Equal(pending, want) {
		t.Fatalf("Plex refusing: alerts %v, scans asked %v, kept %v; want none, some, all of %v", rep.Alerts, scanned(0), pending, want)
	}
	refused := len(f.h.Plex.Calls("GET", "/library/sections/4/refresh"))
	refusing.Store(false)
	f.run(t)
	if got := scanned(refused); !slices.Equal(got, want) {
		t.Errorf("next run: scans %v; want each of %v asked again, once", got, want)
	}
	f.run(t)
	if n := len(f.h.Plex.Calls("GET", "/library/sections/4/refresh")); n != refused+len(want) {
		t.Errorf("%d scans after a third run; want %d: none once Plex took them", n, refused+len(want))
	}
}
