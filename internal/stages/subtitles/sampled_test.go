package subtitles

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// from speech), another film's subtitle or another cut's written, a window with too few matched lines accepted, and a
// ratio the windows cannot tell from another (all four within six minutes) taken anyway.
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
		drift[i] *= 1.0002
	}
	thin := captionSamples(truth, wins)
	other := thin[2].captions[:3:3] // window 3 keeps 3 of its lines and gains another film's
	for _, c := range dense(1000, 11) {
		if w := thin[2].win; c >= w.start && c < w.start+w.dur {
			other = append(other, c)
		}
	}
	thin[2].captions = other
	close := []sampleWindow{{1000, 90}, {1090, 90}, {1180, 90}, {1270, 90}}
	for _, c := range []struct {
		name          string
		cues          []span
		samples       []sample
		ratio, offset float64
		refused       string
	}{
		{"captions, 7.3 s late", cueSpans(truth, 1, 7.3), captionSamples(truth, wins), 1, 7.3, ""},
		{"captions, PAL", cueSpans(truth, 25/23.976, -0.4), captionSamples(truth, wins), 25 / 23.976, -0.4, ""},
		{"captions, 23.976 against 24", cueSpans(truth, 23.976/24, 1.2), captionSamples(truth, wins), 23.976 / 24, 1.2, ""},
		{"captions, another film", cueSpans(dense(1000, 11), 1, 0), captionSamples(truth, wins), 0, 0, "lacks evidence"},
		{"captions, another cut", cueSpans(truth, 1, 0), captionSamples(cut, wins), 0, 0, "window 3 lacks evidence"},
		{"captions, a drift", cueSpans(truth, 1, 0), captionSamples(drift, wins), 0, 0, "disagree"},
		{"captions, a thin window", cueSpans(truth, 1, 0), thin, 0, 0, "window 3 lacks evidence"},
		{"captions, ratio undecided", cueSpans(truth, 1, 0), captionSamples(truth, close), 0, 0, "another frame-rate ratio"},
		{"speech, 3.1 s early", cueSpans(truth, 1, -3.1), speechSamples(truth, wins), 1, -3.1, ""},
		{"speech, PAL", cueSpans(truth, 25/23.976, 0.6), speechSamples(truth, wins), 25 / 23.976, 0.6, ""},
		{"speech, another film", cueSpans(dense(1000, 11), 1, 0), speechSamples(truth, wins), 0, 0, "lacks evidence"},
		{"speech, another cut", cueSpans(truth, 1, 0), speechSamples(cut, wins), 0, 0, "lacks evidence"},
		{"speech, a drift", cueSpans(truth, 1, 0), speechSamples(drift, wins), 0, 0, "disagree"},
	} {
		f := fitSamples(c.cues, c.samples)
		switch {
		case c.refused != "" && !strings.Contains(f.Refused, c.refused):
			t.Errorf("%s: refused %q (fit %+v), want refused for %q", c.name, f.Refused, f, c.refused)
		case c.refused == "" && (f.Refused != "" || math.Abs(f.Ratio-c.ratio) > 1e-9 || math.Abs(f.Offset-c.offset) > 0.05):
			t.Errorf("%s: %+v, want ratio %.5f offset %.2f accepted", c.name, f, c.ratio, c.offset)
		}
	}
}

// Catches windows placed in the first or last 3 % (logos, recaps, credits), bunched, or overrunning a short video.
func TestSampleWindowsFor(t *testing.T) {
	for _, c := range []struct {
		duration float64
		length   float64
		want     []sampleWindow
	}{
		{6000, 90, []sampleWindow{{840, 90}, {2250, 90}, {3660, 90}, {5070, 90}}},
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

// packetsJSON is ffprobe's answer for an image track: a drawing packet (and a "clear screen" one after it) at each start.
func packetsJSON(starts []float64, offset float64) string {
	var b strings.Builder
	b.WriteString(`{"packets":[`)
	for i, s := range starts {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"pts_time":"%.3f","size":"4096"},{"pts_time":"%.3f","size":"30"}`, s+offset, s+offset+1.5)
	}
	b.WriteString(`]}`)
	return b.String()
}

// Catches the image track read over the whole file instead of the windows (one ffprobe over all of them, seeking by
// the index), a window without dialogue judged instead of replaced by the stretch after it, more than two replacement
// windows read, the "clear screen" packets taken for captions, or the file's start time left in the caption times.
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
		{"every window talks", captions, 1, "", []float64{840, 2250, 3660, 5070}},
		{"the second is quiet", quiet(2250, 2340), 2, "", []float64{840, 2340, 3660, 5070}},
		{"quiet twice over", quiet(2250, 2430), 3, "", []float64{840, 2430, 3660, 5070}},
		{"quiet three times", quiet(2250, 2520), 3, "window 2 holds no dialogue", nil},
	} {
		h := fake.NewEnv(t, nil)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "packets.json"), []byte(packetsJSON(c.track, 1.4)), 0o644)
		h.Script(t, "ffprobe", `case "$*" in *format=start_time*) echo 1.400; exit 0;; esac
echo "$*" >> "`+dir+`/calls"; cat "`+dir+`/packets.json"`+"\n")
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
		if err != nil || why != c.refused || len(lines) != c.reads || !strings.Contains(lines[0], "-select_streams 3 -read_intervals 841.400%+90.000,2251.400%+90.000,") {
			t.Fatalf("%s: why %q err %v, %d reads %q; want %q, %d reads of the windows", c.name, why, err, len(lines), lines, c.refused, c.reads)
		}
		for i, want := range c.starts {
			smp := s.samples[i]
			if smp.win.start != want || len(smp.captions) < 25 || smp.captions[0] < want || math.Abs(smp.captions[0]-captionAt(captions, want)) > 1e-6 {
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

// Catches an AVI without an index sampled (every seek would read it from the start), or an indexed one (idx1 after
// the movi list, or an OpenDML indx) sent to the queue.
func TestAVIIndexed(t *testing.T) {
	chunk := func(id string, body []byte) []byte {
		b := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
		b = append(b, body...)
		if len(body)%2 == 1 {
			b = append(b, 0)
		}
		return b
	}
	list := func(kind string, parts ...[]byte) []byte {
		return chunk("LIST", append([]byte(kind), bytes.Join(parts, nil)...))
	}
	riff := func(parts ...[]byte) []byte { return chunk("RIFF", append([]byte("AVI "), bytes.Join(parts, nil)...)) }
	hdrl := list("hdrl", chunk("avih", make([]byte, 56)), list("strl", chunk("strh", make([]byte, 56))))
	odml := list("hdrl", chunk("avih", make([]byte, 56)), list("strl", chunk("strh", make([]byte, 56)), chunk("indx", make([]byte, 24))))
	movi := list("movi", chunk("00dc", make([]byte, 999)))
	dir := t.TempDir()
	for name, c := range map[string]struct {
		data []byte
		want bool
	}{
		"idx1":     {riff(hdrl, movi, chunk("idx1", make([]byte, 16))), true},
		"OpenDML":  {riff(odml, movi), true},
		"no index": {riff(hdrl, movi), false},
		"not RIFF": {[]byte("not an avi at all"), false},
	} {
		p := filepath.Join(dir, name+".avi")
		os.WriteFile(p, c.data, 0o644)
		if got := aviIndexed(p); got != c.want {
			t.Errorf("%s: indexed %v, want %v", name, got, c.want)
		}
	}
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
// starts): ffprobe's packets for the image track, ffmpeg's PCM (a 1 kHz tone wherever a line is spoken) for the
// audio windows. Every ffprobe and ffmpeg call is logged.
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
	os.WriteFile(filepath.Join(ff, "packets.json"), []byte(packetsJSON(truth, 0)), 0o644)
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
case "$*" in *format=start_time*) echo 0.000; exit 0;; *-read_intervals*) cat "`+ff+`/packets.json"; exit 0;; esac
case "$*" in *"/Show "*) cat "`+ff+`/Show.json"; exit 0;; esac; exit 1
`)
	for _, w := range sampleWindowsFor(3000, 4, 90) {
		pcm := make([]byte, int(w.dur*16000)*2)
		for _, s := range truth {
			for k := max(0, int((s-w.start)*16000)); k < min(len(pcm)/2, int((s+2-w.start)*16000)); k++ {
				binary.LittleEndian.PutUint16(pcm[2*k:], uint16(int16(8000*math.Sin(2*math.Pi*1000*float64(k)/16000))))
			}
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
			if n != 2 {
				t.Errorf("%s: %d rejected_sampled events, want both candidates", c.why, n)
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
