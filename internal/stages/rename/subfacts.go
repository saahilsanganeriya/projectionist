package rename

// Facts read from a subtitle file's bytes (port of scripts/sub_stats.py): hash, cue count, share of SDH lines,
// advert-only, script, VobSub languages, and a stop-word language guess for names that carry no language.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	xunicode "golang.org/x/text/encoding/unicode"

	"projectionist/internal/releasesites"
	"projectionist/internal/stage"
)

var (
	adLine     = releasesites.New(`(?i)({sites}|opensubtitles|subscene|provided by|downloaded from|advertise|www\.|\.com|\.org|sync(ed)? by|subtitles? by|resync)`)
	cueTime    = regexp.MustCompile(`^\d\d:\d\d:\d\d[,.]\d+` + pyS + `*-->`)
	sdhLine    = regexp.MustCompile(`^` + pyS + `*([\[(♪]|[A-Z][A-Z .'\-]{2,}:)`)
	idxLang    = regexp.MustCompile(`(?m)^id:` + pyS + `*([a-z]{2})`)
	markupRe   = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)
	letterWord = regexp.MustCompile(`[\pL\p{Nl}\p{No}]+(?:'[\pL\p{Nl}\p{No}]+)?`)
	scripts    = []struct {
		name string
		lo   rune
		hi   rune
	}{{"devanagari", 0x900, 0x97f}, {"arabic", 0x600, 0x6ff}, {"cyrillic", 0x400, 0x4ff}, {"han", 0x4e00, 0x9fff},
		{"thai", 0xe00, 0xe7f}, {"hangul", 0xac00, 0xd7af}, {"kana", 0x3040, 0x30ff}, {"hebrew", 0x590, 0x5ff}, {"greek", 0x370, 0x3ff}}

	// The commonest words of each language; a language wins when its words are >= 12 % of the text and 1.5x the runner-up.
	stopWords = map[string]map[string]bool{}
	stopLangs []string
)

func init() {
	for lang, ws := range map[string]string{
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
	} {
		stopWords[lang] = set(strings.Fields(ws)...)
		stopLangs = append(stopLangs, lang)
	}
	sort.Strings(stopLangs)
}

// decodeText follows sub_stats.decode: UTF-8 BOM, UTF-16 BOM, strict UTF-8, else cp1252.
func decodeText(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}):
		return strings.ToValidUTF8(string(raw[3:]), "\ufffd")
	case bytes.HasPrefix(raw, []byte{0xff, 0xfe}) || bytes.HasPrefix(raw, []byte{0xfe, 0xff}):
		out, _ := xunicode.UTF16(xunicode.LittleEndian, xunicode.ExpectBOM).NewDecoder().Bytes(raw)
		return string(out)
	case utf8.Valid(raw):
		return string(raw)
	}
	out, _ := charmap.Windows1252.NewDecoder().Bytes(raw)
	return string(out)
}

// splitLines is str.splitlines(): every Unicode line boundary, no trailing empty line.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', 0x85, 0x2028, 0x2029:
			lines, i, start = append(lines, text[start:i]), i+n, i+n
		case '\r':
			lines = append(lines, text[start:i])
			if i+1 < len(text) && text[i+1] == '\n' {
				n++
			}
			i, start = i+n, i+n
		default:
			i += n
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// readSubFacts computes a subtitle's facts from its bytes; ext is the lowercased suffix.
func readSubFacts(raw []byte, ext string) SubFacts {
	sum := sha1.Sum(raw)
	f := SubFacts{SHA1: hex.EncodeToString(sum[:]), Size: int64(len(raw))}
	switch {
	case ext == ".idx":
		seen := map[string]bool{}
		for _, m := range idxLang.FindAllStringSubmatch(decodeText(raw), -1) {
			seen[m[1]] = true
		}
		f.Idx = strings.Join(sortedKeys(seen), ",")
		return f
	case ext == ".sub" && len(raw) > 0 && raw[0] == 0: // binary VobSub
		return f
	}
	text := decodeText(raw)
	var lines, body []string
	for _, l := range splitLines(text) {
		lines = append(lines, strings.TrimFunc(l, isPySpace))
	}
	dialogue := 0
	for _, l := range lines {
		low := strings.ToLower(l)
		switch {
		case cueTime.MatchString(l):
			f.Cues++
		case strings.HasPrefix(low, "dialogue:") || strings.HasPrefix(low, "<sync"):
			dialogue++
		}
		if l != "" && !cueTime.MatchString(l) && !allDigits(l) {
			body = append(body, l)
		}
	}
	if f.Cues == 0 {
		f.Cues = dialogue
	}
	if len(body) > 0 {
		sdh, ads := 0, 0
		for _, l := range body {
			if sdhLine.MatchString(l) {
				sdh++
			}
			if adLine.Re().MatchString(l) {
				ads++
			}
		}
		// sub_stats.tsv kept 3 decimals and the planner read that back: round the same way.
		f.SDH, _ = strconv.ParseFloat(fmt.Sprintf("%.3f", float64(sdh)/float64(len(body))), 64)
		f.Ad = float64(ads) >= float64(len(body))*0.8
	}
	f.Script = textScript(markupRe.ReplaceAllString(text, " ")) // a font name in a <font face> tag is not the text's script
	if f.Script == "latin" {
		f.Guess = guessLang(body)
	}
	return f
}

// textScript is the first script with more than 20 characters (sub_stats.script) that are also more than a fifth of the
// Latin letters, counting only characters in words with no Latin letter: OCR'd English writes Greek or Cyrillic
// capitals for their Latin look-alikes ("PLAYIΝG"), and an English track can keep a few untranslated lines (Evil Does
// Not Exist's: 39 Chinese characters among 4,853 English words); reading either as another script would archive a
// film's only English track, which the subtitle stage then writes again.
func textScript(text string) string {
	latinLetters := 0
	for _, r := range text {
		if r < 0x80 && unicode.IsLetter(r) {
			latinLetters++
		}
	}
	for _, s := range scripts {
		n, mixed, word, latin := 0, 0, 0, false
		flush := func() {
			n += word
			if latin {
				mixed += word
			}
			word, latin = 0, false
		}
		for _, r := range text {
			switch {
			case r >= s.lo && r <= s.hi:
				word++
			case r < 0x80 && unicode.IsLetter(r):
				latin = true
			case !unicode.IsLetter(r) && !unicode.Is(unicode.Mn, r):
				flush()
			}
		}
		flush()
		if n-mixed > 20 && 5*(n-mixed) > latinLetters {
			return s.name
		}
	}
	return "latin"
}

func guessLang(body []string) string {
	ws := letterWord.FindAllString(strings.ToLower(markupRe.ReplaceAllString(strings.Join(body, " "), " ")), -1)
	if len(ws) < 15 {
		return ""
	}
	type share struct {
		v    float64
		lang string
	}
	shares := make([]share, 0, len(stopLangs))
	for _, lang := range stopLangs {
		n := 0
		for _, w := range ws {
			if stopWords[lang][w] {
				n++
			}
		}
		shares = append(shares, share{float64(n) / float64(len(ws)), lang})
	}
	sort.Slice(shares, func(i, j int) bool { // Python sorted((share, lang), reverse=True)
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

// factExt are the subtitles sub_stats.py read (not .sup: image subtitles carry no text to read).
var factExt = set(".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx", ".smi")

// The cache table is named for the facts' version: a change to how facts are read must bump it, so every file is
// read again rather than planned from stale facts.
const subFactsSchema = `DROP TABLE IF EXISTS rename_subfacts;
DROP TABLE IF EXISTS rename_subfacts_v2;
CREATE TABLE IF NOT EXISTS rename_subfacts_v3 (
  path     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL,
  facts    TEXT NOT NULL
)`

var readFile = os.ReadFile // a test stops a run mid-way through it

// factsBatch is how many freshly read files are committed at once: a stop mid-way (the first run reads every file,
// ~20 min on a busy NAS) keeps what it read, and one transaction never holds more than this.
const factsBatch = 500

// subFacts returns the facts of every subtitle in files, reading only those new or changed since the last run (the
// cache is keyed by path, size and mtime, and forgets files that are gone). It stops at ctx's end, keeping what it read.
func subFacts(ctx context.Context, env *stage.Env, files map[string]int64) (map[string]SubFacts, error) {
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, subFactsSchema); err != nil {
		return nil, err
	}
	type row struct {
		size, mtime int64
		facts       SubFacts
	}
	cached := map[string]row{}
	rows, err := db.QueryContext(ctx, `SELECT path, size, mtime_ns, facts FROM rename_subfacts_v3`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, raw string
		var r row
		if err := rows.Scan(&p, &r.size, &r.mtime, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &r.facts) == nil {
			cached[p] = r
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fresh := map[string]row{}
	commit := func() error { // a stop must not lose the batch, so it is written without ctx
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for p, r := range fresh {
			raw, _ := json.Marshal(r.facts)
			if _, err := tx.Exec(`INSERT OR REPLACE INTO rename_subfacts_v3(path, size, mtime_ns, facts) VALUES (?, ?, ?, ?)`,
				p, r.size, r.mtime, string(raw)); err != nil {
				return err
			}
		}
		clear(fresh)
		return tx.Commit()
	}
	out := map[string]SubFacts{}
	for _, p := range sortedKeys(files) {
		if !factExt[lowerExt(p)] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, commit())
		}
		info, err := os.Lstat(p)
		if err != nil {
			continue // gone since the walk
		}
		r, ok := cached[p]
		if !ok || r.size != info.Size() || r.mtime != info.ModTime().UnixNano() {
			raw, err := readFile(p)
			if err != nil {
				continue
			}
			r = row{info.Size(), info.ModTime().UnixNano(), readSubFacts(raw, lowerExt(p))}
			if fresh[p] = r; len(fresh) >= factsBatch {
				if err := commit(); err != nil {
					return nil, err
				}
			}
		}
		out[p] = r.facts
	}
	if err := commit(); err != nil {
		return nil, err
	}
	for p := range cached {
		if _, live := out[p]; !live {
			if _, err := db.ExecContext(ctx, `DELETE FROM rename_subfacts_v3 WHERE path = ?`, p); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
