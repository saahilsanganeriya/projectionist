package lists

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/stage"
)

// Jellyfin: one BoxSet per shelf; DisplayOrder "Default" keeps the order members were added in.

type jellyfinSink struct {
	env      *stage.Env
	opt      Options
	log      *slog.Logger
	w        *writer
	user     string         // the owner's user id
	byTMDB   map[int]string // item id
	sets     []boxSet       // the BoxSets, as load read them
	strays   []string       // ids of BoxSets of dropped canon lists that have no record
	taken    int
	retired  int
	scanning bool // the movie library is being scanned: its listings come and go, so BoxSets wait
}

type jellyfinMap struct {
	User  string           `json:"user"`
	Items map[int]plexItem `json:"items"` // item id + played flag
}

// load reads the owner's user id, the TMDB id → item id map and his played films every run, falling back to the
// last copy when Jellyfin does not answer or is scanning; then the BoxSets. During a scan movies drop out of the
// listings and come back (2026-10-05: 144 of 2,236 folders missing, a different 144 a few minutes later; a
// user-scoped listing showed 189 of 2,220), so a map read then would make the BoxSets shrink and regrow. The scan
// task runs on after the Movies library reports Idle (same day, 11:17-11:30Z: Movies Idle, "Scan Media Library" at
// 66%, the movie listing 2,099 then 2,037, a BoxSet's members 123 then 14), so both signals count.
func (j *jellyfinSink) load(ctx context.Context) appReport {
	j.w = &writer{app: "jellyfin", log: j.log}
	ctx, cancel := context.WithTimeout(ctx, j.opt.JellyfinBudget)
	defer cancel()
	var m jellyfinMap
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	type movies struct {
		Items []struct {
			ID          string            `json:"Id"`
			ProviderIDs map[string]string `json:"ProviderIds"`
		} `json:"Items"`
	}
	var all, played movies
	ok := j.w.read(ctx, "users", func(ctx context.Context) error { return j.env.Jellyfin.GetJSON(ctx, "/Users", &users) })
	for _, u := range users {
		if strings.EqualFold(u.Name, j.opt.JellyfinUser) {
			m.User = u.ID
		}
	}
	var libraries []struct {
		CollectionType string `json:"CollectionType"`
		RefreshStatus  string `json:"RefreshStatus"`
	}
	var tasks []struct {
		Key   string `json:"Key"`
		State string `json:"State"`
	}
	ok = ok && j.w.read(ctx, "libraries", func(ctx context.Context) error {
		return j.env.Jellyfin.GetJSON(ctx, "/Library/VirtualFolders", &libraries)
	}) && j.w.read(ctx, "tasks", func(ctx context.Context) error {
		return j.env.Jellyfin.GetJSON(ctx, "/ScheduledTasks?isHidden=false", &tasks)
	})
	j.scanning = !ok // a Jellyfin that does not say whether it is scanning is taken to be scanning
	for _, l := range libraries {
		j.scanning = j.scanning || (l.CollectionType == "movies" && l.RefreshStatus == "Active")
	}
	for _, t := range tasks {
		j.scanning = j.scanning || (t.Key == "RefreshLibrary" && t.State == "Running")
	}
	// collapseBoxSetItems=false: by default Jellyfin answers a film in a collection with the collection, the shelves too
	const list = "/Items?Recursive=true&IncludeItemTypes=Movie&collapseBoxSetItems=false&Fields=ProviderIds&EnableImages=false"
	if ok && m.User != "" && !j.scanning &&
		j.w.read(ctx, "movies", func(ctx context.Context) error { return j.env.Jellyfin.GetJSON(ctx, list, &all) }) &&
		j.w.read(ctx, "played movies", func(ctx context.Context) error {
			return j.env.Jellyfin.GetJSON(ctx, list+"&IsPlayed=true&userId="+m.User, &played)
		}) {
		seen := map[string]bool{}
		for _, it := range played.Items {
			seen[it.ID] = true
		}
		m.Items = map[int]plexItem{}
		for _, it := range all.Items {
			if n, err := strconv.Atoi(it.ProviderIDs["Tmdb"]); err == nil {
				if _, dup := m.Items[n]; !dup {
					m.Items[n] = plexItem{Key: it.ID, Watched: seen[it.ID]}
				}
			}
		}
		if err := j.env.Store.PutJSON(ctx, name, "jellyfin_map", m); err != nil {
			j.log.Warn("jellyfin map not kept", "event", "store_error", "err", err)
		}
	} else {
		user := m.User
		if _, err := j.env.Store.GetJSON(ctx, name, "jellyfin_map", &m); err != nil {
			j.log.Warn("jellyfin map unreadable", "event", "store_error", "err", err)
		}
		if user != "" {
			m.User = user
		}
	}
	j.w.stopped = ""
	j.user = m.User
	var r appReport
	j.byTMDB = map[int]string{}
	for t, it := range m.Items {
		j.byTMDB[t] = it.Key
		if it.Watched {
			r.Watched = append(r.Watched, t)
		}
	}
	if j.user == "" {
		j.w.stopped = "no Jellyfin user " + j.opt.JellyfinUser + " yet"
		return r
	}
	sets, ok := j.boxSets(ctx)
	if !ok {
		return r
	}
	mine, intents, err := madeIn(ctx, j.env.Store.DB(), "jellyfin")
	if err != nil {
		j.w.stopped = "state: " + err.Error()
		return r
	}
	j.sets = sets
	for _, b := range sets {
		if !mine[b.ID] && !strings.HasPrefix(b.Overview, summaryMarker) && !answersCreate(b.DateCreated, intents...) {
			r.Taken = append(r.Taken, fold(b.Name))
		}
		if !mine[b.ID] && stray(b.Name, b.Overview, j.opt) {
			j.strays = append(j.strays, b.ID)
			j.log.Info("shelf of a dropped canon list without a record", "event", "stray_shelf", "app", "jellyfin", "title", b.Name, "id", b.ID)
		}
	}
	if err := j.env.Store.PutJSON(ctx, name, "jellyfin_report", r); err != nil {
		j.log.Warn("jellyfin report not kept", "event", "store_error", "err", err)
	}
	return r
}

type boxSet struct {
	ID          string    `json:"Id"`
	Type        string    `json:"Type"`
	Name        string    `json:"Name"`
	Overview    string    `json:"Overview"`
	DateCreated time.Time `json:"DateCreated"`
}

func (j *jellyfinSink) boxSets(ctx context.Context) ([]boxSet, bool) {
	var out struct {
		Items []boxSet `json:"Items"`
	}
	ok := j.w.read(ctx, "boxsets", func(ctx context.Context) error {
		return j.env.Jellyfin.GetJSON(ctx, "/Items?Recursive=true&IncludeItemTypes=BoxSet&Fields=Overview,DateCreated&userId="+j.user, &out)
	})
	return out.Items, ok
}

func (j *jellyfinSink) reconcile(ctx context.Context, shelves []shelf, retire []string) {
	if j.w.stopped != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, j.opt.JellyfinBudget)
	defer cancel()
	strays, err := recordStrays(ctx, j.env.Store.DB(), "jellyfin", j.strays)
	if err != nil {
		j.w.stopped = "state: " + err.Error()
		return
	}
	// deleting a BoxSet does not depend on the movie listings, so it goes ahead during a scan
	j.retire(ctx, slices.Concat(retire, strays))
	if j.scanning {
		j.w.stopped = "Jellyfin is scanning its movie library"
		return
	}
	if len(j.byTMDB) == 0 {
		j.w.stopped = "no TMDB id → item map yet"
		return
	}
	for _, s := range shelves {
		j.shelf(ctx, s, j.sets)
		if j.w.stopped != "" {
			return
		}
	}
}

// retire deletes the BoxSets of retired lists, each looked up by its id. As for Plex, only a BoxSet with the stage's
// overview or none is deleted (DELETE /Items would delete a movie's files too, so the type is checked first), a
// record is forgotten only once the BoxSet is confirmed gone, and a 404 confirms nothing while Jellyfin scans: items
// drop out of its answers then and come back.
func (j *jellyfinSink) retire(ctx context.Context, keys []string) {
	db := j.env.Store.DB()
	for _, key := range keys {
		row, err := getRow(ctx, db, "jellyfin", key)
		if err != nil {
			j.w.stopped = "state: " + err.Error()
			return
		}
		if row.ID == "" {
			continue
		}
		var b boxSet
		gone := false
		if !j.w.read(ctx, "look up "+key, func(ctx context.Context) error {
			err := j.env.Jellyfin.GetJSON(ctx, "/Items/"+row.ID+"?userId="+j.user, &b)
			if errors.Is(err, jellyfin.ErrNotFound) {
				gone = true
				return nil
			}
			return err
		}) {
			return
		}
		switch {
		case gone && j.scanning:
			continue
		case gone:
		case b.Type != "BoxSet":
			j.log.Warn("record names an item that is not a BoxSet; left alone", "event", "retire_refused", "app", "jellyfin", "list", key, "id", row.ID)
			continue
		case b.Overview == "" || strings.HasPrefix(b.Overview, summaryMarker):
			if !j.w.write(ctx, "retire "+b.Name, func(ctx context.Context) error { return j.env.Jellyfin.Delete(ctx, "/Items/"+row.ID) }) {
				return
			}
			j.retired++
			j.log.Info("shelf retired", "event", "collection_retired", "app", "jellyfin", "list", key, "title", b.Name, "id", row.ID)
		default:
			continue // the owner rewrote its overview: his now
		}
		if err := deleteRow(ctx, db, "jellyfin", key); err != nil {
			j.log.Warn("state write failed", "event", "store_error", "err", err)
		}
	}
}

func (j *jellyfinSink) shelf(ctx context.Context, s shelf, sets []boxSet) {
	db := j.env.Store.DB()
	row, err := getRow(ctx, db, "jellyfin", s.List.Key)
	if err != nil {
		j.w.stopped = "state: " + err.Error()
		return
	}
	var want []string
	for _, t := range s.Members {
		if id, ok := j.byTMDB[t]; ok {
			want = append(want, id)
		}
	}
	var set *boxSet
	for i := range sets {
		if row.ID != "" && sets[i].ID == row.ID {
			set = &sets[i]
		}
	}
	if set == nil {
		if set = j.adoptOrCreate(ctx, s, &row, sets, want); set == nil {
			return
		}
	}
	// metadata first: DisplayOrder "Default" lists members in the order they were added, which is how the
	// list's order is kept (Jellyfin has no per-collection manual order and ForcedSortName would re-sort the film
	// everywhere)
	var meta map[string]any
	if !j.w.read(ctx, "describe "+s.List.Name, func(ctx context.Context) error {
		return j.env.Jellyfin.GetJSON(ctx, "/Items/"+set.ID+"?userId="+j.user, &meta)
	}) {
		return
	}
	dirty := false
	for k, v := range map[string]string{"Name": s.Title, "ForcedSortName": s.SortTitle, "Overview": s.Summary, "DisplayOrder": "Default"} {
		if have, _ := meta[k].(string); have != v {
			meta[k] = v
			dirty = true
		}
	}
	if dirty && !j.w.write(ctx, "describe "+s.List.Name, func(ctx context.Context) error {
		return j.env.Jellyfin.PostJSON(ctx, "/Items/"+set.ID, meta, nil)
	}) {
		return
	}
	var kids struct {
		Items []struct {
			ID string `json:"Id"`
		} `json:"Items"`
	}
	if !j.w.read(ctx, "members of "+s.List.Name, func(ctx context.Context) error {
		return j.env.Jellyfin.GetJSON(ctx, "/Items?ParentId="+set.ID+"&userId="+j.user+"&EnableImages=false", &kids)
	}) {
		return
	}
	var have []string
	for _, k := range kids.Items {
		have = append(have, k.ID)
	}
	// keep the longest common prefix, drop the rest, append the list's tail in order
	p := 0
	for p < len(have) && p < len(want) && have[p] == want[p] {
		p++
	}
	for _, b := range batches(have[p:], 50) {
		path := "/Collections/" + set.ID + "/Items?ids=" + strings.Join(b, ",")
		if !j.w.write(ctx, "drop from "+s.List.Name, func(ctx context.Context) error { return j.env.Jellyfin.Delete(ctx, path) }) {
			return
		}
	}
	for _, b := range batches(want[p:], 50) {
		path := "/Collections/" + set.ID + "/Items?ids=" + strings.Join(b, ",")
		if !j.w.write(ctx, "add to "+s.List.Name, func(ctx context.Context) error { return j.env.Jellyfin.PostJSON(ctx, path, nil, nil) }) {
			return
		}
	}
	if s.Art.key != "" && row.Poster != s.Art.key {
		img, ctype, err := s.Art.get()
		if err != nil {
			j.log.Warn("poster unavailable", "event", "poster_error", "list", s.List.Name, "err", err)
			return
		}
		body := []byte(base64.StdEncoding.EncodeToString(img))
		if !j.w.write(ctx, "poster of "+s.List.Name, func(ctx context.Context) error {
			return j.env.Jellyfin.PostBody(ctx, "/Items/"+set.ID+"/Images/Primary", ctype, body)
		}) {
			return
		}
		row.Poster = s.Art.key
		if err := putRow(ctx, db, "jellyfin", s.List.Key, row); err != nil {
			j.log.Warn("state write failed", "event", "store_error", "err", err)
		}
	}
}

func (j *jellyfinSink) adoptOrCreate(ctx context.Context, s shelf, row *sinkRow, sets []boxSet, want []string) *boxSet {
	db := j.env.Store.DB()
	// as for Plex: the stage's overview, else the only BoxSet with the title, made in answer to the pending create
	adoptable := func(sets []boxSet) *boxSet {
		var same []int
		for i, b := range sets {
			if !sameTitle(b.Name, s.Title) {
				continue
			}
			if strings.HasPrefix(b.Overview, summaryMarker) {
				return &sets[i]
			}
			same = append(same, i)
		}
		if len(same) == 1 && answersCreate(sets[same[0]].DateCreated, row.IntentAt) {
			return &sets[same[0]]
		}
		return nil
	}
	keep := func(b *boxSet) *boxSet {
		if row.ID != b.ID { // another BoxSet than the record's (the old one deleted by hand): no poster yet
			row.Poster = ""
		}
		row.ID, row.IntentAt = b.ID, time.Time{}
		if err := putRow(ctx, db, "jellyfin", s.List.Key, *row); err != nil {
			j.w.stopped = "state: " + err.Error()
			return nil
		}
		return b
	}
	if b := adoptable(sets); b != nil {
		return keep(b)
	}
	for _, b := range sets {
		if sameTitle(b.Name, s.Title) {
			j.taken++
			j.log.Warn("a BoxSet of the owner's has this title; list not shelved", "event", "name_taken", "app", "jellyfin",
				"list", s.List.Name, "title", s.Title, "boxset", b.ID)
			return nil
		}
	}
	if len(want) == 0 {
		return nil
	}
	row.IntentAt = j.env.Now()
	if err := putRow(ctx, db, "jellyfin", s.List.Key, *row); err != nil {
		j.w.stopped = "state: " + err.Error()
		return nil
	}
	first := want
	if len(first) > 50 {
		first = first[:50]
	}
	var made struct {
		ID string `json:"Id"`
	}
	path := "/Collections?" + url.Values{"name": {s.Title}, "ids": {strings.Join(first, ",")}, "isLocked": {"true"}}.Encode()
	if !j.w.writeOnce(ctx, "create "+s.List.Name, func(ctx context.Context) error {
		if err := j.env.Jellyfin.PostJSON(ctx, path, nil, &made); err != nil || made.ID != "" {
			return err
		}
		return errors.New("no id in the answer")
	}) {
		return nil
	}
	j.log.Info("boxset made", "event", "collection_created", "app", "jellyfin", "list", s.List.Name, "id", made.ID)
	return keep(&boxSet{ID: made.ID, Name: s.Title})
}
