package rename

// The planner: every file under Movies/ and TV/ gets exactly one decision (port of rename_engine.py). It is a pure
// function of Input, so a run plans the whole library in milliseconds and the executor acts only on the units that
// own new work. Identity (which film or show a video is) is an input; nothing here reads Plex.
//
//	op = move     rename on the same volume (instant)        archive  move under <root>/_archive (reversible)
//	     keep     already at its final path                  delete   identical copy of a kept file (bytes compared when acting)
//	     leave    deliberately not touched; listed for review

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Title is a film or show as the metadata source (TMDB) names it.
type Title struct {
	ID       string // the scheme's tag body: "tmdb-348", "imdb-tt0078748", "tvdb-79169"
	Name     string
	Original string // original-language title; "" when unknown
	Year     int
}

type VideoFacts struct {
	Width, Height int
	Codec         string // ffprobe codec_name: hevc, h264, ...
}

// SubLang is a language decision made outside the name (a text-based guess); Lang "none" means no dialogue.
type SubLang struct {
	Lang string
	SDH  bool
}

type EpisodeKey struct{ Season, Episode int }

// Input is everything the planner decides from. Paths are absolute; Files lists every file under Movies/ and TV/.
type Input struct {
	Root          string // the media share, e.g. /volume1/media
	Files         map[string]int64
	Movies        map[string]Title // video → its film
	Shows         map[string]Title // video → its show
	EpisodeTitles map[string]map[EpisodeKey]string
	Video         map[string]VideoFacts
	Subs          map[string]SubFacts
	Aligned       map[string]bool // subtitle file names the nightly aligner wrote
	SubLang       map[string]SubLang
}

type Op struct{ Unit, Op, Src, Dst, Reason string }

type Review struct{ Unit, Item, Issue string }

type Plan struct {
	Ops      []Op
	Review   []Review
	Problems map[string][]string // unit → reasons it must not run (collisions, existing targets, bad names)
}

const (
	bigFile = 1_500_000_000
	maxName = 255
	dead    = "dead"
)

// Subtitle-to-video matches and version labels that needed a person (paths relative to Movies/); decided once.
var (
	manualSubOwner = map[string]string{
		"Batman Mask of the Phantasm/Batman - Mask of The Phantasm, Open Matte Version (1993 - 1080p BluRay).en.srt": "Batman Mask of the Phantasm/Batman - Mask of The Phantasm(1993 - 1080p BluRay).mp4",
	}
	manualLabel = map[string]string{"Batman Mask of the Phantasm/Batman - Mask of The Phantasm(1993 - 1080p BluRay).mp4": "1080p Open Matte"}
	episodeN    = regexp.MustCompile(`(?i)^episode \d+$`)
)

type planned struct{ unit, folder, stem, ext, kind string }

// videoPlan keeps insertion order: subtitle matching breaks ties by it, as the Python dict did.
type videoPlan struct {
	order []string
	by    map[string]planned
}

func (v *videoPlan) set(p string, x planned) {
	if _, ok := v.by[p]; !ok {
		v.order = append(v.order, p)
	}
	v.by[p] = x
}

type planner struct {
	in                         *Input
	media, movies, tv, archive string
	ops                        []Op
	review                     []Review
}

func (p *planner) op(unit, op, src, dst, reason string) {
	p.ops = append(p.ops, Op{unit, op, src, dst, reason})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func depthThenPath(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
}

func relParts(root, p string) []string { return strings.Split(strings.TrimPrefix(p, root+"/"), "/") }

// Build plans the whole library.
func Build(in *Input) Plan {
	p := &planner{in: in, media: in.Root, movies: in.Root + "/Movies", tv: in.Root + "/TV", archive: in.Root + "/_archive"}
	movies := p.planMovies()
	shows := p.planTV()
	p.planRoot(p.movies, movies)
	p.planRoot(p.tv, shows)
	plan := Plan{Ops: p.ops, Review: p.review, Problems: check(p.ops, in.Files)}
	reviewed := map[string]bool{}
	for _, r := range plan.Review {
		reviewed[r.Item] = true
	}
	for _, o := range plan.Ops {
		if o.Op == "leave" && !reviewed[o.Src] {
			plan.Review = append(plan.Review, Review{o.Unit, o.Src, o.Reason})
		}
	}
	return plan
}

// ---- movies

func (p *planner) planMovies() *videoPlan {
	in := p.in
	videos := &videoPlan{by: map[string]planned{}}
	var ids []string
	parts := map[string][]string{}
	for _, f := range sortedKeys(in.Movies) {
		if _, ok := in.Files[f]; !ok {
			continue
		}
		id := in.Movies[f].ID
		if parts[id] == nil {
			ids = append(ids, id)
		}
		parts[id] = append(parts[id], f)
	}
	for _, id := range ids {
		unit, files := "movie:"+id, parts[id]
		first := in.Movies[files[0]]
		if !strings.HasPrefix(id, "tmdb-") || first.Year == 0 {
			for _, f := range files {
				p.op(unit, "leave", f, "", "movie has no TMDB id or year: rematch first")
			}
			p.review = append(p.review, Review{unit, first.Name, fmt.Sprintf("match incomplete (year=%d id=%s): rematch", first.Year, id)})
			continue
		}
		base := component(withYear(first.Name, first.Year) + " {" + id + "}")
		folder := p.movies + "/" + base
		var largest int64
		for _, f := range files {
			largest = max(largest, in.Files[f])
		}
		var real []string
		for _, f := range files {
			name, size, rel := path.Base(f), in.Files[f], relParts(p.movies, f)
			isMain := size == largest && size > bigFile // never treat the film itself as a sample
			small := len(files) > 1 && float64(size) < 0.05*float64(largest) && size < bigFile
			switch {
			case !isMain && (sampleRe.MatchString(name) || small):
				p.op(unit, "archive", f, p.archive+"/samples/Movies/"+strings.Join(rel, "/"), "sample/ad clip filed as a version")
			case len(rel) >= 3 && rel[0] != "Remote Versions" && extraFolder.MatchString(rel[len(rel)-2]):
				videos.set(f, planned{unit, folder + "/" + extraCategory(rel[:len(rel)-1]), component(clean(stem(name))), lowerExt(f), "extra"})
			default:
				real = append(real, f)
			}
		}
		depthThenPath(real)
		var keep []string
		for _, f := range real {
			twin := ""
			for _, k := range keep {
				if in.Files[k] == in.Files[f] && suffix(path.Base(k)) == suffix(path.Base(f)) {
					twin = k
					break
				}
			}
			if twin == "" {
				keep = append(keep, f)
				continue
			}
			p.op(unit, "delete", f, twin, "identical copy of another version (bytes compared at run time)")
			videos.set(f, planned{unit: dead, kind: "deleted"})
		}
		if len(keep) == 0 {
			continue
		}
		labels := p.movieLabels(keep, folder, base)
		if labels == nil {
			continue
		}
		for i, f := range keep {
			s := base
			if len(keep) > 1 {
				s = component(base + " - " + labels[i])
			}
			videos.set(f, planned{unit, folder, s, lowerExt(f), "movie"})
		}
	}
	return videos
}

// movieLabels names each version; nil when two still share a name (then every version is left for review).
func (p *planner) movieLabels(keep []string, folder, base string) []string {
	in := p.in
	labels := make([]string, len(keep))
	for i, f := range keep {
		if s := stem(path.Base(f)); path.Dir(f) == folder && strings.HasPrefix(s, base+" - ") {
			labels[i] = s[len(base)+3:] // already named by this scheme ('1080p Remote', '2160p 14Mbps'): re-runs keep it
		} else if l := manualLabel[strings.TrimPrefix(f, p.movies+"/")]; l != "" {
			labels[i] = l
		} else {
			labels[i] = resolution(in.Video[f].Width, in.Video[f].Height)
			if strings.Contains(f, "/Remote Versions/") {
				labels[i] += " Remote"
			}
		}
	}
	if len(keep) == 1 {
		return labels
	}
	codec := func(f string) string { return strings.ToUpper(in.Video[f].Codec) }
	refines := []func(string) string{
		cutWord,
		func(f string) string {
			if strings.Contains(strings.ToLower(f), "remux") {
				return "Remux"
			}
			return codec(f)
		},
		func(f string) string { return clean(releaseGroup(f)) },
		func(f string) string { return fmt.Sprintf("%.1fGB", float64(in.Files[f])/1e9) },
	}
	for _, refine := range refines {
		counts := map[string]int{}
		for _, l := range labels {
			counts[l]++
		}
		for i, f := range keep {
			if counts[labels[i]] < 2 {
				continue
			}
			if extra := refine(f); extra != "" && !strings.Contains(labels[i], extra) {
				labels[i] += " " + extra
			}
		}
	}
	for i := range labels { // no version name may be a prefix of another (Jellyfin attaches subs by prefix)
		for _, o := range labels {
			if o != labels[i] && strings.HasPrefix(o, labels[i]+" ") {
				extra := cutWord(keep[i])
				if extra == "" {
					extra = codec(keep[i])
				}
				if extra == "" {
					extra = "LOCAL" // Python's ('Local').upper()
				}
				labels[i] += " " + extra
				break
			}
		}
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if seen[l] {
			unit := "movie:" + p.in.Movies[keep[0]].ID
			for _, f := range keep {
				p.op(unit, "leave", f, "", fmt.Sprintf("versions still share a label %q", labels))
			}
			p.review = append(p.review, Review{unit, p.in.Movies[keep[0]].Name, fmt.Sprintf("versions share a label: %q", labels)})
			return nil
		}
		seen[l] = true
	}
	return labels
}

// ---- tv

type candidate struct {
	unit, showDir     string
	season, ep, last  int
	title, reason     string
	show              Title
}

func (p *planner) planTV() *videoPlan {
	in := p.in
	videos := &videoPlan{by: map[string]planned{}}
	cands := map[string]candidate{}
	var order []string
	for _, f := range sortedKeys(in.Shows) {
		if _, ok := in.Files[f]; !ok {
			continue
		}
		show := in.Shows[f]
		unit := "show:" + show.ID
		if !strings.HasPrefix(show.ID, "tmdb-") && !strings.HasPrefix(show.ID, "tvdb-") {
			p.op(unit, "leave", f, "", "show has no TMDB/TVDB id")
			p.review = append(p.review, Review{unit, f, "show has no TMDB/TVDB id"})
			continue
		}
		showDir := p.tv + "/" + component(withYear(show.Name, show.Year)+" {"+show.ID+"}")
		name, parent := path.Base(f), path.Base(path.Dir(f))
		if extraFolder.MatchString(parent) {
			videos.set(f, planned{unit, showDir + "/" + extraCategory([]string{parent}), component(clean(stem(name))), lowerExt(f), "extra"})
			continue
		}
		if !sameShow(showPrefix(name), show.Name, show.Original) {
			p.op(unit, "leave", f, "", fmt.Sprintf("file names a different show than its match (%s)", show.Name))
			p.review = append(p.review, Review{unit, f, fmt.Sprintf("matched to %q; the file name says otherwise: rematch", show.Name)})
			continue
		}
		num, ok := episodeNumbers(p.tv, f)
		if !ok {
			p.op(unit, "leave", f, "", "no episode number in the name")
			continue
		}
		season, ep, last := num.season, num.first, num.last
		if ep == 0 && season > 0 {
			p.op(unit, "leave", f, "", "SxxE00 special: needs its Season 00 number")
			p.review = append(p.review, Review{unit, f, "special numbered as episode 0 of a season: map to Season 00 by hand"})
			continue
		}
		known := in.EpisodeTitles[show.ID][EpisodeKey{season, ep}]
		title, reason := "", "episode"
		switch {
		case last > ep:
			title = clean(num.title)
		case known != "" && num.title != "" && !titlesAgree(num.title, known):
			title, reason = clean(num.title), "episode (kept the file's own title: TMDB numbering differs)"
		case known != "":
			title = clean(known)
		default:
			title = clean(num.title)
		}
		if episodeN.MatchString(title) {
			title = ""
		}
		if r := []rune(title); len(r) > 80 {
			title = string(r[:80])
		}
		cands[f] = candidate{unit, showDir, season, ep, last, title, reason, show}
		order = append(order, f)
	}
	type key struct {
		unit        string
		season, ep  int
	}
	perEp := map[key]int{}
	groups := map[key][]string{}
	var groupOrder []key
	for _, f := range order {
		c := cands[f]
		k := key{c.unit, c.season, c.ep}
		if groups[k] == nil {
			groupOrder = append(groupOrder, k)
		}
		groups[k] = append(groups[k], f)
		perEp[k]++
	}
	for _, k := range groupOrder { // identical copies of one episode (Sonny Boy x2): keep one, delete the rest
		olds := groups[k]
		if len(olds) < 2 {
			continue
		}
		depthThenPath(olds)
		for _, o := range olds[1:] {
			if in.Files[o] == in.Files[olds[0]] && suffix(path.Base(o)) == suffix(path.Base(olds[0])) {
				p.op(k.unit, "delete", o, olds[0], "identical copy of another episode file (bytes compared at run time)")
				videos.set(o, planned{unit: dead, kind: "deleted"})
				delete(cands, o)
				perEp[k]--
			}
		}
	}
	for _, f := range order {
		c, ok := cands[f]
		if !ok {
			continue
		}
		code := fmt.Sprintf("S%02dE%02d", c.season, c.ep)
		if c.last > c.ep {
			code += fmt.Sprintf("-E%02d", c.last)
		}
		s := withYear(c.show.Name, c.show.Year) + " - " + code
		if c.title != "" {
			s += " - " + c.title
		}
		if perEp[key{c.unit, c.season, c.ep}] > 1 {
			label := clean(releaseGroup(f))
			if label == "" {
				label = fmt.Sprintf("%.1fGB", float64(in.Files[f])/1e9)
			}
			s += fmt.Sprintf(" [%s %s]", resolution(in.Video[f].Width, in.Video[f].Height), label)
		}
		videos.set(f, planned{c.unit, fmt.Sprintf("%s/Season %02d", c.showDir, c.season), component(s), lowerExt(f), c.reason})
	}
	return videos
}

// ---- everything else under a root: sidecars, extras the resolver does not list, junk

func (p *planner) planRoot(root string, videos *videoPlan) {
	in := p.in
	rootName := path.Base(root)
	owner := p.attachSubtitles(root, videos)
	subs := p.nameSubtitles(owner, videos)
	taken := map[string]bool{}
	unitHome := map[string]string{}
	planned := map[string]map[string]bool{} // folder → units with a planned video below it
	for _, v := range videos.order {
		x := videos.by[v]
		if x.unit == dead {
			continue
		}
		if _, ok := unitHome[x.unit]; !ok {
			if x.kind == "movie" {
				unitHome[x.unit] = x.folder
			} else {
				unitHome[x.unit] = path.Dir(x.folder)
			}
		}
		for d := path.Dir(v); strings.HasPrefix(d, root) && d != root; d = path.Dir(d) {
			if planned[d] == nil {
				planned[d] = map[string]bool{}
			}
			planned[d][x.unit] = true
		}
	}
	for _, f := range sortedKeys(in.Files) {
		if !strings.HasPrefix(f, root+"/") {
			continue
		}
		rel := relParts(root, f)
		name, ext, size := path.Base(f), lowerExt(f), in.Files[f]
		if contains(rel, "@eaDir") {
			continue
		}
		if root == p.movies && rel[0] == "POSTERS" {
			p.op("assets", "archive", f, p.media+"/_assets/POSTERS/"+strings.Join(rel[1:], "/"), "poster artwork, kept outside the libraries")
			continue
		}
		if low := strings.ToLower(f); root == p.tv && (strings.Contains(low, "don't need") || strings.Contains(low, "dont need")) {
			p.op("misc", "archive", f, p.archive+"/junk/TV/"+strings.Join(rel, "/"), "folder named 'Subs that you don't need'")
			continue
		}
		if ignoreName[name] || contains(rel, "#recycle") {
			continue
		}
		if len(rel) >= 2 && rel[len(rel)-2] == ".details" { // write_details notes travel with their video; never junk
			if v, ok := videos.by[path.Dir(path.Dir(f))+"/"+stem(name)]; ok && v.unit != dead {
				dst := v.folder + "/.details/" + v.stem + v.ext + ".txt"
				p.op(v.unit, keepOr(dst, f, "move"), f, dst, "details note")
			}
			continue
		}
		if v, ok := videos.by[f]; ok {
			if v.unit != dead {
				dst := v.folder + "/" + v.stem + v.ext
				p.op(v.unit, keepOr(dst, f, "move"), f, dst, v.kind)
			}
			continue
		}
		if s, ok := subs[f]; ok {
			op := "leave"
			switch {
			case s.dst == "":
			case s.dst == f:
				op = "keep"
			case strings.HasPrefix(s.dst, p.archive):
				op = "archive"
			default:
				op = "move"
			}
			p.op(s.unit, op, f, s.dst, s.reason)
			continue
		}
		ownerUnit := ""
		for d := path.Dir(f); d != root && strings.HasPrefix(d, root); d = path.Dir(d) {
			if units := planned[d]; len(units) > 0 {
				if len(units) == 1 {
					for u := range units {
						ownerUnit = u
					}
				}
				break
			}
		}
		unitOrMisc := ownerUnit
		if unitOrMisc == "" {
			unitOrMisc = "misc"
		}
		switch {
		case videoExt[ext]:
			extraDir := ""
			for i := len(rel) - 2; i >= 0; i-- {
				if extraFolder.MatchString(rel[i]) {
					extraDir = rel[i]
					break
				}
			}
			home := unitHome[ownerUnit]
			switch {
			case sampleRe.MatchString(name) && size < 600_000_000:
				p.op(unitOrMisc, "archive", f, p.archive+"/samples/"+rootName+"/"+strings.Join(rel, "/"), "sample clip")
			case extraDir != "" && ownerUnit != "" && home != "":
				cat := extraCategory(rel[:len(rel)-1])
				dst := home + "/" + cat + "/" + component(clean(stem(name))) + ext
				if taken[casefold.String(dst)] {
					dst = home + "/" + cat + "/" + component(clean(stem(name))+" ("+clean(path.Base(path.Dir(f)))+")") + ext
				}
				taken[casefold.String(dst)] = true
				p.op(ownerUnit, keepOr(dst, f, "move"), f, dst, "extra ("+extraDir+")")
			default:
				p.op(unitOrMisc, "leave", f, "", "video no metadata source lists")
			}
		case subExt[ext]:
			p.op(unitOrMisc, "archive", f, p.archive+"/subtitles/"+rootName+"/"+strings.Join(rel, "/"), "subtitle with no matching video")
		default:
			what := ext
			if what == "" {
				what = "no extension"
			}
			p.op(unitOrMisc, "archive", f, p.archive+"/junk/"+rootName+"/"+strings.Join(rel, "/"), "junk ("+what+")")
		}
	}
}

func keepOr(dst, src, op string) string {
	if dst == src {
		return "keep"
	}
	return op
}

func contains(parts []string, s string) bool {
	for _, x := range parts {
		if x == s {
			return true
		}
	}
	return false
}

var illegalChar = regexp.MustCompile(`[:*?"<>|]`)

// check finds what would make a unit unsafe to run: two files onto one name (case-insensitively, as SMB sees them),
// a target that already exists, a component too long or ending in a dot/space, an SMB-illegal character, or a
// video stem that prefixes another in its folder (Jellyfin attaches subtitles by prefix).
func check(ops []Op, files map[string]int64) map[string][]string {
	problems := map[string][]string{}
	add := func(unit, msg string) {
		for _, m := range problems[unit] {
			if m == msg {
				return
			}
		}
		problems[unit] = append(problems[unit], msg)
	}
	dsts, srcs := map[string]int{}, map[string]int{}
	for _, o := range ops {
		if o.Op == "move" || o.Op == "keep" || o.Op == "archive" {
			dsts[casefold.String(o.Dst)]++
		}
		srcs[o.Src]++
	}
	type stemAt struct{ unit, stem string }
	folders := map[string][]stemAt{}
	for _, o := range ops {
		if srcs[o.Src] > 1 {
			add(o.Unit, "file planned twice: "+o.Src)
		}
		if o.Op == "move" || o.Op == "archive" {
			if dsts[casefold.String(o.Dst)] > 1 {
				add(o.Unit, "collision: "+o.Dst)
			}
			if _, exists := files[o.Dst]; exists && o.Dst != o.Src {
				add(o.Unit, "target exists: "+o.Dst)
			}
			parts := strings.Split(o.Dst, "/")
			for _, c := range parts[1:] {
				if len(c) > maxName || c != strings.TrimRight(c, " .") {
					add(o.Unit, "bad component: "+o.Dst)
					break
				}
			}
			if illegalChar.MatchString(o.Dst) {
				add(o.Unit, "illegal character: "+o.Dst)
			}
		}
		if (o.Op == "move" || o.Op == "keep") && videoExt[lowerExt(o.Dst)] && !extrasNames[path.Base(path.Dir(o.Dst))] {
			folders[path.Dir(o.Dst)] = append(folders[path.Dir(o.Dst)], stemAt{o.Unit, stem(path.Base(o.Dst))})
		}
	}
	for folder, stems := range folders {
		for _, a := range stems {
			for _, b := range stems {
				if b.stem != a.stem && strings.HasPrefix(b.stem, a.stem) {
					add(a.unit, "stem prefix clash in "+folder+": "+a.stem)
				}
			}
		}
	}
	return problems
}
