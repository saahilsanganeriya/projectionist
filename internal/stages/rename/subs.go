package rename

// Subtitles: tie each one to its video, then name or archive it (rename_engine.py's attach_subtitles,
// parse_name and name_subtitles). Identical bytes keep one copy; English text tracks become SDH by their cues;
// the aligner's copy wins the primary name; at most one primary, one ".eng", one SDH and one forced per language.

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// SubFacts are read from the subtitle file itself (see subfacts.go).
type SubFacts struct {
	SHA1   string
	Cues   int
	SDH    float64 // share of text lines that are sound cues or speaker labels
	Ad     bool    // nearly every line is an advert
	Script string  // latin, devanagari, han, ...
	Idx    string  // VobSub .idx: comma-separated languages
	Size   int64
	Guess  string // language from the text's stop words; "" when unsure
}

// SubLang is a language decision made outside the name (a text classifier); Lang "" means it found no dialogue.
type SubLang struct {
	Lang string
	SDH  bool
}

var (
	keepLangs  = []string{"en", "hi"} // sorted, as Python's sorted(KEEP_LANGS)
	scriptLang = map[string]string{"devanagari": "hi", "han": "zh", "hangul": "ko", "kana": "ja", "arabic": "ar", "thai": "th",
		"hebrew": "he", "greek": "el", "cyrillic": "ru"}
	langWords = map[string]string{
		"english": "en", "eng": "en", "en": "en", "british": "en", "american": "en",
		"hindi": "hi", "hin": "hi", "spanish": "es", "spa": "es", "es": "es", "castilian": "es",
		"french": "fr", "fre": "fr", "fra": "fr", "fr": "fr", "german": "de", "ger": "de", "deu": "de", "de": "de",
		"italian": "it", "ita": "it", "it": "it", "portuguese": "pt", "por": "pt", "pt": "pt", "brazilian": "pt",
		"chinese": "zh", "chi": "zh", "zho": "zh", "zh": "zh", "japanese": "ja", "jpn": "ja", "ja": "ja",
		"korean": "ko", "kor": "ko", "ko": "ko", "dutch": "nl", "dut": "nl", "nld": "nl", "nl": "nl",
		"finnish": "fi", "fin": "fi", "fi": "fi", "hebrew": "he", "heb": "he", "he": "he", "indonesian": "id", "ind": "id",
		"swedish": "sv", "swe": "sv", "sv": "sv", "thai": "th", "tha": "th", "th": "th", "greek": "el", "gre": "el",
		"ell": "el", "el": "el", "danish": "da", "dan": "da", "da": "da", "turkish": "tr", "tur": "tr", "tr": "tr",
		"czech": "cs", "cze": "cs", "ces": "cs", "cs": "cs", "hungarian": "hu", "hun": "hu", "hu": "hu",
		"russian": "ru", "rus": "ru", "ru": "ru", "arabic": "ar", "ara": "ar", "ar": "ar", "polish": "pl", "pol": "pl",
		"pl": "pl", "malay": "ms", "may": "ms", "msa": "ms", "vietnamese": "vi", "vie": "vi", "vi": "vi",
		"bokmal": "nb", "norwegian": "no", "nor": "no", "nob": "nb", "nno": "nn", "romanian": "ro", "rum": "ro", "ron": "ro",
		"ro": "ro", "ukrainian": "uk", "ukr": "uk", "croatian": "hr", "hrv": "hr", "slovenian": "sl", "slo": "sk",
		"slv": "sl", "slovak": "sk", "slk": "sk", "bulgarian": "bg", "bul": "bg", "serbian": "sr", "srp": "sr",
		"lithuanian": "lt", "lit": "lt", "latvian": "lv", "lav": "lv", "estonian": "et", "est": "et",
		"icelandic": "is", "ice": "is", "isl": "is", "catalan": "ca", "cat": "ca", "basque": "eu", "baq": "eu",
		"galician": "gl", "glg": "gl", "persian": "fa", "per": "fa", "fas": "fa", "tamil": "ta", "tam": "ta",
		"telugu": "te", "tel": "te", "bengali": "bn", "ben": "bn", "marathi": "mr", "mar": "mr", "urdu": "ur",
		"urd": "ur", "malayalam": "ml", "mal": "ml", "kannada": "kn", "kan": "kn", "filipino": "tl", "fil": "tl",
		"tagalog": "tl", "tgl": "tl", "simplified": "zh", "traditional": "zh", "cantonese": "zh", "mac": "mk",
		"macedonian": "mk", "ewe": "ee",
	}
	flagSDH    = regexp.MustCompile(`(?i)\b(sdh|cc|hearing[ ._-]?impaired)\b`)
	flagForced = regexp.MustCompile(`(?i)\b(forced|foreign)\b`)
	asciiWords = regexp.MustCompile(`[a-z]+`)
	hiTag      = regexp.MustCompile(`(^|[ .])hi($|[ .])`)

	// Subtitle-to-video matches that needed a person (paths relative to Movies/); decided once.
	manualSubOwner = map[string]string{
		"Batman Mask of the Phantasm/Batman - Mask of The Phantasm, Open Matte Version (1993 - 1080p BluRay).en.srt": "Batman Mask of the Phantasm/Batman - Mask of The Phantasm(1993 - 1080p BluRay).mp4",
	}
)

// langUnsureHI is parse_name's "hi?": a bare "hi" tag that is either Hindi or hearing-impaired English.
const langUnsureHI = "hi?"

// parseName reads language, SDH and forced flags from the text after the video's stem ('.en.sdh', '_Hindi').
func parseName(tokens string) (lang string, sdh, forced bool) {
	low := strings.NewReplacer("_", " ", ",", " ").Replace(strings.ToLower(tokens))
	flat := strings.ReplaceAll(low, ".", " ")
	ws := asciiWords.FindAllString(low, -1)
	for i := len(ws) - 1; i >= 0; i-- {
		if l, ok := langWords[ws[i]]; ok && ws[i] != "hi" && ws[i] != "cc" {
			lang = l
			break
		}
	}
	tagged := hiTag.MatchString(low)
	sdh = flagSDH.MatchString(flat) || (tagged && lang != "")
	if lang == "" && tagged {
		lang = langUnsureHI
	}
	return lang, sdh, flagForced.MatchString(flat)
}

type subOwner struct{ video, rest string }

type stemVideo struct{ stem, video string }

// attachSubtitles ties every subtitle it can to a video (live or deleted), keyed by subtitle path. moved maps a video's
// old path to its current one (the rename logs), so a subtitle left in the old folder still finds its video: there it
// is matched against the name and folder the video had when the subtitle was made.
func (p *planner) attachSubtitles(root string, files []string, videos *videoPlan) map[string]subOwner {
	vidsInDir := map[string][]stemVideo{}
	videoDirs := map[string]bool{}
	for _, f := range files {
		if videoExt[lowerExt(f)] {
			videoDirs[path.Dir(f)] = true
		}
	}
	for _, o := range sortedKeys(p.in.Moved) {
		videoDirs[path.Dir(o)] = true
	}
	byCode := map[string]map[epCode]string{}
	add := func(at, v string) {
		x := videos.by[v]
		vidsInDir[path.Dir(at)] = append(vidsInDir[path.Dir(at)], stemVideo{stem(path.Base(at)), v})
		if strings.HasPrefix(x.kind, "episode") && strings.HasPrefix(at, p.tv+"/") {
			if code, ok := episodeCode(x.stem); ok {
				top := relParts(p.tv, at)[0]
				if byCode[top] == nil {
					byCode[top] = map[epCode]string{}
				}
				if _, seen := byCode[top][code]; !seen {
					byCode[top][code] = v
				}
			}
		}
	}
	for _, v := range videos.order {
		add(v, v)
	}
	for _, o := range sortedKeys(p.in.Moved) {
		if v := p.in.Moved[o]; videos.has(v) && !videos.has(o) {
			add(o, v)
		}
	}
	owner := map[string]subOwner{}
	for _, f := range files {
		if !subExt[lowerExt(f)] || strings.Contains(f, "@eaDir") {
			continue
		}
		parent, name := path.Dir(f), path.Base(f)
		if v, ok := manualSubOwner[strings.TrimPrefix(f, p.movies+"/")]; ok && strings.HasPrefix(f, p.movies+"/") && videos.has(p.movies+"/"+v) {
			v = p.movies + "/" + v
			rest := name
			if vs := stem(path.Base(v)); strings.HasPrefix(name, vs) {
				rest = name[len(vs):]
			}
			owner[f] = subOwner{v, rest}
			continue
		}
		prefixed := func(list []stemVideo) []stemVideo {
			var out []stemVideo
			for _, c := range list {
				if strings.HasPrefix(name, c.stem) {
					out = append(out, c)
				}
			}
			return out
		}
		cands := prefixed(vidsInDir[parent])
		if lowParent := strings.ToLower(path.Base(parent)); len(cands) == 0 && (lowParent == "subs" || lowParent == "subtitles" || lowParent == "sub") {
			up := vidsInDir[path.Dir(parent)]
			if len(up) == 1 {
				cands = up
			} else {
				cands = prefixed(up)
			}
		}
		if low := strings.ToLower(path.Base(path.Dir(parent))); len(cands) == 0 && (low == "subs" || low == "subtitles") {
			for _, c := range vidsInDir[path.Dir(path.Dir(parent))] {
				if c.stem == path.Base(parent) {
					cands = append(cands, c)
				}
			}
		}
		if same := vidsInDir[parent]; len(cands) == 0 && len(same) == 1 {
			cands = same
		}
		if up := vidsInDir[path.Dir(parent)]; len(cands) == 0 && !videoDirs[parent] && len(up) == 1 {
			cands = up // 'Subtitle,info/<file>': one video one level up; its own folder never held one
		}
		if here := vidsInDir[parent]; len(cands) == 0 && len(here) > 1 { // several videos: distinctive shared words only
			mine := words(name)
			type score struct {
				n     int
				video string
			}
			scored := make([]score, 0, len(here))
			for _, c := range here {
				n := 0
				for w := range words(c.stem) {
					if mine[w] {
						n++
					}
				}
				scored = append(scored, score{n, c.video})
			}
			sort.Slice(scored, func(i, j int) bool {
				if scored[i].n != scored[j].n {
					return scored[i].n > scored[j].n
				}
				return scored[i].video > scored[j].video
			})
			if scored[0].n >= 2 && scored[0].n > scored[1].n {
				owner[f] = subOwner{scored[0].video, name}
				continue
			}
		}
		if len(cands) == 0 && strings.HasPrefix(f, p.tv+"/") {
			code, ok := episodeCode(name)
			if !ok {
				code, ok = episodeCode(path.Base(parent))
			}
			if v := byCode[relParts(p.tv, f)[0]][code]; ok && v != "" {
				owner[f] = subOwner{v, name}
				continue
			}
		}
		if len(cands) == 0 {
			continue
		}
		best := cands[0]
		for _, c := range cands[1:] {
			if len(c.stem) > len(best.stem) {
				best = c
			}
		}
		if len(cands) > 1 && !strings.HasPrefix(name, best.stem) {
			continue
		}
		rest := name
		if strings.HasPrefix(name, best.stem) {
			rest = name[len(best.stem):]
		}
		owner[f] = subOwner{best.video, rest}
	}
	return owner
}

type subDecision struct{ unit, dst, reason string } // dst "" with a reason: left for review

type subEntry struct {
	sub, lang, ext       string
	sdh, forced, aligned bool
	beside               bool
	f                    SubFacts
}

// nameSubtitles decides every attached subtitle: its name beside the video, an archive path, or left (language unknown).
func (p *planner) nameSubtitles(owner map[string]subOwner, videos *videoPlan) map[string]subDecision {
	in := p.in
	result := map[string]subDecision{}
	groups := map[string][]*subEntry{}
	var groupOrder []string
	for _, sub := range sortedKeys(owner) {
		o := owner[sub]
		f, ok := in.Subs[sub]
		if !ok {
			f = SubFacts{SHA1: sub, Size: in.Files[sub]}
		}
		tokens := o.rest
		if i := strings.LastIndexByte(tokens, '.'); i >= 0 {
			tokens = tokens[:i]
		}
		lang, sdh, forced := parseName(tokens)
		ext := lowerExt(sub)
		if text := scriptLang[f.Script]; text != "" && (lang == "" || lang == langUnsureHI || lang == "en" || text == "hi") {
			lang = text // the text decides (UTF-8/BOM-aware decoding)
		}
		if lang == langUnsureHI {
			if f.Script == "latin" {
				lang, sdh = "en", true
			} else {
				lang, sdh = "hi", false
			}
		}
		if lang == "" && f.Guess != "" {
			lang = f.Guess // the name says nothing: the text's own stop words decide
		}
		if l, ok := in.SubLang[sub]; lang == "" && ok {
			lang = l.Lang
			if lang == "" {
				lang = "none"
			}
			sdh = sdh || l.SDH
		}
		if ext == ".idx" || ext == ".sub" {
			var langs []string
			seen := map[string]bool{}
			for _, l := range strings.Split(in.Subs[strings.TrimSuffix(sub, suffix(path.Base(sub)))+".idx"].Idx, ",") {
				if l != "" && !seen[l] {
					seen[l] = true
					langs = append(langs, l)
				}
			}
			if lang == "" {
				switch {
				case seen["en"]:
					lang = "en"
				case len(langs) == 1:
					lang = langs[0]
				}
			}
		}
		if textSubExt[ext] && lang == "en" && f.SDH >= 0.05 {
			sdh = true
		}
		if groups[o.video] == nil {
			groupOrder = append(groupOrder, o.video)
		}
		groups[o.video] = append(groups[o.video], &subEntry{sub: sub, lang: lang, ext: ext, sdh: sdh, forced: forced, f: f,
			aligned: in.Aligned[path.Base(sub)],
			beside:  path.Dir(sub) == path.Dir(o.video) && strings.HasPrefix(path.Base(sub), stem(path.Base(o.video)))})
	}
	for _, video := range groupOrder {
		subs := groups[video]
		v := videos.by[video]
		archive := func(s *subEntry, why string) {
			unit := v.unit
			if unit == dead {
				unit = "misc"
			}
			result[s.sub] = subDecision{unit, p.archive + "/subtitles/" + strings.TrimPrefix(s.sub, p.media+"/"), "archive: " + why}
		}
		if v.kind == "deleted" {
			for _, s := range subs {
				archive(s, "sidecar of a deleted duplicate video")
			}
			continue
		}
		byHash := map[string][]*subEntry{} // identical bytes: keep the copy beside the video; it inherits every flag
		var hashOrder []string
		for _, s := range subs {
			if byHash[s.f.SHA1] == nil {
				hashOrder = append(hashOrder, s.f.SHA1)
			}
			byHash[s.f.SHA1] = append(byHash[s.f.SHA1], s)
		}
		var live []*subEntry
		for _, h := range hashOrder {
			same := byHash[h]
			sort.SliceStable(same, func(i, j int) bool {
				a, b := same[i], same[j]
				if a.beside != b.beside {
					return a.beside
				}
				if a.aligned != b.aligned {
					return a.aligned
				}
				return a.sub < b.sub
			})
			keep := same[0]
			for _, s := range same {
				keep.sdh = keep.sdh || s.sdh
				keep.forced = keep.forced || s.forced
				keep.aligned = keep.aligned || s.aligned
			}
			for _, s := range same {
				if s.lang != "" && s.lang != "none" {
					keep.lang = s.lang
					break
				}
			}
			live = append(live, keep)
			for _, s := range same[1:] {
				archive(s, "identical copy of "+path.Base(keep.sub))
			}
		}
		taken := map[string]bool{}
		assign := func(s *subEntry, sfx, why string) {
			target := v.folder + "/" + v.stem + "." + sfx + s.ext
			if taken[target] {
				archive(s, "extra copy ("+why+")")
				return
			}
			taken[target] = true
			result[s.sub] = subDecision{v.unit, target, why}
		}
		pairs := map[string][]*subEntry{} // VobSub: idx + sub keep one shared name
		var pairOrder []string
		for _, s := range live {
			if s.ext == ".idx" || s.ext == ".sub" {
				base := strings.TrimSuffix(s.sub, suffix(path.Base(s.sub)))
				if pairs[base] == nil {
					pairOrder = append(pairOrder, base)
				}
				pairs[base] = append(pairs[base], s)
			}
		}
		inPair := map[*subEntry]bool{}
		for _, base := range pairOrder {
			pair := pairs[base]
			lang := ""
			for _, s := range pair {
				if s.lang != "" {
					lang = s.lang
					break
				}
			}
			for _, s := range pair {
				inPair[s] = true
				target := v.folder + "/" + v.stem + "." + langOrNone(lang) + s.ext
				if (lang == "en" || lang == "hi") && !taken[target] {
					taken[target] = true
					result[s.sub] = subDecision{v.unit, target, "vobsub pair (" + lang + " in idx)"}
				} else {
					unknown := lang
					if unknown == "" {
						unknown = "unknown"
					}
					archive(s, "vobsub pair, language "+unknown)
				}
			}
		}
		var rest []*subEntry
		for _, s := range live {
			if !inPair[s] {
				rest = append(rest, s)
			}
		}
		live = rest
		for _, s := range live {
			switch {
			case s.f.Ad || s.lang == "none":
				archive(s, "ad-only or no dialogue")
			case textSubExt[s.ext] && s.lang == "en" && !s.forced && (s.f.Cues < 20 || s.f.Size < 6000):
				s.forced = true // a handful of lines: foreign-dialogue-only track
			}
		}
		rest = nil
		for _, s := range live {
			if _, done := result[s.sub]; !done {
				rest = append(rest, s)
			}
		}
		live = rest
		for _, lang := range keepLangs {
			var mine, plain, sdhFull, forcedOnes []*subEntry
			for _, s := range live {
				if s.lang == lang {
					mine = append(mine, s)
				}
			}
			for _, s := range mine {
				if !s.sdh && !s.forced {
					plain = append(plain, s)
				}
			}
			sort.SliceStable(plain, func(i, j int) bool {
				a, b := plain[i], plain[j]
				switch {
				case a.aligned != b.aligned:
					return a.aligned
				case a.beside != b.beside:
					return a.beside
				case a.f.Cues != b.f.Cues:
					return a.f.Cues > b.f.Cues
				case a.f.Size != b.f.Size:
					return a.f.Size > b.f.Size
				}
				return a.sub < b.sub
			})
			if len(plain) == 0 { // the only full track is SDH: name it plain so 'subtitles always on' picks it in both apps
				for _, s := range mine {
					if s.sdh && !s.forced {
						sdhFull = append(sdhFull, s)
					}
				}
				sort.SliceStable(sdhFull, func(i, j int) bool {
					a, b := sdhFull[i], sdhFull[j]
					switch {
					case a.f.SDH != b.f.SDH:
						return a.f.SDH < b.f.SDH
					case a.f.Cues != b.f.Cues:
						return a.f.Cues > b.f.Cues
					}
					return a.sub < b.sub
				})
				if len(sdhFull) > 0 {
					sdhFull[0].sdh = false
					plain = sdhFull[:1]
				}
			}
			for i, s := range plain {
				switch {
				case i == 0:
					why := lang + " primary"
					if s.aligned {
						why += " (aligned)"
					}
					assign(s, lang, why)
				case i == 1 && lang == "en":
					assign(s, "eng", "english, second full track")
				default:
					archive(s, "extra "+lang+" track")
				}
			}
			byCues := func(list []*subEntry) {
				sort.SliceStable(list, func(i, j int) bool {
					if list[i].f.Cues != list[j].f.Cues {
						return list[i].f.Cues > list[j].f.Cues
					}
					return list[i].sub < list[j].sub
				})
			}
			var sdhOnes []*subEntry
			for _, s := range mine {
				if s.sdh && !s.forced {
					sdhOnes = append(sdhOnes, s)
				}
				if s.forced {
					forcedOnes = append(forcedOnes, s)
				}
			}
			byCues(sdhOnes)
			for i, s := range sdhOnes {
				if i == 0 {
					assign(s, lang+".sdh", lang+" sdh")
				} else {
					archive(s, "extra "+lang+" sdh track")
				}
			}
			byCues(forcedOnes)
			for i, s := range forcedOnes {
				if i == 0 {
					assign(s, lang+".forced", lang+" forced")
				} else {
					archive(s, "extra "+lang+" forced track")
				}
			}
		}
		for _, s := range live {
			if _, done := result[s.sub]; done {
				continue
			}
			if s.lang == "" {
				result[s.sub] = subDecision{v.unit, "", "subtitle language unknown"}
			} else {
				archive(s, "language "+s.lang)
			}
		}
	}
	return result
}

// langOrNone is Python's f"{lang}" for a language that may be None.
func langOrNone(lang string) string {
	if lang == "" {
		return "None"
	}
	return lang
}
