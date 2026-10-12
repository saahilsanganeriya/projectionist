package subtitles

// Port of scripts/sub_stats.py: read-only facts about every subtitle file (hash, cues, SDH-cue share, ad-only,
// script, idx languages, a text sample, and a language guess from stop words). The rename planner reads them; the
// TSV format and the walk order are the script's, byte for byte.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"projectionist/internal/releasesites"
)

const statsHeader = "path\tsize\tsha1\tcues\tsdh_share\tad_only\tscript\tidx_langs\tsample\tlang_guess\n"

var (
	subtitleExt = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sub": true, ".idx": true, ".smi": true}
	adRe        = releasesites.New(`(?i)({sites}|opensubtitles|subscene|provided by|downloaded from|advertise|www\.|\.com|\.org|sync(ed)? by|subtitles? by|resync)`)
	timeRe      = regexp.MustCompile(`^` + pyDigit + pyDigit + `:` + pyDigit + pyDigit + `:` + pyDigit + pyDigit + `[,.]` + pyDigit + `+` + pySpace + `*-->`)
	sdhRe       = regexp.MustCompile(`^` + pySpace + `*([\[(♪]|[A-Z][A-Z .'\-]{2,}:)`)
	idxLangRe   = regexp.MustCompile(`(?m)^id:` + pySpace + `*([a-z]{2})`)
	markupRe    = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)
	spacesRe    = regexp.MustCompile(pySpace + `+`)
)

// Script ranges, tried in this order; the first with more than 20 characters in the text names it.
var scripts = []struct {
	lo, hi rune
	name   string
}{
	{0x0900, 0x097f, "devanagari"}, {0x0600, 0x06ff, "arabic"}, {0x0400, 0x04ff, "cyrillic"}, {0x4e00, 0x9fff, "han"},
	{0x0e00, 0x0e7f, "thai"}, {0xac00, 0xd7af, "hangul"}, {0x3040, 0x30ff, "kana"}, {0x0590, 0x05ff, "hebrew"},
	{0x0370, 0x03ff, "greek"},
}

// The commonest words of each language; a language wins when its words are >= 12 % of the text and 1.5x the runner-up.
var stopWords = func() map[string]map[string]bool {
	src := map[string]string{
		"en": "the you and to of is it that in what me this we he for my your have be not are on was with do just know but all so can they don't i'm",
		"es": "que de no la el y es en lo un por una te los se con para está si pero yo eso las qué muy aquí bien",
		"fr": "je est pas le vous la tu que un il et à les ce ne une on des mais pour c'est suis",
		"de": "ich sie das ist du nicht die und es der wir was zu ein er mich mit ja wie den auf",
		"it": "non che di è e la il un per mi ti ho sono lo una ma si cosa questo perché",
		"pt": "que não de o a é e um eu se você do para isso com uma os está me no em",
		"nl": "de het ik je een is niet dat van en wat ze we op zijn hij te met",
		"sv": "jag det är du inte att och en som på har vi för med han till",
		"tr": "bir bu ne ve için de da ben sen mi çok değil o var",
		"pl": "nie to się że w na jest co ja jak tak z do ale",
		"ro": "nu e să și de ce la că o pe un în mai",
		"id": "yang aku kau tidak ini itu dan di apa kita dia ke saya",
	}
	out := map[string]map[string]bool{}
	for lang, words := range src {
		out[lang] = map[string]bool{}
		for _, w := range strings.Fields(words) {
			out[lang][w] = true
		}
	}
	return out
}()

// isWordRune is re's [^\W\d_]: a letter, or a numeric character that is not a decimal digit.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.In(r, unicode.Nl, unicode.No)
}

// words is re.findall(r"[^\W\d_]+(?:'[^\W\d_]+)?", s).
func words(s string) []string {
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if !isWordRune(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && isWordRune(rs[j]) {
			j++
		}
		if j+1 < len(rs) && rs[j] == '\'' && isWordRune(rs[j+1]) {
			for j++; j < len(rs) && isWordRune(rs[j]); j++ {
			}
		}
		out = append(out, string(rs[i:j]))
		i = j
	}
	return out
}

func guessLang(body []string) string {
	ws := words(pyLower(markupRe.ReplaceAllString(strings.Join(body, " "), " ")))
	if len(ws) < 15 {
		return ""
	}
	type share struct {
		v    float64
		lang string
	}
	var shares []share
	for lang, stop := range stopWords {
		n := 0
		for _, w := range ws {
			if stop[w] {
				n++
			}
		}
		shares = append(shares, share{float64(n) / float64(len(ws)), lang})
	}
	sort.Slice(shares, func(i, j int) bool { // sorted(..., reverse=True) on (share, lang) tuples
		if shares[i].v != shares[j].v {
			return shares[i].v > shares[j].v
		}
		return shares[i].lang > shares[j].lang
	})
	floor := 0.2 // a few lines (a forced track) must be unmistakable
	if len(ws) >= 50 {
		floor = 0.12
	}
	if shares[0].v >= floor && shares[0].v >= 1.5*shares[1].v {
		return shares[0].lang
	}
	return ""
}

func script(text string) string {
	for _, s := range scripts {
		n := 0
		for _, r := range text {
			if r >= s.lo && r <= s.hi {
				n++
			}
		}
		if n > 20 {
			return s.name
		}
	}
	return "latin"
}

// pyExt is os.path.splitext(name)[1].lower(): leading dots never start an extension.
func pyExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || strings.Trim(name[:i], ".") == "" {
		return ""
	}
	return strings.ToLower(name[i:])
}

// statsRow is the TSV line sub_stats.py writes for one file, without the leading path.
func statsRow(ext string, raw []byte) string {
	sum := sha1.Sum(raw)
	cues, sdh, adOnly := 0, 0.0, 0
	langs, text, scr, guess := "", "", "", ""
	if ext == ".idx" {
		set := map[string]bool{}
		for _, m := range idxLangRe.FindAllStringSubmatch(decodeStats(raw), -1) {
			set[m[1]] = true
		}
		var l []string
		for k := range set {
			l = append(l, k)
		}
		sort.Strings(l)
		langs = strings.Join(l, ",")
	} else if ext != ".sub" || len(raw) == 0 || raw[0] != 0 {
		text = decodeStats(raw)
		var lines, body []string
		for _, l := range pySplitLines(text) {
			lines = append(lines, pyStrip(l))
		}
		for _, l := range lines {
			if timeRe.MatchString(l) {
				cues++
			}
		}
		if cues == 0 {
			for _, l := range lines {
				if lo := pyLower(l); strings.HasPrefix(lo, "dialogue:") || strings.HasPrefix(lo, "<sync") {
					cues++
				}
			}
		}
		sdhLines, adLines := 0, 0
		for _, l := range lines {
			if l != "" && !timeRe.MatchString(l) && !pyIsDigit(l) {
				body = append(body, l)
				if sdhRe.MatchString(l) {
					sdhLines++
				}
				if adRe.Re().MatchString(l) {
					adLines++
				}
			}
		}
		if len(body) > 0 {
			sdh = float64(sdhLines) / float64(len(body))
			if float64(adLines) >= float64(len(body))*0.8 {
				adOnly = 1
			}
		}
		scr = script(text)
		if scr == "latin" {
			guess = guessLang(body)
		}
	}
	var kept []string
	for i, l := range pySplitLines(text) {
		if i == 80 {
			break
		}
		if l != "" && !timeRe.MatchString(pyStrip(l)) && !pyIsDigit(pyStrip(l)) {
			kept = append(kept, l)
		}
	}
	sample := []rune(spacesRe.ReplaceAllString(strings.Join(kept, " "), " "))
	if len(sample) > 300 {
		sample = sample[:300]
	}
	return fmt.Sprintf("%d\t%s\t%d\t%s\t%d\t%s\t%s\t%s\t%s", len(raw), hex.EncodeToString(sum[:]), cues,
		strconv.FormatFloat(sdh, 'f', 3, 64), adOnly, scr, langs, strings.ReplaceAll(string(sample), "\t", " "), guess)
}

// pyWalk visits root like os.walk (top-down, readdir order, @eaDir pruned, symlinked folders listed but not
// entered, unreadable folders skipped): fn gets each folder and its non-folder entries.
func pyWalk(root string, fn func(dir string, files []string)) {
	f, err := os.Open(root)
	if err != nil {
		return
	}
	entries, err := f.ReadDir(-1) // directory order; os.ReadDir would sort
	f.Close()
	if err != nil {
		return
	}
	var dirs, files []string
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			st, err := os.Stat(root + "/" + e.Name())
			isDir = err == nil && st.IsDir()
		}
		if isDir {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	fn(root, files)
	for _, d := range dirs {
		p := root + "/" + d
		if st, err := os.Lstat(p); d == "@eaDir" || err != nil || st.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		pyWalk(p, fn)
	}
}

// subtitleFile reports whether a file name is one sub_stats.py reads.
func subtitleFile(name string) bool { return subtitleExt[pyExt(filepath.Base(name))] }
