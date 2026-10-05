package subtitles

// The sampled fit: a downloaded subtitle's timing checked, and corrected, against a few short stretches of the video
// rather than the whole file. Each window gives a reference: the caption starts of an embedded English image track
// (packet times only, never the images), or else the level of the voice band in the first audio track (the centre
// channel where there is one). The subtitle is fitted to all windows at once as t' = ratio*t + offset, ratio one of
// the usual frame-rate changes:
//
//	the video  [3 %...........................................................97 %] of its runtime
//	               |w1|           |w2|           |w3|           |w4|          K windows, evenly spread
//	per ratio  every window scores every offset; the windows' sum picks the ratio and the offset
//	written    only when every window has evidence at that offset, the windows' own best offsets agree, and the chosen
//	           ratio beats every other ratio by a margin
//
// A subtitle of another cut puts its windows on different offsets, one timed for another frame rate drifts between
// them, and another film's matches no window: none is written, and the title goes on to the whole-file alignment
// queue. A window whose reference holds no dialogue (fewer than quietLines captions, or under quietSpeech of its
// frames voiced) cannot judge: it is replaced by the stretch right after it, at most maxExtra times per title.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/probe"
)

// sampleRatios are the frame-rate changes a subtitle may need: none, the PAL speed-up of 23.976 and of 24 fps, and
// 23.976 against 24, each either way.
var sampleRatios = []float64{1, 25 / 23.976, 23.976 / 25, 24 / 23.976, 23.976 / 24, 25.0 / 24, 24.0 / 25}

const (
	sampleEdge  = 0.03  // share of the runtime skipped at each end: logos, recaps, credits
	sampleShift = 120.0 // s: offsets searched either way, as the whole-file fit does
	frame       = 0.01  // s: the level grid and the offset grid
	maxExtra    = 2     // replacement windows per title
	// A caption start matches a cue start within captionTol: subtitle authors start a line a few frames from where the
	// captions do. The window and joint offsets are then the median of the matched pairs, which is exact to a frame.
	captionTol = 0.25
	// Every window's own best offset must lie within agreeSpread of every other's. The windows span about 70 % of the
	// runtime, so a drift the ratio set does not cover that stays inside 0.3 s across them is at most about 0.2 s at
	// the video's first and last 3 %, inside the gate's 0.25 s; anything larger (another cut, another rate) fails.
	agreeSpread = 0.3
	// Caption evidence: at the joint offset a window must match at least minLines caption starts and half of all it
	// holds. By chance a caption has a cue start within 0.25 s about 15 % of the time (one cue every 3-4 s), so six
	// matches in every window at one shared offset do not happen to another film's subtitle.
	minLines   = 6
	quietLines = 8
	// Speech evidence: a window's onset score at the joint offset must stand minZ deviations above its scores over all
	// 24,000 offsets searched. On the calibration windows (11 titles
	// with known timing, 5 variants each, and 154 wrong subtitles: other films and other cuts) no wrong subtitle passed
	// at any threshold from 2.0 up, since its windows peak at different offsets; a correct one's windows with dialogue
	// scored 3.0-8.5, and those without, under 3.
	minZ        = 3.0
	quietSpeech = 0.2
	// The onset score compares the voice band's level in the edgeSpan after a cue's start with the edgeSpan before it
	// (clipped at edgeClip dB): high where cues begin as someone starts talking. It finds the offset; the overlap of
	// cues with voiced frames within refineSpan of it then sets it, since cues lead speech by a variable few frames.
	edgeSpan   = 60
	edgeClip   = 15.0
	refineSpan = 0.5
	// The chosen ratio must score minMargin times the best other ratio. Correct subtitles scored 1.5-4.2 times, other
	// films 1.0-1.3 times (speech); captions separate further.
	minMargin = 1.4
)

// sampleWindow is a stretch of the video: start and length in seconds of its presentation time.
type sampleWindow struct{ start, dur float64 }

// sampleWindowsFor spreads k windows of w seconds evenly between the runtime's first and last 3 %.
func sampleWindowsFor(duration float64, k int, w float64) []sampleWindow {
	lo, hi := sampleEdge*duration, (1-sampleEdge)*duration
	w = math.Min(w, (hi-lo)/float64(k))
	out := make([]sampleWindow, k)
	for i := range out {
		c := lo + (float64(i)+0.5)*(hi-lo)/float64(k)
		out[i] = sampleWindow{c - w/2, w}
	}
	return out
}

// sample is what one window says: the caption starts in it (seconds of presentation time), or the voice band's level
// (dB) and voicing per 10 ms frame from its start.
type sample struct {
	win      sampleWindow
	captions []float64
	level    []float64
	voiced   []bool
}

func (s sample) quiet() bool {
	if s.level == nil {
		return len(s.captions) < quietLines
	}
	n := 0
	for _, v := range s.voiced {
		if v {
			n++
		}
	}
	return float64(n) < quietSpeech*float64(len(s.voiced))
}

// sampledFit is a fit to the samples: the mapping, each window's own best offset and its evidence there (matched
// caption starts, or the z of its onset score), the scores of the chosen ratio and of the best other one, and why it
// is refused ("" when it holds).
type sampledFit struct {
	Ratio, Offset   float64
	Offsets         []float64
	Evidence        []float64
	Score, RunnerUp float64
	Refused         string
}

// fitSamples fits cue spans to samples of one kind.
func fitSamples(spans []span, samples []sample) sampledFit {
	if len(spans) < minCues || len(samples) == 0 {
		return sampledFit{Refused: "fewer than 40 cues"}
	}
	byCaptions := samples[0].level == nil
	n := int(math.Round(2 * sampleShift / frame))
	var fit sampledFit
	var windows [][]float64
	at := 0
	for _, r := range sampleRatios {
		total := make([]float64, n+1)
		var per [][]float64
		for _, s := range samples {
			var f []float64
			if byCaptions {
				f = captionVotes(spans, s, r, n)
			} else {
				f = onsetScores(spans, s, r, n)
			}
			for i, v := range f {
				total[i] += v
			}
			per = append(per, f)
		}
		peak, top := plateau(total)
		switch {
		case windows == nil || top > fit.Score:
			fit.RunnerUp = math.Max(fit.RunnerUp, fit.Score)
			fit.Ratio, fit.Score, windows, at = r, top, per, peak
		default:
			fit.RunnerUp = math.Max(fit.RunnerUp, top)
		}
	}
	fit.Offset = binOffset(at)
	if byCaptions {
		var all []float64
		for _, s := range samples {
			all = append(all, matched(spans, s.captions, fit.Ratio, fit.Offset)...)
		}
		fit.Offset = medianOr(all, fit.Offset)
	}
	for w, f := range windows {
		own, _ := plateau(f)
		s := samples[w]
		if byCaptions {
			fit.Offsets = append(fit.Offsets, medianOr(matched(spans, s.captions, fit.Ratio, binOffset(own)), binOffset(own)))
			fit.Evidence = append(fit.Evidence, float64(len(matched(spans, s.captions, fit.Ratio, fit.Offset))))
			continue
		}
		fit.Offsets = append(fit.Offsets, binOffset(own))
		fit.Evidence = append(fit.Evidence, peakZ(f, at))
	}
	if !byCaptions {
		fit.Offset = refine(spans, samples, fit.Ratio, fit.Offset)
	}
	for w, ev := range fit.Evidence {
		if byCaptions && (ev < minLines || 2*ev < float64(len(samples[w].captions))) || !byCaptions && ev < minZ {
			fit.Refused = fmt.Sprintf("window %d lacks evidence", w+1)
			return fit
		}
	}
	if slices.Max(fit.Offsets)-slices.Min(fit.Offsets) > agreeSpread {
		fit.Refused = "the windows disagree on the offset"
	} else if fit.Score < minMargin*fit.RunnerUp {
		fit.Refused = "another frame-rate ratio fits almost as well"
	}
	return fit
}

func binOffset(bin int) float64 { return float64(bin)*frame - sampleShift }

// plateau is the middle of f's highest plateau and its height.
func plateau(f []float64) (int, float64) {
	top := slices.Max(f)
	lo := slices.Index(f, top)
	hi := lo
	for hi+1 < len(f) && f[hi+1] == top {
		hi++
	}
	return (lo + hi) / 2, top
}

func medianOr(xs []float64, or float64) float64 {
	if len(xs) == 0 {
		return or
	}
	return pyMedian(xs)
}

// captionVotes is, per offset bin, how many of the window's caption starts have a cue start mapped within captionTol.
func captionVotes(spans []span, s sample, ratio float64, n int) []float64 {
	votes := make([]float64, n+1)
	tol := int(math.Round(captionTol / frame))
	for _, c := range s.captions {
		last := -1                             // a caption counts once per bin, however many cue starts land near it
		for i := len(spans) - 1; i >= 0; i-- { // latest cue first: its offset is the smallest
			d := c - ratio*spans[i].start
			if d < -sampleShift-captionTol || d > sampleShift+captionTol {
				continue
			}
			mid := int(math.Round((d + sampleShift) / frame))
			for b := max(mid-tol, last+1, 0); b <= min(mid+tol, n); b++ {
				votes[b]++
				last = b
			}
		}
	}
	return votes
}

// matched are the offsets that would put each caption exactly on its nearest mapped cue start, for the captions that
// have one within captionTol at offset o.
func matched(spans []span, captions []float64, ratio, o float64) []float64 {
	starts := make([]float64, len(spans))
	for i, s := range spans {
		starts[i] = ratio*s.start + o
	}
	sort.Float64s(starts)
	var out []float64
	for _, c := range captions {
		i := sort.SearchFloat64s(starts, c)
		best := math.Inf(1)
		for _, j := range []int{i - 1, i} {
			if j >= 0 && j < len(starts) && math.Abs(c-starts[j]) < math.Abs(best) {
				best = c - starts[j]
			}
		}
		if math.Abs(best) <= captionTol {
			out = append(out, o+best)
		}
	}
	return out
}

// onsetScores is, per offset bin, the sum over the window's mapped cue starts of how much louder the voice band is in
// the edgeSpan after the start than in the edgeSpan before it.
func onsetScores(spans []span, s sample, ratio float64, n int) []float64 {
	prefix := make([]float64, len(s.level)+1)
	for i, v := range s.level {
		prefix[i+1] = prefix[i] + v
	}
	edge := make([]float64, len(s.level)) // the score of a cue starting at each frame
	for i := edgeSpan; i+edgeSpan <= len(s.level); i++ {
		rise := (prefix[i+edgeSpan] - 2*prefix[i] + prefix[i-edgeSpan]) / edgeSpan
		edge[i] = math.Max(-edgeClip, math.Min(edgeClip, rise))
	}
	out := make([]float64, n+1)
	for _, c := range spans {
		first := int(math.Round((ratio*c.start - s.win.start - sampleShift) / frame)) // its frame at bin 0
		for b := max(0, -first); b <= n && first+b < len(edge); b++ {
			out[b] += edge[first+b]
		}
	}
	return out
}

// refine moves the speech fit's offset, within refineSpan, to where the cues cover the most voiced frames and the
// fewest unvoiced ones across all windows (the middle of the best plateau).
func refine(spans []span, samples []sample, ratio, o float64) float64 {
	steps := int(math.Round(refineSpan / frame))
	scores := make([]float64, 2*steps+1)
	for k := range scores {
		d := float64(k-steps) * frame
		for _, s := range samples {
			for _, c := range spans {
				a := int(math.Round((ratio*c.start + o + d - s.win.start) / frame))
				b := int(math.Round((ratio*c.end + o + d - s.win.start) / frame))
				for f := max(a, 0); f < min(b, len(s.voiced)); f++ {
					if s.voiced[f] {
						scores[k]++
					} else {
						scores[k]--
					}
				}
			}
		}
	}
	best, _ := plateau(scores)
	return o + float64(best-steps)*frame
}

// peakZ is how many standard deviations f[bin] stands above f's mean.
func peakZ(f []float64, bin int) float64 {
	var sum, sq float64
	for _, v := range f {
		sum += v
	}
	mean := sum / float64(len(f))
	for _, v := range f {
		sq += (v - mean) * (v - mean)
	}
	if sd := math.Sqrt(sq / float64(len(f))); sd > 0 {
		return (f[bin] - mean) / sd
	}
	return 0
}

// ---- reading the windows ----

// sampler reads a title's windows: from its English image track when it has one, else from its first audio track.
type sampler struct {
	r       *run
	t       *title
	track   *probe.Subtitle // nil: the audio
	audio   probe.Audio
	windows []sampleWindow
	samples []sample
	start   *float64 // the file's start time: ffprobe's intervals are in its timestamps, not presentation time
	read    int64    // bytes the helpers read (rchar)
	disk    int64    // of which from the disk (read_bytes)
	started time.Time
}

// newSampler is nil, with why, for a title the sampled fit cannot serve cheaply.
func (r *run) newSampler(t *title) (*sampler, string) {
	info := t.info
	switch {
	case info.Duration < 10*60:
		return nil, "shorter than 10 minutes"
	case strings.EqualFold(filepath.Ext(t.path), ".avi") && !aviIndexed(t.path):
		return nil, "an AVI without an index: every seek would read from the start"
	}
	s := &sampler{r: r, t: t, started: r.env.Now(),
		windows: sampleWindowsFor(info.Duration, r.opt.SampleWindows, r.opt.SampleLength.Seconds())}
	if images := imageTracks(info); len(images) > 0 {
		s.track = &images[0]
		return s, ""
	}
	if len(info.Audio) == 0 {
		return nil, "no English image track and no audio"
	}
	s.audio = info.Audio[0]
	return s, ""
}

// readAll reads every window, then for each that holds no dialogue the stretch next to it instead, at most maxExtra
// in all. It stops, with why, when a read costs far more than its share of the file (no usable index).
func (s *sampler) readAll(ctx context.Context) (string, error) {
	got, why, err := s.readWindows(ctx, s.windows)
	for extra := 0; why == "" && err == nil; {
		var quiet []int
		for i, smp := range got {
			if smp.quiet() {
				quiet = append(quiet, i)
			}
		}
		switch {
		case len(quiet) == 0:
			s.samples = got
			return "", nil
		case extra+len(quiet) > maxExtra:
			return fmt.Sprintf("window %d holds no dialogue", quiet[0]+1), nil
		}
		extra += len(quiet)
		var next []sampleWindow
		for _, i := range quiet {
			w := got[i].win
			if w.start = w.start + w.dur; w.start+w.dur > (1-sampleEdge)*s.t.info.Duration {
				w.start -= 2 * w.dur // the last window's neighbour lies before it
			}
			next = append(next, w)
		}
		var more []sample
		if more, why, err = s.readWindows(ctx, next); why == "" && err == nil {
			for k, i := range quiet {
				got[i] = more[k]
			}
		}
	}
	return why, err
}

// readWindows reads windows from the image track (one ffprobe over all of them) or the audio (one ffmpeg each).
func (s *sampler) readWindows(ctx context.Context, wins []sampleWindow) ([]sample, string, error) {
	budget := func(n int) int64 { // a read's fair share of the file, with slack for the seek to a keyframe
		share := float64(s.t.info.Size) * float64(n) * wins[0].dur / s.t.info.Duration
		return int64(4*share) + 64<<20
	}
	if s.track != nil {
		if s.start == nil {
			out, _, err := s.helper(ctx, s.r.env.FFmpeg.FFprobePath(), "-v", "error", "-show_entries", "format=start_time",
				"-of", "default=nw=1:nk=1", s.t.path)
			if err != nil {
				return nil, "", err
			}
			start, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64) // "N/A": 0
			s.start = &start
		}
		var ivals []string
		for _, w := range wins {
			ivals = append(ivals, fmt.Sprintf("%.3f%%+%.3f", *s.start+w.start, w.dur))
		}
		out, io, err := s.helper(ctx, s.r.env.FFmpeg.FFprobePath(), "-v", "error", "-select_streams", strconv.Itoa(s.track.Index),
			"-read_intervals", strings.Join(ivals, ","), "-show_entries", "packet=pts_time,size", "-of", "json", s.t.path)
		if err != nil {
			return nil, "", err
		}
		if io > budget(len(wins)) {
			return nil, fmt.Sprintf("reading the windows took %d bytes, past %d: no usable index", io, budget(len(wins))), nil
		}
		starts, err := captionPackets(out, s.track.Codec, *s.start)
		if err != nil {
			return nil, "", err
		}
		got := make([]sample, len(wins))
		for i, w := range wins {
			got[i].win = w
			for _, t := range starts {
				if t >= w.start && t < w.start+w.dur {
					got[i].captions = append(got[i].captions, t)
				}
			}
		}
		return got, "", nil
	}
	var got []sample
	for _, w := range wins {
		pcm, io, err := s.helper(ctx, s.r.env.FFmpeg.FFmpegPath(), "-nostdin", "-hide_banner", "-loglevel", "error",
			"-ss", fmt.Sprintf("%.3f", w.start), "-t", fmt.Sprintf("%.3f", w.dur), "-i", s.t.path,
			"-map", fmt.Sprintf("0:%d", s.audio.Index), dialogue(s.audio)[0], dialogue(s.audio)[1], "-ar", "16000", "-f", "s16le", "-")
		if err != nil {
			return nil, "", err
		}
		if io > budget(1) {
			return nil, fmt.Sprintf("reading a window took %d bytes, past %d: no usable index", io, budget(1)), nil
		}
		level := voiceLevel(pcm)
		got = append(got, sample{win: w, level: level, voiced: voiced(level)})
	}
	return got, "", nil
}

// label names the reference for the history: "captions" or "speech".
func (s *sampler) label() string {
	if s.track != nil {
		return "captions"
	}
	return "speech"
}

// starts are the windows' starts in whole seconds, replacements included.
func (s *sampler) starts() []float64 {
	var out []float64
	for _, smp := range s.samples {
		out = append(out, math.Round(smp.win.start))
	}
	return out
}

// helper runs one reading helper on the video and returns its stdout and the bytes it read.
func (s *sampler) helper(ctx context.Context, bin string, args ...string) ([]byte, int64, error) {
	before := s.t.read
	out, err := s.r.cmd(ctx, s.t, bin, args...)
	s.read += s.t.read - before
	s.disk = s.t.disk
	return out, s.t.read - before, err
}

// dialogue is the output option that keeps where films put their dialogue: the centre channel when the layout has
// one, else a mono mix.
func dialogue(a probe.Audio) [2]string {
	if centred[a.Layout] {
		return [2]string{"-af", "pan=mono|c0=FC"}
	}
	return [2]string{"-ac", "1"}
}

var centred = map[string]bool{"3.0": true, "3.1": true, "4.0": true, "4.1": true, "5.0": true, "5.0(side)": true, "5.1": true,
	"5.1(side)": true, "6.0": true, "6.1": true, "6.1(back)": true, "7.0": true, "7.0(front)": true, "7.1": true, "7.1(wide)": true,
	"7.1(wide-side)": true, "7.1(top)": true, "hexagonal": true, "octagonal": true, "5.1.2": true, "5.1.4": true, "7.1.2": true,
	"7.1.4": true}

// captionPackets are the display starts in ffprobe's JSON for an image track, in presentation time (less the file's
// start): for PGS the packets that draw (larger than a "clear screen" one), for VobSub every packet.
func captionPackets(raw []byte, codec string, start float64) ([]float64, error) {
	var out struct {
		Packets []struct {
			PTS  string `json:"pts_time"`
			Size string `json:"size"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ffprobe packets: %w", err)
	}
	var starts []float64
	for _, p := range out.Packets {
		size, _ := strconv.Atoi(p.Size)
		t, err := strconv.ParseFloat(p.PTS, 64)
		if err != nil || codec == "hdmv_pgs_subtitle" && size <= pgsMinPkt {
			continue
		}
		starts = append(starts, t-start)
	}
	sort.Float64s(starts)
	return slices.Compact(starts), nil
}

// voiceLevel is the voice band's (300-3000 Hz) level in dB per 10 ms frame of 16 kHz mono s16le PCM.
func voiceLevel(pcm []byte) []float64 {
	const rate, hop = 16000, 160
	x := make([]float64, len(pcm)/2)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
	}
	x = biquad(biquad(x, highPass(300, rate)), lowPass(3000, rate))
	level := make([]float64, len(x)/hop)
	for f := range level {
		var e float64
		for _, v := range x[f*hop : (f+1)*hop] {
			e += v * v
		}
		level[f] = 10 * math.Log10(e/hop+1)
	}
	return level
}

// voiced marks the frames that stand vadAbove dB over the window's quiet floor (its 20th percentile level), with
// pauses under 200 ms bridged and bursts under 100 ms dropped. Loud music counts too; the fit needs only most speech.
func voiced(level []float64) []bool {
	if len(level) == 0 {
		return nil
	}
	sorted := slices.Clone(level)
	slices.Sort(sorted)
	floor := sorted[len(sorted)/5]
	out := make([]bool, len(level))
	for f, v := range level {
		out[f] = v > floor+vadAbove
	}
	fill(out, false, 20)
	fill(out, true, 10)
	return out
}

const vadAbove = 12.0

// fill flips every run of value shorter than frames that has the other value on both sides.
func fill(b []bool, value bool, frames int) {
	for i := 0; i < len(b); {
		if b[i] != value {
			i++
			continue
		}
		j := i
		for j < len(b) && b[j] == value {
			j++
		}
		if i > 0 && j < len(b) && j-i < frames {
			for k := i; k < j; k++ {
				b[k] = !value
			}
		}
		i = j
	}
}

// biquad filters x with RBJ cookbook coefficients {b0, b1, b2, a1, a2}, a0 normalised to 1.
func biquad(x []float64, c [5]float64) []float64 {
	y := make([]float64, len(x))
	var x1, x2, y1, y2 float64
	for i, v := range x {
		out := c[0]*v + c[1]*x1 + c[2]*x2 - c[3]*y1 - c[4]*y2
		x2, x1, y2, y1 = x1, v, y1, out
		y[i] = out
	}
	return y
}

func highPass(f, rate float64) [5]float64 {
	w := 2 * math.Pi * f / rate
	alpha, cw := math.Sin(w)/math.Sqrt2, math.Cos(w) // Q = 1/sqrt(2)
	a0 := 1 + alpha
	return [5]float64{(1 + cw) / 2 / a0, -(1 + cw) / a0, (1 + cw) / 2 / a0, -2 * cw / a0, (1 - alpha) / a0}
}

func lowPass(f, rate float64) [5]float64 {
	w := 2 * math.Pi * f / rate
	alpha, cw := math.Sin(w)/math.Sqrt2, math.Cos(w)
	a0 := 1 + alpha
	return [5]float64{(1 - cw) / 2 / a0, (1 - cw) / a0, (1 - cw) / 2 / a0, -2 * cw / a0, (1 - alpha) / a0}
}

// aviIndexed tells whether an AVI carries an index (an idx1 chunk after its movi list, or an OpenDML indx in its
// header), reading only chunk headers.
func aviIndexed(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil || string(head[:4]) != "RIFF" || string(head[8:12]) != "AVI " {
		return false
	}
	for pos := int64(12); ; {
		hdr := make([]byte, 12)
		n, _ := f.ReadAt(hdr, pos)
		if n < 8 {
			return false
		}
		id, size := string(hdr[:4]), int64(binary.LittleEndian.Uint32(hdr[4:8]))
		switch {
		case id == "idx1":
			return true
		case id == "LIST" && n == 12 && string(hdr[8:12]) == "hdrl":
			list := make([]byte, min(size, 1<<20))
			if _, err := f.ReadAt(list, pos+8); err == nil && bytes.Contains(list, []byte("indx")) {
				return true
			}
		}
		pos += 8 + size + size%2
	}
}
