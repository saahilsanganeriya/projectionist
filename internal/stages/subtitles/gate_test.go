//go:build gate

package subtitles

// Gate harness, not a test (build tag "gate"): replays real NAS files through the ported functions and writes what
// the Python scripts write, for a byte comparison with their output. Build it for the NAS with
//
//	GOOS=linux GOARCH=amd64 go test -c -tags gate -o subtitles-gate.test ./internal/stages/subtitles
//
// and run it with GATE_OUT=<dir> plus any of: GATE_ROOTS (folders, one per line) -> stats.tsv as sub_stats.py prints
// it; GATE_VIDEOS (files, one per line) -> hashes.tsv; GATE_PAIRS (pairs.tsv: id, framecrc file, duration, candidate
// file) -> fits.tsv.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
