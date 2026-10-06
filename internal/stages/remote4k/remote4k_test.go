package remote4k

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fake"
	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

func uhd(path string, width int, kbps int64, dv int) version {
	return version{path, probe.Info{BitRate: kbps * 1000, Video: &probe.Video{Width: width, DVProfile: dv}}}
}

// Catches: encoding a film that already fits the upload, a 1080p-only film, the smaller of two 4K sources (also
// when only size and duration give a bitrate), a scope (3840x1600) source missed, or a DV5 source encoded.
func TestPick(t *testing.T) {
	sized := version{"sized", probe.Info{Size: 45e9, Duration: 6000, Video: &probe.Video{Width: 3840}}} // 60000 kbps
	cases := []struct {
		name       string
		vs         []version
		wantSrc    string
		wantReason string
		wantDone   bool
	}{
		{"1080p only", []version{uhd("hd", 1920, 9000, 0)}, "", "not a 4K movie", false},
		{"highest-bitrate 4K", []version{uhd("hd", 1920, 9000, 0), uhd("a", 3840, 48000, 0), uhd("b", 3840, 61000, 0)}, "b", "", false},
		{"scope 4K", []version{{"scope", probe.Info{BitRate: 7e7, Video: &probe.Video{Width: 3840, Height: 1600}}}}, "scope", "", false},
		{"small 4K exists", []version{uhd("big", 3840, 70000, 0), uhd("small", 3840, 18000, 0)}, "", "", true},
		{"bitrate from size and duration", []version{uhd("a", 3840, 48000, 0), sized}, "sized", "", false},
		{"Dolby Vision profile 5", []version{uhd("dv5", 3840, 70000, 5)}, "", "Dolby Vision profile 5", false},
	}
	for _, c := range cases {
		src, reason, done := pick(c.vs, 20000)
		if src.Path != c.wantSrc || reason != c.wantReason || done != c.wantDone {
			t.Errorf("%s: pick = %q %q %v; want %q %q %v", c.name, src.Path, reason, done, c.wantSrc, c.wantReason, c.wantDone)
		}
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
		a       probe.Audio
		want    []string
		without []string
	}{
		{"10-bit HDR, TrueHD 7.1", hdr, probe.Audio{Codec: "truehd", Channels: 8},
			[]string{"-profile:v main10", "-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc", "-c:a:0 eac3 -b:a:0 640k -ac:a:0 6"}, nil},
		{"DTS 5.1 keeps channels", hdr, probe.Audio{Codec: "dts", Channels: 6}, []string{"-c:a:0 eac3 -b:a:0 640k -c:s"}, []string{"-ac:a:0"}},
		{"8-bit SDR, E-AC-3", sdr, probe.Audio{Codec: "eac3", Channels: 6}, []string{"-profile:v main ", "-c:a:0 copy"}, []string{"-color_trc"}},
	}
	for _, c := range cases {
		got := strings.Join(ffmpegArgs(ff, "/src.mkv", "/w/work.mkv", "/w/p.txt", c.v, c.a), " ")
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

// Catches, end to end against fake Plex/Jellyfin: a Jellyfin playlist or collection named "Remote 4K" ignored (or
// another collection obeyed, or both asked for in the one query the live server cannot answer in time), a film
// listed in both apps handled twice, a stale or non-movie entry failing the run, the copy not landing under the
// scheme's name, an encode that keeps running while a guest watches (it must be SIGSTOPped, then resumed),
// Jellyfin not told (or its failed notify failing the run and losing the history), Plex asked to rescan, a DV5
// source encoded, and a rerun re-encoding a film that is already done.
func TestRunEndToEnd(t *testing.T) {
	h := fake.NewEnv(t, nil)
	lib := t.TempDir()
	heat := filepath.Join(lib, "Heat (1995) {tmdb-949}")
	ran := filepath.Join(lib, "Ran (1985) {tmdb-11645}")
	clerks := filepath.Join(lib, "Clerks (1994) {tmdb-2292}")
	for _, d := range []string{heat, ran, clerks} {
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
 {"type":"movie","title":"Ran","year":1985,"Media":[{"Part":[{"file":"`+ran+`/Ran (1985) {tmdb-11645}.mkv"}]}]},
 {"type":"movie","title":"Gone","year":2001,"Media":[{"Part":[{"file":"`+lib+`/Gone (2001) {tmdb-1}/Gone (2001) {tmdb-1}.mkv"}]}]}]}}`)
	children := map[string]string{
		"p1": `{"Items":[{"Name":"Ran","Type":"Movie","Path":"` + ran + `/Ran (1985) {tmdb-11645}.mkv"},{"Name":"Pilot","Type":"Episode","ProductionYear":2008,"Path":"/tv/x.mkv"}]}`,
		"c1": `{"Items":[{"Name":"Heat","Type":"Movie","Path":"` + heat + `/Heat (1995) {tmdb-949}.mkv"}]}`,
		"c2": `{"Items":[{"Name":"Clerks","Type":"Movie","Path":"` + clerks + `/Clerks (1994) {tmdb-2292}.mkv"}]}`,
	}
	lists := map[string]string{
		"Playlist": `{"Items":[{"Id":"p1","Name":"Remote 4K","Type":"Playlist"}]}`,
		"BoxSet":   `{"Items":[{"Id":"c1","Name":"Remote 4K","Type":"BoxSet"},{"Id":"c2","Name":"Other","Type":"BoxSet"}]}`,
	}
	h.Jellyfin.HandleFunc("GET", "/Items", func(w http.ResponseWriter, r *http.Request) {
		if id := r.URL.Query().Get("ParentId"); id != "" {
			io.WriteString(w, children[id])
			return
		}
		body, ok := lists[r.URL.Query().Get("IncludeItemTypes")]
		if !ok { // e.g. "Playlist,BoxSet": the live server takes 34-40 s on that, past any client's patience
			http.Error(w, "combined type query", http.StatusGatewayTimeout)
			return
		}
		io.WriteString(w, body)
	})
	h.Jellyfin.Handle("POST", "/Library/Media/Updated", 500, "") // a failing notify must not fail the run
	done := make(chan error, 1)
	var rep stage.Report
	go func() { done <- (Stage{}).Run(context.Background(), h.Env, &rep) }()

	waitFor(t, "encoder started", func() bool {
		select {
		case err := <-done:
			t.Fatalf("run ended before encoding Heat: err=%v counts=%v", err, rep.Counts)
		default:
		}
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
	if _, err := os.Stat(filepath.Join(clerks, "Clerks (1994) {tmdb-2292} - 2160p 14Mbps.mkv")); !os.IsNotExist(err) {
		t.Fatal("a film from another collection was encoded")
	}
	if refresh := h.Plex.Calls("GET", "/library/sections/4/refresh"); len(refresh) != 0 {
		t.Fatalf("plex asked to rescan: %+v", refresh)
	}
	if upd := h.Jellyfin.Calls("POST", "/Library/Media/Updated"); len(upd) != 1 || !strings.Contains(string(upd[0].Body), `"Path":"`+heat+`"`) {
		t.Fatalf("jellyfin update = %+v", upd)
	}
	if rep.Counts["made"] != 1 || rep.Counts["skip"] != 3 || rep.Counts["requested"] != 4 {
		t.Fatalf("counts = %v", rep.Counts)
	}
	for item, reason := range map[string]string{"Ran (1985) {tmdb-11645}": "Dolby Vision profile 5", "Gone (2001) {tmdb-1}": "folder missing", "Pilot (2008)": "not a 4K movie"} {
		if status, fields, ok, _ := h.Env.Store.LastEvent(context.Background(), name, item); !ok || status != "skip" || fields["reason"] != reason {
			t.Errorf("%s history = %s %v; want skip %q", item, status, fields, reason)
		}
	}

	var again stage.Report
	if err := (Stage{}).Run(context.Background(), h.Env, &again); err != nil {
		t.Fatal(err)
	}
	if runs, _ := os.ReadFile(filepath.Join(ffdir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("encoder ran %d times; a finished film must not be re-encoded", strings.Count(string(runs), "run"))
	}
}

// Catches: a slow app (Plex or Jellyfin) failing or hanging the run instead of leaving its half of the list empty
// while the other half is still worked through; and the lists bound by the process-wide clients' shorter timeout
// instead of list_timeout (a busy Plex, slower than 5 s, left its half empty in 4 runs on the NAS).
func TestSlowAppLeavesItsHalfEmpty(t *testing.T) {
	for _, slow := range []string{"plex", "jellyfin", ""} { // "": both answer, though the shared clients time out at once
		h := fake.NewEnv(t, nil)
		release := make(chan struct{})
		hang := func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
		h.Plex.Handle("GET", "/playlists", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"77","title":"Remote 4K"}]}}`)
		h.Plex.Handle("GET", "/playlists/77/items", 200, `{"MediaContainer":{"Metadata":[{"type":"episode","title":"Pilot","year":2008}]}}`)
		h.Jellyfin.HandleFunc("GET", "/Items", func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Query().Get("ParentId") == "p1":
				io.WriteString(w, `{"Items":[{"Name":"Pilot","Type":"Episode","ProductionYear":2009}]}`)
			case r.URL.Query().Get("IncludeItemTypes") == "Playlist":
				io.WriteString(w, `{"Items":[{"Id":"p1","Name":"Remote 4K","Type":"Playlist"}]}`)
			default:
				io.WriteString(w, `{"Items":[]}`)
			}
		})
		shared, listTimeout := time.Hour, "20ms"
		switch slow {
		case "plex":
			h.Plex.HandleFunc("GET", "/playlists", hang)
		case "jellyfin":
			h.Jellyfin.HandleFunc("GET", "/Items", hang)
		default:
			shared, listTimeout = time.Nanosecond, "1h"
		}
		t.Cleanup(func() { close(release) }) // runs before the servers' own cleanup, so Close does not wait on hang
		h.Env.Plex = plex.New(h.Plex.URL, h.PrefsPath, shared)
		h.Env.Jellyfin = jellyfin.New(h.Jellyfin.URL, writeKey(t), shared)
		h.SetOptions(`list_timeout = "` + listTimeout + `"
status_file = "` + filepath.Join(t.TempDir(), "STATUS.txt") + `"`)
		var rep stage.Report
		done := make(chan error, 1)
		go func() { done <- (Stage{}).Run(context.Background(), h.Env, &rep) }()
		select {
		case err := <-done:
			if want := map[bool]int{true: 1, false: 0}[slow != ""]; err != nil || rep.Counts["requested"] != 2-want || rep.Counts["list_unavailable"] != want {
				t.Errorf("%q hanging: err=%v counts=%v; want %d requests and %d unavailable lists", slow, err, rep.Counts, 2-want, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("run hung on a slow %s: the list_timeout bound is missing", slow)
		}
	}
}

func writeKey(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "jellyfin_key")
	os.WriteFile(path, []byte(fake.JellyfinKey), 0o600)
	return path
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
