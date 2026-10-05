package lists

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/stage"
)

// Plex: one collection per shelf in the Movies section, custom order, Home rows for the configured lists.

type plexSink struct {
	env     *stage.Env
	opt     Options
	log     *slog.Logger
	w       *writer
	byTMDB  map[int]string // ratingKey
	watched map[int]bool   // the owner's Plex watched flags, from the same answer
	machine string
	taken   int
}

// plexPage is how many movies one listing call asks for: on the busy NAS (2026-10-05) the whole section in one answer
// took over three minutes, and 300 at a time 4 s to over a minute.
var plexPage = 100

type plexMap struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Items     map[int]plexItem `json:"items"`
}

type plexItem struct {
	Key     string `json:"k"`
	Watched bool   `json:"w,omitempty"`
}

// load reads the TMDB id → ratingKey map, from Plex at most once per plex_refresh (one paged listing of the
// section), else from the copy kept in the store.
func (p *plexSink) load(ctx context.Context) {
	p.w = &writer{app: "plex", log: p.log}
	var m plexMap
	found, err := p.env.Store.GetJSON(ctx, name, "plex_map", &m)
	if err != nil {
		p.log.Warn("plex map unreadable", "event", "store_error", "err", err)
	}
	if !found || p.env.Now().Sub(m.FetchedAt) >= p.opt.PlexRefresh {
		ctx, cancel := context.WithTimeout(ctx, p.opt.PlexBudget)
		defer cancel()
		fresh := plexMap{FetchedAt: p.env.Now(), Items: map[int]plexItem{}}
		complete := false
		for start := 0; ; start += plexPage {
			var page struct {
				TotalSize int `json:"totalSize"`
				Metadata  []struct {
					RatingKey string `json:"ratingKey"`
					ViewCount int    `json:"viewCount"`
					// Plex sends "guid" (plex://movie/…) beside "Guid" (the external ids); encoding/json matches keys
					// case-insensitively, so without its own field "guid" would be decoded into Guid and fail
					PlexGUID string `json:"guid"`
					Guid     []struct {
						ID string `json:"id"`
					} `json:"Guid"`
				} `json:"Metadata"`
			}
			path := fmt.Sprintf("/library/sections/%d/all?type=1&includeGuids=1&excludeFields=summary,tagline"+
				"&excludeElements=Media,Genre,Country,Director,Writer,Role,Image,Collection"+
				"&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", p.opt.PlexSection, start, plexPage)
			if !p.w.read(ctx, "section listing", func(ctx context.Context) error { return p.env.Plex.GetJSON(ctx, path, &page) }) {
				break
			}
			for _, it := range page.Metadata {
				for _, g := range it.Guid {
					n, err := strconv.Atoi(strings.TrimPrefix(g.ID, "tmdb://"))
					if _, dup := fresh.Items[n]; err == nil && strings.HasPrefix(g.ID, "tmdb://") && !dup {
						fresh.Items[n] = plexItem{Key: it.RatingKey, Watched: it.ViewCount > 0}
					}
				}
			}
			if len(page.Metadata) == 0 || start+len(page.Metadata) >= page.TotalSize {
				complete = true
				break
			}
		}
		switch {
		case complete:
			m = fresh
		case len(fresh.Items) > 0:
			// a slow Plex answered part of the listing: keep it over the old copy (a film missing from a partial map
			// would be dropped from its shelves) and list again next run, as fetched_at stays old
			if m.Items == nil {
				m.Items = map[int]plexItem{}
			}
			for t, it := range fresh.Items {
				m.Items[t] = it
			}
		}
		if len(fresh.Items) > 0 {
			if err := p.env.Store.PutJSON(ctx, name, "plex_map", m); err != nil {
				p.log.Warn("plex map not kept", "event", "store_error", "err", err)
			}
		}
		p.w.stopped = "" // a failed listing only means the old copy (if any) is used
	}
	p.byTMDB, p.watched = map[int]string{}, map[int]bool{}
	for t, it := range m.Items {
		p.byTMDB[t] = it.Key
		if it.Watched {
			p.watched[t] = true
		}
	}
}

type plexColl struct {
	RatingKey      string `json:"ratingKey"`
	Title          string `json:"title"`
	TitleSort      string `json:"titleSort"`
	Summary        string `json:"summary"`
	CollectionSort string `json:"collectionSort"`
	CollectionMode string `json:"collectionMode"`
	AddedAt        int64  `json:"addedAt"`
}

func (p *plexSink) collections(ctx context.Context) ([]plexColl, bool) {
	var out struct {
		Metadata []plexColl `json:"Metadata"`
	}
	ok := p.w.read(ctx, "collections", func(ctx context.Context) error {
		return p.env.Plex.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/collections", p.opt.PlexSection), &out)
	})
	return out.Metadata, ok
}

func (p *plexSink) reconcile(ctx context.Context, shelves []shelf) {
	if len(p.byTMDB) == 0 {
		p.w.stopped = "no TMDB id → ratingKey map yet"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, p.opt.PlexBudget)
	defer cancel()
	var id struct {
		MachineIdentifier string `json:"machineIdentifier"`
	}
	if !p.w.read(ctx, "identity", func(ctx context.Context) error { return p.env.Plex.GetJSON(ctx, "/identity", &id) }) {
		return
	}
	p.machine = id.MachineIdentifier
	colls, ok := p.collections(ctx)
	if !ok {
		return
	}
	ids := map[string]string{} // list name → collection ratingKey
	for _, s := range shelves {
		if key := p.shelf(ctx, s, colls); key != "" {
			ids[s.List.Name] = key
		}
		if p.w.stopped != "" {
			return
		}
	}
	p.home(ctx, shelves, ids)
}

func (p *plexSink) uri(keys []string) string {
	return "server://" + p.machine + "/com.plexapp.plugins.library/library/metadata/" + strings.Join(keys, ",")
}

// shelf brings one collection to the shelf's state and returns its ratingKey ("" when it was not made).
func (p *plexSink) shelf(ctx context.Context, s shelf, colls []plexColl) string {
	db := p.env.Store.DB()
	row, err := getRow(ctx, db, "plex", s.List.Key)
	if err != nil {
		p.w.stopped = "state: " + err.Error()
		return ""
	}
	var want []string
	for _, t := range s.Members {
		if k, ok := p.byTMDB[t]; ok {
			want = append(want, k)
		}
	}
	var coll *plexColl
	for i := range colls {
		if row.ID != "" && colls[i].RatingKey == row.ID {
			coll = &colls[i]
		}
	}
	if coll == nil {
		if coll = p.adoptOrCreate(ctx, s, &row, colls, want); coll == nil {
			return ""
		}
	}
	base := "/library/collections/" + coll.RatingKey
	if coll.CollectionSort != "2" || coll.CollectionMode != "0" {
		// custom order (the list's), and hidden from the library grid: the shelf lives in Collections and on Home
		path := "/library/metadata/" + coll.RatingKey + "/prefs?collectionSort=2&collectionMode=0"
		if !p.w.write(ctx, "prefs of "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "PUT", path) }) {
			return ""
		}
	}
	have, ok := p.children(ctx, coll.RatingKey)
	if !ok {
		return ""
	}
	drop, add := diffSets(have, want)
	for _, k := range drop {
		if !p.w.write(ctx, "drop from "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "DELETE", base+"/items/"+k) }) {
			return ""
		}
	}
	for _, b := range batches(add, 100) {
		path := base + "/items?" + url.Values{"uri": {p.uri(b)}}.Encode()
		if !p.w.write(ctx, "add to "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "PUT", path) }) {
			return ""
		}
	}
	// Plex puts members it has no custom position for after the placed ones, by title (seen on 1.43): read the
	// order back so only what is out of place moves
	if len(drop)+len(add) > 0 {
		if have, ok = p.children(ctx, coll.RatingKey); !ok {
			return ""
		}
	}
	for _, m := range planMoves(have, want) {
		path := base + "/items/" + m.Item + "/move"
		if m.After != "" {
			path += "?after=" + m.After
		}
		if !p.w.write(ctx, "order "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "PUT", path) }) {
			return ""
		}
	}
	edit := url.Values{}
	for _, f := range []struct{ field, have, want string }{
		{"title", coll.Title, s.List.Name}, {"titleSort", coll.TitleSort, s.SortTitle}, {"summary", coll.Summary, s.Summary}} {
		if f.have != f.want {
			edit.Set(f.field+".value", f.want)
			edit.Set(f.field+".locked", "1")
		}
	}
	if len(edit) > 0 {
		edit.Set("type", "18")
		edit.Set("id", coll.RatingKey)
		path := fmt.Sprintf("/library/sections/%d/all?%s", p.opt.PlexSection, edit.Encode())
		if !p.w.write(ctx, "describe "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "PUT", path) }) {
			return ""
		}
	}
	img, ctype, err := poster(s)
	if err != nil {
		p.log.Warn("poster unavailable", "event", "poster_error", "list", s.List.Name, "err", err)
	} else if sum := digest(img); row.Poster != sum {
		path := "/library/metadata/" + coll.RatingKey + "/posters"
		if !p.w.write(ctx, "poster of "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Send(ctx, "POST", path, ctype, img) }) {
			return ""
		}
		row.Poster = sum
		if err := putRow(ctx, db, "plex", s.List.Key, row); err != nil {
			p.log.Warn("state write failed", "event", "store_error", "err", err)
		}
	}
	return coll.RatingKey
}

func (p *plexSink) children(ctx context.Context, key string) ([]string, bool) {
	var kids struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
		} `json:"Metadata"`
	}
	ok := p.w.read(ctx, "members", func(ctx context.Context) error {
		return p.env.Plex.GetJSON(ctx, "/library/collections/"+key+"/children", &kids)
	})
	var out []string
	for _, k := range kids.Metadata {
		out = append(out, k.RatingKey)
	}
	return out, ok
}

// adoptOrCreate finds the shelf's collection when the state lost track of it, or makes it. A collection with the
// shelf's title is adopted only when it is provably this stage's (its summary, or a create this stage sent); any
// other is the owner's and is left alone.
func (p *plexSink) adoptOrCreate(ctx context.Context, s shelf, row *sinkRow, colls []plexColl, want []string) *plexColl {
	db := p.env.Store.DB()
	if c := p.adoptable(s, row, colls); c != nil {
		row.ID = c.RatingKey
		return p.keep(ctx, db, s, row, c)
	}
	for _, c := range colls {
		if sameTitle(c.Title, s.List.Name) {
			p.taken++
			p.log.Warn("a collection of the owner's has this title; list not shelved", "event", "name_taken", "app", "plex",
				"list", s.List.Name, "collection", c.RatingKey)
			return nil
		}
	}
	if len(want) == 0 {
		return nil // nothing on the NAS yet: no empty collection
	}
	row.IntentAt = p.env.Now()
	if err := putRow(ctx, db, "plex", s.List.Key, *row); err != nil { // log the intent before acting
		p.w.stopped = "state: " + err.Error()
		return nil
	}
	first := want
	if len(first) > 100 {
		first = first[:100]
	}
	path := "/library/collections?" + url.Values{"type": {"1"}, "smart": {"0"}, "title": {s.List.Name},
		"sectionId": {strconv.Itoa(p.opt.PlexSection)}, "uri": {p.uri(first)}}.Encode()
	if !p.w.writeOnce(ctx, "create "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "POST", path) }) {
		return nil
	}
	fresh, ok := p.collections(ctx)
	if !ok {
		return nil
	}
	if c := p.adoptable(s, row, fresh); c != nil {
		row.ID = c.RatingKey
		p.log.Info("collection made", "event", "collection_created", "app", "plex", "list", s.List.Name, "id", c.RatingKey)
		return p.keep(ctx, db, s, row, c)
	}
	p.w.stopped = "created " + s.List.Name + " but cannot find it"
	return nil
}

func (p *plexSink) adoptable(s shelf, row *sinkRow, colls []plexColl) *plexColl {
	for i, c := range colls {
		if !sameTitle(c.Title, s.List.Name) {
			continue
		}
		if strings.HasPrefix(c.Summary, summaryMarker) || (!row.IntentAt.IsZero() && c.AddedAt >= row.IntentAt.Unix()-60) {
			return &colls[i]
		}
	}
	return nil
}

func (p *plexSink) keep(ctx context.Context, db *sql.DB, s shelf, row *sinkRow, c *plexColl) *plexColl {
	row.IntentAt = time.Time{}
	if err := putRow(ctx, db, "plex", s.List.Key, *row); err != nil {
		p.w.stopped = "state: " + err.Error()
		return nil
	}
	return c
}

// home promotes the configured lists to the owner's Home and Recommended (not friends' Homes), in the configured
// order after opt.HomeAfter, and demotes shelves this stage promoted that are no longer configured.
func (p *plexSink) home(ctx context.Context, shelves []shelf, ids map[string]string) {
	db := p.env.Store.DB()
	manage := fmt.Sprintf("/hubs/sections/%d/manage", p.opt.PlexSection)
	type hub struct {
		Identifier            string `json:"identifier"`
		PromotedToOwnHome     bool   `json:"promotedToOwnHome"`
		PromotedToRecommended bool   `json:"promotedToRecommended"`
	}
	var hubs struct {
		Hub []hub `json:"Hub"`
	}
	read := func() bool {
		hubs.Hub = nil
		return p.w.read(ctx, "home rows", func(ctx context.Context) error { return p.env.Plex.GetJSON(ctx, manage, &hubs) })
	}
	if !read() {
		return
	}
	ident := func(key string) string { return fmt.Sprintf("custom.collection.%d.%s", p.opt.PlexSection, key) }
	state := map[string]hub{}
	for _, h := range hubs.Hub {
		state[h.Identifier] = h
	}
	var want []string
	wanted := map[string]bool{}
	for _, n := range p.opt.Home {
		for name, key := range ids {
			if strings.EqualFold(name, n) && !wanted[key] {
				want = append(want, ident(key))
				wanted[key] = true
			}
		}
	}
	changed := false
	for _, s := range shelves {
		key := ids[s.List.Name]
		if key == "" {
			continue
		}
		row, err := getRow(ctx, db, "plex", s.List.Key)
		if err != nil {
			p.w.stopped = "state: " + err.Error()
			return
		}
		h, on := state[ident(key)], wanted[key]
		need := false
		switch {
		case on:
			need = !h.PromotedToOwnHome || !h.PromotedToRecommended
		case row.Promoted: // promoted by this stage, no longer configured; a row the owner promoted stays
			need = h.PromotedToOwnHome || h.PromotedToRecommended
		}
		if need {
			v := map[bool]string{true: "1", false: "0"}[on]
			path := manage + "?" + url.Values{"metadataItemId": {key}, "promotedToRecommended": {v}, "promotedToOwnHome": {v},
				"promotedToSharedHome": {"0"}}.Encode()
			if !p.w.write(ctx, "home row "+s.List.Name, func(ctx context.Context) error { return p.env.Plex.Do(ctx, "POST", path) }) {
				return
			}
			changed = true
		}
		if row.Promoted != on {
			row.Promoted = on
			if err := putRow(ctx, db, "plex", s.List.Key, row); err != nil {
				p.log.Warn("state write failed", "event", "store_error", "err", err)
			}
		}
	}
	if changed && !read() {
		return
	}
	order := make([]string, 0, len(hubs.Hub))
	for _, h := range hubs.Hub {
		order = append(order, h.Identifier)
	}
	for _, m := range planHubMoves(order, want, p.opt.HomeAfter) {
		path := manage + "/" + m.Item + "/move"
		if m.After != "" {
			path += "?after=" + m.After
		}
		if !p.w.write(ctx, "home order", func(ctx context.Context) error { return p.env.Plex.Do(ctx, "PUT", path) }) {
			return
		}
	}
}

// planHubMoves returns the moves that put the rows in want, in that order, right after the row named after (a
// missing after puts the first at the top); rows already in place cost nothing.
func planHubMoves(order, want []string, after string) []move {
	order = append([]string(nil), order...)
	var out []move
	prev := after
	for _, id := range want {
		at, prevAt := indexOf(order, id), indexOf(order, prev)
		if at < 0 {
			continue
		}
		if at != prevAt+1 {
			m := move{Item: id}
			if prevAt >= 0 {
				m.After = prev
			}
			out = append(out, m)
			order = append(order[:at], order[at+1:]...)
			to := indexOf(order, prev) + 1
			order = append(order[:to], append([]string{id}, order[to:]...)...)
		}
		prev = id
	}
	return out
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
