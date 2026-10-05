package subtitles

// The cheap route: a subtitle made for this very release is written without reading the video. What the file is
// comes from the probe cache or its .details note, and which release it was from the note's original names (or the
// rename logs); what a candidate is comes from its release text. Nothing here opens the video.

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// note is what a .details note says about its video; the details stage writes it from ffprobe and the rename logs.
type note struct {
	names        []string // the Original name and Original folder lines
	durLo, durHi float64  // seconds; the Container line gives whole minutes, so the duration lies in [lo, lo+60)
	fps          float64
	audio, subs  []string // items of the Audio and Subtitles lines, e.g. "eng AAC 5.1", "eng hdmv_pgs_subtitle forced"
}

var (
	noteFPSRe = regexp.MustCompile(`([0-9.]+) fps`)
	noteDurRe = regexp.MustCompile(`(\d+)h (\d+)m`)
)

func readNote(video string) (note, bool) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(video), ".details", filepath.Base(video)+".txt"))
	if err != nil {
		return note{}, false
	}
	var n note
	for _, m := range originalRe.FindAllStringSubmatch(string(raw), -1) {
		n.names = append(n.names, m[1])
	}
	for _, line := range strings.Split(string(raw), "\n") {
		label, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch label {
		case "Video":
			if m := noteFPSRe.FindStringSubmatch(value); m != nil {
				n.fps, _ = strconv.ParseFloat(m[1], 64)
			}
		case "Container":
			if m := noteDurRe.FindStringSubmatch(value); m != nil {
				h, _ := strconv.Atoi(m[1])
				mins, _ := strconv.Atoi(m[2])
				n.durLo = float64(h*3600 + mins*60)
				n.durHi = n.durLo + 60
			}
		case "Audio":
			n.audio = strings.Split(value, "; ")
		case "Subtitles":
			n.subs = strings.Split(value, "; ")
		}
	}
	return n, true
}

// noteTrack tells whether an Audio or Subtitles item of a note is an English stream of one of the codecs.
func noteTrack(item string, codecs ...string) bool {
	f := strings.Fields(item)
	return len(f) >= 2 && english(f[0]) && slices.Contains(codecs, f[1]) && !slices.Contains(f, "forced")
}

// release is what a release name says about its encode, in lower case; "" where it says nothing.
type release struct {
	source, res, service, editions, sxe string
	tokens                              map[string]bool
}

var (
	tokenClasses = map[string][2]string{
		"bluray": {"source", "bluray"}, "blu": {"source", "bluray"}, "bdrip": {"source", "bluray"}, "brrip": {"source", "bluray"},
		"bd": {"source", "bluray"}, "web": {"source", "web"}, "webdl": {"source", "web"}, "webrip": {"source", "web"},
		"hdtv": {"source", "hdtv"}, "pdtv": {"source", "hdtv"}, "dvdrip": {"source", "dvd"}, "dvd": {"source", "dvd"},
		"480p": {"res", "480p"}, "576p": {"res", "576p"}, "720p": {"res", "720p"}, "1080p": {"res", "1080p"},
		"2160p": {"res", "2160p"}, "4k": {"res", "2160p"}, "uhd": {"res", "2160p"},
		"amzn": {"service", "amzn"}, "nf": {"service", "nf"}, "dsnp": {"service", "dsnp"}, "hmax": {"service", "hmax"},
		"atvp": {"service", "atvp"}, "hulu": {"service", "hulu"}, "pcok": {"service", "pcok"}, "pmtp": {"service", "pmtp"},
		"stan": {"service", "stan"},
	}
	editionTokens = map[string]string{"extended": "extended", "unrated": "unrated", "uncut": "uncut", "theatrical": "theatrical",
		"imax": "imax", "dc": "dc", "directors": "dc", "director": "dc", "redux": "redux"}
	sxeRe = regexp.MustCompile(`\bs(\d{1,2})e(\d{1,3})|\b(\d{1,2})x(\d{2,3})\b`) // S01E02, 1x02
)

func parseRelease(text string) release {
	low := pyLower(text)
	r := release{tokens: map[string]bool{}}
	var editions []string
	toks := tokenRe.FindAllString(low, -1)
	for i, tok := range toks {
		r.tokens[tok] = true
		if c, ok := tokenClasses[tok]; ok {
			switch c[0] {
			case "source":
				r.source = c[1]
			case "res":
				r.res = c[1]
			case "service":
				r.service = c[1]
			}
		}
		e, ok := editionTokens[tok]
		if !ok && (tok == "cut" || tok == "edition") && i > 0 && toks[i-1] != "s" && editionTokens[toks[i-1]] == "" {
			e, ok = toks[i-1], true // Final Cut, Special Edition, Ultimate Cut, Rogue Cut: any named cut is an edition
		}
		if ok && !slices.Contains(editions, e) {
			editions = append(editions, e)
		}
	}
	slices.Sort(editions)
	r.editions = strings.Join(editions, " ")
	if m := sxeRe.FindStringSubmatch(low); m != nil {
		s, _ := strconv.Atoi(m[1] + m[3])
		e, _ := strconv.Atoi(m[2] + m[4])
		r.sxe = sxe(s, e)
	}
	return r
}

func sxe(season, episode int) string { return "s" + strconv.Itoa(season) + "e" + strconv.Itoa(episode) }

var (
	groupSuffixRe  = regexp.MustCompile(`-([a-z0-9]+)(?:\[[^\]]*\])?$`)      // scene: ...x264-GROUP[tracker]
	groupBracketRe = regexp.MustCompile(`\[([a-z0-9]+)(?:\.[a-z]{2,3})?\]$`) // ... [SITEB.MX], [SkyLake]
	groupParenRe   = regexp.MustCompile(`\(([^()]*)\)$`)                     // re-encoders: (1080p BluRay x265 Silence)
	notGroup       = map[string]bool{"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "avc": true, "av1": true,
		"aac": true, "ac3": true, "eac3": true, "dts": true, "opus": true, "flac": true, "truehd": true, "atmos": true, "hdr": true,
		"sdr": true, "english": true, "french": true, "japanese": true, "swedish": true, "italian": true, "spanish": true,
		"german": true, "korean": true, "hindi": true, "multi": true, "dl": true, "rip": true}
)

// releaseGroup is the group that made a release name ("" when it names none): the scene suffix, a trailing bracket
// tag, or the last word of a re-encoder's trailing parentheses that name a resolution or source.
func releaseGroup(name string) string {
	low := strings.TrimSpace(pyLower(name))
	if videoExt[filepath.Ext(low)] {
		low = strings.TrimSpace(strings.TrimSuffix(low, filepath.Ext(low)))
	}
	group := func(w string) string {
		if tokenRe.FindString(w) != w || !strings.ContainsAny(w, "abcdefghijklmnopqrstuvwxyz") || notGroup[w] ||
			tokenClasses[w] != [2]string{} || strings.HasSuffix(w, "bit") {
			return ""
		}
		return w
	}
	if m := groupSuffixRe.FindStringSubmatch(low); m != nil && group(m[1]) != "" {
		return m[1]
	}
	if m := groupBracketRe.FindStringSubmatch(low); m != nil && group(m[1]) != "" {
		return m[1]
	}
	if m := groupParenRe.FindStringSubmatch(low); m != nil {
		inside := parseRelease(m[1])
		if words := strings.Fields(m[1]); inside.res != "" || inside.source != "" {
			return group(words[len(words)-1])
		}
	}
	return ""
}

// origin is what the title's original release names say: its group and what its first name (else its folder) says.
type origin struct {
	group string
	rel   release
}

func originOf(names []string) origin {
	var o origin
	for _, n := range names {
		if o.group == "" {
			o.group = releaseGroup(n)
		}
		r := parseRelease(n)
		for _, f := range []struct {
			dst *string
			v   string
		}{{&o.rel.source, r.source}, {&o.rel.res, r.res}, {&o.rel.service, r.service},
			{&o.rel.editions, r.editions}, {&o.rel.sxe, r.sxe}} {
			if *f.dst == "" {
				*f.dst = f.v
			}
		}
	}
	return o
}

// releaseMatched tells whether c was made for this very release, so it may be written without reading the video. An
// OpenSubtitles candidate must match the file's hash and be labelled with this title. Any other must be labelled with
// it too (this film, this episode), and one of the releases its text names (Addic7ed lists every release a subtitle
// fits: "WEB.InChY, PSA, iP.WEB-DL.RAWR") must carry the file's group while contradicting neither its source,
// resolution, streaming service, edition nor episode.
func (t *title) releaseMatched(c Candidate) bool {
	if metered(c.provider) {
		return c.HashMatch && c.ours
	}
	if !c.ours || t.origin.group == "" || t.origin.rel.sxe != "" && t.show != "" && t.origin.rel.sxe != sxe(t.season, t.episode) {
		return false
	}
	differ := func(a, b string) bool { return a != "" && b != "" && a != b }
	for _, alt := range append(strings.FieldsFunc(c.Release, func(r rune) bool { return r == ',' || r == '/' }), c.FileName) {
		r := parseRelease(alt)
		if !r.tokens[t.origin.group] || differ(r.source, t.origin.rel.source) || differ(r.res, t.origin.rel.res) ||
			differ(r.service, t.origin.rel.service) || r.editions != t.origin.rel.editions || t.show != "" && differ(r.sxe, sxe(t.season, t.episode)) {
			continue
		}
		return true
	}
	return false
}

// plausible tells whether quick mode may write c unaligned although it was not made for this release: labelled with
// this title (this film, this episode), naming the file's edition and no other, and, for an episode, neither c's names
// nor the file's original name naming another episode.
func (t *title) plausible(c Candidate) bool {
	r, ep := parseRelease(c.Release+" "+c.FileName), sxe(t.season, t.episode)
	return c.ours && r.editions == t.origin.rel.editions &&
		(t.show == "" || (r.sxe == "" || r.sxe == ep) && (t.origin.rel.sxe == "" || t.origin.rel.sxe == ep))
}

// specialSure: TMDB and the subtitle sites number a show's specials (season 0) each their own way, so quick mode
// writes a special's subtitle unaligned only when the file's original name carries the same SxxEyy.
func (t *title) specialSure() bool {
	return t.show == "" || t.season != 0 || t.origin.rel.sxe == sxe(0, t.episode)
}

var idxStampRe = regexp.MustCompile(`^timestamp:\s*(\d+):(\d\d):(\d\d):(\d{3})`)

// vobsubReference is the caption starts of the video's English VobSub sidecar, <stem>.en.idx, read from that text
// alone (the .sub images are never opened): its first English stream with at least minCues captions over half the
// video and none after its end (an .idx left from a longer cut). Nil when there is none. On 2026-10-05 the roots held
// 50 .idx, each a <stem>.en.idx beside its video with "time offset: 0" and no "delay:" line, so the timestamps are
// taken as they are (the NAS's 23 others, untagged or not English, sit in _archive beside no video).
func vobsubReference(video string, duration float64) *reference {
	raw, err := os.ReadFile(strings.TrimSuffix(video, filepath.Ext(video)) + ".en.idx")
	if err != nil {
		return nil
	}
	var streams [][]float64
	english := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if id, ok := strings.CutPrefix(line, "id:"); ok { // "id: en, index: 0" opens a stream
			if english = strings.HasPrefix(strings.TrimSpace(id), "en,"); english {
				streams = append(streams, nil)
			}
		} else if m := idxStampRe.FindStringSubmatch(line); m != nil && english {
			h, _ := strconv.Atoi(m[1])
			mins, _ := strconv.Atoi(m[2])
			sec, _ := strconv.Atoi(m[3])
			ms, _ := strconv.Atoi(m[4])
			streams[len(streams)-1] = append(streams[len(streams)-1], float64(h*3600+mins*60+sec)+float64(ms)/1000)
		}
	}
	for _, ev := range streams {
		if starts, bits, ok := captionReference(ev, duration); ok && starts[len(starts)-1] <= duration {
			return &reference{label: "idx", starts: starts, bits: bits}
		}
	}
	return nil
}

var fpsRe = regexp.MustCompile(`\b(23\.976|23\.98|24|25|29\.97|30|50|59\.94|60) ?fps\b|\b(23\.976|23\.98|29\.97)\b`)

// subFPS is the frame rate a candidate says it was timed for, 0 when it does not say.
func subFPS(c Candidate) float64 {
	if c.FPS > 0 {
		return c.FPS
	}
	m := fpsRe.FindStringSubmatch(pyLower(c.Release + " " + c.FileName))
	if m == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(m[1]+m[2], 64)
	return f
}

// cheapCheck is the test a release-matched subtitle passes before it is written unaligned: at least minCues cues, the
// last ending within the video and no more than 8 % before its end, the cue starts spanning 70 % of it (sane), and a frame rate,
// where the subtitle names one, equal to the video's (23.976 and 23.98 are equal; 24 and 25 are not). The duration
// is [lo, hi] (exact from the probe cache, the minute from a note), so the test must hold for any duration in it.
// It returns why the subtitle fails, "" when it passes.
func cheapCheck(spans []span, lo, hi, videoFPS, fps float64) string {
	switch {
	case hi == 0:
		return "video duration unknown"
	case len(spans) < minCues:
		return "fewer than " + strconv.Itoa(minCues) + " cues"
	case fps > 0 && videoFPS > 0 && math.Abs(fps-videoFPS) > 0.0005*videoFPS:
		return "timed for " + strconv.FormatFloat(fps, 'f', -1, 64) + " fps, the video runs at " + strconv.FormatFloat(videoFPS, 'f', -1, 64)
	}
	last, first, latest := spans[0].end, spans[0].start, spans[0].start
	for _, s := range spans {
		last, first, latest = math.Max(last, s.end), math.Min(first, s.start), math.Max(latest, s.start)
	}
	switch {
	case last > lo:
		return "last cue ends after the video"
	case last < 0.92*hi:
		return "last cue ends more than 8 % before the video's end"
	case latest-first < 0.7*hi:
		return "cue starts span less than 70 % of the video"
	}
	return ""
}
