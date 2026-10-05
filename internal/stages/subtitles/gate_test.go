//go:build gate

package subtitles

// Gate harness, not a test (build tag "gate"): replays real NAS files through the ported functions and writes what
// the Python scripts write, for a byte comparison with their output. Build it for the NAS with
//
//	GOOS=linux GOARCH=amd64 go test -c -tags gate -o subtitles-gate.test ./internal/stages/subtitles
//
// and run it with GATE_OUT=<dir> plus any of: GATE_ROOTS (folders, one per line) -> stats.tsv as sub_stats.py prints
// it; GATE_VIDEOS (files, one per line) -> hashes.tsv; GATE_PAIRS (pairs.tsv: id, framecrc file, duration, candidate
// file) -> fits.tsv; GATE_ALASS (id, video, framecrc, duration, candidate files...) with GATE_ALASS_BIN and
// GATE_FFMPEG_DIR -> alass.tsv: the stage's alass route per candidate, scored against the PGS captions it never saw.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"projectionist/internal/config"
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

func TestGateAlass(t *testing.T) {
	lines := gateLines(t, "GATE_ALASS")
	r := &run{env: &stage.Env{FFmpeg: config.FFmpeg{Dir: os.Getenv("GATE_FFMPEG_DIR")}}, opt: Options{Alass: os.Getenv("GATE_ALASS_BIN")}}
	out := make([]string, len(lines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, line := range lines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
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
			dir := t.TempDir()
			ref := reference{label: "alass", dir: dir, speech: filepath.Join(dir, "speech.srt")}
			if _, err := r.cmd(context.Background(), r.opt.Alass, f[1], "_", ref.speech, "--index", audio); err != nil {
				out[i] = f[0] + "\tspeech failed: " + err.Error() + "\n"
				return
			}
			framecrc, _ := os.ReadFile(f[2])
			duration, _ := strconv.ParseFloat(f[3], 64)
			starts, _, _ := pgsReference(string(framecrc), duration)
			for _, cand := range f[4:] {
				txt, _ := os.ReadFile(cand)
				attempt := map[string]any{}
				aligned, result, ok := r.fit(context.Background(), ref, string(txt), attempt)
				med, check := "", ""
				if ok {
					med = strconv.FormatFloat(medianOffsetMS(cues(aligned), starts), 'f', 0, 64)
				}
				if aligned == "" { // what alass made of it, even when rejected
					b, _ := os.ReadFile(filepath.Join(dir, "aligned.srt"))
					aligned = string(b)
				}
				speech, _ := os.ReadFile(ref.speech)
				var first []span // the Python's speech test: each cue's first second against the speech segments
				for _, c := range cues(aligned) {
					first = append(first, span{c.start, min(c.end, c.start+1)})
				}
				if fit, fok := align(first, spanBits(cues(string(speech)), 1)); len(first) >= minCues {
					check = fmt.Sprintf("%v z=%.1f agree=%d ratio=%.5f off=%.2f", fok, fit.Z, fit.Agree, fit.Ratio, fit.Offset)
				}
				var raw []span // the same test on the candidate as downloaded
				for _, c := range cues(string(txt)) {
					raw = append(raw, span{c.start, min(c.end, c.start+1)})
				}
				asIs := ""
				if fit, fok := align(raw, spanBits(cues(string(speech)), 1)); len(raw) >= minCues {
					asIs = fmt.Sprintf("as-is %v z=%.1f agree=%d ratio=%.5f off=%.2f", fok, fit.Z, fit.Agree, fit.Ratio, fit.Offset)
				}
				out[i] += fmt.Sprintf("%s\t%s\t%v\t%s\t%v\t%v\t%v\t%s\t%s\t%s\n", f[0], filepath.Base(cand), ok, result,
					attempt["offset_ms"], attempt["ratio"], attempt["agree"], med, check, asIs)
			}
		}()
	}
	wg.Wait()
	gateWrite(t, "alass.tsv", strings.Join(out, ""))
}
