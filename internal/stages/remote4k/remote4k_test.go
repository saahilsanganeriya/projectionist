package remote4k

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fake"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

func ptr(n int) *int { return &n }

func uhd(kbps int, file string) media {
	m := media{VideoResolution: "4k", Bitrate: kbps}
	m.Part = append(m.Part, struct {
		File string `json:"file"`
	}{file})
	return m
}

// Catches: encoding a film that already fits the upload, a TV item, or the smaller of two 4K sources.
func TestPlan(t *testing.T) {
	items := []item{
		{Type: "movie", Title: "Heat", Year: 1995, LibrarySectionID: 4, Media: []media{
			{VideoResolution: "1080", Bitrate: 9000}, uhd(48000, "/m/Heat (1995) {tmdb-949}/a.mkv"), uhd(61000, "/m/Heat (1995) {tmdb-949}/b.mkv")}},
		{Type: "movie", Title: "Ran", Year: 1985, Media: []media{uhd(70000, "/m/Ran/r.mkv"), uhd(18000, "/m/Ran/small.mkv")}},
		{Type: "movie", Title: "Clerks", Year: 1994, Media: []media{{VideoResolution: "1080", Bitrate: 5000}}},
		{Type: "episode", Title: "Pilot", Year: 2008, Media: []media{uhd(40000, "/tv/x.mkv")}},
		{Type: "movie", Title: "Dune", Year: 2021, Media: []media{uhd(20000, "/m/Dune/d.mkv")}},
	}
	jobs, skips := plan(items, 20000, 4)
	if len(jobs) != 1 || jobs[0].Title != "Heat (1995)" || jobs[0].Source.Bitrate != 61000 ||
		jobs[0].Folder != "/m/Heat (1995) {tmdb-949}" || jobs[0].Section != 4 {
		t.Fatalf("jobs = %+v", jobs)
	}
	want := []skip{{"Clerks (1994)", "not a 4K movie"}, {"Pilot (2008)", "not a 4K movie"}}
	if !reflect.DeepEqual(skips, want) {
		t.Fatalf("skips = %+v", skips)
	}
}

// Catches: HDR metadata dropped (wrong profile or colour tags), or audio a TV cannot play passed through.
func TestFFmpegArgs(t *testing.T) {
	ff := config.FFmpeg{Dir: "/ff", Device: "/dev/dri/renderD128"}
	hdr := &probe.Video{PixFmt: "yuv420p10le", ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc"}
	sdr := &probe.Video{PixFmt: "yuv420p"}
	cases := []struct {
		name    string
		v       *probe.Video
		m       media
		want    []string
		without []string
	}{
		{"10-bit HDR, TrueHD 7.1", hdr, media{AudioCodec: "truehd", AudioChannels: ptr(8)},
			[]string{"-profile:v main10", "-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc", "-c:a:0 eac3 -b:a:0 640k -ac:a:0 6"}, nil},
		{"DTS 5.1 keeps channels", hdr, media{AudioCodec: "dca", AudioChannels: ptr(6)}, []string{"-c:a:0 eac3 -b:a:0 640k -c:s"}, []string{"-ac:a:0"}},
		{"8-bit SDR, E-AC-3", sdr, media{AudioCodec: "eac3", AudioChannels: ptr(6)}, []string{"-profile:v main ", "-c:a:0 copy"}, []string{"-color_trc"}},
	}
	for _, c := range cases {
		got := strings.Join(ffmpegArgs(ff, "/src.mkv", "/w/work.mkv", "/w/p.txt", c.v, c.m), " ")
		for _, w := range append(c.want, "-c:v hevc_qsv -b:v 14M -maxrate 20M -bufsize 28M", "-map 0:v:0 -map 0:a:0 -map 0:s?", "-y /w/work.mkv") {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in %s", c.name, w, got)
			}
		}
		for _, w := range c.without {
			if strings.Contains(got, w) {
				t.Errorf("%s: unexpected %q in %s", c.name, w, got)
			}
		}
	}
}

// Catches: a folder name with & ( ) { } breaking Plex's scan request (must match Python's urllib quote).
func TestQuote(t *testing.T) {
	got := quote("/volume1/media/Movies/Fast & Furious (2009) {tmdb-13804}")
	if want := "/volume1/Plex%20Media/Movies/Fast%20%26%20Furious%20%282009%29%20%7Btmdb-13804%7D"; got != want {
		t.Fatalf("quote = %s", got)
	}
}

// Catches, end to end against fake Plex/Jellyfin: the copy not landing under the scheme's name, an encode that
// keeps running while a guest watches (it must be SIGSTOPped, then resumed), Plex/Jellyfin not told, a DV5 source
// encoded, and a rerun re-encoding a film that is already done.
func TestRunEndToEnd(t *testing.T) {
	h := fake.NewEnv(t, nil)
	lib := t.TempDir()
	heat := filepath.Join(lib, "Heat (1995) {tmdb-949}")
	ran := filepath.Join(lib, "Ran (1985) {tmdb-11645}")
	for _, d := range []string{heat, ran} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, filepath.Base(d)+".mkv"), []byte("source"), 0o644)
	}
	work, statusDir := filepath.Join(lib, ".projectionist", "remote4k"), t.TempDir()
	h.SetOptions(`work_dir = "` + work + `"
status_file = "` + filepath.Join(statusDir, "STATUS.txt") + `"`)

	dv8, _ := os.ReadFile("../../probe/testdata/dv8_hevc.json")
	os.WriteFile(filepath.Join(h.Env.FFmpeg.Dir, "dv8.json"), dv8, 0o644)
	os.WriteFile(filepath.Join(h.Env.FFmpeg.Dir, "dv5.json"), []byte(strings.Replace(string(dv8), `"dv_profile": 8`, `"dv_profile": 5`, 1)), 0o644)
	ffdir := h.Env.FFmpeg.Dir
	h.Script(t, "ffprobe", `case "$*" in *Ran*) cat "`+ffdir+`/dv5.json";; *) cat "`+ffdir+`/dv8.json";; esac`+"\n")
	// Stand-in encoder: records its pid, waits for the test's go-ahead, then writes the output and progress.
	h.Script(t, "ffmpeg", `echo $$ > "`+ffdir+`/pid"; echo run >> "`+ffdir+`/runs"
while [ ! -f "`+ffdir+`/go" ]; do sleep 0.01; done
for a; do prev2=$prev; prev=$a; [ "$prev2" = "-progress" ] && progress=$a; done
echo out_time_ms=6612000000 > "$progress"; echo encoded > "$prev"
`)

	h.Plex.Handle("GET", "/playlists", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"9","title":"Other"},{"ratingKey":"77","title":"Remote 4K"}]}}`)
	h.Plex.Handle("GET", "/playlists/77/items", 200, `{"MediaContainer":{"Metadata":[
 {"type":"movie","title":"Heat","year":1995,"librarySectionID":4,"Media":[{"videoResolution":"4k","bitrate":61000,"duration":6612480,"audioCodec":"truehd","audioChannels":8,"Part":[{"file":"`+heat+`/Heat (1995) {tmdb-949}.mkv"}]}]},
 {"type":"movie","title":"Ran","year":1985,"librarySectionID":4,"Media":[{"videoResolution":"4k","bitrate":70000,"duration":9600000,"audioCodec":"aac","Part":[{"file":"`+ran+`/Ran (1985) {tmdb-11645}.mkv"}]}]}]}}`)
	h.Plex.Handle("GET", "/library/sections/4/refresh", 200, "")
	h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	done := make(chan error, 1)
	var rep stage.Report
	go func() { done <- (Stage{}).Run(context.Background(), h.Env, &rep) }()

	waitFor(t, "encoder started", func() bool {
		pid, _ := os.ReadFile(filepath.Join(ffdir, "pid"))
		return len(pid) > 0
	})
	h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="1"><Video type="episode" title="Pilot"><User title="roger"/></Video></MediaContainer>`)
	waitFor(t, "encode paused for the guest", func() bool {
		raw, _ := os.ReadFile(filepath.Join(statusDir, "STATUS.txt"))
		return strings.Contains(string(raw), "paused: someone is watching")
	})
	pid, _ := os.ReadFile(filepath.Join(ffdir, "pid"))
	if state := psState(t, strings.TrimSpace(string(pid))); !strings.HasPrefix(state, "T") {
		t.Fatalf("encoder process state %q, want stopped (T)", state)
	}
	os.WriteFile(filepath.Join(ffdir, "go"), nil, 0o644)
	h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`) // guest leaves
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(heat, "Heat (1995) {tmdb-949} - 2160p 14Mbps.mkv")
	if got, err := os.ReadFile(out); err != nil || string(got) != "encoded\n" {
		t.Fatalf("copy = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(ran, "Ran (1985) {tmdb-11645} - 2160p 14Mbps.mkv")); !os.IsNotExist(err) {
		t.Fatal("Dolby Vision profile 5 source was encoded")
	}
	if left, _ := filepath.Glob(filepath.Join(work, "*.mkv")); len(left) != 0 {
		t.Fatalf("work files left behind: %v", left)
	}
	refresh := h.Plex.Calls("GET", "/library/sections/4/refresh")
	if len(refresh) != 1 || refresh[0].RawQuery != "path="+quote(heat) {
		t.Fatalf("plex refresh = %+v", refresh)
	}
	if upd := h.Jellyfin.Calls("POST", "/Library/Media/Updated"); len(upd) != 1 || !strings.Contains(string(upd[0].Body), `"Path":"`+heat+`"`) {
		t.Fatalf("jellyfin update = %+v", upd)
	}
	if rep.Counts["made"] != 1 || rep.Counts["skip"] != 1 {
		t.Fatalf("counts = %v", rep.Counts)
	}
	if status, fields, ok, _ := h.Env.Store.LastEvent(context.Background(), name, "Ran (1985)"); !ok || status != "skip" || fields["reason"] != "Dolby Vision profile 5" {
		t.Fatalf("Ran history = %s %v", status, fields)
	}

	var again stage.Report
	if err := (Stage{}).Run(context.Background(), h.Env, &again); err != nil {
		t.Fatal(err)
	}
	if runs, _ := os.ReadFile(filepath.Join(ffdir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("encoder ran %d times; a finished film must not be re-encoded", strings.Count(string(runs), "run"))
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func psState(t *testing.T, pid string) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	if err != nil {
		t.Fatalf("ps %s: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}
