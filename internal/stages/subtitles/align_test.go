package subtitles

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// captionTimes are irregular (not periodic, so only one lag fits) caption starts over a 3000 s video.
func captionTimes() []float64 {
	var ts []float64
	for i := 0; i < 60; i++ {
		ts = append(ts, 30+float64(i)*45+float64(i*i*7%23))
	}
	return ts
}

// otherTimes are another film's caption starts: they fit no lag of captionTimes.
func otherTimes() []float64 {
	other := make([]float64, 60)
	for i := range other {
		other[i] = 40 + float64(i)*41 + float64(i*i*11%37)
	}
	return other
}

func framecrc(ts []float64) string {
	var b strings.Builder
	b.WriteString("#software: Lavf\n#tb 0: 1/1000\n#media_type 0: subtitle\n")
	for _, t := range ts {
		ms := int(math.Round(t * 1000))
		fmt.Fprintf(&b, "0, %10d, %10d,     2000,      4096, 0x12345678\n", ms, ms)
		fmt.Fprintf(&b, "0, %10d, %10d,        0,        30, 0x00000000\n", ms+2000, ms+2000) // "clear screen" packet
	}
	return b.String()
}

func srt(ts []float64, shift float64) string {
	var b strings.Builder
	for i, t := range ts {
		fmt.Fprintf(&b, "%d\n%s --> %s\nline %d\n\n", i+1, stampOf(t+shift), stampOf(t+shift+1.5), i)
	}
	return b.String()
}

func stampOf(t float64) string {
	ms := int64(math.Round(t * 1000))
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func starts(txt string) []span {
	var out []span
	for _, s := range cues(txt) {
		out = append(out, span{s.start, s.start})
	}
	return out
}

// Catches: Python's round-half-even, the clamp at zero, or the frame-rate stretch lost in retime (expected strings
// are subfetch2.py's own retime() output).
func TestRetime(t *testing.T) {
	cases := []struct {
		in         string
		ratio, off float64
		want       string
	}{
		{"1\n00:00:01,000 --> 00:00:02,500\nA\n\n2\n01:59:59,999 --> 02:00:00,000\nB\n", 25 / 23.976, -1.2,
			"1\n00:00:00,000 --> 00:00:01,407\nA\n\n2\n02:05:06,306 --> 02:05:06,308\nB\n"},
		{"1\n00:00:00,500 --> 00:00:01,000\nA\n", 1.0, -0.75, "1\n00:00:00,000 --> 00:00:00,250\nA\n"},
		{"1\n00:00:00,001 --> 00:00:00,003\nA\n", 0.5, 0, "1\n00:00:00,000 --> 00:00:00,002\nA\n"},
	}
	for _, c := range cases {
		if got := retime(c.in, c.ratio, c.off); got != c.want {
			t.Errorf("retime(%q, %v, %v) = %q, want %q", c.in, c.ratio, c.off, got, c.want)
		}
	}
}

// Catches: a shifted or 25/23.976-stretched candidate not recovered from the caption starts, an in-sync candidate
// not returned as exactly (1, 0), or another film's subtitle accepted. Like the Python, the fit takes the first lag
// of the overlap plateau (the reference marks +-0.2 s around each caption), so it may land up to 0.2 s early.
func TestAlign(t *testing.T) {
	ts := captionTimes()
	_, ref, ok := pgsReference(framecrc(ts), 3000)
	if !ok {
		t.Fatal("reference rejected")
	}
	stretched := make([]float64, len(ts))
	for i, x := range ts {
		stretched[i] = x / (25 / 23.976)
	}
	other := otherTimes()
	cases := []struct {
		name          string
		cand          string
		ok            bool
		ratio, offset float64
	}{
		{"in sync", srt(ts, 0), true, 1, 0},
		{"2.5 s late", srt(ts, -2.5), true, 1, 2.5},
		{"PAL speed-up", srt(stretched, 0), true, 25 / 23.976, 0},
		{"another film", srt(other, 0), false, 0, 0},
	}
	for _, c := range cases {
		f, ok := align(starts(c.cand), ref)
		if ok != c.ok || ok && (math.Abs(f.Ratio-c.ratio) > 1e-3 || math.Abs(f.Offset-c.offset) > 0.21) {
			t.Errorf("%s: align = %+v, %v; want ratio %.4f offset %.2f ok %v", c.name, f, ok, c.ratio, c.offset, c.ok)
		}
		if c.name == "in sync" && (f.Ratio != 1 || f.Offset != 0) {
			t.Errorf("in sync must be exactly (1, 0) so the file is kept as is: %+v", f)
		}
	}
}

// Catches: PGS "clear screen" packets counted as captions, or a track covering too little of the film accepted.
func TestPGSReference(t *testing.T) {
	ts := captionTimes()
	got, _, ok := pgsReference(framecrc(ts), 3000)
	if !ok || len(got) != len(ts) {
		t.Fatalf("starts = %d, ok %v; want %d", len(got), ok, len(ts))
	}
	if _, _, ok := pgsReference(framecrc(ts[:39]), 3000); ok {
		t.Error("39 captions accepted")
	}
	if _, _, ok := pgsReference(framecrc(ts), 6000); ok {
		t.Error("captions covering under half the video accepted")
	}
}
