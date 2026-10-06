package subtitles

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

// dense are n cue starts from 10 s, 1.5 to 4.5 s apart at random (seed picks another film's).
func dense(n, seed int) []float64 {
	out := make([]float64, n)
	x, t := uint32(seed), 10.0
	for i := range out {
		x = x*1664525 + 1013904223
		out[i], t = t, t+1.5+3*float64(x>>8)/(1<<24)
	}
	return out
}

// cueSpans are 2 s cues at starts mapped back through t' = ratio*t + offset, so that the fit must find (ratio, offset).
func cueSpans(starts []float64, ratio, offset float64) []span {
	out := make([]span, len(starts))
	for i, s := range starts {
		out[i] = span{(s - offset) / ratio, (s + 2 - offset) / ratio}
	}
	return out
}

// captionSamples are windows whose captions sit on starts, every eleventh dropped (lines the subtitle lacks) and a
// caption of its own added every seventh (lines the captions split).
func captionSamples(starts []float64, wins []sampleWindow) []sample {
	var out []sample
	for _, w := range wins {
		smp := sample{win: w}
		for i, s := range starts {
			if s >= w.start && s < w.start+w.dur && i%11 != 0 {
				smp.captions = append(smp.captions, s+float64(i%3-1)*0.04)
				if i%7 == 0 {
					smp.captions = append(smp.captions, s+1.1)
				}
			}
		}
		out = append(out, smp)
	}
	return out
}

// speechSamples are windows voiced (70 dB against a 30 dB floor, with a ripple) for 2 s from each of starts.
func speechSamples(starts []float64, wins []sampleWindow) []sample {
	var out []sample
	for _, w := range wins {
		level := make([]float64, int(w.dur/frame))
		for f := range level {
			level[f] = 30 + float64(f*f%7)
		}
		for _, s := range starts {
			for f := int((s - w.start) / frame); f < int((s+2-w.start)/frame); f++ {
				if f >= 0 && f < len(level) {
					level[f] = 70 + float64(f%5)
				}
			}
		}
		out = append(out, sample{win: w, level: level, voiced: voiced(level)})
	}
	return out
}

// Catches each guard of the sampled fit: a shift, a PAL speed-up or a 23.976/24 change not recovered (from captions or
// from speech), another film's subtitle or another cut's written (the difference in the middle, or near either end of
// a film or an episode, between the 3 % edge and the windows next to it), a window with too few matched lines
// accepted, a ratio the windows cannot tell from another (all four within six minutes) taken anyway, and a subtitle
// written with more lines than a quiet stretch holds in the edge stretch an outer window moved inward from (the last by
// two windows, 14 lines allowed; the first by one, 7) never read.
func TestFitSamples(t *testing.T) {
	const film = 3000.0
	truth := dense(1000, 7)
	wins := sampleWindowsFor(film, 4, 90)
	cut := append([]float64(nil), truth...)   // the second half is another edit, 4 s later
	drift := append([]float64(nil), truth...) // a speed change no ratio covers: 0.4 s apart across the windows
	for i := range cut {
		if cut[i] > film/2 {
			cut[i] += 4
		}
		drift[i] *= 1.00015
	}
	thin := captionSamples(truth, wins)
	other := thin[2].captions[:7:7] // window 3 keeps 7 of its lines and gains another film's 30: under half match
	for _, c := range dense(1000, 11) {
		if w := thin[2].win; c >= w.start && c < w.start+w.dur {
			other = append(other, c)
		}
	}
	thin[2].captions = other
	five := captionSamples(truth, wins) // window 2 holds five lines, every one matched
	five[1].captions = five[1].captions[:5]
	var fewCues []span // the subtitle has five lines in window 2; the speech is all there
	inSecond := 0
	for _, c := range cueSpans(truth, 1, 0) {
		if c.start >= wins[1].start && c.start < wins[1].start+wins[1].dur {
			if inSecond++; inSecond > 5 {
				continue
			}
		}
		fewCues = append(fewCues, c)
	}
	close := []sampleWindow{{1000, 90}, {1090, 90}, {1180, 90}, {1270, 90}}
	inward := append(wins[:3:3], sampleWindow{wins[3].start - 180, 90})     // the last window moved inward twice
	onward := append([]sampleWindow{{wins[0].start + 90, 90}}, wins[1:]...) // the first moved inward once
	skipped, hi := inward[3].start+90, wins[3].start+wins[3].dur
	var hushed []float64 // the truth with only 14 lines, 12 s apart, in the 180 s it skipped
	for _, c := range truth {
		if c < skipped || c >= hi {
			hushed = append(hushed, c)
		}
	}
	for k := range 14 {
		hushed = append(hushed, skipped+6+12*float64(k))
	}
	slices.Sort(hushed)
	type fitCase struct {
		name          string
		cues          []span
		samples       []sample
		ratio, offset float64
		refused       string
		layout        []sampleWindow // where the windows were laid out (wins when nil)
	}
	cases := []fitCase{
		{"captions, 7.3 s late", cueSpans(truth, 1, 7.3), captionSamples(truth, wins), 1, 7.3, "", nil},
		{"captions, PAL", cueSpans(truth, 25/23.976, -0.4), captionSamples(truth, wins), 25 / 23.976, -0.4, "", nil},
		{"captions, 23.976 against 24", cueSpans(truth, 23.976/24, 1.2), captionSamples(truth, wins), 23.976 / 24, 1.2, "", nil},
		{"captions, another film", cueSpans(dense(1000, 11), 1, 0), captionSamples(truth, wins), 0, 0, "lacks evidence", nil},
		{"captions, another cut", cueSpans(truth, 1, 0), captionSamples(cut, wins), 0, 0, "window 3 lacks evidence", nil},
		{"captions, a drift", cueSpans(truth, 1, 0), captionSamples(drift, wins), 0, 0, "disagree", nil},
		{"captions, a thin window", cueSpans(truth, 1, 0), thin, 0, 0, "window 3 lacks evidence", nil},
		{"captions, a window of five lines", cueSpans(truth, 1, 0), five, 0, 0, "window 2 lacks evidence", nil},
		{"captions, ratio undecided", cueSpans(truth, 1, 0), captionSamples(truth, close), 0, 0, "another frame-rate ratio", nil},
		{"speech, 3.1 s early", cueSpans(truth, 1, -3.1), speechSamples(truth, wins), 1, -3.1, "", nil},
		{"speech, PAL", cueSpans(truth, 25/23.976, 0.6), speechSamples(truth, wins), 25 / 23.976, 0.6, "", nil},
		{"speech, another film", cueSpans(dense(1000, 11), 1, 0), speechSamples(truth, wins), 0, 0, "lacks evidence", nil},
		{"speech, a window of five lines", fewCues, speechSamples(truth, wins), 0, 0, "window 2 lacks evidence", nil},
		{"speech, another cut", cueSpans(truth, 1, 0), speechSamples(cut, wins), 0, 0, "lacks evidence", nil},
		{"speech, a drift", cueSpans(truth, 1, 0), speechSamples(drift, wins), 0, 0, "disagree", nil},
		{"captions, the last window moved inward over 60 lines", cueSpans(truth, 1, 0), captionSamples(truth, inward), 0, 0, "lines in the edge stretches", nil},
		{"captions, the last window moved inward over 14 lines", cueSpans(hushed, 1, 0), captionSamples(hushed, inward), 1, 0, "", nil},
		{"speech, the first window moved inward over 30 lines", cueSpans(truth, 1, 0), speechSamples(truth, onward), 0, 0, "lines in the edge stretches", nil},
	}
	for _, dur := range []float64{7200, 1320} { // a film and an episode: another cut 4 s longer from 8 % or from 92 %
		w, truth := sampleWindowsFor(dur, 4, 90), dense(int(dur/3), 7)
		for at, refused := range map[float64]string{0.08: "window 1 lacks evidence", 0.92: "window 4 lacks evidence"} {
			jumped := slices.Clone(truth)
			for i := range jumped {
				if jumped[i] > at*dur {
					jumped[i] += 4
				}
			}
			name := fmt.Sprintf("%.0f s, another cut from %.0f %%", dur, 100*at)
			cases = append(cases, fitCase{"captions, " + name, cueSpans(truth, 1, 0), captionSamples(jumped, w), 0, 0, refused, w},
				fitCase{"speech, " + name, cueSpans(truth, 1, 0), speechSamples(jumped, w), 0, 0, refused, w})
		}
	}
	for _, c := range cases {
		layout := c.layout
		if layout == nil {
			layout = wins
		}
		f := fitSamples(c.cues, c.samples, layout[0].start, layout[3].start+layout[3].dur)
		switch {
		case c.refused != "" && !strings.Contains(f.Refused, c.refused):
			t.Errorf("%s: refused %q (fit %+v), want refused for %q", c.name, f.Refused, f, c.refused)
		case c.refused == "" && (f.Refused != "" || math.Abs(f.Ratio-c.ratio) > 1e-9 || math.Abs(f.Offset-c.offset) > 0.05):
			t.Errorf("%s: %+v, want ratio %.5f offset %.2f accepted", c.name, f, c.ratio, c.offset)
		}
	}
}

// Catches windows placed in the first or last 3 % (logos, recaps, credits), short of them (the runtime between the edge
// and the outer window then goes unchecked), bunched, or overrunning a short video.
func TestSampleWindowsFor(t *testing.T) {
	for _, c := range []struct {
		duration float64
		length   float64
		want     []sampleWindow
	}{
		{6000, 90, []sampleWindow{{180, 90}, {2030, 90}, {3880, 90}, {5730, 90}}},
		{600, 150, []sampleWindow{{18, 141}, {159, 141}, {300, 141}, {441, 141}}}, // 94 % of 600 s in 4: 141 s each
	} {
		got := sampleWindowsFor(c.duration, 4, c.length)
		for i := range got {
			if math.Abs(got[i].start-c.want[i].start) > 1e-6 || math.Abs(got[i].dur-c.want[i].dur) > 1e-6 {
				t.Errorf("%v s: windows %v, want %v", c.duration, got, c.want)
				break
			}
		}
	}
}

// packetsCSV is ffprobe's answer for the windows with every stream shown: for the image track (stream track) a drawing
// packet and a "clear screen" one after it at each start, and between them packets of the video (stream 0) and of a
// second image track (stream 9), which are not captions.
func packetsCSV(track int, starts []float64, offset float64) string {
	var b strings.Builder
	for _, s := range starts {
		fmt.Fprintf(&b, "%d,%.3f,4096\n0,%.3f,60000\n9,%.3f,4096\n%d,%.3f,30\n", track, s+offset, s+offset+0.5, s+offset+0.7, track, s+offset+1.5)
	}
	return b.String()
}

// Catches the image track read over the whole file instead of the windows (one ffprobe over all of them, seeking by
// the index, each window ending at its absolute end with every stream shown: shown one track, ffprobe reads on to that
// track's next packet, the end of the file after the last caption), a window without dialogue judged instead of
// replaced by the stretch next to it toward the middle (the last window by the one before it, and then the one before
// that, not the quiet window again), more than two replacement windows read, another stream's packets or the "clear
// screen" packets taken for captions, or the file's start time left in the caption times.
func TestSamplerReadsWindowsAndReplacesQuietOnes(t *testing.T) {
	const film = 6000.0
	captions := dense(2000, 7)
	quiet := func(lo, hi float64) []float64 { // no captions in [lo, hi)
		var out []float64
		for _, c := range captions {
			if c < lo || c >= hi {
				out = append(out, c)
			}
		}
		return out
	}
	for _, c := range []struct {
		name    string
		track   []float64
		reads   int
		refused string
		starts  []float64
	}{
		{"every window talks", captions, 1, "", []float64{180, 2030, 3880, 5730}},
		{"the second is quiet", quiet(2030, 2120), 2, "", []float64{180, 2120, 3880, 5730}},
		{"quiet twice over", quiet(2030, 2210), 3, "", []float64{180, 2210, 3880, 5730}},
		{"quiet three times", quiet(2030, 2300), 3, "window 2 holds no dialogue", nil},
		{"the last is quiet twice over (credits)", quiet(5640, 6000), 3, "", []float64{180, 2030, 3880, 5550}},
	} {
		h := fake.NewEnv(t, nil)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "packets.csv"), []byte(packetsCSV(3, c.track, 1.4)), 0o644)
		h.Script(t, "ffprobe", `case "$*" in *format=start_time*) echo 1.400; exit 0;; esac
echo "$*" >> "`+dir+`/calls"; cat "`+dir+`/packets.csv"`+"\n")
		video := filepath.Join(dir, "v.mkv")
		os.WriteFile(video, []byte("v"), 0o644)
		r := &run{env: h.Env, opt: Options{SampleWindows: 4, SampleLength: 90e9}, rep: &stage.Report{}}
		ti := &title{path: video, info: probe.Info{Duration: film, Size: 6 << 30,
			Subtitles: []probe.Subtitle{{Index: 3, Codec: "hdmv_pgs_subtitle", Language: "eng"}}}}
		s, why := r.newSampler(ti)
		if s == nil {
			t.Fatalf("%s: no sampler: %s", c.name, why)
		}
		why, err := s.readAll(context.Background())
		calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
		lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
		if err != nil || why != c.refused || len(lines) != c.reads || !strings.Contains(lines[0], "-v error -read_intervals 181.400%271.400,2031.400%2121.400,") {
			t.Fatalf("%s: why %q err %v, %d reads %q; want %q, %d reads of the windows", c.name, why, err, len(lines), lines, c.refused, c.reads)
		}
		for i, want := range c.starts {
			smp := s.samples[i]
			var in int
			for _, c := range captions {
				if c >= want && c < want+90 {
					in++
				}
			}
			if smp.win.start != want || len(smp.captions) != in || smp.captions[0] < want || math.Abs(smp.captions[0]-captionAt(captions, want)) > 1e-6 {
				t.Errorf("%s: window %d at %v with %d captions from %v; want at %v, its captions in video time", c.name, i+1, smp.win.start,
					len(smp.captions), smp.captions[:min(1, len(smp.captions))], want)
			}
		}
	}
}

// captionAt is the first caption at or after t.
func captionAt(cs []float64, t float64) float64 {
	for _, c := range cs {
		if c >= t {
			return math.Round(c*1000) / 1000
		}
	}
	return -1
}

// srtOf is an SRT of the spans.
func srtOf(spans []span) string {
	var b strings.Builder
	for i, s := range spans {
		fmt.Fprintf(&b, "%d\n%s --> %s\nline %d\n\n", i+1, stampOf(s.start), stampOf(s.end), i)
	}
	return b.String()
}

// sampledFixture is the fixture's Show (3000 s, English audio, with or without an English PGS track) served by
// Gestdown with the given subtitles in that order, and helpers that answer from the video's true timing (dense cue
// starts): ffprobe's packets for the image track, ffmpeg's PCM (a 1 kHz tone wherever a line is spoken, 16 dB over a
// noise floor: against digital silence the clipped onset score saturates into a plateau a second wide) for the audio
// windows. Every ffprobe and ffmpeg call is logged.
func sampledFixture(t *testing.T, captions bool, subs ...string) *fixture {
	f := newFixture(t)
	f.mode = "sampled"
	truth := dense(1000, 7)
	f.h.TMDB.Handle("GET", "/tv/1", 200, `{"id":1,"external_ids":{"tvdb_id":81189}}`)
	gd, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.URL.Path == "/shows/external/tvdb/81189":
			w.Write([]byte(`{"shows":[{"id":"s"}]}`))
		case r.URL.Path == "/subtitles/get/s/1/1/English":
			var list []string
			for i := range subs {
				list = append(list, fmt.Sprintf(`{"subtitleId":"%d","version":"WEB","completed":true,"downloadUri":"/dl/%d","language":"English","downloadCount":%d}`,
					i, i, 999-i))
			}
			w.Write([]byte(`{"matchingSubtitles":[` + strings.Join(list, ",") + `],"episode":{"season":1,"number":1}}`))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			var i int
			fmt.Sscanf(r.URL.Path, "/dl/%d", &i)
			w.Write([]byte(subs[i]))
		}
	})
	f.st.gestdownAPI = gd.URL
	f.options(`providers = ["gestdown"]` + "\nalign_per_day = 0")
	f.note(show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", "none", "0h 50m")
	ff := f.h.Env.FFmpeg.Dir
	streams := `{"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"channel_layout":"stereo","tags":{"language":"eng"}}`
	if captions {
		streams += `,{"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle","tags":{"language":"eng"}}`
	}
	os.WriteFile(filepath.Join(ff, "Show.json"), []byte(probeJSON(streams)), 0o644)
	os.WriteFile(filepath.Join(ff, "packets.csv"), []byte(packetsCSV(2, truth, 0)), 0o644)
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
case "$*" in *format=start_time*) echo 0.000; exit 0;; *-read_intervals*) cat "`+ff+`/packets.csv"; exit 0;; esac
case "$*" in *"/Show "*) cat "`+ff+`/Show.json"; exit 0;; esac; exit 1
`)
	for _, w := range sampleWindowsFor(3000, 4, 90) {
		samples := make([]float64, int(w.dur*16000))
		x := uint32(w.start)
		for k := range samples {
			x = x*1664525 + 1013904223
			samples[k] = float64(int32(x>>16)-32768) / 12
		}
		for _, s := range truth {
			for k := max(0, int((s-w.start)*16000)); k < min(len(samples), int((s+2-w.start)*16000)); k++ {
				samples[k] += 8000 * math.Sin(2*math.Pi*1000*float64(k)/16000)
			}
		}
		pcm := make([]byte, 2*len(samples))
		for k, v := range samples {
			binary.LittleEndian.PutUint16(pcm[2*k:], uint16(int16(v)))
		}
		os.WriteFile(filepath.Join(ff, fmt.Sprintf("pcm-%.3f", w.start)), pcm, 0o644)
	}
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
while [ $# -gt 0 ]; do [ "$1" = "-ss" ] && ss=$2; shift; done
[ -n "$ss" ] && cat "`+ff+`/pcm-$ss" || exit 1
`)
	return f
}

// Catches the sampled fit not reached, or not written retimed: a subtitle for the PAL release, 2.4 s early, is fitted
// to four windows of the video, from its English image track (one ffprobe over the windows, no audio decoded) or else
// from its speech (one ffmpeg per window, the image track unread), and written as ratio*t + offset, with the ratio,
// offset, each window's offset and the reads in the history; the video is never read whole (no framecrc of a track,
// no alass), and the second subtitle is never downloaded. Another film's subtitle is refused on both counts and the
// title queued for the whole-file alignment.
func TestRunSampled(t *testing.T) {
	truth := dense(1000, 7)
	pal := srtOf(cueSpans(truth, 25/23.976, -2.4))
	other := srtOf(cueSpans(dense(1000, 11), 1, 0))
	for _, c := range []struct {
		why      string
		captions bool
		subs     []string
		want     string
	}{
		{"captions", true, []string{pal, other}, "sampled"},
		{"speech", false, []string{pal, other}, "sampled"},
		{"another film, captions", true, []string{other, other}, "queued"},
		{"another film, speech", false, []string{other, other}, "queued"},
	} {
		f := sampledFixture(t, c.captions, c.subs...)
		rep := f.run(t)
		ff := f.h.Env.FFmpeg.Dir
		probes, _ := os.ReadFile(filepath.Join(ff, "ffprobe.calls"))
		helpers, _ := os.ReadFile(filepath.Join(ff, "helper.calls"))
		intervals, audio := strings.Count(string(probes), "-read_intervals"), strings.Count(string(helpers), "-ss ")
		whole := strings.Contains(string(helpers), "framecrc") || strings.Contains(string(helpers), " _ ")
		if o := f.outcomes()[f.path(show)]; o != c.want || whole || c.captions && (intervals != 1 || audio != 0) || !c.captions && (intervals != 0 || audio != 4) {
			t.Errorf("%s: %q (want %q); %d image-track reads, %d audio windows, whole-file read %v", c.why, o, c.want, intervals, audio, whole)
			continue
		}
		if c.want == "queued" {
			var n int
			f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='rejected_sampled'`, f.path(show)).Scan(&n)
			f.h.Now = f.h.Now.Add(169 * time.Hour) // due again: the refused subtitles are not sampled again
			f.run(t)
			again, _ := os.ReadFile(filepath.Join(ff, "ffprobe.calls"))
			more, _ := os.ReadFile(filepath.Join(ff, "helper.calls"))
			reads := func(b []byte) int {
				return strings.Count(string(b), "-read_intervals") + strings.Count(string(b), "-ss ")
			}
			if n != 2 || reads(again)+reads(more) != reads(probes)+reads(helpers) || f.outcomes()[f.path(show)] != "queued" {
				t.Errorf("%s: %d rejected_sampled events, windows read %d times, then %d after a second run; want both candidates refused once",
					c.why, n, reads(probes)+reads(helpers), reads(again)+reads(more))
			}
			continue
		}
		got := cues(f.sidecar(t, show))
		for i, s := range got {
			if len(got) != len(truth) || math.Abs(s.start-truth[i]) > 0.05 {
				t.Fatalf("%s: cue %d of %d at %.3f, want %.3f", c.why, i, len(got), s.start, truth[i])
			}
		}
		v := f.verdict(show, "sampled")
		ref := map[bool]string{true: "captions", false: "speech"}[c.captions]
		if v["reference"] != ref || v["ratio"] != 25/23.976 || math.Abs(v["offset_ms"].(float64)+2400) > 50 || len(v["window_offsets_ms"].([]any)) != 4 ||
			v["id"] != "gestdown:0" || rep.Counts["written_sampled"] != 1 || strings.Count(strings.Join(f.requests, " "), "/dl/") != 0 {
			t.Errorf("%s: event %v, counts %v; want %s, PAL, -2400 ms, 4 window offsets, gestdown:0 only", c.why, v, rep.Counts, ref)
		}
	}
}

// Catches a title sampled that cannot be sampled cheaply, exactly or meaningfully: under 10 minutes, in a container
// whose seeks do not land where playback does (AVI, MPEG-TS), or with no reference at all; and the reference picked
// wrong: the English image track whenever there is one (not a forced one, not a commentary), else the first audio track.
func TestNewSampler(t *testing.T) {
	pgs := probe.Subtitle{Index: 4, Codec: "hdmv_pgs_subtitle", Language: "eng"}
	audio := []probe.Audio{{Index: 1, Layout: "5.1(side)"}}
	r := &run{env: fake.NewEnv(t, nil).Env, opt: Options{SampleWindows: 4, SampleLength: 90e9}}
	for _, c := range []struct {
		why, path string
		info      probe.Info
		want      string // "captions", "speech", or "" for none
	}{
		{"a film with English captions", "v.mkv", probe.Info{Duration: 6000, Audio: audio, Subtitles: []probe.Subtitle{pgs}}, "captions"},
		{"only forced captions", "v.mkv", probe.Info{Duration: 6000, Audio: audio, Subtitles: []probe.Subtitle{{Index: 4, Codec: "hdmv_pgs_subtitle",
			Language: "eng", Forced: true}}}, "speech"},
		{"only a commentary in images", "v.mkv", probe.Info{Duration: 6000, Audio: audio, Subtitles: []probe.Subtitle{{Index: 4, Codec: "hdmv_pgs_subtitle",
			Language: "eng", Title: "Director's Commentary"}}}, "speech"},
		{"no captions", "v.mp4", probe.Info{Duration: 6000, Audio: audio}, "speech"},
		{"no audio either", "v.mkv", probe.Info{Duration: 6000}, ""},
		{"a featurette", "v.mkv", probe.Info{Duration: 540, Audio: audio}, ""},
		{"an AVI", "v.avi", probe.Info{Duration: 6000, Audio: audio}, ""},
		{"an MPEG-TS", "v.ts", probe.Info{Duration: 6000, Audio: audio}, ""},
		{"a QuickTime file", "v.mov", probe.Info{Duration: 6000, Audio: audio}, "speech"},
	} {
		s, why := r.newSampler(&title{path: c.path, info: c.info})
		got := ""
		if s != nil {
			got = s.label()
		}
		if got != c.want || s == nil && why == "" {
			t.Errorf("%s: %q (why %q), want %q", c.why, got, why, c.want)
		}
	}
}

// Catches the dialogue taken from a mix where a centre channel holds it, or a pan asked of a layout without one (ffmpeg
// would fail the read).
func TestDialogue(t *testing.T) {
	for layout, want := range map[string]string{"5.1(side)": "-af pan=mono|c0=FC", "7.1": "-af pan=mono|c0=FC",
		"stereo": "-ac 1", "mono": "-ac 1", "": "-ac 1", "quad": "-ac 1"} {
		if got := dialogue(probe.Audio{Layout: layout}); got[0]+" "+got[1] != want {
			t.Errorf("%q: %v, want %s", layout, got, want)
		}
	}
}

// Catches a helper left to read on when a seek does not land (no usable index: it reads the file from the start):
// it is killed once it has read past its allowance, four times its share of the file plus 64 MiB (a 1 GiB, 100-minute
// film: 131.5 MB for one 90 s audio window, 324.8 MB for the image track's four), long before the 200 GiB it would
// read. Linux only: the count is rchar from /proc.
func TestSamplerKillsAReadPastItsAllowance(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rchar is read from /proc/<pid>/io")
	}
	const endless = "exec dd if=/dev/zero of=/dev/null bs=4M count=51200 2>/dev/null\n"
	for _, c := range []struct {
		name, helper, script string
		subs                 []probe.Subtitle
		allowance            int64
	}{
		{"audio", "ffmpeg", endless, nil, 131533373},
		{"image track", "ffprobe", "case \"$*\" in *format=start_time*) echo 0; exit 0;; esac\n" + endless,
			[]probe.Subtitle{{Index: 2, Codec: "hdmv_pgs_subtitle", Language: "eng"}}, 324806901},
	} {
		h := fake.NewEnv(t, nil)
		h.Script(t, c.helper, c.script)
		video := filepath.Join(t.TempDir(), "v.mkv")
		os.WriteFile(video, []byte("v"), 0o644)
		r := &run{env: h.Env, opt: Options{SampleWindows: 4, SampleLength: 90e9}, rep: &stage.Report{}}
		s, _ := r.newSampler(&title{path: video, info: probe.Info{Duration: 6000, Size: 1 << 30, Audio: []probe.Audio{{Index: 1}}, Subtitles: c.subs}})
		why, err := s.readAll(context.Background())
		if err != nil || !strings.Contains(why, fmt.Sprintf("stopped past %d bytes: no usable index", c.allowance)) || s.read < c.allowance || s.read > 20<<30 {
			t.Errorf("%s: why %q err %v after %d bytes; want killed soon after %d", c.name, why, err, s.read, c.allowance)
		}
	}
}
