package rename

// Episode numbers and show names read from file names (rename_engine.py's file_episode, own_show, best_hint,
// same_show and episode_code).

import (
	"regexp"
	"strings"
)

type fileEp struct {
	season      int // -1: the name carries no season (absolute numbering)
	first, last int
	title       string
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

var (
	episodeMark   = regexp.MustCompile(`(?i)[ ._-]*(?:\bS(\d{1,2})[ ._-]?E(\d{1,3})|\bSeason[ ._]?(\d{1,2})[ ._]?Episode[ ._]?(\d{1,3}))`)
	trailingSepRe = regexp.MustCompile(`[ ._\-–]+$`)
)

type ownName struct {
	show            string
	season, episode int
}

// ownShow is the show a file names itself by ('Show (2019) - S01E02 ...', 'Show.Season 3 Episode 1...').
func ownShow(stemName string) (ownName, bool) {
	loc := episodeMark.FindStringSubmatchIndex(stemName)
	if loc == nil {
		return ownName{}, false
	}
	show := clean(trailingSepRe.ReplaceAllString(strings.NewReplacer(".", " ", "_", " ").Replace(stemName[:loc[0]]), ""))
	if show == "" {
		return ownName{}, false
	}
	group := func(i int) int { return atoi(stemName[loc[2*i]:loc[2*i+1]]) }
	if loc[2] < 0 { // the "Season N Episode M" spelling
		return ownName{show, group(3), group(4)}, true
	}
	return ownName{show, group(1), group(2)}, true
}

// bestHint is the one listed episode whose title matches title (ratio >= 0.8), else ok=false (also on a tie).
func bestHint(hints []Episode, title string) (Episode, bool) {
	want := normTitle(title)
	if want == "" || len(hints) == 0 {
		return Episode{}, false
	}
	best, bestRatio, tie := Episode{}, -1.0, false
	for _, h := range hints {
		r := seqRatio(want, normTitle(h.Title))
		switch {
		case r > bestRatio:
			best, bestRatio, tie = h, r, false
		case r == bestRatio:
			tie = true
		}
	}
	return best, bestRatio >= 0.8 && !tie
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

// sameShow is the TV same_show guard: the show a file name starts with must be the show it was matched to.
func sameShow(prefix, show, original string) bool {
	if prefix == "" {
		return true
	}
	pw := map[string]bool{}
	for _, w := range strings.Fields(prefix) {
		if len(w) > 2 && !isDigits(w) {
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

var (
	codeSxE   = regexp.MustCompile(`[Ss](\d{1,2})[ ._-]?[Ee](\d{1,3})`)
	codeNxNN  = regexp.MustCompile(`\b(\d{1,2})x(\d{2})\b`)
	partRoman = regexp.MustCompile(`Part[ ._]([IVX]+)\b`)
	partNum   = regexp.MustCompile(`Part[ ._](\d{1,2})\b`)
	roman     = map[string]int{"I": 1, "II": 2, "III": 3, "IV": 4, "V": 5, "VI": 6, "VII": 7, "VIII": 8, "IX": 9, "X": 10, "XI": 11, "XII": 12, "XIII": 13}
)

type epCode struct{ season, episode int }

// episodeCode is the (season, episode) a subtitle or video name mentions, for tying a subtitle to its episode.
func episodeCode(name string) (epCode, bool) {
	if m := codeSxE.FindStringSubmatch(name); m != nil {
		return epCode{atoi(m[1]), atoi(m[2])}, true
	}
	if m := codeNxNN.FindStringSubmatch(name); m != nil {
		return epCode{atoi(m[1]), atoi(m[2])}, true
	}
	if m := partRoman.FindStringSubmatch(name); m != nil && roman[m[1]] > 0 {
		return epCode{1, roman[m[1]]}, true
	}
	if m := partNum.FindStringSubmatch(name); m != nil {
		return epCode{1, atoi(m[1])}, true
	}
	return epCode{}, false
}
