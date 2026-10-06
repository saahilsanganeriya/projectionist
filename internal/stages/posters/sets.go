package posters

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"projectionist/internal/library"
	"projectionist/internal/llm"
	"projectionist/internal/mediux"
	"projectionist/internal/stages/collections"
	"projectionist/internal/tmdb"
)

// ---- poster sets: one MediUX set per franchise shelf ----
//
// The owner wants a shelf's films to look alike ("all marvel movies follow a poster template"). MediUX artists make
// matching posters for a whole TMDB collection, and its collection page lists every set. For each TMDB collection a
// franchise shelf holds (the approved collections and the owner's own), the sets made in English that have a poster
// for every film on disk whose poster the owner did not upload or pick are the candidates. The collections judge
// scores up to five of them as a whole, each shown as one image (the collection poster and three films), and the best
// one is worn: its posters join the pools, each film's and, for an approved collection (never one of the owner's),
// the collection's. A film's judge gives the set's poster a +1 "consistent shelf" bonus over the other remote
// candidates; Plex's default still yields only to a raw 2-point lead, and the owner's upload or pick is never
// replaced. A set fewer than half of whose films take its poster is dropped: those films are decided again without
// the bonus. Thumbnails serve the judges; only a worn set's posters are ever fetched at full size, to upload.

const srcMediUX = "mediux" // a poster of the MediUX set worn by the film's TMDB collection

const (
	setsAtMost     = 5        // candidate sets one judge call compares
	setFilms       = 3        // films shown beside a set's collection poster
	panelW, panelH = 160, 240 // one poster in a set's montage
)

// mediuxSleep waits between MediUX pages; tests replace it.
var mediuxSleep = wikiSleep

// setsPrompt judges whole sets for the collections judge, in the owner's words like franchisePrompt.
const setsPrompt = `You choose the posters for one cinephile's film library, to his taste. You will see candidate poster SETS for one
film series, each labelled with a letter. A set is one artist's matching posters for the whole series. Each image shows
one set side by side: its collection poster (when it has one) and three of the films' posters. Score every set from 1 to
10 for how much he would want every film of the series on his shelf to wear it.

His taste, in his words: "I like illustrated artistic posters, fanart usually, not mainstream posters. I do not need
minimalism, something creative is often good. The only text on the poster should be the movie name."

Reward: illustrated, painted or drawn art, fan art, and an original, creative idea; one look carried through every
film; a mood true to the series; each film's title set as part of the design, or no text at all. Creative beats plain:
a bare minimalist set scores below an inventive illustrated one.

Penalise mainstream studio key art (floating heads, rows of star faces, photo collages) and any text besides the
titles: credit blocks, taglines, review quotes, dates and logos are a heavy penalty. Also penalise heavily: text in a
language other than English or the series' own; watermarks; low-resolution or stretched images; and posters that do
not match each other.

Answer with JSON only, in exactly this shape:
{"scores": {"A": 7, "B": 4}, "reason": "<at most 15 words on why the best one wins>"}`

// setChoice is the set a TMDB collection wears, stored as "set/<TMDB collection id>".
type setChoice struct {
	Of         string             `json:"of"`  // the candidate sets and the films they had to cover
	Set        string             `json:"set"` // the MediUX set's id; "" when none qualified
	Credit     string             `json:"credit,omitempty"`
	Collection string             `json:"collection,omitempty"` // the set's collection poster (asset id)
	Films      map[string]string  `json:"films,omitempty"`      // each film it had to cover (title key) → its poster
	Rule       string             `json:"rule,omitempty"`       // single | judged | unjudged
	Scores     map[string]float64 `json:"scores,omitempty"`     // by set id
	Reason     string             `json:"reason,omitempty"`
	Model      string             `json:"model,omitempty"`
	Dropped    bool               `json:"dropped,omitempty"` // fewer than half its films took its poster: no bonus
	At         time.Time          `json:"at"`
}

// shelfSet is one TMDB collection a franchise shelf holds: its films on disk, and the approved collections of this run
// its collection poster may dress.
type shelfSet struct {
	id      int
	name    string
	films   []tmdb.Details // by release date
	shelves []string       // collection title keys
}

type candidateSet struct {
	set        mediux.Set
	collection string         // its collection poster (asset id); "" when it has none
	films      map[int]string // TMDB id → poster (asset id)
}

type wornSet struct {
	key    string
	choice setChoice
}

// sets picks the set every TMDB collection of a franchise shelf wears, and puts its posters in r.setPosters for the
// pools to gather. A page MediUX does not serve keeps the collection's last choice; so does one the client cannot
// read, which also raises an alert: MediUX has changed its pages.
func (r *run) sets(ctx context.Context, titles []Title) error {
	shelves, err := collections.Franchises(ctx, r.env.Store.DB(), r.opt.CollectionsOwner, r.opt.CollectionsApproved)
	if err != nil {
		return err
	}
	onDisk := map[int]bool{}
	for _, t := range titles {
		if id, err := strconv.Atoi(t.ID.Value); err == nil && t.Kind == movie && t.ID.Source == "tmdb" {
			onDisk[id] = true
		}
	}
	groups := map[int]*shelfSet{}
	for _, s := range shelves {
		for _, id := range s.Members {
			if !onDisk[id] {
				continue
			}
			d, err := r.env.TMDB.Details(ctx, tmdb.Movie, id)
			if errors.Is(err, tmdb.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			c := d.Collection
			if c == nil || len(s.Sets) > 0 && !slices.ContainsFunc(s.Sets, func(n string) bool { return strings.EqualFold(n, c.Name) }) {
				continue
			}
			g := groups[c.ID]
			if g == nil {
				g = &shelfSet{id: c.ID, name: c.Name}
				groups[c.ID] = g
			}
			if !slices.ContainsFunc(g.films, func(f tmdb.Details) bool { return f.ID == id }) {
				g.films = append(g.films, d)
			}
			if k := collectionKey(s.Name); !s.Owned && r.collections[k] && !slices.Contains(g.shelves, k) {
				g.shelves = append(g.shelves, k)
			}
		}
	}
	if r.refs, err = r.references(ctx); err != nil {
		return err
	}
	session := r.env.LLM.Session("collections")
	var unreadable []int
	for _, id := range slices.Sorted(maps.Keys(groups)) {
		g := groups[id]
		slices.SortStableFunc(g.films, func(a, b tmdb.Details) int { return a.Date.Compare(b.Date.Time) })
		readable, err := r.dress(ctx, session, g)
		if err != nil {
			return err
		}
		if !readable {
			unreadable = append(unreadable, id)
		}
	}
	if len(unreadable) > 0 {
		r.alert("posters: %d MediUX collection page(s) could not be read (first: TMDB collection %d); their films keep the "+
			"set they wear, if any. MediUX may have changed its pages", len(unreadable), unreadable[0])
	}
	r.count("sets_worn", len(r.worn))
	return nil
}

// dress decides the set one TMDB collection wears and wears it; readable is false when MediUX's page could not be
// read. The decision is stored, and made again only when the candidate sets or the films they must cover change.
func (r *run) dress(ctx context.Context, s *llm.Session, g *shelfSet) (readable bool, err error) {
	stored := "set/" + strconv.Itoa(g.id)
	var old setChoice
	found, err := r.env.Store.GetJSON(ctx, name, stored, &old)
	if err != nil {
		return true, err
	}
	keep := func() {
		if found {
			r.wear(stored, old, g)
		}
	}
	keys := map[int]string{}
	var open []mediux.Film
	for _, d := range g.films {
		k := key(movie, library.ID{Source: "tmdb", Value: strconv.Itoa(d.ID)}, 0)
		if p := r.owners[k]; p.State == ownerUpload || p.State == ownerPick {
			continue // his poster stays: no set needs one for it
		}
		keys[d.ID] = k
		open = append(open, mediux.Film{TMDB: d.ID, Title: d.Title, Year: d.Date.Year()})
	}
	if len(open) == 0 {
		return true, nil
	}
	coll, err := r.mx.CollectionSets(ctx, g.id)
	if err != nil {
		r.env.Log.Warn("mediux", "event", "mediux_failed", "tmdb_collection", g.id, "err", err)
		r.count("mediux_failed", 1)
		keep()
		return !errors.Is(err, mediux.ErrParse), ctx.Err()
	}
	names := []string{coll.Name, g.name}
	for _, d := range g.films {
		names = append(names, d.Title)
	}
	var cands []candidateSet
	for _, set := range coll.Sets {
		if set.Creator == "" || set.Foreign(names...) {
			continue
		}
		if cp, byFilm := set.Posters(open); len(byFilm) == len(open) {
			cands = append(cands, candidateSet{set, cp, byFilm})
		}
	}
	cands = shortlist(cands)
	of := setsOf(cands, keys)
	if found && old.Of == of {
		r.wear(stored, old, g)
		return true, nil
	}
	choice := setChoice{Of: of, At: r.env.Now()}
	choose := func(i int, rule string) {
		c := cands[i]
		choice.Set, choice.Credit, choice.Collection, choice.Rule = c.set.ID, c.set.Credit(), c.collection, rule
		choice.Films = map[string]string{}
		for id, asset := range c.films {
			choice.Films[keys[id]] = asset
		}
	}
	item := collectionPrefix + "set:" + strconv.Itoa(g.id)
	switch {
	case len(cands) == 0:
	case len(cands) == 1:
		choose(0, "single")
	case r.guestsWatching(ctx):
		r.count("sets_deferred", 1)
		keep()
		return true, nil
	default:
		scores, model, reason, err := r.judgeSets(ctx, s, item, of, g.name, cands, open)
		switch {
		case errors.Is(err, errUnusable):
			r.alert("posters: %s got two unusable judge answers on its poster sets; it wears the first (%s)", g.name, cands[0].set.Credit())
			choose(0, "unjudged")
		case err != nil:
			r.env.Log.Warn("sets judge", "event", "judge_failed", "item", item, "err", err)
			keep()
			return true, ctx.Err()
		default:
			best := 0
			for i := range cands {
				if scores[i] > scores[best] {
					best = i
				}
			}
			choose(best, "judged")
			choice.Model, choice.Reason, choice.Scores = model, reason, map[string]float64{}
			for i, sc := range scores {
				choice.Scores[cands[i].set.ID] = sc
			}
		}
	}
	if err := r.env.Store.PutJSON(context.WithoutCancel(ctx), name, stored, choice); err != nil {
		return true, err
	}
	r.env.Log.Info("set chosen", "event", "set_chosen", "tmdb_collection", g.id, "name", g.name, "candidates", len(cands),
		"set", choice.Set, "rule", choice.Rule, "credit", choice.Credit)
	r.wear(stored, choice, g)
	return true, nil
}

// shortlist keeps each artist's best set, those with a collection poster first, then the newest, and the first
// setsAtMost of them.
func shortlist(cands []candidateSet) []candidateSet {
	num := func(c candidateSet) int { n, _ := strconv.Atoi(c.set.ID); return n }
	has := func(c candidateSet) int {
		if c.collection != "" {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(cands, func(a, b candidateSet) int { return cmp.Or(cmp.Compare(has(b), has(a)), cmp.Compare(num(b), num(a))) })
	var out []candidateSet
	for _, c := range cands {
		if len(out) < setsAtMost && !slices.ContainsFunc(out, func(o candidateSet) bool { return o.set.Creator == c.set.Creator }) {
			out = append(out, c)
		}
	}
	return out
}

// setsOf names the candidate sets and the posters each gives the films it must cover.
func setsOf(cands []candidateSet, keys map[int]string) string {
	h := sha256.New()
	for _, c := range cands {
		fmt.Fprintln(h, c.set.ID, c.collection)
		for _, id := range slices.Sorted(maps.Keys(c.films)) {
			fmt.Fprintln(h, keys[id], c.films[id])
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// wear puts a chosen set's posters in the pools: each film's, and its collection poster in each approved collection's.
func (r *run) wear(key string, c setChoice, g *shelfSet) {
	if c.Set == "" {
		return
	}
	r.worn = append(r.worn, wornSet{key, c})
	for film, asset := range c.Films {
		u := r.mx.AssetURL(asset, 0)
		r.setPosters[film] = append(r.setPosters[film], Candidate{URL: u, Thumb: r.mx.AssetURL(asset, 500), Source: srcMediUX, Caption: c.Credit})
		r.credits[u] = c.Credit
		if !c.Dropped {
			r.bonus[film] = u
		}
	}
	if c.Collection == "" {
		return
	}
	u := r.mx.AssetURL(c.Collection, 0)
	r.credits[u] = c.Credit
	for _, shelf := range g.shelves {
		r.setPosters[shelf] = append(r.setPosters[shelf], Candidate{URL: u, Thumb: r.mx.AssetURL(c.Collection, 500), Source: srcMediUX,
			Caption: "MediUX, made for the " + g.name + ". " + c.Credit})
	}
}

// setSig names the set posters a pool holds: another set gathers the pool again.
func (r *run) setSig(k string) string {
	var sig string
	for _, c := range r.setPosters[k] {
		sig += " mediux=" + c.URL
	}
	return sig
}

// bonusIndex is the film's candidate that gets the shelf bonus (its worn set's poster), or -1.
func (r *run) bonusIndex(it *row) int {
	u, ok := r.bonus[it.Title.Key]
	if !ok || it.Title.Kind != movie {
		return -1
	}
	return slices.IndexFunc(it.Cands, func(c Candidate) bool { return c.URL == u })
}

// dropUnworn drops a worn set fewer than half of whose films took its poster, once each of them has a verdict on a pool
// holding it, and decides those films again without the bonus, from their stored scores.
func (r *run) dropUnworn(ctx context.Context, rows []*row) error {
	byKey := map[string]*row{}
	for _, it := range rows {
		byKey[it.Title.Key] = it
	}
	for i := range r.worn {
		w := &r.worn[i]
		took, decided := 0, !w.choice.Dropped
		for film, asset := range w.choice.Films {
			u, it := r.mx.AssetURL(asset, 0), byKey[film]
			if it == nil || it.Verdict == nil || it.Verdict.Of != setID(it.Cands) || !slices.ContainsFunc(it.Cands, func(c Candidate) bool { return c.URL == u }) {
				decided = false
				break
			}
			if it.Verdict.Choice == u {
				took++
			}
		}
		if !decided || 2*took >= len(w.choice.Films) {
			continue
		}
		w.choice.Dropped = true
		if err := r.env.Store.PutJSON(context.WithoutCancel(ctx), name, w.key, w.choice); err != nil {
			return err
		}
		r.count("sets_dropped", 1)
		r.env.Log.Info("set dropped: fewer than half its films took its poster", "event", "set_dropped", "set", w.choice.Set,
			"took", took, "films", len(w.choice.Films))
		for film := range w.choice.Films {
			delete(r.bonus, film)
			it := byKey[film]
			if v := redecide(it.Cands, *it.Verdict, -1); v.Choice != it.Verdict.Choice || v.Rule != it.Verdict.Rule {
				v.At = r.env.Now()
				if err := r.saveVerdict(ctx, it, v); err != nil {
					return err
				}
				r.count("redecided", 1)
			}
		}
	}
	return nil
}

var errUnusable = errors.New("two unusable judge answers")

// judgeSets scores the candidate sets in one call on the collections task (one more when the answer cannot be read),
// with the owner's taste references, under the collections budget. The scores are by candidate index.
func (r *run) judgeSets(ctx context.Context, s *llm.Session, item, of, series string, cands []candidateSet, films []mediux.Film) (map[int]float64, string, string, error) {
	if err := s.Parked(); err != nil {
		return nil, "", "", err
	}
	head := fmt.Sprintf("FILM SERIES: %s. %d candidate poster sets follow.", series, len(cands))
	content := []llm.Part{llm.Text(head)}
	tokens := llm.EstimateTokens(setsPrompt) + llm.EstimateTokens(head) + 100
	ord := order(item, len(cands))
	for li, ci := range ord {
		img, width, err := r.montage(ctx, cands[ci], films)
		if err != nil {
			return nil, "", "", err
		}
		label := letters[li:li+1] + ":"
		content = append(content, llm.Text(label), llm.JPEG(img))
		tokens += llm.EstimateTokens(label) + ((width+27)/28)*((panelH+27)/28)
	}
	content = append(content, llm.Text("Score every set."))
	messages := []llm.Message{llm.System(setsPrompt)}
	if r.refs.message != nil {
		messages = append(messages, *r.refs.message)
		tokens += r.refs.tokens
	}
	req := llm.Request{MaxTokens: r.opt.MaxTokens, InputTokens: tokens, ResponseFormat: answerFormat(franchise, len(cands)),
		Messages: append(messages, llm.User(content...))}
	for range 2 {
		held, err := s.WorstCase(ctx, req)
		if err != nil {
			return nil, "", "", err
		}
		id, err := r.reserve(ctx, item, of, s.Model(), held)
		if err != nil {
			r.budgetHit(item, err)
			return nil, "", "", err
		}
		res, err := s.Chat(ctx, req)
		r.finishCall(ctx, id, res, err)
		if err != nil {
			return nil, "", "", err
		}
		a, err := parseAnswer(res.Text)
		if err != nil {
			r.env.Log.Warn("unusable judge answer", "event", "judge_unusable", "item", item, "err", err)
			r.count("judge_unusable", 1)
			r.closeCall(ctx, id, "unusable", err.Error(), false)
			continue
		}
		scores := map[int]float64{}
		for li, ci := range ord {
			if sc, ok := a.Scores[letters[li:li+1]]; ok {
				scores[ci] = sc
			}
		}
		r.count("sets_judged", 1)
		return scores, res.Model, a.Reason, nil
	}
	return nil, "", "", errUnusable
}

// montage is one candidate set as the judge sees it: its collection poster (when it has one) and its first setFilms
// films' posters, side by side, panelW×panelH each, from thumbnails; and its width.
func (r *run) montage(ctx context.Context, c candidateSet, films []mediux.Film) ([]byte, int, error) {
	var assets []string
	if c.collection != "" {
		assets = append(assets, c.collection)
	}
	for _, f := range films[:min(len(films), setFilms)] {
		assets = append(assets, c.films[f.TMDB])
	}
	dst := image.NewRGBA(image.Rect(0, 0, len(assets)*panelW, panelH))
	for i, a := range assets {
		b, err := r.thumb(ctx, Candidate{Thumb: r.mx.AssetURL(a, 300)})
		if err != nil {
			return nil, 0, err
		}
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, 0, err
		}
		draw.CatmullRom.Scale(dst, image.Rect(i*panelW, 0, (i+1)*panelW, panelH), img, img.Bounds(), draw.Src, nil)
	}
	var buf bytes.Buffer
	err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	return buf.Bytes(), dst.Bounds().Dx(), err
}
