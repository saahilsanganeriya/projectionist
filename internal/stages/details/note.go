package details

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"projectionist/internal/probe"
	"projectionist/internal/releasesites"
)

// render is the note's text. The first ten lines match write_details_files.py field for field (so the notes it
// wrote compare line by line); the lines after them are the richer facts, each present only when known.
func render(v video, info probe.Info, orig string, sidecars []string, row *fileRow) string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "%-17s%s\n", label+":", value) }
	vid := info.Video
	src, group := sourceGroup(filepath.Base(orig))

	line("Title", strings.TrimSuffix(filepath.Base(v.path), filepath.Ext(v.path)))
	line("Original name", filepath.Base(orig))
	line("Original folder", filepath.Base(filepath.Dir(orig)))
	line("Resolution", fmt.Sprintf("%dx%d (%s)", vid.Width, vid.Height, resLabel(vid.Width, vid.Height)))
	line("Video", fmt.Sprintf("%s · %d-bit · %s · %s fps · %.1f Mb/s", family(vid.Codec), max(vid.BitDepth, 8), hdrKind(vid), fps(vid.FPS), float64(info.BitRate)/1e6))
	line("Audio", joinOr(mapSlice(info.Audio, audioLine), "none"))
	line("Subtitles", joinOr(append(mapSlice(info.Subtitles, subtitleItem), mapSlice(sidecars, func(s string) string { return s + " (file beside video)" })...), "none"))
	container, _, _ := strings.Cut(info.Container, ",")
	dur := int(info.Duration)
	line("Container", fmt.Sprintf("%s · %.2f GB · %dh %02dm · %d chapters", container, float64(info.Size)/1e9, dur/3600, dur%3600/60, info.Chapters))
	line("Source · group", src+" · "+group)
	line("Identity", v.identity)

	if c := colour(vid); c != "" {
		line("Colour", c)
	}
	if h := hdrMetadata(vid); h != "" {
		line("HDR metadata", h)
	}
	if vid.HDR == probe.DolbyVision {
		el := ""
		if vid.DVEL {
			el = " · enhancement layer"
		}
		line("Dolby Vision", fmt.Sprintf("profile %d · level %d · base layer %s (compatibility id %d)%s", vid.DVProfile, vid.DVLevel, dvBase(vid.DVBLCompat), vid.DVBLCompat, el))
	}
	if e := encoder(vid); e != "" {
		line("Encoder", e)
	}
	for i, a := range info.Audio {
		line(fmt.Sprintf("Audio %d", i+1), audioDetail(a))
	}
	if len(info.Subtitles) > 0 {
		line("Subtitle kinds", subtitleKinds(info.Subtitles))
	}
	if a := attachments(info.Attachments); a != "" {
		line("Attachments", a)
	}
	if t := releaseTags(info); t != "" {
		line("Release tags", t)
	}
	switch {
	case row.PeakKbps.Valid && row.PeakKbps.Int64 == 0:
		line("Peak bitrate", "unknown (the packet scan failed)")
	case row.PeakKbps.Valid:
		line("Peak bitrate", fmt.Sprintf("%.1f Mb/s over 10 s at %s (all streams)", float64(row.PeakKbps.Int64)/1000, clock(int(row.PeakAt.Int64))))
	}
	switch {
	case row.Clean.Valid && row.Clean.Bool:
		line("Streams cleanly", "yes (no zero-filled blocks in 256 probes; 5 s decoded at the start, middle and end)")
	case row.Clean.Valid:
		line("Streams cleanly", "NO: "+row.CleanDetail)
	}
	return b.String()
}

// ffAddress is the pointer ffprobe prints in its messages ("[matroska,webm @ 0x55d1c0a3c2c0]"); it differs per run.
var ffAddress = regexp.MustCompile(` @ 0x[0-9a-f]+`)

// renderUnreadable is the note for a video ffprobe cannot read: what is known without it, and why not more.
func renderUnreadable(v video, orig, reason string) string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "%-17s%s\n", label+":", value) }
	src, group := sourceGroup(filepath.Base(orig))
	line("Title", strings.TrimSuffix(filepath.Base(v.path), filepath.Ext(v.path)))
	line("Original name", filepath.Base(orig))
	line("Original folder", filepath.Base(filepath.Dir(orig)))
	line("Unreadable", clip(ffAddress.ReplaceAllString(strings.ReplaceAll(reason, v.path, "file"), "")))
	line("Size", fmt.Sprintf("%.2f GB", float64(v.size)/1e9))
	line("Source · group", src+" · "+group)
	line("Identity", v.identity)
	return b.String()
}

// ---- the first ten lines, as write_details_files.py writes them ----

func resLabel(w, h int) string {
	switch {
	case w >= 3200 || h >= 2000:
		return "4K"
	case w >= 1800 || h >= 1000:
		return "1080p"
	case w >= 1200 || h >= 700:
		return "720p"
	}
	return "SD"
}

var families = map[string]string{"hevc": "x265 (HEVC)", "h264": "x264 (AVC)", "av1": "AV1", "vp9": "VP9", "mpeg4": "MPEG-4 ASP", "mpeg2video": "MPEG-2", "vc1": "VC-1"}

func family(codec string) string {
	if f, ok := families[codec]; ok {
		return f
	}
	if codec == "" {
		return "?"
	}
	return strings.ToUpper(codec)
}

// hdrKind names the dynamic range. HDR10+ shows only when it is not Dolby Vision (the Dolby Vision line notes it).
func hdrKind(v *probe.Video) string {
	base := "SDR"
	switch v.ColorTransfer {
	case "smpte2084":
		base = "HDR10"
	case "arib-std-b67":
		base = "HLG"
	}
	switch {
	case v.HDR == probe.DolbyVision:
		s := fmt.Sprintf("Dolby Vision profile %d", v.DVProfile)
		if base != "SDR" || v.DVBLCompat != 0 {
			s += " (+" + base + " base)"
		}
		return s
	case v.HDR10Plus:
		return "HDR10+"
	}
	return base
}

// fps prints three decimals without trailing zeros: 23.976, 25, 29.97.
func fps(f float64) string {
	if f == 0 {
		return "?"
	}
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(f, 'f', 3, 64), "0"), ".")
}

var audioNames = map[string]string{"eac3": "E-AC-3", "ac3": "AC-3", "truehd": "TrueHD", "dts": "DTS", "aac": "AAC", "flac": "FLAC", "opus": "Opus", "mp3": "MP3"}

func audioName(a probe.Audio) string {
	codec, ok := audioNames[a.Codec]
	switch {
	case !ok && a.Codec == "":
		codec = "?"
	case !ok:
		codec = strings.ToUpper(a.Codec)
	}
	switch {
	case strings.Contains(a.Profile, "Atmos") || strings.Contains(a.Profile, "JOC"):
		return codec + " Atmos"
	case strings.Contains(a.Profile, "DTS:X"): // "DTS-HD MA + DTS:X": test before the MA case below
		return "DTS:X"
	case strings.Contains(a.Profile, "DTS-HD MA"):
		return "DTS-HD MA"
	}
	return codec
}

func layout(a probe.Audio) string {
	if a.Layout != "" {
		return a.Layout
	}
	return strconv.Itoa(a.Channels) + "ch"
}

func audioLine(a probe.Audio) string {
	s := lang(a.Language) + " " + audioName(a) + " " + layout(a)
	if commentary(a) {
		s += " (commentary)"
	}
	return s
}

func subtitleItem(s probe.Subtitle) string {
	codec := s.Codec
	if codec == "" {
		codec = "unknown"
	}
	out := lang(s.Language) + " " + codec
	if s.Forced {
		out += " forced"
	}
	if s.HearingImpaired {
		out += " SDH"
	}
	return out
}

var (
	sourcePatterns = []struct {
		name string
		re   *regexp.Regexp
	}{{"Remux", regexp.MustCompile("remux")}, {"BluRay", regexp.MustCompile("blu-?ray|bdrip|brrip")}, {"WEB-DL", regexp.MustCompile("web-?dl")},
		{"WEBRip", regexp.MustCompile("webrip")}, {"HDTV", regexp.MustCompile("hdtv")}, {"DVD", regexp.MustCompile("dvd")}}
	groupPatterns = []*releasesites.Pattern{
		releasesites.New(`-([A-Za-z0-9]+)(?:\[[^\]]+\])?(?:\.\w{2,4})?$`),
		releasesites.New(`\[([A-Za-z0-9.]+)\]`),
		releasesites.New(`(?i)\b({sites}|Tigole|QxR|Silence)\b`),
	}
)

// sourceGroup reads the release source and group from a torrent-style name (same patterns as the Python).
func sourceGroup(name string) (source, group string) {
	low := strings.ToLower(name)
	source, group = "unknown", "unknown"
	for _, p := range sourcePatterns {
		if p.re.MatchString(low) {
			source = p.name
			break
		}
	}
	for _, re := range groupPatterns {
		if m := re.Re().FindStringSubmatch(name); m != nil {
			group = m[1]
			break
		}
	}
	return source, group
}

// ---- the richer facts ----

func colour(v *probe.Video) string {
	var parts []string
	for _, p := range [][2]string{{v.ColorPrimaries, "primaries"}, {v.ColorTransfer, "transfer"}, {v.ColorSpace, "matrix"}} {
		if p[0] != "" {
			parts = append(parts, p[0]+" "+p[1])
		}
	}
	return strings.Join(parts, " · ")
}

func hdrMetadata(v *probe.Video) string {
	var parts []string
	if v.MasteringMax > 0 {
		parts = append(parts, fmt.Sprintf("mastering %.4f-%.0f cd/m²", v.MasteringMin, v.MasteringMax))
	}
	if v.MaxCLL > 0 || v.MaxFALL > 0 {
		parts = append(parts, fmt.Sprintf("MaxCLL %d cd/m² · MaxFALL %d cd/m²", v.MaxCLL, v.MaxFALL))
	}
	if v.HDR10Plus {
		parts = append(parts, "HDR10+ dynamic metadata")
	}
	return strings.Join(parts, " · ")
}

func dvBase(compat int) string {
	switch compat {
	case 0:
		return "none"
	case 1:
		return "HDR10"
	case 2:
		return "SDR"
	case 4:
		return "HLG"
	case 6:
		return "Blu-ray HDR10"
	}
	return "unknown"
}

var (
	x265Version = regexp.MustCompile(`^x265 \(build \d+\) - ([0-9][0-9.+]*)`)
	x264Version = regexp.MustCompile(`^x264 - core (\d+)`)
	rateControl = regexp.MustCompile(` rc=(\S+)`)
	crfValue    = regexp.MustCompile(` crf=([0-9.]+)`)
	bitrate     = regexp.MustCompile(` bitrate=(\d+)`)
)

// encoder summarises the x264/x265 settings (version and rate control; the full string is in probe_cache) and
// the stream's encoder tag.
func encoder(v *probe.Video) string {
	var parts []string
	if s := v.EncoderSettings; s != "" {
		name := "x264"
		if m := x264Version.FindStringSubmatch(s); m != nil {
			name = "x264 core " + m[1]
		} else if m := x265Version.FindStringSubmatch(s); m != nil {
			name = "x265 " + m[1]
		} else if strings.HasPrefix(s, "x265") {
			name = "x265"
		}
		if m := rateControl.FindStringSubmatch(s); m != nil {
			switch m[1] {
			case "crf":
				if c := crfValue.FindStringSubmatch(s); c != nil {
					name += " · crf " + c[1]
				}
			default:
				name += " · " + m[1]
				if br := bitrate.FindStringSubmatch(s); br != nil {
					name += " " + br[1] + " kb/s"
				}
			}
		}
		parts = append(parts, name)
	}
	if v.Encoder != "" {
		parts = append(parts, v.Encoder)
	}
	return strings.Join(parts, " · ")
}

func audioDetail(a probe.Audio) string {
	name := audioNames[a.Codec]
	if name == "" {
		name = strings.ToUpper(a.Codec)
	}
	if a.Profile != "" {
		name += " (" + a.Profile + ")"
	}
	parts := []string{name, layout(a)}
	switch {
	case a.BitRate >= 1e6:
		parts = append(parts, fmt.Sprintf("%.1f Mb/s", float64(a.BitRate)/1e6))
	case a.BitRate > 0:
		parts = append(parts, fmt.Sprintf("%d kb/s", a.BitRate/1000))
	}
	if a.Default {
		parts = append(parts, "default")
	}
	if commentary(a) {
		parts = append(parts, "commentary")
	}
	if a.VisualImpaired || strings.Contains(strings.ToLower(a.Title), "descri") {
		parts = append(parts, "descriptive")
	}
	if a.Title != "" {
		parts = append(parts, strconv.Quote(clip(a.Title)))
	}
	return strings.Join(parts, " · ")
}

var sdhTitle = regexp.MustCompile(`(?i)\bsdh\b|\bcc\b|hearing`)

// subtitleKinds counts text and image tracks and names the forced and SDH ones, by disposition or by title.
func subtitleKinds(subs []probe.Subtitle) string {
	var text, image, forced, sdh []string
	nText, nImage := 0, 0
	for _, s := range subs {
		if s.Text {
			nText++
			text = appendUnique(text, s.Codec)
		} else {
			nImage++
			image = appendUnique(image, s.Codec)
		}
		if s.Forced || strings.Contains(strings.ToLower(s.Title), "forced") {
			forced = append(forced, lang(s.Language))
		}
		if s.HearingImpaired || sdhTitle.MatchString(s.Title) {
			sdh = append(sdh, lang(s.Language))
		}
	}
	var parts []string
	if nText > 0 {
		parts = append(parts, fmt.Sprintf("%d text (%s)", nText, strings.Join(text, ", ")))
	}
	if nImage > 0 {
		parts = append(parts, fmt.Sprintf("%d image (%s)", nImage, strings.Join(image, ", ")))
	}
	if len(forced) > 0 {
		parts = append(parts, "forced: "+strings.Join(forced, ", "))
	}
	if len(sdh) > 0 {
		parts = append(parts, "SDH: "+strings.Join(sdh, ", "))
	}
	return strings.Join(parts, " · ")
}

func attachments(mimes []string) string {
	fonts, images, other := 0, 0, 0
	for _, m := range mimes {
		switch {
		case strings.Contains(m, "font") || strings.Contains(m, "truetype") || strings.Contains(m, "opentype"):
			fonts++
		case strings.HasPrefix(m, "image/"):
			images++
		default:
			other++
		}
	}
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{{fonts, "font"}, {images, "image"}, {other, "other file"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, plural(c.n, c.what)))
		}
	}
	return strings.Join(parts, " · ")
}

func releaseTags(info probe.Info) string {
	var parts []string
	for _, t := range [][2]string{{"title", info.Title}, {"comment", info.Comment}} {
		if v := clip(t[1]); v != "" {
			parts = append(parts, t[0]+" "+strconv.Quote(v))
		}
	}
	return strings.Join(parts, " · ")
}

// ---- small helpers ----

func lang(l string) string {
	if l == "" {
		return "und"
	}
	return l
}

func commentary(a probe.Audio) bool {
	return a.Comment || strings.Contains(strings.ToLower(a.Title), "comment")
}

// clip collapses whitespace (titles can hold newlines) and keeps a free-text tag to one readable line.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

func clock(s int) string { return fmt.Sprintf("%dh %02dm %02ds", s/3600, s%3600/60, s%60) }

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func mapSlice[T any](in []T, f func(T) string) []string {
	out := make([]string, 0, len(in))
	for _, x := range in {
		out = append(out, f(x))
	}
	return out
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, "; ")
}
