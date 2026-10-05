package subtitles

// Port of subfetch2.py's timing code: cues, bits, best_lag, align, retime, sane, and the PGS reference read
// from ffmpeg's framecrc. Every float expression keeps the Python's operation order, and products that are
// added are wrapped in float64() so the compiler cannot fuse them into an FMA (arm64 does by default): the
// numbers must equal the Python's bit for bit, not just closely.

import (
	"math"
	"math/big"
	"math/bits"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	step      = 0.1  // seconds per bin
	maxLag    = 1200 // bins: offsets searched up to +-120 s
	zOK       = 3.5
	chunks    = 8
	near      = 50
	resTol    = 0.5
	minAgree  = 6
	maxSlope  = 0.003
	minCues   = 40
	pgsMinPkt = 64 // bytes; smaller PGS packets are "clear screen" events
)

// Frame-rate ratios tried for the whole file; finer rate errors come out of the per-chunk line fit.
var fpsRatios = []float64{1.0, 25 / 23.976, 23.976 / 25}

// cueRe is the Python's CUE with re's str semantics: \d is any Unicode decimal digit, \s any str.isspace().
var cueRe = regexp.MustCompile(`(` + pyDigit + `{1,2}):(` + pyDigit + `{2}):(` + pyDigit + `{2})[,.](` + pyDigit + `{3})` +
	pySpace + `*-->` + pySpace + `*(` + pyDigit + `{1,2}):(` + pyDigit + `{2}):(` + pyDigit + `{2})[,.](` + pyDigit + `{3})`)

type span struct{ start, end float64 }

// cues returns the (start, end) seconds of every cue.
func cues(txt string) []span {
	var out []span
	for _, m := range cueRe.FindAllStringSubmatch(txt, -1) {
		out = append(out, span{cueSeconds(m[1:5]), cueSeconds(m[5:9])})
	}
	return out
}

func cueSeconds(g []string) float64 {
	return float64(pyInt(g[0])*3600+pyInt(g[1])*60+pyInt(g[2])) + float64(pyInt(g[3]))/1000
}

// sane: at least 40 cues, none ending more than 10 s after the video, together spanning at least 40% of it.
func sane(spans []span, duration float64) bool {
	if len(spans) < minCues {
		return false
	}
	maxEnd, minStart, maxStart := spans[0].end, spans[0].start, spans[0].start
	for _, s := range spans {
		maxEnd, minStart, maxStart = math.Max(maxEnd, s.end), math.Min(minStart, s.start), math.Max(maxStart, s.start)
	}
	return maxEnd <= duration+10 && maxStart-minStart >= float64(0.4*duration)
}

// retime rewrites every cue time t as ratio*t + off (seconds), rounding half to even like Python's round().
func retime(txt string, ratio, off float64) string {
	stamp := func(g []string) string {
		ms := pyInt(g[0])*3600000 + pyInt(g[1])*60000 + pyInt(g[2])*1000 + pyInt(g[3])
		t := int64(math.Max(0, math.RoundToEven(float64(float64(ms)*ratio)+float64(off*1000))))
		return pad(t/3600000, 2) + ":" + pad(t/60000%60, 2) + ":" + pad(t/1000%60, 2) + "," + pad(t%1000, 3)
	}
	var b strings.Builder
	last := 0
	for _, m := range cueRe.FindAllStringSubmatchIndex(txt, -1) {
		g := make([]string, 8)
		for i := range g {
			g[i] = txt[m[2+2*i]:m[3+2*i]]
		}
		b.WriteString(txt[last:m[0]])
		b.WriteString(stamp(g[:4]) + " --> " + stamp(g[4:]))
		last = m[1]
	}
	b.WriteString(txt[last:])
	return b.String()
}

func pad(n int64, width int) string {
	s := strconv.FormatInt(n, 10)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// ---- bitsets of 100 ms bins (bit i = bin i), as the Python's big ints ----

type bitset []uint64

func (b *bitset) setRange(lo, hi int) { // bits [lo, hi)
	for len(*b)*64 < hi {
		*b = append(*b, 0)
	}
	for i := lo; i < hi; i++ {
		(*b)[i/64] |= 1 << (i % 64)
	}
}

// word returns bits p..p+63 of b; bits outside b (including negative positions) are zero.
func (b bitset) word(p int) uint64 {
	q, r := p>>6, uint(p&63) // arithmetic shift: floor division, also for negative p
	at := func(i int) uint64 {
		if i < 0 || i >= len(b) {
			return 0
		}
		return b[i]
	}
	if r == 0 {
		return at(q)
	}
	return at(q)>>r | at(q+1)<<(64-r)
}

// spanBits covers each span's bins, times scaled by ratio; every span covers at least one bin.
func spanBits(spans []span, ratio float64) bitset {
	var x bitset
	for _, s := range spans {
		i := max(0, int(s.start*ratio/step))
		x.setRange(i, max(int(s.end*ratio/step), i+1))
	}
	return x
}

// overlap is popcount((sub << k if k >= 0 else sub >> -k) & ref).
func overlap(sub, ref bitset, k int) int {
	n := 0
	for w := range ref {
		n += bits.OnesCount64(sub.word(w*64-k) & ref[w])
	}
	return n
}

// bestLag is (lag in bins, z-score of its overlap among all lags lo..hi) for sub shifted against ref.
func bestLag(sub, ref bitset, lo, hi int) (int, float64) {
	counts := make([]int64, 0, hi-lo+1)
	best := 0
	for k := lo; k <= hi; k++ {
		counts = append(counts, int64(overlap(sub, ref, k)))
		if counts[len(counts)-1] > counts[best] { // max() keeps the first maximum
			best = len(counts) - 1
		}
	}
	mean, sd := pyMeanPstdevInts(counts)
	if sd == 0 {
		sd = 1e-9
	}
	return lo + best, (float64(counts[best]) - mean) / sd
}

type fit struct {
	Ratio, Offset, Z float64
	Agree            int
}

// align maps cue time t -> ratio*t + off onto ref, or ok=false if unconvincing. The best whole-file lag must
// stand out (z), then 8 chunks of the file each find their own lag near it and at least 6 must sit on one line (a
// cut, an extra scene or another episode scatters them). Already in sync gives (1.0, 0.0).
func align(spans []span, ref bitset) (fit, bool) {
	ratio, lag, z := 0.0, 0, math.Inf(-1)
	for _, r := range fpsRatios {
		if l, rz := bestLag(spanBits(spans, r), ref, -maxLag, maxLag); rz > z {
			ratio, lag, z = r, l, rz
		}
	}
	if z < zOK {
		return fit{}, false
	}
	n := len(spans) / chunks
	type point struct{ t, l float64 }
	pts := make([]point, 0, chunks)
	for i := 0; i < chunks; i++ {
		chunk := spans[i*n:]
		if i < chunks-1 {
			chunk = spans[i*n : (i+1)*n]
		}
		starts := make([]float64, len(chunk))
		for j, s := range chunk {
			starts[j] = s.start
		}
		l, _ := bestLag(spanBits(chunk, ratio), ref, lag-near, lag+near)
		pts = append(pts, point{ratio * pyMeanFloats(starts), step * float64(l)})
	}
	if ratio == 1.0 {
		inSync := 0
		for _, p := range pts {
			if math.Abs(p.l) <= resTol {
				inSync++
			}
		}
		if inSync >= minAgree {
			return fit{1.0, 0.0, z, inSync}, true
		}
	}
	var slopes []float64
	for i, a := range pts {
		for _, b := range pts[i+1:] {
			if b.t > a.t {
				slopes = append(slopes, (b.l-a.l)/(b.t-a.t))
			}
		}
	}
	if len(slopes) == 0 {
		return fit{}, false // the Python raises here (median of nothing); unreachable with >= 40 cues
	}
	slope := pyMedian(slopes)
	offs := make([]float64, len(pts))
	for i, p := range pts {
		offs[i] = p.l - float64(slope*p.t)
	}
	off := pyMedian(offs)
	agree := 0
	for _, p := range pts {
		if math.Abs(p.l-off-float64(slope*p.t)) <= resTol {
			agree++
		}
	}
	if agree >= minAgree && math.Abs(slope) <= maxSlope {
		return fit{ratio * (1 + slope), off, z, agree}, true
	}
	return fit{}, false
}

// pgsReference reads `ffmpeg -map 0:<pgs> -c copy -f framecrc -`: bins within 0.2 s of each caption start, or
// ok=false when the track has fewer than 40 captions or covers less than half of the video.
func pgsReference(framecrc string, duration float64) (bitset, bool) {
	tb := 1.0 / 1000
	seen := map[float64]bool{}
	var ev []float64
	for _, line := range pySplitLines(framecrc) {
		if strings.HasPrefix(line, "#tb") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) > 1 {
				n, d, _ := strings.Cut(pyStrip(parts[1]), "/")
				tb = float64(pyInt(n)) / float64(pyInt(d))
			}
			continue
		}
		if strings.HasPrefix(line, "#") || pyStrip(line) == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		if size, _ := strconv.Atoi(pyStrip(f[4])); size > pgsMinPkt {
			pts, _ := strconv.ParseInt(pyStrip(f[2]), 10, 64)
			if t := float64(pts) * tb; !seen[t] {
				seen[t] = true
				ev = append(ev, t)
			}
		}
	}
	sort.Float64s(ev)
	if len(ev) < minCues || ev[len(ev)-1]-ev[0] < float64(0.5*duration) {
		return nil, false
	}
	spans := make([]span, len(ev))
	for i, t := range ev {
		spans[i] = span{t - 0.2, t + 0.2}
	}
	return spanBits(spans, 1.0), true
}

// ---- Python 3.8 statistics, exactly ----

// pyMeanPstdevInts is statistics.mean and statistics.pstdev of ints. 3.8 computes the deviations from the
// float mean in float arithmetic, sums them exactly, and corrects by (sum of deviations)^2/n.
func pyMeanPstdevInts(xs []int64) (mean, pstdev float64) {
	n := int64(len(xs))
	var sum int64
	for _, x := range xs {
		sum += x
	}
	if sum%n == 0 { // the mean is an int: every deviation is exact
		m := sum / n
		ss := new(big.Int)
		for _, x := range xs {
			d := big.NewInt(x - m)
			ss.Add(ss, d.Mul(d, d))
		}
		v, _ := new(big.Rat).SetFrac(ss, big.NewInt(n)).Float64()
		return float64(m), math.Sqrt(v)
	}
	mean = float64(sum) / float64(n)
	total, total2 := new(big.Rat), new(big.Rat)
	for _, x := range xs {
		d := float64(x) - mean
		total.Add(total, new(big.Rat).SetFloat64(d*d))
		total2.Add(total2, new(big.Rat).SetFloat64(d))
	}
	total2.Mul(total2, total2)
	total.Sub(total, total2.Quo(total2, new(big.Rat).SetInt64(n)))
	v, _ := total.Quo(total, new(big.Rat).SetInt64(n)).Float64()
	return mean, math.Sqrt(v)
}

// pyMeanFloats is statistics.mean of floats: the exact sum, divided, rounded once.
func pyMeanFloats(xs []float64) float64 {
	total := new(big.Rat)
	for _, x := range xs {
		total.Add(total, new(big.Rat).SetFloat64(x))
	}
	m, _ := total.Quo(total, new(big.Rat).SetInt64(int64(len(xs)))).Float64()
	return m
}

func pyMedian(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
