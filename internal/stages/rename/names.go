package rename

// Name rules of the library scheme, ported from rename_engine.py. Python's re/pathlib/str semantics differ from Go's
// in a few places that change real names (Unicode \s, pathlib suffixes, lookarounds RE2 lacks); each helper here
// keeps the Python behaviour so the Go planner names files byte-identically.

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// pyS is Python's \s for str patterns (Unicode whitespace); Go's \s is ASCII-only.
const pyS = `[\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

var (
	videoExt   = set(".mkv", ".mp4", ".avi", ".m4v", ".mov", ".ts", ".wmv", ".mpg", ".webm")
	subExt     = set(".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx", ".sup", ".smi")
	textSubExt = set(".srt", ".ass", ".ssa", ".vtt", ".smi")
	ignoreName = set(".DS_Store", "Thumbs.db", "desktop.ini")
	extrasDirs = map[string]string{"behind the scenes": "Behind The Scenes", "deleted scenes": "Deleted Scenes",
		"featurettes": "Featurettes", "interviews": "Interviews", "scenes": "Scenes", "shorts": "Shorts", "trailers": "Trailers",
		"other": "Other"}
	extrasNames = set("Behind The Scenes", "Deleted Scenes", "Featurettes", "Interviews", "Scenes", "Shorts", "Trailers", "Other")

	// A folder is an extras folder only when its WHOLE name says so ('Generation Kill ... + Extras (...)' is a season pack).
	extraFolder = regexp.MustCompile(`(?i)^(extras?|bonus( features)?|special features|featurettes?|making[ .-]of|behind the scenes|` +
		`deleted scenes|interviews|scenes|shorts|trailers|other|criterion extras|plex versions? extras?)` +
		`( ?[-–] ?season \d+| season \d+| season [a-z]+)?$`)
	sampleRe = regexp.MustCompile(`(?i)(^|[ ._\-\[(])sample([ ._\-\])]|$)|^sitea\.com|^sitee\.|^main menu`)
	quality  = regexp.MustCompile(`(?i)\b(\d{3,4}p|web[ .-]?(dl|rip)?|blu[ .-]?ray|bdrip|brrip|hdtv|x26[45]|h[ .]?26[45]|hevc|avc|aac|ac3|` +
		`dts|ddp?\d|atmos|remux|10bit|amzn|nf|dsnp|hmax|atvp|repack|proper|internal|sitea)\b`)
	genericWords = set("the", "a", "an", "of", "and", "version", "cut", "edition", "bluray", "web", "dl", "x264", "x265", "hevc", "h264",
		"1080p", "720p", "2160p", "4k", "uhd", "en", "eng", "english", "srt", "sub", "subs", "mkv", "mp4")
	cuts = []struct {
		re    *regexp.Regexp
		label string
	}{
		{regexp.MustCompile(`open[ ._-]?matte`), "Open Matte"}, {regexp.MustCompile(`theatrical`), "Theatrical"},
		{regexp.MustCompile(`extended`), "Extended"}, {regexp.MustCompile(`unrated`), "Unrated"},
		{regexp.MustCompile(`director'?s[ ._-]?cut`), "Director's Cut"}, // plus a standalone "dc": see cutWord
		{regexp.MustCompile(`final[ ._-]?cut`), "Final Cut"}, {regexp.MustCompile(`imax`), "IMAX"},
		{regexp.MustCompile(`super[ ._-]?duper[ ._-]?cut`), "Super Duper Cut"}, {regexp.MustCompile(`encore[ ._-]?edition`), "Encore Edition"},
		{regexp.MustCompile(`redux`), "Redux"}, {regexp.MustCompile(`criterion`), "Criterion"}, {regexp.MustCompile(`remaster`), "Remastered"},
	}
	casefold = cases.Fold()
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// suffix and stem follow pathlib: "a." and ".a" have no suffix (filepath.Ext disagrees on both).
func suffix(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

func stem(name string) string { return name[:len(name)-len(suffix(name))] }

func lowerExt(p string) string { return strings.ToLower(suffix(path.Base(p))) }

var (
	colonRe = regexp.MustCompile(pyS + `*:` + pyS + `+`)
	slashRe = regexp.MustCompile(`[:/\\|]`)
	starRe  = regexp.MustCompile(`\*+`)
	badRe   = regexp.MustCompile(`[<>?\x00-\x1f]`)
	spaceRe = regexp.MustCompile(pyS + `+`)
)

// clean makes text safe inside a path component on btrfs, SMB (macOS/Windows), exFAT and Jellyfin. Keeps inner dots (E.T.).
func clean(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) { // BOM, word joiner, zero-width space
			return -1
		}
		return r
	}, norm.NFC.String(text))
	text = strings.NewReplacer("꞉", ":", "？", "?").Replace(text)
	text = colonRe.ReplaceAllString(text, " - ")
	text = strings.ReplaceAll(slashRe.ReplaceAllString(text, "-"), `"`, "'")
	text = starRe.ReplaceAllString(text, "-")
	text = badRe.ReplaceAllString(text, "")
	return strings.TrimFunc(spaceRe.ReplaceAllString(text, " "), isPySpace)
}

// component finishes a path component: no trailing dot or space (illegal on SMB/exFAT).
func component(name string) string { return strings.TrimRight(name, " .") }

func withYear(title string, year int) string {
	title = clean(title)
	if year == 0 {
		return title
	}
	if y := fmt.Sprintf("(%d)", year); strings.HasSuffix(title, y) {
		return title
	}
	return fmt.Sprintf("%s (%d)", title, year)
}

func resolution(width, height int) string {
	switch {
	case width >= 3200 || height >= 1700:
		return "2160p"
	case width >= 1700 || height >= 950:
		return "1080p"
	case width >= 1100 || height >= 650:
		return "720p"
	}
	return "480p"
}

var (
	groupDash    = regexp.MustCompile(`-([A-Za-z0-9]+)(?:\[[^\]]+\])?$`)
	groupBracket = regexp.MustCompile(`\[([A-Za-z0-9.]+)\]$`)
)

func releaseGroup(p string) string {
	s := stem(path.Base(p))
	if m := groupDash.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := groupBracket.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func isLowerASCII(b byte) bool { return b >= 'a' && b <= 'z' }

// cutWord names the edition a path mentions. Python's (?<![a-z])dc(?![a-z]) has no RE2 form, so a standalone "dc"
// is checked by hand, as an alternative of the Director's Cut pattern.
func cutWord(p string) string {
	low := strings.ToLower(p)
	for _, c := range cuts {
		if c.re.MatchString(low) || (c.label == "Director's Cut" && standaloneDC(low)) {
			return c.label
		}
	}
	return ""
}

func standaloneDC(low string) bool {
	for i := 0; i+2 <= len(low); i++ {
		if low[i:i+2] == "dc" && (i == 0 || !isLowerASCII(low[i-1])) && (i+2 == len(low) || !isLowerASCII(low[i+2])) {
			return true
		}
	}
	return false
}

var (
	alnumRun   = regexp.MustCompile(`[a-z0-9]+`)
	nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)
)

func words(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range alnumRun.FindAllString(strings.ToLower(text), -1) {
		if !genericWords[w] && len(w) > 1 {
			out[w] = true
		}
	}
	return out
}

func normTitle(text string) string {
	return strings.Trim(nonAlnumRe.ReplaceAllString(strings.ToLower(norm.NFKD.String(text)), " "), " ")
}

func titlesAgree(a, b string) bool {
	a, b = normTitle(a), normTitle(b)
	if a == "" || b == "" || strings.Contains(b, a) || strings.Contains(a, b) {
		return true
	}
	return seqRatio(a, b) >= 0.72
}

// seqRatio is difflib.SequenceMatcher(None, a, b).ratio() with its default autojunk, so fuzzy title checks decide
// exactly as the Python planner did.
func seqRatio(a, b string) float64 {
	ar, br := []rune(a), []rune(b)
	b2j := map[rune][]int{}
	for j, r := range br {
		b2j[r] = append(b2j[r], j)
	}
	if n := len(br); n >= 200 {
		for r, idx := range b2j {
			if len(idx) > n/100+1 {
				delete(b2j, r)
			}
		}
	}
	matches := 0
	queue := [][4]int{{0, len(ar), 0, len(br)}}
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := longestMatch(ar, br, b2j, q[0], q[1], q[2], q[3])
		if k == 0 {
			continue
		}
		matches += k
		if q[0] < i && q[2] < j {
			queue = append(queue, [4]int{q[0], i, q[2], j})
		}
		if i+k < q[1] && j+k < q[3] {
			queue = append(queue, [4]int{i + k, q[1], j + k, q[3]})
		}
	}
	if total := len(ar) + len(br); total > 0 {
		return 2 * float64(matches) / float64(total)
	}
	return 1
}

func longestMatch(a, b []rune, b2j map[rune][]int, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, best := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		next := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			next[j] = k
			if k > best {
				besti, bestj, best = i-k+1, j-k+1, k
			}
		}
		j2len = next
	}
	// Popular elements were dropped from b2j but are not junk, so the match still extends over them.
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, best = besti-1, bestj-1, best+1
	}
	for besti+best < ahi && bestj+best < bhi && a[besti+best] == b[bestj+best] {
		best++
	}
	return besti, bestj, best
}

func extraCategory(parts []string) string {
	for i := len(parts) - 1; i >= 0; i-- {
		low := strings.ToLower(parts[i])
		switch {
		case extrasDirs[low] != "":
			return extrasDirs[low]
		case strings.Contains(low, "deleted"):
			return "Deleted Scenes"
		case strings.Contains(low, "behind") || strings.Contains(low, "inside the episode") || strings.Contains(low, "making of"):
			return "Behind The Scenes"
		case strings.Contains(low, "interview"):
			return "Interviews"
		case strings.Contains(low, "trailer"):
			return "Trailers"
		}
	}
	return "Featurettes"
}

// ---- episode numbers

type fileEp struct {
	season       int // -1: the name carries no season (absolute numbering)
	first, last  int
	title        string
}

var (
	seinfeldPair = regexp.MustCompile(`^(\d)(\d\d)` + pyS + `*&` + pyS + `*(\d)(\d\d)` + pyS + `*-` + pyS + `*(.+)$`)
	seinfeldOne  = regexp.MustCompile(`^(\d)(\d\d)` + pyS + `*-` + pyS + `*(.+)$`)
	sxxeyy       = regexp.MustCompile(`[Ss](\d{1,2})[ ._-]?[Ee](\d{1,3})(?:[ ._-]?(?:-|[Ee]){1,2}[Ee]?(\d{1,3}))?`)
	absolute     = regexp.MustCompile(`(?:^|` + pyS + `)-` + pyS + `*(\d{2,3})` + pyS + `*-` + pyS + `*(.+?)` + pyS + `*(?:\[|$)`)
	titleCut     = regexp.MustCompile(pyS + `[\[(]|` + pyS + `{2,}`)
)

// fileEpisode parses an episode file name as rename_engine.file_episode did; ok is false when it names no episode.
func fileEpisode(name string) (fileEp, bool) {
	s := stem(name)
	if m := seinfeldPair.FindStringSubmatch(s); m != nil && m[3] == m[1] { // Seinfeld '317 & 318 - The Boyfriend'
		return fileEp{atoi(m[1]), atoi(m[2]), atoi(m[4]), m[5]}, true
	}
	if m := seinfeldOne.FindStringSubmatch(s); m != nil { // Seinfeld '901 - The Butter Shave'
		return fileEp{atoi(m[1]), atoi(m[2]), atoi(m[2]), m[3]}, true
	}
	loc := sxxeyy.FindStringSubmatchIndex(s)
	if loc == nil {
		if m := absolute.FindStringSubmatch(s); m != nil { // 'Case Closed - 013 - Kidnapped Amy   [DarkDream]'
			return fileEp{-1, atoi(m[1]), atoi(m[1]), m[2]}, true
		}
		return fileEp{}, false
	}
	end := loc[1]
	// Python's range group ends in (?![\dp]): a range followed by a digit or "p" (a resolution) is no range.
	if loc[6] >= 0 && end < len(s) && (s[end] == 'p' || (s[end] >= '0' && s[end] <= '9')) {
		loc[6], loc[7], end = -1, -1, loc[5]
	}
	first := atoi(s[loc[4]:loc[5]])
	last := first
	if loc[6] >= 0 {
		if n := atoi(s[loc[6]:loc[7]]); n-first > 0 && n-first <= 3 {
			last = n
		}
	}
	rest := strings.NewReplacer(".", " ", "_", " ").Replace(s[end:])
	if i := titleCut.FindStringIndex(rest); i != nil {
		rest = rest[:i[0]]
	}
	if q := quality.FindStringIndex(rest); q != nil {
		rest = rest[:q[0]]
	}
	return fileEp{atoi(s[loc[2]:loc[3]]), first, last, strings.TrimFunc(rest, func(r rune) bool { return isPySpace(r) || r == '-' || r == '–' })}, true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

var showPrefixRe = regexp.MustCompile(`[Ss]\d{1,2}[ ._-]?[Ee]\d`)

func showPrefix(name string) string {
	if loc := showPrefixRe.FindStringIndex(name); loc != nil {
		return normTitle(name[:loc[0]])
	}
	return ""
}

// sameShowAliases are show names a file may use for the title the metadata source gives (decided once by hand).
var sameShowAliases = [][2]string{{"shinya shokudo", "midnight diner"}, {"plur1bus", "pluribus"}}

func sameShow(prefix, show, original string) bool {
	if prefix == "" {
		return true
	}
	pw := map[string]bool{}
	for _, w := range strings.Fields(prefix) {
		if len([]rune(w)) > 2 && !isDigits(w) {
			pw[w] = true
		}
	}
	var names []string
	for _, n := range []string{normTitle(show), normTitle(original)} {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(pw) == 0 {
		return true
	}
	for _, n := range names {
		for _, w := range strings.Fields(n) {
			if pw[w] {
				return true
			}
		}
	}
	for _, alias := range sameShowAliases {
		if strings.Contains(prefix, alias[0]) {
			for _, n := range names {
				if strings.Contains(n, alias[1]) {
					return true
				}
			}
		}
	}
	for _, n := range names {
		if seqRatio(prefix, n) >= 0.75 {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}
