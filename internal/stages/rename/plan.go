package rename

// The planner: every file under Movies/ and TV/ gets exactly one decision (port of rename_engine.py). It is a pure
// function of Input, so a run plans the whole library in well under a second and the executor acts only on the units
// that own new work. Identity (which film or show a video is) and episode numbers are inputs; nothing here reads Plex.
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

// Title is a film or show: its scheme tag body ("tmdb-348", "tvdb-79169"), name, original-language name and year.
type Title struct {
	ID       string
	Name     string
	Original string
	Year     int
}

// Episode is one episode a TV file holds (a file may hold several), with the episode's title from the metadata source.
type Episode struct {
	Season, Episode int
	Title           string
}

type VideoFacts struct {
	Width, Height int
	Codec         string // ffprobe codec_name: hevc, h264, ...
}

// Input is everything the planner decides from. Paths are absolute; Files lists every file under Movies/ and TV/.
type Input struct {
	Root         string           // the media share, e.g. /volume1/media
	Files        map[string]int64 // path → size
	Movies       map[string]Title // video → its film
	Shows        map[string]Title // video → its show
	Episodes     map[string][]Episode
	Hints        map[string][]Episode // show ID → every episode the source lists; present (even empty) = fetched
	Video        map[string]VideoFacts
	Subs         map[string]SubFacts
	Aligned      map[string]bool    // subtitle file names the nightly aligner wrote
	SubLang      map[string]SubLang // subtitle → language decided by a text classifier
	Moved        map[string]string  // old video path → where it is now (the rename logs)
	Unidentified map[string]string  // video → why no film or show could be named for it
}

type Op struct{ Unit, Op, Src, Dst, Reason string }

type Review struct{ Unit, Item, Issue string }

type Plan struct {
	Ops       []Op
	Review    []Review
	Problems  map[string][]string // unit → reasons it must not run (collisions, existing targets, bad names)
	Unplanned []string            // files no op covers (a planner gap; never acted on)
}

// needsFacts starts the reason of a video left because its label needs a resolution the input lacks.
const needsFacts = "label needs the resolution ffprobe could not read"

const (
	bigFile = 1_500_000_000
	maxName = 255
	dead    = "dead"
)

var (
	manualLabel = map[string]string{"Batman Mask of the Phantasm/Batman - Mask of The Phantasm(1993 - 1080p BluRay).mp4": "1080p Open Matte"}
	// Owner decision: these TV episodes stored inside Movies duplicate the TV library's copies.
	manualTVInMovies = []string{"Sharp.Objects.S01", "Reacher.S03E07"}
	episodeN         = regexp.MustCompile(`(?i)^episode \d+$`)
)

type planned struct{ unit, folder, stem, ext, kind string }

// videoPlan keeps insertion order: subtitle matching breaks ties by it, as the Python dict did.
type videoPlan struct {
	order []string
	by    map[string]planned
}

func newVideoPlan() *videoPlan { return &videoPlan{by: map[string]planned{}} }

func (v *videoPlan) set(p string, x planned) {
	if _, ok := v.by[p]; !ok {
		v.order = append(v.order, p)
	}
	v.by[p] = x
}

func (v *videoPlan) has(p string) bool {
	_, ok := v.by[p]
	return ok
}

func (v *videoPlan) remove(p string) {
	if !v.has(p) {
		return
	}
	delete(v.by, p)
	for i, o := range v.order {
		if o == p {
			v.order = append(v.order[:i], v.order[i+1:]...)
			break
		}
	}
}

// opSet is an insertion-ordered map src → op (Python's {o[2]: o} dicts of special cases).
type opSet struct {
	order []string
	by    map[string]Op
}

func (s *opSet) put(o Op) {
	if s.by == nil {
		s.by = map[string]Op{}
	}
	if _, ok := s.by[o.Src]; !ok {
		s.order = append(s.order, o.Src)
	}
	s.by[o.Src] = o
}

func (s *opSet) ops() []Op {
	out := make([]Op, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.by[k])
	}
	return out
}

type planner struct {
	in                         *Input
	media, movies, tv, archive string
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

func hasPart(parts []string, s string) bool {
	for _, x := range parts {
		if x == s {
			return true
		}
	}
	return false
}

func underRoot(files []string, root string) []string {
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f, root+"/") {
			out = append(out, f)
		}
	}
	return out
}

// Build plans the whole library.
func Build(in *Input) Plan {
	p := &planner{in: in, media: in.Root, movies: in.Root + "/Movies", tv: in.Root + "/TV", archive: in.Root + "/_archive"}
	all := sortedKeys(in.Files)
	mfiles, tfiles := underRoot(all, p.movies), underRoot(all, p.tv)
	movies, mops, mreview := p.planMovies()
	shows, tops, treview := p.planTV()
	var specialM, specialT opSet
	for _, o := range mops {
		specialM.put(o)
	}
	for _, o := range tops {
		specialT.put(o)
	}
	unitOfShowDir := map[string]string{}
	for _, v := range shows.order {
		if x := shows.by[v]; x.unit != dead {
			if top := relParts(p.tv, v)[0]; unitOfShowDir[top] == "" {
				unitOfShowDir[top] = x.unit
			}
		}
	}
	for _, f := range mfiles {
		rel := relParts(p.movies, f)
		if hasPart(rel, "@eaDir") {
			continue
		}
		if rel[0] == "POSTERS" {
			specialM.put(Op{"assets", "archive", f, p.media + "/_assets/POSTERS/" + strings.Join(rel[1:], "/"), "poster artwork, kept outside the libraries"})
			continue
		}
		if !hasPrefixAny(rel[0], manualTVInMovies) {
			continue
		}
		movies.remove(f)
		if !videoExt[lowerExt(f)] {
			specialM.put(Op{"dup:tv-in-movies", "archive", f, p.archive + "/junk/Movies/" + strings.Join(rel, "/"), "sidecar of a deleted duplicate"})
			continue
		}
		twin := ""
		for _, t := range tfiles {
			if path.Base(t) == path.Base(f) && in.Files[t] == in.Files[f] && !strings.Contains(t, "@eaDir") {
				twin = t
				break
			}
		}
		if twin == "" {
			specialM.put(Op{"dup:tv-in-movies", "leave", f, "", "no TV twin found"})
			continue
		}
		unit := unitOfShowDir[relParts(p.tv, twin)[0]]
		if unit == "" {
			unit = "dup:tv-in-movies"
		}
		specialM.put(Op{unit, "delete", f, twin, "duplicate of the TV library copy (owner approved)"})
	}
	for _, f := range tfiles {
		if low := strings.ToLower(f); (strings.Contains(low, "don't need") || strings.Contains(low, "dont need")) && !strings.Contains(f, "@eaDir") {
			specialT.put(Op{"misc", "archive", f, p.archive + "/junk/TV/" + strings.TrimPrefix(f, p.tv+"/"), "folder named 'Subs that you don't need'"})
		}
	}
	ops := append(specialM.ops(), p.planRoot(p.movies, movies, mfiles, specialM.by)...)
	ops = append(ops, specialT.ops()...)
	ops = append(ops, p.planRoot(p.tv, shows, tfiles, specialT.by)...)

	plan := Plan{Ops: ops, Review: append(mreview, treview...), Problems: check(ops, in.Files)}
	accounted := map[string]bool{}
	for _, o := range ops {
		accounted[o.Src] = true
	}
	for _, f := range append(mfiles, tfiles...) {
		if !accounted[f] && !strings.Contains(f, "@eaDir") && !ignoreName[path.Base(f)] && !strings.Contains(f, "#recycle") && !strings.Contains(f, "/.details/") {
			plan.Unplanned = append(plan.Unplanned, f)
		}
	}
	reviewed := map[string]bool{}
	for _, r := range plan.Review {
		reviewed[r.Item] = true
	}
	for _, o := range ops {
		if o.Op == "leave" && !reviewed[o.Src] {
			plan.Review = append(plan.Review, Review{o.Unit, o.Src, o.Reason})
		}
	}
	return plan
}

func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ---- movies

func (p *planner) planMovies() (*videoPlan, []Op, []Review) {
	in := p.in
	videos := newVideoPlan()
	var ops []Op
	var review []Review
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
				ops = append(ops, Op{unit, "leave", f, "", "movie has no TMDB id or year: rematch first"})
			}
			review = append(review, Review{unit, first.Name, fmt.Sprintf("match incomplete (year=%d id=%s): rematch", first.Year, id)})
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
				ops = append(ops, Op{unit, "archive", f, p.archive + "/samples/Movies/" + strings.Join(rel, "/"), "sample/ad clip filed as a version"})
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
			ops = append(ops, Op{unit, "delete", f, twin, "identical copy of another version (bytes compared at run time)"})
			videos.set(f, planned{unit: dead, kind: "deleted"})
		}
		if len(keep) == 0 {
			continue
		}
		labels, unread := p.movieLabels(keep, folder, base)
		if unread != "" {
			for _, f := range keep {
				ops = append(ops, Op{unit, "leave", f, "", needsFacts + ": " + path.Base(unread)})
			}
			continue
		}
		if !distinct(labels) {
			for _, f := range keep {
				ops = append(ops, Op{unit, "leave", f, "", "versions still share a label " + pyList(labels)})
			}
			review = append(review, Review{unit, first.Name, "versions share a label: " + pyList(labels)})
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
	return videos, ops, review
}

// movieLabels names each version: resolution, then cut, codec, release group and size until they differ. unread
// names a version whose label needs facts the input lacks (ffprobe could not read it).
func (p *planner) movieLabels(keep []string, folder, base string) (labels []string, unread string) {
	in := p.in
	labels = make([]string, len(keep))
	for i, f := range keep {
		if s := stem(path.Base(f)); path.Dir(f) == folder && strings.HasPrefix(s, base+" - ") {
			labels[i] = s[len(base)+3:] // already named by this scheme ('1080p Remote', '2160p 14Mbps'): re-runs keep it
		} else if l := manualLabel[strings.TrimPrefix(f, p.movies+"/")]; l != "" {
			labels[i] = l
		} else {
			if _, known := in.Video[f]; !known && len(keep) > 1 {
				unread = f
			}
			labels[i] = resolution(in.Video[f].Width, in.Video[f].Height)
			if strings.Contains(f, "/Remote Versions/") {
				labels[i] += " Remote"
			}
		}
	}
	if len(keep) == 1 || unread != "" {
		return labels, unread
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
	return labels, ""
}

func distinct(labels []string) bool {
	seen := map[string]bool{}
	for _, l := range labels {
		if seen[l] {
			return false
		}
		seen[l] = true
	}
	return true
}

// pyList renders labels as Python's str(list) for the review text.
func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = pyRepr(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// ---- tv

type candidate struct {
	unit, showDir    string
	season, ep, last int
	title, reason    string
	show             Title
}

func (p *planner) planTV() (*videoPlan, []Op, []Review) {
	in := p.in
	videos := newVideoPlan()
	var ops []Op
	var review []Review
	cands := map[string]candidate{}
	var order []string
	type showSeason struct {
		show   string
		season int
	}
	dvdSeasons := map[showSeason]int{}
	for _, f := range sortedKeys(in.Shows) {
		eps := in.Episodes[f]
		if _, ok := in.Files[f]; !ok || len(eps) == 0 {
			continue
		}
		show := in.Shows[f]
		unit := "show:" + show.ID
		if !strings.HasPrefix(show.ID, "tmdb-") && !strings.HasPrefix(show.ID, "tvdb-") {
			ops = append(ops, Op{unit, "leave", f, "", "show has no TMDB/TVDB id"})
			review = append(review, Review{unit, f, "show has no TMDB/TVDB id"})
			continue
		}
		showDir := p.tv + "/" + component(withYear(show.Name, show.Year)+" {"+show.ID+"}")
		name, parent := path.Base(f), path.Base(path.Dir(f))
		if extraFolder.MatchString(parent) {
			videos.set(f, planned{unit, showDir + "/" + extraCategory([]string{parent}), component(clean(stem(name))), lowerExt(f), "extra"})
			continue
		}
		if !sameShow(showPrefix(name), show.Name, show.Original) {
			mine, ok := ownShow(stem(name))
			dst := f
			if ok {
				dst = fmt.Sprintf("%s/%s/Season %02d/%s", p.tv, component(mine.show), mine.season, name)
			}
			if dst != f { // a folder named for the file's own show: the next run matches it anew
				ops = append(ops, Op{"isolate:" + mine.show, "move", f, dst,
					fmt.Sprintf("file names %s, matched to %s: own folder", pyRepr(mine.show), pyRepr(show.Name))})
			} else {
				ops = append(ops, Op{unit, "leave", f, "", fmt.Sprintf("file names a different show than its match (%s)", show.Name)})
				review = append(review, Review{unit, f, fmt.Sprintf("matched to %s; the file name says otherwise: rematch", pyRepr(show.Name))})
			}
			continue
		}
		season, ep := eps[0].Season, eps[0].Episode
		if ep >= 100 && ep/100 == season { // Seinfeld 'Season #9/901' read as E901
			ep %= 100
		}
		lastRaw := eps[len(eps)-1].Episode
		if lastRaw >= 100 && lastRaw/100 == season {
			lastRaw %= 100
		}
		last := max(ep, lastRaw)
		fileTitle := ""
		if parsed, ok := fileEpisode(name); ok {
			fileTitle = parsed.title
			if (parsed.season == -1 || parsed.season == season) && parsed.first <= ep && ep <= parsed.last && parsed.last > parsed.first {
				ep, last = parsed.first, max(last, parsed.last) // the file's own range wins when it contains the listed number
			}
		}
		if ep == 0 && season > 0 {
			known, fetched := in.Hints[show.ID]
			guess := fileTitle
			if guess == "" {
				guess = eps[0].Title
			}
			if hit, ok := bestHint(known, guess); ok { // listed under its real number (Season 00, or a mislabeled episode)
				cands[f] = candidate{unit, showDir, hit.Season, hit.Episode, hit.Episode, truncRunes(clean(hit.Title), 80),
					"episode (numbered by its title in TMDB's episode list)", show}
				order = append(order, f)
			} else if fetched { // numbered nowhere: bonus material, kept as the show's extra
				s := clean(guess)
				if s == "" {
					s = fmt.Sprintf("Season %d special", season)
				}
				videos.set(f, planned{unit, showDir + "/Featurettes", component(s), lowerExt(f), "extra"})
			} else {
				ops = append(ops, Op{unit, "leave", f, "", "SxxE00 special: needs its Season 00 number"})
				review = append(review, Review{unit, f, "special numbered as episode 0 of a season: map to Season 00 by hand"})
			}
			continue
		}
		known, title, reason := eps[0].Title, "", "episode"
		switch {
		case last > ep:
			title = clean(fileTitle)
		case known != "" && fileTitle != "" && !titlesAgree(fileTitle, known):
			title, reason = clean(fileTitle), "episode (kept the file's own title: TMDB numbering differs)"
			dvdSeasons[showSeason{show.Name, season}]++
		case known != "":
			title = clean(known)
		default:
			title = clean(fileTitle)
		}
		if episodeN.MatchString(title) {
			title = ""
		}
		cands[f] = candidate{unit, showDir, season, ep, last, truncRunes(title, 80), reason, show}
		order = append(order, f)
	}
	type epKey struct {
		unit       string
		season, ep int
	}
	perEp := map[epKey]int{}
	groups := map[epKey][]string{}
	var groupOrder []epKey
	for _, f := range order {
		c := cands[f]
		k := epKey{c.unit, c.season, c.ep}
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
				ops = append(ops, Op{k.unit, "delete", o, olds[0], "identical copy of another episode file (bytes compared at run time)"})
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
		folder := fmt.Sprintf("%s/Season %02d", c.showDir, c.season)
		if cur := stem(path.Base(f)); perEp[epKey{c.unit, c.season, c.ep}] > 1 && path.Dir(f) == folder &&
			strings.HasPrefix(cur, component(s)+" [") && strings.HasSuffix(cur, "]") {
			s = cur // already labelled by this scheme: re-runs keep the label (its release group is no longer in the name)
		} else if perEp[epKey{c.unit, c.season, c.ep}] > 1 {
			if _, known := in.Video[f]; !known {
				ops = append(ops, Op{c.unit, "leave", f, "", needsFacts})
				continue
			}
			label := clean(releaseGroup(f))
			if label == "" {
				label = fmt.Sprintf("%.1fGB", float64(in.Files[f])/1e9)
			}
			s += fmt.Sprintf(" [%s %s]", resolution(in.Video[f].Width, in.Video[f].Height), label)
		}
		videos.set(f, planned{c.unit, folder, component(s), lowerExt(f), c.reason})
	}
	seasons := make([]showSeason, 0, len(dvdSeasons))
	for k := range dvdSeasons {
		seasons = append(seasons, k)
	}
	sort.Slice(seasons, func(i, j int) bool {
		if seasons[i].show != seasons[j].show {
			return seasons[i].show < seasons[j].show
		}
		return seasons[i].season < seasons[j].season
	})
	for _, k := range seasons {
		review = append(review, Review{"show:" + k.show, fmt.Sprintf("Season %d", k.season),
			fmt.Sprintf("%d file(s) kept their own titles: TMDB numbering differs (renumber later)", dvdSeasons[k])})
	}
	return videos, ops, review
}

// ---- everything else under a root: sidecars, extras no source lists, junk

func (p *planner) planRoot(root string, videos *videoPlan, files []string, special map[string]Op) []Op {
	in := p.in
	var ops []Op
	rootName := path.Base(root)
	owner := p.attachSubtitles(root, files, videos)
	subs := p.nameSubtitles(owner, videos)
	taken := map[string]bool{}
	unitHome := map[string]string{}
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
	}
	plannedDirs := map[string]map[string]bool{} // folder → units with a planned video below it
	mark := func(v, unit string) {
		if unit == dead { // a video's old folder (moved) still belongs to its unit: its leftovers follow the video
			return
		}
		for d := path.Dir(v); strings.HasPrefix(d, root) && d != root; d = path.Dir(d) {
			if plannedDirs[d] == nil {
				plannedDirs[d] = map[string]bool{}
			}
			plannedDirs[d][unit] = true
		}
	}
	for _, v := range videos.order {
		mark(v, videos.by[v].unit)
	}
	for _, o := range sortedKeys(in.Moved) {
		if n := in.Moved[o]; videos.has(n) {
			mark(o, videos.by[n].unit)
		}
	}
	for _, f := range files {
		rel := relParts(root, f)
		name, ext, size := path.Base(f), lowerExt(f), in.Files[f]
		if _, isSpecial := special[f]; hasPart(rel, "@eaDir") || ignoreName[name] || hasPart(rel, "#recycle") || isSpecial {
			continue
		}
		if len(rel) >= 2 && rel[len(rel)-2] == ".details" { // write_details notes travel with their video; never junk
			if v, ok := videos.by[path.Dir(path.Dir(f))+"/"+stem(name)]; ok && v.unit != dead {
				dst := v.folder + "/.details/" + v.stem + v.ext + ".txt"
				ops = append(ops, Op{v.unit, keepOr(dst, f), f, dst, "details note"})
			}
			continue
		}
		if v, ok := videos.by[f]; ok {
			if v.unit != dead {
				dst := v.folder + "/" + v.stem + v.ext
				ops = append(ops, Op{v.unit, keepOr(dst, f), f, dst, v.kind})
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
			ops = append(ops, Op{s.unit, op, f, s.dst, s.reason})
			continue
		}
		ownerUnit := ""
		for d := path.Dir(f); d != root && strings.HasPrefix(d, root); d = path.Dir(d) {
			if units := plannedDirs[d]; len(units) > 0 {
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
			switch home := unitHome[ownerUnit]; {
			case sampleRe.MatchString(name) && size < 600_000_000:
				ops = append(ops, Op{unitOrMisc, "archive", f, p.archive + "/samples/" + rootName + "/" + strings.Join(rel, "/"), "sample clip"})
			case extraDir != "" && ownerUnit != "" && home != "":
				cat := extraCategory(rel[:len(rel)-1])
				dst := home + "/" + cat + "/" + component(clean(stem(name))) + ext
				if taken[casefold.String(dst)] {
					dst = home + "/" + cat + "/" + component(clean(stem(name))+" ("+clean(path.Base(path.Dir(f)))+")") + ext
				}
				taken[casefold.String(dst)] = true
				ops = append(ops, Op{ownerUnit, keepOr(dst, f), f, dst, "extra (" + extraDir + ")"})
			case in.Unidentified[f] != "":
				ops = append(ops, Op{unitOrMisc, "leave", f, "", in.Unidentified[f]})
			default:
				mine, ok := ownName{}, false
				if root == p.tv && extraDir == "" {
					mine, ok = ownShow(stem(name))
				}
				dst := f
				if ok {
					dst = fmt.Sprintf("%s/%s/Season %02d/%s%s", p.tv, component(mine.show), mine.season,
						component(fmt.Sprintf("%s - S%02dE%02d", mine.show, mine.season, mine.episode)), ext)
				}
				if dst == f {
					ops = append(ops, Op{unitOrMisc, "leave", f, "", "video no source lists"})
					break
				}
				// a name the apps cannot read ('Season 3 Episode 1-'): SxxEyy in a folder of its own show
				ops = append(ops, Op{"isolate:" + mine.show, "move", f, dst, "episode no source lists: a name the apps read"})
			}
		case subExt[ext]:
			ops = append(ops, Op{unitOrMisc, "archive", f, p.archive + "/subtitles/" + rootName + "/" + strings.Join(rel, "/"), "subtitle with no matching video"})
		default:
			what := ext
			if what == "" {
				what = "no extension"
			}
			ops = append(ops, Op{unitOrMisc, "archive", f, p.archive + "/junk/" + rootName + "/" + strings.Join(rel, "/"), "junk (" + what + ")"})
		}
	}
	return ops
}

func keepOr(dst, src string) string {
	if dst == src {
		return "keep"
	}
	return "move"
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
			for _, c := range strings.Split(o.Dst, "/")[1:] {
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
