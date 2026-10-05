package details

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

// Catches: any drift from what write_details_files.py wrote for the same file (its ten lines, captured from the
// NAS, are the oracle), and the extra lines misreporting the file: Dolby Vision 8.6 + HDR10+, SDH named only in
// a title, DTS-HD MA, forced/HI by disposition, a commentary and a descriptive track, a font attachment.
func TestRenderMatchesPython(t *testing.T) {
	cases := []struct {
		fixture, frames string
		extra           []string
	}{
		{"../../probe/testdata/dv8_hdr10plus", "../../probe/testdata/dv8_hdr10plus_frames.json", []string{
			"HDR metadata:    mastering 0.0050-1000 cd/m² · MaxCLL 820 cd/m² · MaxFALL 330 cd/m² · HDR10+ dynamic metadata\n",
			"Dolby Vision:    profile 8 · level 6 · base layer HDR10 (compatibility id 1)\n",
			"Subtitle kinds:  2 text (subrip) · SDH: eng\n",
			`Release tags:    title "SiteI.com | Dungeons.and.Dragons.Honor.Among.Thieves.2023.2160p`}},
		{"testdata/cronos_dtshdma", "", []string{"Audio 1:         DTS (DTS-HD MA) · 5.1(side) · 1.5 Mb/s · default\n",
			"Subtitle kinds:  1 text (subrip) · 1 image (hdmv_pgs_subtitle) · forced: eng, eng\n"}},
		{"testdata/atropia_forced_hi", "", []string{"Subtitle kinds:  4 text (subrip) · forced: eng · SDH: eng\n",
			"Encoder:         Lavc59.48.100 libx265\n"}},
		{"testdata/american_pie_commentary", "", []string{`Audio 2:         AAC (LC) · stereo · commentary · "Commentary by the directors and cast"` + "\n"}},
		{"testdata/all_of_us_strangers_descriptive", "", []string{`Audio 2:         AC-3 · stereo · 192 kb/s · descriptive · "Descriptive"` + "\n"}},
		{"testdata/evil_font", "", []string{"Attachments:     1 font\n"}},
	}
	for _, c := range cases {
		raw, _ := os.ReadFile(c.fixture + ".json")
		info, err := probe.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if c.frames != "" {
			frames, _ := os.ReadFile(c.frames)
			probe.ParseFrames(frames, info.Video)
		}
		want, _ := os.ReadFile("testdata/" + filepath.Base(c.fixture) + ".python.txt")
		field := map[string]string{}
		for _, l := range strings.Split(string(want), "\n") {
			if len(l) > 17 {
				field[strings.TrimSpace(l[:17])] = l[17:]
			}
		}
		info.Size = int64(sizeFromNote(t, field["Container:"]))
		dir := filepath.Join("/volume1/media/Movies", strings.TrimSuffix(field["Title:"], ""))
		v := video{path: filepath.Join(dir, field["Title:"]+".mkv"), identity: field["Identity:"]}
		orig := filepath.Join("/volume1/media", field["Original folder:"], field["Original name:"])
		got := render(v, info, orig, nil, &fileRow{})
		if !strings.HasPrefix(got, string(want)) {
			t.Errorf("%s: first ten lines differ from the Python's\n got:\n%s\nwant:\n%s", c.fixture, got, want)
		}
		for _, e := range c.extra {
			if !strings.Contains(got, e) {
				t.Errorf("%s: missing %q in\n%s", c.fixture, e, got)
			}
		}
	}
}

// sizeFromNote turns "matroska · 3.51 GB · ..." back into bytes close enough to print the same GB.
func sizeFromNote(t *testing.T, container string) float64 {
	var gb float64
	if _, err := fmt.Sscanf(strings.Split(container, " · ")[1], "%f GB", &gb); err != nil {
		t.Fatalf("container %q: %v", container, err)
	}
	return gb * 1e9
}

// Catches: the HDR10+ the Python never saw (it looked for it in stream side data), Dolby Vision without a base
// layer called "+SDR base", "DTS-HD MA + DTS:X" named as plain DTS-HD MA (the Python tested MA first), and the
// x264/x265 rate control misread from the settings string.
func TestNames(t *testing.T) {
	for _, c := range []struct {
		v    probe.Video
		want string
	}{
		{probe.Video{HDR: probe.DolbyVision, DVProfile: 8, DVBLCompat: 1, ColorTransfer: "smpte2084", HDR10Plus: true}, "Dolby Vision profile 8 (+HDR10 base)"},
		{probe.Video{HDR: probe.DolbyVision, DVProfile: 5}, "Dolby Vision profile 5"},
		{probe.Video{HDR: probe.HDR10, ColorTransfer: "smpte2084", HDR10Plus: true}, "HDR10+"},
		{probe.Video{HDR: probe.HLG, ColorTransfer: "arib-std-b67"}, "HLG"},
	} {
		if got := hdrKind(&c.v); got != c.want {
			t.Errorf("hdrKind(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
	for _, c := range []struct{ codec, profile, want string }{
		{"dts", "DTS-HD MA + DTS:X", "DTS:X"}, {"dts", "DTS-HD MA", "DTS-HD MA"}, {"dts", "DTS", "DTS"},
		{"eac3", "Dolby Digital Plus + Dolby Atmos", "E-AC-3 Atmos"}, {"truehd", "Dolby TrueHD + Dolby Atmos", "TrueHD Atmos"}, {"pcm_s24le", "", "PCM_S24LE"},
	} {
		if got := audioName(probe.Audio{Codec: c.codec, Profile: c.profile}); got != c.want {
			t.Errorf("audioName(%s, %q) = %q, want %q", c.codec, c.profile, got, c.want)
		}
	}
	for settings, want := range map[string]string{
		"x265 (build 209) - 3.6+28-8f16d2257:[Windows][GCC 14.1.0][64 bit] 10bit - H.265/HEVC codec - options: cpuid=1111039 rc=crf crf=20.0 qcomp=0.60": "x265 3.6+28 · crf 20.0",
		"x264 - core 152 r2851M ba24899 - H.264/MPEG-4 AVC codec - options: cabac=1 rc=2pass mbtree=1 bitrate=8000 ratetol=1.0":                      "x264 core 152 · 2pass 8000 kb/s",
	} {
		if got := encoder(&probe.Video{EncoderSettings: settings}); got != want {
			t.Errorf("encoder(%.30q) = %q, want %q", settings, got, want)
		}
	}
}

// Catches: a release source or group read differently from the Python's regexes (expected values are the
// Python's own output for these names).
func TestSourceGroup(t *testing.T) {
	for name, want := range map[string][2]string{
		"Heat.1995.1080p.BluRay.x264-SPARKS[sitea].mkv":            {"BluRay", "SPARKS"},
		"Atropia.2025.2160p.4K.WEB.x265.10bit.AAC5.1-[SITEB.BZ].mkv": {"unknown", "SITEB.BZ"},
		"The.Movie.2010.BluRay.REMUX.1080p-FraMeSToR.mkv":          {"Remux", "FraMeSToR"},
		"Show S01E01 (1080p BluRay x265 Silence).mkv":              {"BluRay", "Silence"},
		"show.s01e02.720p.hdtv.x264-killers.mkv":                   {"HDTV", "killers"},
		"Movie (2001) DVDRip XviD.avi":                             {"DVD", "unknown"},
		"Movie.2019.WEB-DL.1080p.H264-NTb.mkv":                     {"WEB-DL", "NTb"},
		"Amélie.2001.1080p.BRRip-ÉQUIPE.mkv":                       {"BluRay", "unknown"},
		"movie.2012.1080p.webrip.x264.sitec.mp4":                    {"WEBRip", "sitec"},
	} {
		if src, group := sourceGroup(name); src != want[0] || group != want[1] {
			t.Errorf("sourceGroup(%q) = %s, %s; want %s, %s", name, src, group, want[0], want[1])
		}
	}
}

// Catches: a file renamed twice showing its middle name instead of its first, archive rows read as moves, and a
// missing log (the auto-renamer's appears on first use) failing the run.
func TestOriginals(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "undo.tsv"), filepath.Join(dir, "undo_auto.tsv")
	os.WriteFile(first, []byte("t\tmove\t/m/Heat.1995.BluRay-GRP.mkv\t/m/Heat (1995) {tmdb-949}/Heat.mkv\tmovie:1\n"+
		"t\tarchive\t/m/old.nfo\t/m/_archive/old.nfo\tmovie:1\n"+
		"t\tunit-done\t\t\tmovie:1\n"), 0o644)
	os.WriteFile(second, []byte("t\tmove\t/m/Heat (1995) {tmdb-949}/Heat.mkv\t/m/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv\tmovie:1\n"), 0o644)
	orig, err := originals([]string{first, filepath.Join(dir, "missing.tsv"), second})
	if err != nil {
		t.Fatal(err)
	}
	if got := orig["/m/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv"]; got != "/m/Heat.1995.BluRay-GRP.mkv" || len(orig) != 2 {
		t.Fatalf("originals = %v", orig)
	}
}

// Catches: a dry run (notes_root) writing beside the real videos instead of into its mirror.
func TestNotePath(t *testing.T) {
	video := "/L/Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv"
	if got := notePath(Options{Library: "/L"}, video); got != "/L/Movies/Heat (1995) {tmdb-949}/.details/Heat (1995) {tmdb-949}.mkv.txt" {
		t.Fatalf("beside: %s", got)
	}
	if got := notePath(Options{Library: "/L", NotesRoot: "/S"}, video); got != "/S/Movies/Heat (1995) {tmdb-949}/.details/Heat (1995) {tmdb-949}.mkv.txt" {
		t.Fatalf("mirror: %s", got)
	}
}

type lib struct {
	t    *testing.T
	root string
}

// add creates a video (and its folders) with an mtime age hours before the fake clock's now.
func (l lib) add(rel string, age int) string {
	p := filepath.Join(l.root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("video"), 0o644); err != nil {
		l.t.Fatal(err)
	}
	at := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC).Add(-time.Duration(age) * time.Hour)
	os.Chtimes(p, at, at)
	return p
}

func runOnce(t *testing.T, env *stage.Env) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), env, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func row(t *testing.T, env *stage.Env, path string) (status, errText string) {
	t.Helper()
	err := env.Store.DB().QueryRow(`SELECT status, error FROM details_files WHERE path=?`, path).Scan(&status, &errText)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return status, errText
}

// Catches, end to end: notes for files outside the scheme or in hidden folders, the probe bound not deferring
// (or not putting the newest file first), an unchanged note rewritten or an unchanged file re-probed, an
// unreadable or video-less file not listed, alerted twice or re-probed before its retry time, the original name
// lost across two moves, another version's sidecar listed, a stale temp file blocking a write, an SDR file probed
// for frames or a file under 20 Mb/s peak-scanned, stale rows kept for deleted files, and a paths_file run
// processing other videos or deleting their rows.
func TestRunReconciles(t *testing.T) {
	h := fake.NewEnv(t, nil)
	l := lib{t, t.TempDir()}
	audioOnly := l.add("Movies/Audio Only (2000) {tmdb-2}/Audio Only (2000) {tmdb-2}.mkv", 0)
	pilot := l.add("TV/Show (2008) {tmdb-1396}/Season 01/Show (2008) - S01E01 - Pilot.mkv", 1)
	broken := l.add("Movies/Broken (2001) {tmdb-1}/Broken (2001) {tmdb-1}.mkv", 2)
	heat := l.add("Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv", 3)
	extra := l.add("Movies/Heat (1995) {tmdb-949}/Featurettes/Making Of.mkv", 4)
	l.add("Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.en.srt", 5)
	l.add("Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 2160p.en.srt", 5)
	l.add("Movies/Heat (1995) {tmdb-949}/.details/Heat (1995) {tmdb-949}.mkv.txt.tmp", 5) // left by a crashed run
	l.add("Movies/Not In Scheme/Not In Scheme.mkv", 0)
	l.add("TV/Show (2008) {tmdb-1396}/@eaDir/thumb.mkv", 0)
	undo := filepath.Join(t.TempDir(), "undo.tsv")
	os.WriteFile(undo, []byte("t\tmove\t"+l.root+"/Movies/Heat.1995.1080p.BluRay.x264-GRP.mkv\t"+l.root+"/Movies/Heat (1995) {tmdb-949}/a.mkv\tu\n"+
		"t\tmove\t"+l.root+"/Movies/Heat (1995) {tmdb-949}/a.mkv\t"+heat+"\tu\n"), 0o644)
	fixture, _ := filepath.Abs("testdata/cronos_dtshdma.json")
	calls := filepath.Join(h.Env.FFmpeg.Dir, "calls")
	h.Script(t, "ffprobe", `echo "$*" >> "`+calls+`"
case "$*" in *Broken*) echo "Invalid data found when processing input" >&2; exit 1;;
  *"Audio Only"*) echo '{"streams":[{"index":0,"codec_type":"audio","codec_name":"flac"}],"format":{}}'; exit;; esac
cat "`+fixture+`"
`)
	options := fmt.Sprintf("library = %q\nundo_logs = [%q]\nmax_probes = 4\nmax_decode_checks = 0\n", l.root, undo)
	h.SetOptions(options)
	note := func(video string) string {
		raw, _ := os.ReadFile(filepath.Join(filepath.Dir(video), ".details", filepath.Base(video)+".txt"))
		return string(raw)
	}
	probes := func() int { raw, _ := os.ReadFile(calls); return strings.Count(string(raw), "-show_streams") }

	rep := runOnce(t, h.Env)
	if rep.Counts["probed"] != 4 || rep.Counts["probe_deferred"] != 1 || rep.Counts["written"] != 2 || rep.Counts["unreadable"] != 2 || note(extra) != "" {
		t.Fatalf("run 1 counts %v (the oldest file, Making Of, must be the deferred one)", rep.Counts)
	}
	n := note(heat)
	for _, want := range []string{"Original name:   Heat.1995.1080p.BluRay.x264-GRP.mkv\n", "Source · group:  BluRay · GRP\n",
		"Subtitles:       eng subrip forced; eng hdmv_pgs_subtitle; en.srt (file beside video)\n", "Identity:        tmdb-949\n"} {
		if !strings.Contains(n, want) {
			t.Errorf("Heat note lacks %q:\n%s", want, n)
		}
	}
	if status, e := row(t, h.Env, audioOnly); status != "unreadable" || e != "no video stream" {
		t.Errorf("audio-only file: status %q err %q", status, e)
	}
	if !strings.Contains(note(pilot), "Identity:        tmdb-1396\n") {
		t.Errorf("episode note = %q", note(pilot))
	}
	if status, e := row(t, h.Env, broken); status != "unreadable" || !strings.Contains(e, "Invalid data found") ||
		len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "2 video(s) newly unreadable") || !strings.Contains(rep.Alerts[0], broken) {
		t.Fatalf("broken: status %q err %q alerts %v", status, e, rep.Alerts)
	}
	for _, p := range []string{"Movies/Not In Scheme", "TV/Show (2008) {tmdb-1396}/@eaDir"} {
		if _, err := os.Stat(filepath.Join(l.root, p, ".details")); !os.IsNotExist(err) {
			t.Errorf("a note was written under %s", p)
		}
	}

	heatNote, _ := os.Stat(filepath.Join(filepath.Dir(heat), ".details", filepath.Base(heat)+".txt"))
	rep = runOnce(t, h.Env)
	again, _ := os.Stat(filepath.Join(filepath.Dir(heat), ".details", filepath.Base(heat)+".txt"))
	if rep.Counts["probed"] != 1 || rep.Counts["written"] != 1 || note(extra) == "" || len(rep.Alerts) != 0 || probes() != 5 || !again.ModTime().Equal(heatNote.ModTime()) {
		t.Fatalf("run 2: counts %v alerts %v probes %d (only Making Of is new; Broken waits for its retry time)", rep.Counts, rep.Alerts, probes())
	}

	h.Now = h.Now.Add(25 * time.Hour)
	os.WriteFile(heat, []byte("video, re-muxed"), 0o644)
	os.Remove(extra)
	rep = runOnce(t, h.Env)
	log, _ := os.ReadFile(calls)
	last := strings.Split(strings.TrimSpace(string(log)), "\n")
	if probes() != 7 || !strings.Contains(strings.Join(last[len(last)-2:], "\n"), heat) || len(rep.Alerts) != 0 {
		t.Fatalf("run 3: probes %d (want Heat re-probed after its change and Broken retried; the audio-only probe is cached), alerts %v", probes(), rep.Alerts)
	}
	if status, _ := row(t, h.Env, extra); status != "" {
		t.Fatal("row kept for a deleted file")
	}
	if strings.Contains(string(log), "-show_frames") || strings.Contains(string(log), "packet=") {
		t.Fatal("an SDR file was frame-probed, or a 15.7 Mb/s file peak-scanned")
	}

	paths := filepath.Join(t.TempDir(), "paths.txt")
	os.WriteFile(paths, []byte(pilot+"\n"), 0o644)
	h.SetOptions(options + fmt.Sprintf("paths_file = %q\n", paths))
	if rep = runOnce(t, h.Env); !strings.HasPrefix(rep.Summary, "1 videos:") {
		t.Fatalf("paths_file run: %s", rep.Summary)
	}
	if status, _ := row(t, h.Env, heat); status != "ok" {
		t.Fatal("a paths_file run deleted the rows of videos it did not list")
	}
}

// Catches: heavy work (the whole-file packet scan, the decode samples) started while a guest watches or kept
// running when one starts, the viewer count asked again per file once a guest is seen, the peak window miscounted
// (TS timestamp offset, a packet before the first, a broken far-future timestamp), a scanned file scanned again,
// the scan bound ignored, and a changed file not re-scanned.
func TestHeavyWorkWaitsForGuests(t *testing.T) {
	h := fake.NewEnv(t, nil)
	l := lib{t, t.TempDir()}
	heat := l.add("Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv", 1)
	fixture, _ := filepath.Abs("testdata/cronos_dtshdma.json") // 15.7 Mb/s average
	dir := h.Env.FFmpeg.Dir
	// Packets: 1 MB/s for 300 s, except 5 MB/s from 100 s to 109 s, timestamps offset as in a TS file.
	h.Script(t, "ffprobe", `case "$*" in
*packet=*) echo scan >> "`+dir+`/scans"; [ -f "`+dir+`/block" ] && exec sleep 30
  awk 'BEGIN { for (s = 0; s < 300; s++) { b = (s >= 100 && s < 110) ? 5000000 : 1000000
    printf "pts_time=N/A|dts_time=%.3f|size=%d\n", 3600 + s, b; if (s == 0) print "pts_time=3598.000|dts_time=N/A|size=0" }
    print "pts_time=N/A|dts_time=703600.000|size=999999999999" }';;
*) cat "`+fixture+`";;
esac
`)
	h.Script(t, "ffmpeg", `echo "$*" >> "`+dir+`/decodes"; echo frame=120; echo progress=end`+"\n")
	h.SetOptions(fmt.Sprintf("library = %q\nsections = [\"Movies\"]\nundo_logs = []\npeak_above_kbps = 10000", l.root))
	var guest atomic.Bool
	h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
		if guest.Load() {
			fmt.Fprint(w, `<MediaContainer size="1"><Video type="movie" title="Ran"><User title="guest"/></Video></MediaContainer>`)
			return
		}
		fmt.Fprint(w, `<MediaContainer size="0"></MediaContainer>`)
	})
	read := func(name string) string { raw, _ := os.ReadFile(filepath.Join(dir, name)); return string(raw) }
	note := func() string {
		raw, _ := os.ReadFile(filepath.Join(filepath.Dir(heat), ".details", filepath.Base(heat)+".txt"))
		return string(raw)
	}

	guest.Store(true)
	h.Env.Viewers.Poll = time.Hour // only the check before starting can keep the work from running
	rep := runOnce(t, h.Env)
	h.Env.Viewers.Poll = 5 * time.Millisecond
	if read("scans") != "" || read("decodes") != "" || rep.Counts["heavy_deferred"] != 2 || strings.Contains(note(), "Peak bitrate") ||
		len(h.Plex.Calls("GET", "/status/sessions")) != 1 {
		t.Fatalf("guest watching: scans %q decodes %q counts %v", read("scans"), read("decodes"), rep.Counts)
	}

	// A guest who starts during the scan stops it; the result is not stored.
	guest.Store(false)
	os.WriteFile(filepath.Join(dir, "block"), nil, 0o644)
	h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
		if read("scans") != "" { // idle until the scan has started, then a guest
			fmt.Fprint(w, `<MediaContainer size="1"><Video type="movie" title="Ran"><User title="guest"/></Video></MediaContainer>`)
			return
		}
		fmt.Fprint(w, `<MediaContainer size="0"></MediaContainer>`)
	})
	rep = runOnce(t, h.Env)
	if read("scans") != "scan\n" || rep.Counts["heavy_deferred"] != 2 || strings.Contains(note(), "Peak bitrate") {
		t.Fatalf("interrupted scan: scans %q counts %v", read("scans"), rep.Counts)
	}

	os.Remove(filepath.Join(dir, "block"))
	h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	runOnce(t, h.Env)
	n := note()
	if !strings.Contains(n, "Peak bitrate:    40.0 Mb/s over 10 s at 0h 01m 40s (all streams)\n") || !strings.Contains(n, "Streams cleanly: yes") ||
		strings.Count(read("decodes"), "-hwaccel vaapi") != 3 {
		t.Fatalf("note:\n%s\ndecodes:\n%s", n, read("decodes"))
	}
	runOnce(t, h.Env)
	if strings.Count(read("scans"), "scan") != 2 || strings.Count(read("decodes"), "\n") != 3 {
		t.Fatalf("a scanned file was scanned again: scans %q", read("scans"))
	}

	os.WriteFile(heat, []byte("video, re-muxed"), 0o644)
	h.SetOptions(fmt.Sprintf("library = %q\nsections = [\"Movies\"]\nundo_logs = []\npeak_above_kbps = 10000\nmax_peak_scans = 0", l.root))
	if runOnce(t, h.Env); strings.Count(read("scans"), "scan") != 2 {
		t.Fatal("max_peak_scans = 0 still scanned")
	}
	h.SetOptions(fmt.Sprintf("library = %q\nsections = [\"Movies\"]\nundo_logs = []\npeak_above_kbps = 10000", l.root))
	if runOnce(t, h.Env); strings.Count(read("scans"), "scan") != 3 {
		t.Fatal("a changed file kept its old peak")
	}
}

// Catches: a GPU hiccup reported as a broken file (it must be retried on the CPU), a truncated end sample
// passing (too few frames), and the error budget off: two errors after a seek pass, three fail.
func TestDecodeCheck(t *testing.T) {
	cases := []struct{ script, wantDetail string }{
		{`case "$*" in *-hwaccel*) echo "hwaccel init failed" >&2; exit 1;; esac; echo frame=120`, ""},
		{`case "$*" in *"-ss 0.000 "*|*"-ss 2997.500 "*) echo frame=120;; *) echo frame=0;; esac`, "end (1h 39m 54s): 0 frames decoded, want at least 60"},
		{`echo "ref missing" >&2; echo "ref missing" >&2; echo frame=118`, ""},
		{`for i in 1 2 3; do echo "error while decoding MB 1 2" >&2; done; echo frame=120`, "start (0h 00m 00s): 3 decode errors, first: error while decoding MB 1 2"},
	}
	for _, c := range cases {
		h := fake.NewEnv(t, nil)
		h.Script(t, "ffmpeg", c.script+"\n")
		clean, detail, err := decodeCheck(context.Background(), h.Env.FFmpeg, "/m/x.mkv", &probe.Video{Codec: "hevc", FPS: 24}, 6000)
		if err != nil || clean != (c.wantDetail == "") || detail != c.wantDetail {
			t.Errorf("script %q: clean=%v detail=%q err=%v; want detail %q", c.script, clean, detail, err, c.wantDetail)
		}
	}
}
