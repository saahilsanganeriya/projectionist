//go:build gate

package subtitles

// Gate harness, not a test (build tag "gate"): replays real NAS files through the ported functions and writes what
// the Python scripts write, for a byte comparison with their output. Build it for the NAS with
//
//	GOOS=linux GOARCH=amd64 go test -c -tags gate -o subtitles-gate.test ./internal/stages/subtitles
//
// and run it with GATE_OUT=<dir> plus any of: GATE_ROOTS (folders, one per line) -> stats.tsv as sub_stats.py prints
// it; GATE_VIDEOS (files, one per line) -> hashes.tsv; GATE_PAIRS (pairs.tsv: id, framecrc file, duration, candidate
// file) -> fits.tsv; GATE_DOWNLOADS (id, raw download file, declared encoding) -> decoded.tsv: the sha1 of the
// decoded text and of that text with the ad blocks stripped; GATE_ALASS (id, video, framecrc, duration, candidate
// files...) with GATE_ALASS_BIN and GATE_FFMPEG_DIR -> alass.tsv: per candidate, the stage's alass route scored
// against the PGS captions it never saw, and its caption route with the alass rescue; GATE_SPOT (id, video, sidecar,
// framecrc file, "idx" or "-", duration) with the same two -> spot.tsv: a written sidecar's median distance to the
// video's English captions where a framecrc is given (or to its <video>.en.idx captions for "idx"), and the shift
// alass's alignment to the speech would give it; GATE_SYNC (id, video, sidecar, duration, "captions <stream>" or
// "alass <audio stream>", and optionally the starts of the windows the fit read) with the same two -> sync.tsv: a
// written sidecar's offset from an independent whole-file reference (the complete caption track less the windows the
// fit read, or alass's whole-file alignment to the audio) at the start, middle and end;
// GATE_FIT (id, video, candidate file) with GATE_FFMPEG_DIR -> fit.tsv and fit/<id>.srt: the sampled fit of that very
// candidate (the stage's own reads of the windows), and the retimed text when the fit holds; GATE_LOCAL (id, video,
// sidecar) -> local.tsv: a written sidecar's own best offset (speech onsets, ratio 1) in ten 90 s windows at 5, 15 ...
// 95 % of the runtime, each with its z: where it sits beyond the four windows the fit read.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fake"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

func gateLines(t *testing.T, env string) []string {
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s not set", env)
	}
	raw, err := os.ReadFile(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func gateWrite(t *testing.T, name, body string) {
	if err := os.WriteFile(filepath.Join(os.Getenv("GATE_OUT"), name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGateStats(t *testing.T) {
	var b strings.Builder
	b.WriteString(statsHeader)
	for _, root := range gateLines(t, "GATE_ROOTS") {
		pyWalk(root, func(dir string, files []string) {
			for _, f := range files {
				if subtitleFile(f) {
					raw, err := os.ReadFile(dir + "/" + f)
					if err != nil {
						t.Fatal(err)
					}
					b.WriteString(dir + "/" + f + "\t" + statsRow(pyExt(f), raw) + "\n")
				}
			}
		})
	}
	gateWrite(t, "stats.tsv", b.String())
}

func TestGateHash(t *testing.T) {
	var b strings.Builder
	for _, p := range gateLines(t, "GATE_VIDEOS") {
		h, size, err := openSubtitlesHash(p)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\n", h, size, p)
	}
	gateWrite(t, "hashes.tsv", b.String())
}

func g17(f float64) string { return strconv.FormatFloat(f, 'g', 17, 64) }

func TestGateFits(t *testing.T) {
	var b strings.Builder
	for _, line := range gateLines(t, "GATE_PAIRS") {
		f := strings.Split(line, "\t")
		framecrc, _ := os.ReadFile(f[1])
		duration, _ := strconv.ParseFloat(f[2], 64)
		cand, _ := os.ReadFile(f[3])
		_, ref, ok := pgsReference(string(framecrc), duration)
		if !ok {
			fmt.Fprintf(&b, "%s\tnoref\n", f[0])
			continue
		}
		txt := string(cand)
		var starts []span
		for _, s := range cues(txt) {
			starts = append(starts, span{s.start, s.start})
		}
		fit, ok := align(starts, ref)
		if len(starts) < minCues || !ok {
			fmt.Fprintf(&b, "%s\tNone\n", f[0])
			continue
		}
		out := txt
		if fit.Ratio != 1.0 || fit.Offset != 0.0 {
			out = retime(txt, fit.Ratio, fit.Offset)
		}
		sum := sha1.Sum([]byte(out))
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\t%t\n", f[0], g17(fit.Ratio), g17(fit.Offset), g17(fit.Z), fit.Agree,
			hex.EncodeToString(sum[:]), sane(cues(out), duration))
	}
	gateWrite(t, "fits.tsv", b.String())
}

func TestGateDecode(t *testing.T) {
	var b strings.Builder
	for _, line := range gateLines(t, "GATE_DOWNLOADS") {
		f := strings.Split(line, "\t")
		raw, err := os.ReadFile(f[1])
		if err != nil {
			t.Fatal(err)
		}
		txt := decodeDownload(raw, f[2])
		decoded, stripped := sha1.Sum([]byte(txt)), sha1.Sum([]byte(stripAds(txt)))
		fmt.Fprintf(&b, "%s\t%s\t%s\n", f[0], hex.EncodeToString(decoded[:]), hex.EncodeToString(stripped[:]))
	}
	gateWrite(t, "decoded.tsv", b.String())
}

func TestGateAlass(t *testing.T) {
	lines := gateLines(t, "GATE_ALASS")
	h := fake.NewEnv(t, nil) // nobody watching
	h.Env.FFmpeg = config.FFmpeg{Dir: os.Getenv("GATE_FFMPEG_DIR")}
	r := &run{env: h.Env, opt: Options{Alass: os.Getenv("GATE_ALASS_BIN")}, rep: &stage.Report{}, alerted: map[string]bool{}}
	out := make([]string, len(lines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 1) // one alass decode at a time: the NAS is shared
	for i, line := range lines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx := context.Background()
			f := strings.Split(line, "\t")
			raw, _ := exec.Command(r.env.FFmpeg.FFprobePath(), "-v", "error", "-select_streams", "a", "-show_entries",
				"stream=index:stream_disposition=default", "-of", "csv=p=0", f[1]).Output()
			audio := strings.Split(strings.Split(string(raw), "\n")[0], ",")[0] // the first audio stream (the default here)
			for _, l := range strings.Split(string(raw), "\n") {
				if strings.HasSuffix(l, ",1") {
					audio = strings.Split(l, ",")[0]
					break
				}
			}
			index, _ := strconv.Atoi(audio)
			ti := &title{path: f[1], info: probe.Info{Audio: []probe.Audio{{Index: index, Default: true}}}}
			speech := reference{label: "alass"}
			defer speech.cleanup()
			if err := r.hear(ctx, ti, &speech); err != nil {
				out[i] = f[0] + "\tspeech failed: " + err.Error() + "\n"
				return
			}
			framecrc, _ := os.ReadFile(f[2])
			duration, _ := strconv.ParseFloat(f[3], 64)
			starts, bits, _ := pgsReference(string(framecrc), duration)
			for _, cand := range f[4:] {
				txt, _ := os.ReadFile(cand)
				// the alass route, scored against the captions it never saw
				a := map[string]any{}
				aligned, result, ok := r.fit(ctx, &speech, string(txt), a)
				med := ""
				if ok {
					med = strconv.FormatFloat(medianOffsetMS(cues(aligned), starts), 'f', 0, 64)
				}
				// the caption route as fetch runs it, the rescue included (it reuses the speech heard above)
				pgs := reference{label: "pgs", bits: bits, starts: starts, dir: speech.dir, speech: speech.speech, talk: speech.talk}
				p := map[string]any{}
				pout, presult, pok := r.fit(ctx, &pgs, string(txt), p)
				if !pok {
					pout, presult, pok = r.rescue(ctx, ti, &pgs, string(txt), p)
				}
				pmed := ""
				if pok {
					pmed = strconv.FormatFloat(medianOffsetMS(cues(pout), starts), 'f', 0, 64)
				}
				out[i] += fmt.Sprintf("%s\t%s\talass\t%v\t%s\t%v\t%v\t%v\tz=%v\tchunks=%v\t%s\tpgs\t%v\t%s\t%v\t%s\n", f[0], filepath.Base(cand),
					ok, result, a["offset_ms"], a["ratio"], a["agree"], a["z"], a["chunks"], med, pok, presult, p["rescue"], pmed)
			}
		}()
	}
	wg.Wait()
	gateWrite(t, "alass.tsv", strings.Join(out, ""))
}

func TestGateSpot(t *testing.T) {
	lines := gateLines(t, "GATE_SPOT")
	h := fake.NewEnv(t, nil) // nobody watching
	h.Env.FFmpeg = config.FFmpeg{Dir: os.Getenv("GATE_FFMPEG_DIR")}
	r := &run{env: h.Env, opt: Options{Alass: os.Getenv("GATE_ALASS_BIN")}, rep: &stage.Report{}, alerted: map[string]bool{}}
	var b strings.Builder
	b.WriteString("id\tcues\tcaption_median_ms\tspeech_ok\tspeech_shift_ms\tspeech_agree\n")
	for _, line := range lines {
		f := strings.Split(line, "\t")
		sub, _ := os.ReadFile(f[2])
		caption := ""
		duration, _ := strconv.ParseFloat(f[4], 64)
		if ref := vobsubReference(f[1], duration); f[3] == "idx" && ref != nil {
			caption = strconv.FormatFloat(medianOffsetMS(cues(string(sub)), ref.starts), 'f', 0, 64)
		} else if f[3] != "-" {
			framecrc, _ := os.ReadFile(f[3])
			if starts, _, ok := pgsReference(string(framecrc), duration); ok {
				caption = strconv.FormatFloat(medianOffsetMS(cues(string(sub)), starts), 'f', 0, 64)
			}
		}
		raw, _ := exec.Command(r.env.FFmpeg.FFprobePath(), "-v", "error", "-select_streams", "a", "-show_entries",
			"stream=index:stream_disposition=default", "-of", "csv=p=0", f[1]).Output()
		audio := strings.Split(strings.Split(string(raw), "\n")[0], ",")[0]
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.HasSuffix(l, ",1") {
				audio = strings.Split(l, ",")[0]
				break
			}
		}
		index, _ := strconv.Atoi(audio)
		ti := &title{path: f[1], info: probe.Info{Audio: []probe.Audio{{Index: index, Default: true}}}}
		speech := reference{label: "alass"}
		a := map[string]any{}
		ok := false
		if err := r.hear(context.Background(), ti, &speech); err == nil {
			_, _, ok = r.fit(context.Background(), &speech, string(sub), a)
		}
		speech.cleanup()
		fmt.Fprintf(&b, "%s\t%d\t%s\t%v\t%v\t%v\n", f[0], len(cues(string(sub))), caption, ok, a["offset_ms"], a["agree"])
	}
	gateWrite(t, "spot.tsv", b.String())
}

// syncRegions are where TestGateSync measures a written sidecar: 3-13 %, 45-55 % and 87-97 % of the runtime.
var syncRegions = [][2]float64{{0.03, 0.13}, {0.45, 0.55}, {0.87, 0.97}}

// TestGateSync measures each written sidecar against a reference the sampled fit never saw whole. "captions": the
// complete English image track, demuxed from the whole file, less the captions inside the windows the fit read (90 s
// from each start given, 1 s either side for the rounding); in each region the offset is the median distance from
// caption starts to their matched cue starts at the region's own best offset. "alass": alass aligns the sidecar to the
// speech of the whole file; in each region the offset is the median shift alass gives its cues (sign: what alass would
// add). A region with fewer than 5 matches is "n/a".
func TestGateSync(t *testing.T) {
	lines := gateLines(t, "GATE_SYNC")
	h := fake.NewEnv(t, nil) // the gate's runner pauses the whole process group for viewers
	h.Env.FFmpeg = config.FFmpeg{Dir: os.Getenv("GATE_FFMPEG_DIR")}
	r := &run{env: h.Env, opt: Options{Alass: os.Getenv("GATE_ALASS_BIN")}, rep: &stage.Report{}, alerted: map[string]bool{}}
	var b strings.Builder
	b.WriteString("id\tkind\tstart_ms\tmiddle_ms\tend_ms\tmatched\tworst_ms\tread_bytes\tsecs\n")
	for _, line := range lines {
		f := strings.Split(line, "\t")
		raw, _ := os.ReadFile(f[2])
		written := cues(string(raw))
		duration, _ := strconv.ParseFloat(f[3], 64)
		kind, stream, _ := strings.Cut(f[4], " ")
		var read []float64 // the fit's window starts
		if len(f) > 5 {
			for _, w := range strings.Split(f[5], ",") {
				start, _ := strconv.ParseFloat(w, 64)
				read = append(read, start)
			}
		}
		ti := &title{path: f[1]}
		began := time.Now()
		var offsets []string
		var counts []string
		worst := 0.0
		region := func(i int, ms float64, n int) {
			if n < 5 {
				offsets, counts = append(offsets, "n/a"), append(counts, strconv.Itoa(n))
				worst = math.Inf(1)
				return
			}
			offsets, counts = append(offsets, strconv.FormatFloat(math.Round(ms), 'f', 0, 64)), append(counts, strconv.Itoa(n))
			worst = math.Max(worst, math.Abs(ms))
		}
		switch kind {
		case "captions":
			out, err := r.cmd(context.Background(), ti, r.env.FFmpeg.FFmpegPath(), "-nostdin", "-hide_banner", "-loglevel", "error", "-i", f[1],
				"-map", "0:"+stream, "-c", "copy", "-f", "framecrc", "-")
			if err != nil {
				t.Errorf("%s: %v", f[0], err)
				continue
			}
			var caps []float64
			tb := 1.0 / 1000
			for _, l := range pySplitLines(string(out)) {
				if strings.HasPrefix(l, "#tb") {
					parts := strings.SplitN(l, ":", 3)
					n, d, _ := strings.Cut(pyStrip(parts[1]), "/")
					tb = float64(pyInt(n)) / float64(pyInt(d))
					continue
				}
				if fs := strings.Split(l, ","); !strings.HasPrefix(l, "#") && len(fs) >= 5 {
					if size, _ := strconv.Atoi(pyStrip(fs[4])); size > pgsMinPkt {
						pts, _ := strconv.ParseInt(pyStrip(fs[2]), 10, 64)
						caps = append(caps, float64(pts)*tb)
					}
				}
			}
			for i, reg := range syncRegions {
				w := sampleWindow{reg[0] * duration, (reg[1] - reg[0]) * duration}
				smp := sample{win: w}
				for _, c := range caps {
					seen := slices.ContainsFunc(read, func(start float64) bool { return c >= start-1 && c < start+91 })
					if c >= w.start && c < w.start+w.dur && !seen {
						smp.captions = append(smp.captions, c)
					}
				}
				n := int(math.Round(2 * sampleShift / frame))
				at, _ := plateau(captionVotes(written, smp, 1, n))
				d := matched(written, smp.captions, 1, binOffset(at))
				region(i, medianOr(d, 0)*1000, len(d))
			}
		case "alass":
			dir := t.TempDir()
			outPath := filepath.Join(dir, "aligned.srt")
			if _, err := r.cmd(context.Background(), ti, r.opt.Alass, f[1], f[2], outPath, "--index", stream); err != nil {
				t.Errorf("%s: %v", f[0], err)
				continue
			}
			alignedRaw, _ := os.ReadFile(outPath)
			aligned := cues(string(alignedRaw))
			if len(aligned) != len(written) {
				t.Errorf("%s: alass wrote %d cues for %d", f[0], len(aligned), len(written))
				continue
			}
			for i, reg := range syncRegions {
				var d []float64
				for k, c := range written {
					if c.start >= reg[0]*duration && c.start < reg[1]*duration {
						d = append(d, aligned[k].start-c.start)
					}
				}
				region(i, medianOr(d, 0)*1000, len(d))
			}
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%.0f\n", f[0], kind, offsets[0], offsets[1], offsets[2], strings.Join(counts, "/"),
			strconv.FormatFloat(math.Round(worst), 'f', 0, 64), ti.disk, time.Since(began).Seconds())
		gateWrite(t, "sync.tsv", b.String())
	}
}

// TestGateFit runs the sampled fit on a given candidate: the reads and the fit exactly as the stage's route c does them.
// Consecutive lines for one video share its reads, as the stage's candidates for one title do.
func TestGateFit(t *testing.T) {
	lines := gateLines(t, "GATE_FIT")
	h := fake.NewEnv(t, nil)
	h.Env.FFmpeg = config.FFmpeg{Dir: os.Getenv("GATE_FFMPEG_DIR")}
	prober, err := probe.NewProber(h.Env.FFmpeg.FFprobePath(), h.Env.Store.DB())
	if err != nil {
		t.Fatal(err)
	}
	r := &run{env: h.Env, opt: Options{SampleWindows: 4, SampleLength: 90 * time.Second}, rep: &stage.Report{}, alerted: map[string]bool{}}
	os.MkdirAll(filepath.Join(os.Getenv("GATE_OUT"), "fit"), 0o755)
	var b strings.Builder
	b.WriteString("id\treference\tratio\toffset_ms\twindow_offsets_ms\tevidence\trefused\tread_bytes\tunseen_lines\n")
	var smp *sampler
	var ti *title
	var why string
	for _, line := range lines {
		f := strings.Split(line, "\t")
		if ti == nil || ti.path != f[1] {
			info, err := prober.Probe(context.Background(), f[1])
			if err != nil {
				t.Errorf("%s: %v", f[0], err)
				ti = nil
				continue
			}
			ti = &title{path: f[1], info: info}
			if smp, why = r.newSampler(ti); smp != nil {
				why, err = smp.readAll(context.Background())
				if err != nil {
					why += err.Error()
				}
			}
		}
		if smp == nil {
			fmt.Fprintf(&b, "%s\t\t\t\t\t\t%s\t0\n", f[0], why)
			continue
		}
		if why != "" {
			fmt.Fprintf(&b, "%s\t%s\t\t\t\t\t%s\t%d\n", f[0], smp.label(), why, ti.disk)
			continue
		}
		raw, _ := os.ReadFile(f[2])
		lo, hi := smp.span()
		fit := fitSamples(cues(string(raw)), smp.samples, lo, hi)
		fmt.Fprintf(&b, "%s\t%s\t%.6f\t%.0f\t%v\t%v\t%s\t%d\t%d\n", f[0], smp.label(), fit.Ratio, fit.Offset*1000, rounded(fit.Offsets, 1000, 1),
			rounded(fit.Evidence, 10, 0.1), fit.Refused, ti.disk, fit.Unseen)
		if fit.Refused == "" {
			gateWrite(t, filepath.Join("fit", f[0]+".srt"), retime(string(raw), fit.Ratio, fit.Offset))
		}
	}
	gateWrite(t, "fit.tsv", b.String())
}

// TestGateLocal reads ten speech windows across the whole runtime and reports, per window, the offset a written
// sidecar's cue starts would need to land on the speech onsets there (0 = in sync) and how far that peak stands out.
func TestGateLocal(t *testing.T) {
	lines := gateLines(t, "GATE_LOCAL")
	h := fake.NewEnv(t, nil)
	h.Env.FFmpeg = config.FFmpeg{Dir: os.Getenv("GATE_FFMPEG_DIR")}
	prober, err := probe.NewProber(h.Env.FFmpeg.FFprobePath(), h.Env.Store.DB())
	if err != nil {
		t.Fatal(err)
	}
	r := &run{env: h.Env, opt: Options{SampleWindows: 4, SampleLength: 90 * time.Second}, rep: &stage.Report{}, alerted: map[string]bool{}}
	var b strings.Builder
	b.WriteString("id\tposition\toffset_ms\tz\n")
	for _, line := range lines {
		f := strings.Split(line, "\t")
		info, err := prober.Probe(context.Background(), f[1])
		if err != nil {
			t.Errorf("%s: %v", f[0], err)
			continue
		}
		info.Subtitles = nil // the speech, whatever tracks the file has
		smp, why := r.newSampler(&title{path: f[1], info: info})
		if smp == nil {
			t.Errorf("%s: %s", f[0], why)
			continue
		}
		var wins []sampleWindow
		for q := 0.05; q < 1; q += 0.1 {
			wins = append(wins, sampleWindow{q*info.Duration - 45, 90})
		}
		got, why, err := smp.readWindows(context.Background(), wins)
		if err != nil || why != "" {
			t.Errorf("%s: %s %v", f[0], why, err)
			continue
		}
		raw, _ := os.ReadFile(f[2])
		spans := cues(string(raw))
		n := int(math.Round(2 * sampleShift / frame))
		for i, s := range got {
			scores := onsetScores(spans, s, 1, n)
			own, _ := plateau(scores)
			o := refine(spans, []sample{s}, 1, binOffset(own))
			fmt.Fprintf(&b, "%s\t%.0f%%\t%.0f\t%.1f\n", f[0], 100*(0.05+0.1*float64(i)), o*1000, peakZ(scores, own))
		}
	}
	gateWrite(t, "local.tsv", b.String())
}
