package subtitles

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
case "$*" in *matroska*) printf media;; *framecrc*) cat "`+ff+`/ran.framecrc";; *s16le*) cat "`+ff+`/speech.pcm";; *"-f sup"*) p=; for a; do [ "$p" = sup ] && : > "$a"; p=$a; done;; *) exit 1;; esac
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
// the way back, a candidate keyed to another file or provider, so the NAS would try a refused one again, a verdict of
// no fit recorded while Subdl is out), the worker not running the stage's alignment on the tracks streamed to it (a PGS
// track renumbered wrong, the speech audio not the one hear picks), the NAS still reading the audio or captions itself,
// or reading a video no candidate arrived for.
func TestRemoteAlignmentMatchesLocal(t *testing.T) {
	for _, c := range []string{"", "dead", "dead, Subdl out"} { // dead: every download fails, so the history holds the attempts
		dead := c != ""
		local := newFixture(t)
		local.dead = dead
		f := newFixture(t)
		f.dead = dead
		rm := f.remote(t, 2)
		if c == "dead, Subdl out" { // Ran and Clerks wait for Subdl, not refused
			local.parkSubdl()
			local.options(`providers = ["opensubtitles", "subdl"]`)
			f.parkSubdl()
			f.options("providers = [\"opensubtitles\", \"subdl\"]\nalign_remote_lanes = 2")
		}
		lrep := local.run(t)
		rm.worker(t, f)
		rep := f.run(t)
		for _, rel := range []string{ran, clerks} {
			got, _ := os.ReadFile(strings.TrimSuffix(f.path(rel), ".mkv") + ".en.srt")
			want, _ := os.ReadFile(strings.TrimSuffix(local.path(rel), ".mkv") + ".en.srt")
			if string(got) != string(want) {
				t.Errorf("%q, %s: the worker's sidecar differs from the NAS's own:\n%s\nwant\n%s", c, rel, got, want)
			}
		}
		if got, want := relative(f.outcomes(), f.lib), relative(local.outcomes(), local.lib); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: outcomes %v; want %v", c, got, want)
		}
		got, want := f.history(), local.history()
		for rel := range want {
			if !slices.Equal(got[rel], want[rel]) {
				t.Errorf("%q, %s: history\n%s\nwant\n%s", c, rel, strings.Join(got[rel], "\n"), strings.Join(want[rel], "\n"))
			}
		}
		calls, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "helper.calls"))
		for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") { // OCR's copy of a refused title's image track is the NAS's own
			if strings.Contains(line, f.lib) && !strings.Contains(line, "-c copy -f matroska -") && !strings.Contains(line, "-c copy -f sup ") {
				t.Errorf("%q: the NAS read more than a stream copy: %s", c, line)
			}
		}
		streamedRan := strings.Contains(string(calls), "-map 0:1 -map 0:2 -c copy -f matroska -")
		if rep.Counts["remote_leased"] != 2 || streamedRan == dead || rep.Counts["cap_used"] != lrep.Counts["cap_used"] {
			t.Errorf("%q: %d titles leased, Ran's audio and PGS track streamed %v, %d videos read (here %d); want 2, %v, the same",
				c, rep.Counts["remote_leased"], streamedRan, rep.Counts["cap_used"], lrep.Counts["cap_used"], !dead)
		}
	}
}

// lateAudio gives f a fake ffmpeg for videos whose audio starts 8.4 s in, as Lord of War's does, and whose timestamps
// start 1.4 s in, as an MPEG-TS's do. Read from the library, the captions are where they are (ffmpeg counts from the
// file's start), or 1.4 s late with -copyts (its raw timestamps); read from an align worker's media they come 8.4 s
// early unless -copyts keeps the media's own timestamps (the video's), as ffmpeg counts a file's time from its first
// packet (there the audio's); and the speech starts at the first audio sample unless aresample pads it to the start
// (first_pts=0) on the video's time base.
func lateAudio(t *testing.T, f *fixture) {
	ff := f.h.Env.FFmpeg.Dir
	early, late := make([]float64, len(f.ts)), make([]float64, len(f.ts))
	for i, s := range f.ts {
		early[i], late[i] = s-8.4, s+1.4
	}
	os.WriteFile(filepath.Join(ff, "early.framecrc"), []byte(framecrc(early)), 0o644)
	os.WriteFile(filepath.Join(ff, "late.framecrc"), []byte(framecrc(late)), 0o644)
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
base=; case "$*" in *-copyts*"`+f.lib+`"*) base=raw;; *"`+f.lib+`"*|*-copyts*) base=video;; esac
case "$*" in
*matroska*) printf media;;
*framecrc*) case $base in video) cat "`+ff+`/ran.framecrc";; raw) cat "`+ff+`/late.framecrc";; *) cat "`+ff+`/early.framecrc";; esac;;
*s16le*) case "$base $*" in "video "*first_pts=0*) cat "`+ff+`/speech.pcm";; *) tail -c +268801 "`+ff+`/speech.pcm";; esac;;
*"-f sup"*) p=; for a; do [ "$p" = sup ] && : > "$a"; p=$a; done;;
*) exit 1;;
esac
`)
}

// Catches an alignment's reference counted from anywhere but the video's start when its audio starts late (Lord of
// War's, 8.4 s in): an align worker taking the streamed media's time from its first packet (Lord of War's PGS captions
// came 8.4 s early there, and a sidecar 8.2 s early was recorded kept), or the speech heard from the first audio
// sample, here or on a worker. Ran's candidate must land on its captions and Clerks' alass alignment pass its speech
// cross-check, wherever they are aligned.
func TestReferencesCountFromTheVideosStart(t *testing.T) {
	for _, onWorker := range []bool{false, true} {
		f := newFixture(t)
		if onWorker {
			rm := f.remote(t, 2)
			lateAudio(t, f)
			rm.worker(t, f)
		} else {
			lateAudio(t, f)
		}
		f.run(t)
		o := f.outcomes()
		written, _ := os.ReadFile(strings.TrimSuffix(f.path(ran), ".mkv") + ".en.srt")
		got, ok := cues(string(written)), o[f.path(clerks)] == "retimed"
		for i, s := range got {
			ok = ok && len(got) == len(f.ts) && s.start-f.ts[i] <= 0.01 && s.start-f.ts[i] >= -0.21
		}
		if !ok || len(got) == 0 {
			t.Errorf("on a worker %v: Ran %q with %d cues (the first %v, its caption %v), Clerks %q; want both aligned onto the video's own timing",
				onWorker, o[f.path(ran)], len(got), got[:min(1, len(got))], f.ts[0], o[f.path(clerks)])
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
	f.h.Now = f.h.Now.Add(dlGap) // Clerks, aligned here as the lease ran out, took OpenSubtitles' slot
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

// Catches a worker's alass failure refused by the NAS (400): the title would stay queued and be leased again, failing
// the same way, every run. It is recorded as the title's own, as one here is.
func TestRemoteAlassFailureIsRecorded(t *testing.T) {
	f := newFixture(t)
	rm := f.remote(t, 1)
	j, reports := rm.lease(t, f)
	if code, body := rm.call(t, "POST", "/align/result/"+j.Lease, result{Result: "alass_failed", Detail: "alass: exit status 1"}); code != http.StatusNoContent {
		t.Fatalf("alass_failed answered %d %s; want 204", code, body)
	}
	rm.clock.Add(int64(2 * connectedFor))
	<-reports
	if o := f.outcomes()[j.Item]; o != "alass_failed" || slices.Contains(f.queued(), j.Item) {
		t.Errorf("%s %q, queue %v; want alass_failed, out of the queue", j.Item, o, f.queued())
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

// plexDB writes a Plex library database in which each video is the one media part of the item with that ratingKey.
func (f *fixture) plexDB(t *testing.T, keys map[string]int) string {
	p := filepath.Join(t.TempDir(), "com.plexapp.plugins.library.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE media_items (id INTEGER PRIMARY KEY, metadata_item_id INTEGER, deleted_at INTEGER);
		CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT, deleted_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for file, key := range keys {
		res, _ := db.Exec(`INSERT INTO media_items(metadata_item_id) VALUES (?)`, key)
		id, _ := res.LastInsertId()
		db.Exec(`INSERT INTO media_parts(media_item_id, file) VALUES (?, ?)`, id, file)
	}
	return p
}

// refreshed counts Plex's item refreshes by ratingKey.
func (f *fixture) refreshed() map[string]int {
	out := map[string]int{}
	for _, k := range []string{"11", "12", "13", "14", "15"} {
		if n := len(f.h.Plex.Calls("PUT", "/library/metadata/"+k+"/refresh")); n > 0 {
			out[k] = n
		}
	}
	return out
}

// Catches a new sidecar Plex never shows (neither its watcher nor a partial scan of the folder attaches one beside a
// video it holds already: 1,721 stayed hidden through full scans, and each appeared once its item was refreshed), or
// another item refreshed: the item whose media part is each written sidecar's video, and once each video whose sidecar
// was written before the stage refreshed items (not one that is gone), is refreshed once, and no other.
func TestPlexRefreshesEachNewSidecarsItem(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(f.lib, "Movies", "Gone (2001)", "Gone (2001).mkv")
	for _, item := range []string{f.path(dune), gone} {
		f.h.Env.Store.AddEvent(context.Background(), f.h.Now, name, item, "retimed", map[string]any{"sidecar": "x.en.srt"})
	}
	f.options("providers = [\"opensubtitles\"]\nplex_db = \"" + f.plexDB(t, map[string]int{f.path(ran): 11, f.path(clerks): 12,
		f.path(dune): 13, f.path(heat): 14, gone: 15}) + "\"")
	for _, k := range []string{"11", "12", "13", "14", "15"} {
		f.h.Plex.Handle("PUT", "/library/metadata/"+k+"/refresh", 200, "")
	}
	f.run(t)
	f.run(t)
	if got, want := f.refreshed(), map[string]int{"11": 1, "12": 1, "13": 1}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refreshes by ratingKey %v; want %v: Ran's, Clerks' and Dune's items once each", got, want)
	}
}

// Catches Plex trouble failing a run, raising an alert, or losing a refresh: with Plex's database unreadable, then with
// Plex refusing, each run ends without an alert and keeps the videos queued; the run after Plex answers refreshes each.
func TestPlexErrorsNeverFailTheRun(t *testing.T) {
	f := newFixture(t)
	f.options("providers = [\"opensubtitles\"]\nplex_db = \"" + filepath.Join(f.dir, "missing.db") + "\"")
	want := []string{f.path(clerks), f.path(ran)}
	pending := func() []string {
		var out []string
		f.h.Env.Store.GetJSON(context.Background(), name, "plex_pending", &out)
		slices.Sort(out)
		return out
	}
	if rep := f.run(t); len(rep.Alerts) > 0 || !slices.Equal(pending(), want) {
		t.Fatalf("database unreadable: alerts %v, kept %v; want none, %v", rep.Alerts, pending(), want)
	}
	f.options("providers = [\"opensubtitles\"]\nplex_db = \"" + f.plexDB(t, map[string]int{f.path(ran): 11, f.path(clerks): 12}) + "\"")
	var refusing atomic.Bool
	refusing.Store(true)
	for _, k := range []string{"11", "12"} {
		f.h.Plex.HandleFunc("PUT", "/library/metadata/"+k+"/refresh", func(w http.ResponseWriter, _ *http.Request) {
			if refusing.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
	}
	if rep := f.run(t); len(rep.Alerts) > 0 || len(f.refreshed()) == 0 || !slices.Equal(pending(), want) {
		t.Fatalf("Plex refusing: alerts %v, refreshes asked %v, kept %v; want none, some, %v", rep.Alerts, f.refreshed(), pending(), want)
	}
	asked := f.refreshed()
	refusing.Store(false)
	f.run(t)
	for _, k := range []string{"11", "12"} {
		if f.refreshed()[k] != asked[k]+1 {
			t.Errorf("after Plex answered, %s refreshed %d time(s) more; want once", k, f.refreshed()[k]-asked[k])
		}
	}
	if len(pending()) > 0 {
		t.Errorf("still queued %v; want none", pending())
	}
}
