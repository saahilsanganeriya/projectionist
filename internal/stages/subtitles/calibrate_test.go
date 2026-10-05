//go:build calibrate

package subtitles

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Calibration harness, not a test (build tag "calibrate"): the evidence behind the sampled fit's thresholds. CALIBRATE=
// <dir> holds what the NAS gave for 15 titles of the quick gate: sampled/proto/<id>/ (path, windows, probe.json, subs.csv:
// the English image track's packets in the 4 windows), sampled/proto2/<id>/ (a<i>.pcm: each window's dialogue channel
// as 16 kHz s16le, channels), quick/out and quick2/out (the subtitles quick mode wrote), quick/spot/*.framecrc (whole
// caption tracks) and quick/lib/**.en.idx. A title's truth is its subtitle's whole-file caption fit. Run with
//
//	CALIBRATE=<dir> go test -tags calibrate -run TestCalibrate -v ./internal/stages/subtitles

type calibrationTitle struct {
	name, stem string
	dur        float64
	windows    []sampleWindow
	captions   [][]float64
	pcm        [][]byte
	cand       string
	truth      []float64 // full caption starts
}

func loadCalibration(t *testing.T) []calibrationTitle {
	root := os.Getenv("CALIBRATE")
	dirs, _ := filepath.Glob(filepath.Join(root, "sampled/proto2/*"))
	var out []calibrationTitle
	for _, d := range dirs {
		raw, _ := os.ReadFile(filepath.Join(d, "path"))
		p := strings.TrimSpace(string(raw))
		stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		var pj struct {
			Format struct {
				Duration string `json:"duration"`
			} `json:"format"`
		}
		b, _ := os.ReadFile(filepath.Join(d, "probe.json"))
		json.Unmarshal(b, &pj)
		pt := calibrationTitle{name: filepath.Base(p), stem: stem}
		pt.dur, _ = strconv.ParseFloat(pj.Format.Duration, 64)
		wraw, _ := os.ReadFile(filepath.Join(d, "windows"))
		for _, line := range strings.Split(strings.TrimSpace(string(wraw)), "\n") {
			f := strings.Fields(line)
			s, _ := strconv.ParseFloat(f[0], 64)
			w, _ := strconv.ParseFloat(f[1], 64)
			pt.windows = append(pt.windows, sampleWindow{s, w})
		}
		if c, err := os.ReadFile(filepath.Join(root, "sampled/proto", filepath.Base(d), "subs.csv")); err == nil {
			var ev []float64
			for _, line := range strings.Split(string(c), "\n") {
				f := strings.Split(line, ",")
				if len(f) < 3 {
					continue
				}
				size, _ := strconv.Atoi(strings.TrimSpace(f[2]))
				ts, err := strconv.ParseFloat(f[0], 64)
				if err == nil && size > pgsMinPkt {
					ev = append(ev, ts)
				}
			}
			for _, w := range pt.windows {
				var in []float64
				for _, e := range ev {
					if e >= w.start && e < w.start+w.dur {
						in = append(in, e)
					}
				}
				pt.captions = append(pt.captions, in)
			}
		}
		for i := range pt.windows {
			b, _ := os.ReadFile(filepath.Join(d, fmt.Sprintf("a%d.pcm", i)))
			if ch, _ := os.ReadFile(filepath.Join(d, "channels")); strings.TrimSpace(string(ch)) == "2" {
				mono := make([]byte, len(b)/2)
				for k := 0; k+3 < len(b); k += 4 {
					l, r := int16(uint16(b[k])|uint16(b[k+1])<<8), int16(uint16(b[k+2])|uint16(b[k+3])<<8)
					m := uint16(int16((int32(l) + int32(r)) / 2))
					mono[k/2], mono[k/2+1] = byte(m), byte(m>>8)
				}
				b = mono
			}
			pt.pcm = append(pt.pcm, b)
		}
		for _, o := range []string{"quick/out", "quick2/out"} {
			m, _ := filepath.Glob(filepath.Join(root, o, "*", "*", stem+".en.srt"))
			m2, _ := filepath.Glob(filepath.Join(root, o, "*", "*", "*", stem+".en.srt"))
			m = append(m, m2...)
			if len(m) > 0 && pt.cand == "" {
				b, _ := os.ReadFile(m[0])
				pt.cand = string(b)
			}
		}
		if spots, _ := os.ReadFile(filepath.Join(root, "quick/spot_in.tsv")); true {
			for _, line := range strings.Split(string(spots), "\n") {
				f := strings.Split(line, "\t")
				if len(f) > 4 && f[0] == stem {
					fc, _ := os.ReadFile(filepath.Join(root, "quick/spot", filepath.Base(f[3])))
					pt.truth, _, _ = pgsReference(string(fc), pt.dur)
				}
			}
		}
		if pt.truth == nil {
			m, _ := filepath.Glob(filepath.Join(root, "quick/lib", "*", "*", stem+".en.idx"))
			m2, _ := filepath.Glob(filepath.Join(root, "quick/lib", "*", "*", "*", stem+".en.idx"))
			m = append(m, m2...)
			if len(m) > 0 {
				if ref := vobsubReference(strings.TrimSuffix(m[0], ".en.idx")+".mkv", pt.dur); ref != nil {
					pt.truth = ref.starts
				}
			}
		}
		out = append(out, pt)
	}
	return out
}

// truthFit is the whole-file caption fit of the candidate, its offset refined to the median of matched pairs.
func truthFit(spans []span, truth []float64) (float64, float64, bool) {
	st := make([]span, len(spans))
	for i, s := range spans {
		st[i] = span{s.start, s.start}
	}
	_, bits, ok := captionReference(append([]float64(nil), truth...), truth[len(truth)-1])
	if !ok {
		return 0, 0, false
	}
	f, ok := align(st, bits)
	if !ok {
		return 0, 0, false
	}
	return f.Ratio, pyMedian(matched(spans, truth, f.Ratio, f.Offset)), true
}

// TestCalibrate runs the production fit on every title's windows: its own candidate, synthetic variants of its
// truth-aligned text (shifted, PAL, 24 fps), another cut (the second half 4 s later), and every other title's
// candidate (must be refused).
func TestCalibrate(t *testing.T) {
	titles := loadCalibration(t)
	samplesOf := func(pt calibrationTitle, kind string) []sample {
		var smp []sample
		for i, w := range pt.windows {
			if kind == "captions" {
				smp = append(smp, sample{win: w, captions: pt.captions[i]})
				continue
			}
			level := voiceLevel(pt.pcm[i])
			smp = append(smp, sample{win: w, level: level, voiced: voiced(level)})
		}
		return smp
	}
	var accepted, refusedPos, negAccepted, negTotal int
	type res struct {
		f   sampledFit
		pos bool
	}
	var all []res
	for _, pt := range titles {
		if pt.truth == nil {
			continue
		}
		spans := cues(pt.cand)
		tr, to, ok := truthFit(spans, pt.truth)
		if !ok {
			continue
		}
		inSync := cues(retime(pt.cand, tr, to))
		for _, kind := range []string{"captions", "speech"} {
			if kind == "captions" && pt.captions == nil {
				continue
			}
			smp := samplesOf(pt, kind)
			for _, v := range []struct {
				name string
				r, o float64
				cut  bool
			}{{"orig", tr, to, false}, {"+3.7", 1, 3.7, false}, {"pal", 25 / 23.976, 0.8, false}, {"24", 24 / 23.976, -2, false}, {"cut", 1, 0, true}} {
				c := spans
				if v.name != "orig" {
					c = make([]span, len(inSync))
					for i, s := range inSync {
						c[i] = span{(s.start - v.o) / v.r, (s.end - v.o) / v.r}
						if v.cut && s.start > pt.dur/2 {
							c[i] = span{s.start + 4, s.end + 4}
						}
					}
				}
				f := fitSamples(c, smp)
				if kind == "speech" {
					all = append(all, res{f, !v.cut})
				}
				var errs []float64
				for _, q := range []float64{0.05, 0.5, 0.95} {
					tau := q * pt.dur / v.r
					errs = append(errs, (f.Ratio-v.r)*tau+f.Offset-v.o)
				}
				if v.cut {
					if f.Refused == "" {
						negAccepted++
					}
					negTotal++
				} else if f.Refused == "" {
					accepted++
				} else {
					refusedPos++
				}
				t.Logf("%-22.22s %-8s %-5s r=%.4f err=%v offs=%v ev=%v margin=%.2f %s", pt.name, kind, v.name, f.Ratio, round3(errs),
					round3(sub(f.Offsets, f.Offset)), round3(f.Evidence), f.Score/math.Max(f.RunnerUp, 1e-9), f.Refused)
			}
			for _, other := range titles {
				if other.name == pt.name || other.cand == "" {
					continue
				}
				f := fitSamples(cues(other.cand), smp)
				if kind == "speech" {
					all = append(all, res{f, false})
				}
				negTotal++
				if f.Refused == "" {
					negAccepted++
					t.Logf("%-22.22s %-8s NEG ACCEPTED %s: r=%.4f o=%.2f", pt.name, kind, other.name, f.Ratio, f.Offset)
				}
			}
		}
	}
	t.Logf("positives accepted %d, refused %d; negatives accepted %d of %d", accepted, refusedPos, negAccepted, negTotal)
	for _, z := range []float64{2.0, 2.5, 3.0, 3.5} {
		for _, spread := range []float64{0.3, 0.5} {
			var pa, na, pt, nt int
			for _, r := range all {
				ok := len(r.f.Offsets) > 0 && slices.Min(r.f.Evidence) >= z && slices.Max(r.f.Offsets)-slices.Min(r.f.Offsets) <= spread &&
					r.f.Score >= minMargin*r.f.RunnerUp
				if r.pos {
					pt++
					if ok {
						pa++
					}
				} else {
					nt++
					if ok {
						na++
					}
				}
			}
			t.Logf("speech minZ %.1f spread %.1f: positives %d/%d, negatives %d/%d", z, spread, pa, pt, na, nt)
		}
	}
}

func round3(xs []float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = math.Round(x*1000) / 1000
	}
	return out
}

func sub(xs []float64, c float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = x - c
	}
	return out
}
