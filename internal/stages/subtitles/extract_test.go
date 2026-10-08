package subtitles

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// extractFixture is a library of real Matroska files, each 100 s of a 16x16 picture with English subtitle tracks, read
// by the real ffprobe and ffmpeg (ffmpeg's command lines kept in calls), and no subtitle provider.
type extractFixture struct {
	h       *fake.Harness
	lib, ff string
}

// subTrack is one English subtitle track: codec "srt" or "ass", body its file.
type subTrack struct {
	codec, title, body string
	forced, sdh        bool
}

func newExtractFixture(t *testing.T) *extractFixture {
	ffmpeg, err := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err != nil || err2 != nil {
		t.Skip("needs ffmpeg and ffprobe")
	}
	f := &extractFixture{h: fake.NewEnv(t, nil), lib: t.TempDir(), ff: ffmpeg}
	dir := t.TempDir()
	f.h.Script(t, "ffprobe", `exec "`+ffprobe+`" "$@"`)
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+f.h.Env.FFmpeg.Dir+`/calls"; exec "`+ffmpeg+`" "$@"`)
	f.h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	f.h.SetOptions(`roots = ["` + f.lib + `/Movies"]
stats_file = "` + dir + `/sub_stats.tsv"
secrets_dir = "` + dir + `"
plex_db = "` + dir + `/plex.db"
providers = []
mode = "careful"
verify_per_run = 0`)
	return f
}

// movie writes Movies/<name>/<name>.mkv with these tracks, in order, and returns its path.
func (f *extractFixture) movie(t *testing.T, name string, tracks ...subTrack) string {
	t.Helper()
	path := filepath.Join(f.lib, "Movies", name, name+".mkv")
	os.MkdirAll(filepath.Dir(path), 0o755)
	dir := t.TempDir()
	args := []string{"-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=16x16:r=1:d=100"}
	maps := []string{"-map", "0"}
	for i, tr := range tracks {
		in := filepath.Join(dir, fmt.Sprintf("%d.%s", i, tr.codec))
		os.WriteFile(in, []byte(tr.body), 0o644)
		args, maps = append(args, "-i", in), append(maps, "-map", strconv.Itoa(i+1))
	}
	args = append(append(args, maps...), "-c:v", "mpeg4", "-c:s", "copy")
	for i, tr := range tracks {
		s := fmt.Sprintf(":s:%d", i) // the i-th subtitle stream; -metadata needs its stream-metadata prefix too
		var disposition []string
		if tr.forced {
			disposition = append(disposition, "forced")
		}
		if tr.sdh {
			disposition = append(disposition, "hearing_impaired")
		}
		args = append(args, "-metadata:s"+s, "language=eng", "-metadata:s"+s, "title="+tr.title, "-disposition"+s,
			cmp.Or(strings.Join(disposition, "+"), "0"))
	}
	if out, err := exec.Command(f.ff, append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("making %s: %v: %s", name, err, out)
	}
	return path
}

func (f *extractFixture) run(t *testing.T) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), f.h.Env, &rep); err != nil {
		t.Fatalf("run: %v (alerts %v)", err, rep.Alerts)
	}
	return rep
}

// extractions is how many times ffmpeg converted the video's subtitles.
func (f *extractFixture) extractions(video string) int {
	b, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "calls"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, video) && strings.Contains(l, "-f srt") {
			n++
		}
	}
	return n
}

func (f *extractFixture) outcome(video string) string {
	var o string
	f.h.Env.Store.DB().QueryRow(`SELECT outcome FROM subtitles_titles WHERE path=?`, video).Scan(&o)
	return o
}

func sidecarOf(video, suffix string) string { return strings.TrimSuffix(video, ".mkv") + suffix }

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// cueTimes are 45 cue starts 2 s apart from 2 s: over 88 % of the fixture's 100 s.
func cueTimes() []float64 {
	var ts []float64
	for i := range 45 {
		ts = append(ts, 2+2*float64(i))
	}
	return ts
}

// assScript is an ASS track with a cue at each t, 2 s long, its text the lines in turn, each after an override block.
func assScript(ts []float64, lines ...string) string {
	stamp := func(t float64) string {
		cs := int(t * 100)
		return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, cs/6000%60, cs/100%60, cs%100)
	}
	var b strings.Builder
	b.WriteString("[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, " +
		"OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, " +
		"Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Default,Arial,72,&H00FFFFFF,&H000000FF,&H00000000," +
		"&H00000000,-1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, " +
		"MarginR, MarginV, Effect, Text\n")
	for i, t := range ts {
		fmt.Fprintf(&b, "Dialogue: 0,%s,%s,Default,,0,0,0,,{\\fad(200,200)}%s\n", stamp(t), stamp(t+2), lines[i%len(lines)])
	}
	return b.String()
}

// Catches the wrong track written (the SDH track the video lists first, when a full one follows), the track's cues not
// written as they are timed, the ASS styling kept (a 72-point bold font in every cue: a TV app shows the tags or the
// size) or a sign's vector drawing written as a line of numbers, the title not recorded "extracted", and Jellyfin not
// told of the new sidecar.
func TestExtractWritesTheEmbeddedEnglishTrack(t *testing.T) {
	f := newExtractFixture(t)
	ts := cueTimes()
	sign := "Dialogue: 0,0:00:50.50,0:00:52.50,Default,,0,0,0,,{\\an8\\p1}m 0 0 l 100 0 100 100 0 100{\\p0}\n"
	heat := f.movie(t, "Heat (1995) {tmdb-949}",
		subTrack{codec: "srt", title: "English SDH", sdh: true, body: textSRT(ts, "[DOOR CLOSES]", "[PHONE RINGS]")},
		subTrack{codec: "ass", title: "English", body: assScript(ts, englishLines...) + sign})
	f.run(t)
	if got, want := readFile(sidecarOf(heat, ".en.srt")), strings.TrimSuffix(textSRT(ts, englishLines...), "\n"); got != want {
		t.Errorf("Heat's sidecar:\n%.300s\nwant the ASS track's cues as timed, as plain text, without the drawing:\n%.300s", got, want)
	}
	var told string
	for _, c := range f.h.Jellyfin.Calls("POST", "/Library/Media/Updated") {
		told += string(c.Body)
	}
	if f.outcome(heat) != "extracted" || !strings.Contains(told, filepath.Base(sidecarOf(heat, ".en.srt"))) {
		t.Errorf("Heat recorded %q, Jellyfin told %s; want extracted, its sidecar", f.outcome(heat), told)
	}
}

// Catches a sidecar overwritten, or a second English one written beside it, when it appears while the video is read
// (the owner drops one in during a read that takes minutes); and a video whose sidecar was extracted read again once
// that sidecar is removed (a whole-file read every run) though the video did not change.
func TestExtractNeverOverwritesAndReadsAFileOnce(t *testing.T) {
	f := newExtractFixture(t)
	ts := cueTimes()
	heat := f.movie(t, "Heat (1995) {tmdb-949}", subTrack{codec: "srt", body: textSRT(ts, englishLines...)})
	ronin := f.movie(t, "Ronin (1998) {tmdb-8195}", subTrack{codec: "srt", body: textSRT(ts, englishLines...)})
	ff := f.h.Env.FFmpeg.Dir
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/calls"
case "$*" in *"Heat (1995)"*"-f srt"*) [ -e "`+ff+`/dropped" ] || { : > "`+ff+`/dropped"; printf theirs > "`+sidecarOf(heat, ".en.srt")+`"; };; esac
exec "`+f.ff+`" "$@"`)
	f.run(t)
	_, eng := os.Stat(sidecarOf(heat, ".eng.srt"))
	if got := readFile(sidecarOf(heat, ".en.srt")); got != "theirs" || eng == nil {
		t.Errorf("Heat's .en.srt %.40q, an .eng.srt written %v; want theirs, none", got, eng == nil)
	}
	if got := readFile(sidecarOf(ronin, ".en.srt")); got == "" {
		t.Fatal("Ronin's track not extracted")
	}
	os.Remove(sidecarOf(ronin, ".en.srt"))
	f.run(t)
	if _, err := os.Stat(sidecarOf(ronin, ".en.srt")); err == nil || f.extractions(ronin) != 1 {
		t.Errorf("Ronin read %d times, its removed sidecar written again %v; want once, no", f.extractions(ronin), err == nil)
	}
}

// Catches a track that is not the English subtitle written: forced, by its flag or only by its title (War for the
// Planet of the Apes' "Forced only"), a commentary, or one with too few cues (a forced track without a label); a video
// whose only English text tracks are those left unsearched as if the players showed its subtitle; and a refused file
// read again by every run though one whose video changed is read anew.
func TestExtractRefusesATrackThatIsNotTheEnglishSubtitle(t *testing.T) {
	f := newExtractFixture(t)
	ts := cueTimes()
	full := textSRT(ts, englishLines...)
	labelled := f.movie(t, "Labelled (2001) {tmdb-101}", subTrack{codec: "srt", forced: true, body: full},
		subTrack{codec: "srt", title: "Forced only", body: full}, subTrack{codec: "srt", title: "Director's Commentary", body: full})
	partial := f.movie(t, "Partial (2002) {tmdb-102}", subTrack{codec: "srt", body: textSRT(ts[:5], englishLines...)})
	f.run(t)
	for _, v := range []string{labelled, partial} {
		if _, err := os.Stat(sidecarOf(v, ".en.srt")); err == nil {
			t.Errorf("%s: a sidecar was written", filepath.Base(v))
		}
	}
	if f.extractions(labelled) != 0 || f.outcome(labelled) != "no_candidates" {
		t.Errorf("Labelled read %d times, recorded %q; want never, no_candidates (searched)", f.extractions(labelled), f.outcome(labelled))
	}
	var refusals int
	f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='rejected_extract'`, partial).Scan(&refusals)
	if refusals != 1 {
		t.Errorf("Partial: %d rejected_extract events; want 1", refusals)
	}
	f.run(t)
	again := f.extractions(partial)
	later := time.Now().Add(time.Hour)
	os.Chtimes(partial, later, later)
	f.run(t)
	if again != 1 || f.extractions(partial) != 2 {
		t.Errorf("Partial read %d times by the second run, %d once the video changed; want 1, 2", again, f.extractions(partial))
	}
}
