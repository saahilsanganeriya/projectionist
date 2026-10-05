package lists

// Stateful stand-ins for the parts of Plex, Jellyfin and Letterboxd the stage touches, so a test can run the stage
// twice and assert the second run sends nothing. Each records every request.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
)

type plexColl0 struct {
	key, title, titleSort, summary, sort, mode string
	added                                      int64
	items                                      []string        // shown order
	placed                                     map[string]bool // custom sort: members given a position by a move
	poster                                     []byte
}

// show orders members the way Plex 1.43 does under custom sort: placed members keep their order, the others follow
// by title (here: key). Measured: [A] + add [M, P, I] shows [I, A, M, P]; after a move every member is placed.
func (c *plexColl0) show() {
	if c.sort != "2" {
		return
	}
	var placed, loose []string
	for _, k := range c.items {
		if c.placed[k] {
			placed = append(placed, k)
		} else {
			loose = append(loose, k)
		}
	}
	sort.Strings(loose)
	c.items = append(placed, loose...)
}

type fakePlex struct {
	mu       sync.Mutex
	srv      *httptest.Server
	movies   map[string]int // ratingKey → tmdb
	synopsis map[string]string
	watched  map[string]bool
	colls    []*plexColl0
	hubs     []map[string]any
	calls    []string // "METHOD path?query" of every request
	next     int
	now      func() int64
	fail     bool            // answer 500 to everything
	failAt   int             // answer 500 to section listings starting here (0 = never)
	hold     chan struct{}   // when set, every request waits until it is closed: a Plex too busy to answer
	failOn   string          // answer 500 to requests starting with this "METHOD /path"
	loseOn   string          // like failOn, but the request is applied first: its answer is lost
	hidden   map[string]bool // collections left out of the section's collections listing
}

func (p *fakePlex) coll(key string) *plexColl0 {
	for _, c := range p.colls {
		if c.key == key {
			return c
		}
	}
	return nil
}

func (p *fakePlex) writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// rekey re-adds a film under a new ratingKey, as Plex does when a file is deleted and added again: the old key is
// gone from the section and from every collection.
func (p *fakePlex) rekey(old, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.movies[key] = p.movies[old]
	delete(p.movies, old)
	for _, c := range p.colls {
		c.items = without(c.items, old)
	}
}

func newFakePlex(t *testing.T, movies map[string]int, now func() int64) *fakePlex {
	p := &fakePlex{movies: movies, synopsis: map[string]string{}, watched: map[string]bool{}, next: 9000, now: now, hidden: map[string]bool{}}
	p.hubs = []map[string]any{{"identifier": "movie.recentlyreleased"}, {"identifier": "movie.recentlyadded", "promotedToOwnHome": true},
		{"identifier": "movie.curated", "promotedToOwnHome": true}}
	mc := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
	keysOf := func(uri string) []string { // like Plex 1.43: a key it does not hold is dropped
		_, list, _ := strings.Cut(uri, "/library/metadata/")
		var keys []string
		for _, k := range strings.Split(list, ",") {
			if _, ok := p.movies[k]; ok {
				keys = append(keys, k)
			}
		}
		return keys
	}
	collJSON := func(c *plexColl0) map[string]any {
		m := map[string]any{"ratingKey": c.key, "type": "collection", "subtype": "movie", "title": c.title, "summary": c.summary, "addedAt": c.added}
		if c.titleSort != "" {
			m["titleSort"] = c.titleSort
		}
		if c.sort != "" {
			m["collectionSort"] = c.sort
		}
		if c.mode != "" {
			m["collectionMode"] = c.mode
		}
		return m
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, r *http.Request) { mc(w, map[string]any{"machineIdentifier": "mid"}) })
	mux.HandleFunc("GET /library/sections/4/all", func(w http.ResponseWriter, r *http.Request) {
		var keys []string
		for k := range p.movies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
		if p.failAt > 0 && start >= p.failAt {
			http.Error(w, "timeout", http.StatusInternalServerError)
			return
		}
		size, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
		total := len(keys)
		keys = keys[min(start, total):min(start+size, total)]
		var md []map[string]any
		for _, k := range keys {
			tmdb := p.movies[k]
			m := map[string]any{"ratingKey": k, "guid": "plex://movie/5d77" + k, // Plex sends both keys, differing in case
				"Guid": []map[string]string{{"id": "imdb://tt1"}, {"id": fmt.Sprintf("tmdb://%d", tmdb)}}}
			if p.watched[k] {
				m["viewCount"] = 1
			}
			md = append(md, m)
		}
		mc(w, map[string]any{"totalSize": total, "size": len(md), "Metadata": md})
	})
	mux.HandleFunc("PUT /library/sections/4/all", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		c := p.coll(q.Get("id"))
		for f, dst := range map[string]*string{"title": &c.title, "titleSort": &c.titleSort, "summary": &c.summary} {
			if v, ok := q[f+".value"]; ok {
				*dst = v[0]
			}
		}
	})
	mux.HandleFunc("GET /library/sections/4/collections", func(w http.ResponseWriter, r *http.Request) {
		var md []map[string]any
		for _, c := range p.colls {
			if !p.hidden[c.key] {
				md = append(md, collJSON(c))
			}
		}
		mc(w, map[string]any{"Metadata": md})
	})
	mux.HandleFunc("GET /library/metadata/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := p.movies[id]; ok {
			mc(w, map[string]any{"Metadata": []any{map[string]any{"ratingKey": id, "type": "movie", "summary": p.synopsis[id]}}})
			return
		}
		c := p.coll(id)
		if c == nil {
			http.NotFound(w, r)
			return
		}
		mc(w, map[string]any{"Metadata": []any{collJSON(c)}})
	})
	mux.HandleFunc("POST /library/collections", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.next++
		c := &plexColl0{key: fmt.Sprint(p.next), title: q.Get("title"), added: p.now(), items: keysOf(q.Get("uri"))}
		for _, old := range p.colls { // measured on 1.43: a title that differs only in case joins the existing tag
			if strings.EqualFold(old.title, c.title) {
				old.items = append(old.items, c.items...)
				c.title, c.items = old.title, append([]string(nil), old.items...)
			}
		}
		p.colls = append(p.colls, c)
		mc(w, map[string]any{"Metadata": []any{collJSON(c)}})
	})
	mux.HandleFunc("DELETE /library/collections/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		delete(p.movies, id) // what Plex would do with a movie's key here is unmeasured: assume the worst
		var rest []*plexColl0
		for _, c := range p.colls {
			if c.key != id {
				rest = append(rest, c)
			}
		}
		p.colls = rest
		var hubs []map[string]any
		for _, h := range p.hubs { // measured: deleting a collection drops its Home row
			if h["identifier"] != "custom.collection.4."+id {
				hubs = append(hubs, h)
			}
		}
		p.hubs = hubs
	})
	mux.HandleFunc("GET /library/collections/{id}/children", func(w http.ResponseWriter, r *http.Request) {
		// at load 20 a members read with its Media elements took 108 s, five times a 20 s timeout
		if !strings.Contains(r.URL.Query().Get("excludeElements"), "Media") {
			http.Error(w, "timeout", http.StatusGatewayTimeout)
			return
		}
		c := p.coll(r.PathValue("id"))
		var md []map[string]any
		for _, k := range c.items {
			md = append(md, map[string]any{"ratingKey": k})
		}
		mc(w, map[string]any{"Metadata": md})
	})
	mux.HandleFunc("PUT /library/collections/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		c, keys := p.coll(r.PathValue("id")), keysOf(r.URL.Query().Get("uri"))
		if len(keys) == 0 { // measured: a batch of only keys Plex does not hold is refused
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		c.items = append(c.items, keys...)
		c.show()
	})
	mux.HandleFunc("DELETE /library/collections/{id}/items/{item}", func(w http.ResponseWriter, r *http.Request) {
		c := p.coll(r.PathValue("id"))
		c.items = without(c.items, r.PathValue("item"))
	})
	mux.HandleFunc("PUT /library/collections/{id}/items/{item}/move", func(w http.ResponseWriter, r *http.Request) {
		c, item := p.coll(r.PathValue("id")), r.PathValue("item")
		if indexOf(c.items, item) < 0 { // measured: moving a non-member, or a key Plex does not hold, is refused
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, k := range c.items { // a move gives every member a position, in the order shown
			c.placed[k] = true
		}
		c.items = without(c.items, item)
		at := 0
		if after := r.URL.Query().Get("after"); after != "" {
			at = indexOf(c.items, after) + 1 // measured: after a non-member, the item goes to the front
		}
		c.items = append(c.items[:at], append([]string{item}, c.items[at:]...)...)
	})
	mux.HandleFunc("PUT /library/metadata/{id}/prefs", func(w http.ResponseWriter, r *http.Request) {
		c := p.coll(r.PathValue("id"))
		if s := r.URL.Query().Get("collectionSort"); s != "" {
			if s == "2" && c.sort != "2" { // switching to custom: nothing is placed yet
				c.placed = map[string]bool{}
			}
			c.sort = s
			c.show()
		}
		if m := r.URL.Query().Get("collectionMode"); m != "" {
			c.mode = m
		}
	})
	mux.HandleFunc("POST /library/metadata/{id}/posters", func(w http.ResponseWriter, r *http.Request) {
		p.coll(r.PathValue("id")).poster, _ = io.ReadAll(r.Body)
	})
	mux.HandleFunc("GET /hubs/sections/4/manage", func(w http.ResponseWriter, r *http.Request) {
		mc(w, map[string]any{"Hub": p.hubs})
	})
	mux.HandleFunc("POST /hubs/sections/4/manage", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id := "custom.collection.4." + q.Get("metadataItemId")
		h := map[string]any{"identifier": id}
		found := false
		for _, x := range p.hubs {
			if x["identifier"] == id {
				h, found = x, true
			}
		}
		h["promotedToOwnHome"], h["promotedToRecommended"], h["promotedToSharedHome"] =
			q.Get("promotedToOwnHome") == "1", q.Get("promotedToRecommended") == "1", q.Get("promotedToSharedHome") == "1"
		if !found {
			p.hubs = append(p.hubs, h)
		}
	})
	mux.HandleFunc("PUT /hubs/sections/4/manage/{ident}/move", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("ident")
		var moved map[string]any
		var rest []map[string]any
		var ids []string
		for _, h := range p.hubs {
			if h["identifier"] == id {
				moved = h
				continue
			}
			rest = append(rest, h)
			ids = append(ids, h["identifier"].(string))
		}
		at := 0
		if after := r.URL.Query().Get("after"); after != "" {
			at = indexOf(ids, after) + 1
		}
		p.hubs = append(rest[:at], append([]map[string]any{moved}, rest[at:]...)...)
	})
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		hold := p.hold
		p.mu.Unlock()
		if hold != nil {
			<-hold
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-Plex-Token") != fake.PlexToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if p.fail || (p.failOn != "" && strings.HasPrefix(r.Method+" "+r.URL.Path, p.failOn)) {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		if p.loseOn != "" && strings.HasPrefix(r.Method+" "+r.URL.Path, p.loseOn) {
			mux.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "timeout", http.StatusGatewayTimeout)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func without(xs []string, x string) []string {
	out := xs[:0:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

type boxSet0 struct {
	id, name, overview, forcedSort, order string
	created                               time.Time
	items                                 []string
	poster                                []byte
}

type fakeJellyfin struct {
	mu       sync.Mutex
	srv      *httptest.Server
	movies   map[string]int // item id → tmdb
	synopsis map[string]string
	played   map[string]bool
	sets     []*boxSet0
	calls    []string
	next     int
	scanning string            // "library": Movies is Active; "task": only the scan task runs. Half the movies drop out
	onWrite  func(call string) // called with each write, after it is applied
	failOn   string            // answer 500 to requests starting with this "METHOD /path"
	loseOn   string            // like failOn, but the request is applied first: its answer is lost
	hidden   map[string]bool   // BoxSets missing from every answer while it scans, as its movies are
	now      func() time.Time
}

func (j *fakeJellyfin) set(id string) *boxSet0 {
	for _, s := range j.sets {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (j *fakeJellyfin) writes() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for _, c := range j.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func newFakeJellyfin(t *testing.T, movies map[string]int, now func() time.Time) *fakeJellyfin {
	j := &fakeJellyfin{movies: movies, synopsis: map[string]string{}, played: map[string]bool{}, hidden: map[string]bool{}, now: now}
	visible := func(s *boxSet0) bool { return s != nil && !(j.scanning != "" && j.hidden[s.id]) }
	js := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	ids := func(r *http.Request) []string { return strings.Split(r.URL.Query().Get("ids"), ",") }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /Users", func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]string{{"Id": "u-roger", "Name": "roger"}, {"Id": "u-saahil", "Name": "saahil"}})
	})
	mux.HandleFunc("GET /Library/VirtualFolders", func(w http.ResponseWriter, r *http.Request) {
		status := map[bool]string{true: "Active", false: "Idle"}[j.scanning == "library"]
		js(w, []map[string]any{{"Name": "Movies", "CollectionType": "movies", "RefreshStatus": status},
			{"Name": "Collections", "CollectionType": "boxsets", "RefreshStatus": "Idle"}})
	})
	mux.HandleFunc("GET /ScheduledTasks", func(w http.ResponseWriter, r *http.Request) {
		state := map[bool]string{true: "Running", false: "Idle"}[j.scanning != ""]
		js(w, []map[string]any{{"Key": "RefreshLibrary", "Name": "Scan Media Library", "State": state}, {"Key": "CleanCache", "State": "Idle"}})
	})
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var items []map[string]any
		switch {
		case q.Get("IncludeItemTypes") == "Movie":
			for id, tmdb := range j.movies {
				// like Jellyfin mid-rescan: a user-scoped listing shows only part of the library, except for played films
				if q.Get("userId") != "" && (q.Get("IsPlayed") == "true") != j.played[id] {
					continue
				}
				if j.scanning != "" && tmdb%2 == 1 {
					continue
				}
				items = append(items, map[string]any{"Id": id, "ProviderIds": map[string]string{"Tmdb": fmt.Sprint(tmdb)}})
			}
		case q.Get("IncludeItemTypes") == "BoxSet":
			for _, s := range j.sets {
				if visible(s) {
					items = append(items, map[string]any{"Id": s.id, "Name": s.name, "Overview": s.overview, "DateCreated": s.created})
				}
			}
		case q.Get("ParentId") != "":
			s := j.set(q.Get("ParentId"))
			kids := append([]string(nil), s.items...)
			if s.order != "Default" { // like Jellyfin: premiere-date order unless DisplayOrder is Default
				sort.Strings(kids)
			}
			for _, id := range kids {
				if j.scanning != "" && j.movies[id]%2 == 1 { // measured mid-scan: members drop out of a BoxSet's listing too
					continue
				}
				items = append(items, map[string]any{"Id": id})
			}
		}
		js(w, map[string]any{"Items": items})
	})
	mux.HandleFunc("GET /Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := j.movies[id]; ok {
			js(w, map[string]any{"Id": id, "Type": "Movie", "Overview": j.synopsis[id]})
			return
		}
		s := j.set(id)
		if !visible(s) {
			http.NotFound(w, r)
			return
		}
		js(w, map[string]any{"Id": s.id, "Type": "BoxSet", "Name": s.name, "Overview": nilIfEmpty(s.overview), "ForcedSortName": nilIfEmpty(s.forcedSort),
			"DisplayOrder": s.order, "LockData": true, "ProviderIds": map[string]string{}})
	})
	mux.HandleFunc("POST /Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		s := j.set(r.PathValue("id"))
		s.name, _ = m["Name"].(string)
		s.overview, _ = m["Overview"].(string)
		s.forcedSort, _ = m["ForcedSortName"].(string)
		s.order, _ = m["DisplayOrder"].(string)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /Items/{id}/Images/Primary", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		j.set(r.PathValue("id")).poster, _ = base64.StdEncoding.DecodeString(string(b))
	})
	mux.HandleFunc("POST /Collections", func(w http.ResponseWriter, r *http.Request) {
		j.next++
		s := &boxSet0{id: fmt.Sprintf("bs%d", j.next), name: r.URL.Query().Get("name"), order: "PremiereDate", items: ids(r), created: j.now()}
		j.sets = append(j.sets, s)
		js(w, map[string]string{"Id": s.id})
	})
	mux.HandleFunc("POST /Collections/{id}/Items", func(w http.ResponseWriter, r *http.Request) {
		s := j.set(r.PathValue("id"))
		s.items = append(s.items, ids(r)...)
	})
	mux.HandleFunc("DELETE /Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		delete(j.movies, r.PathValue("id")) // like Jellyfin: deleting a movie item deletes the film
		var rest []*boxSet0
		for _, s := range j.sets {
			if s.id != r.PathValue("id") {
				rest = append(rest, s)
			}
		}
		j.sets = rest
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /Collections/{id}/Items", func(w http.ResponseWriter, r *http.Request) {
		s := j.set(r.PathValue("id"))
		for _, id := range ids(r) {
			s.items = without(s.items, id)
		}
	})
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.calls = append(j.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != `MediaBrowser Token="`+fake.JellyfinKey+`"` {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if j.failOn != "" && strings.HasPrefix(r.Method+" "+r.URL.Path, j.failOn) {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		if j.loseOn != "" && strings.HasPrefix(r.Method+" "+r.URL.Path, j.loseOn) {
			mux.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "timeout", http.StatusGatewayTimeout)
			return
		}
		mux.ServeHTTP(w, r)
		if r.Method != http.MethodGet && j.onWrite != nil {
			j.onWrite(j.calls[len(j.calls)-1])
		}
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fakeLetterboxd serves pages by path (a test may swap them between runs) and film pages from a slug → TMDB map.
type fakeLetterboxd struct {
	mu       sync.Mutex
	srv      *httptest.Server
	pages    map[string][]byte
	redirect map[string]string // boxd.it code → path
	films    map[string]string // slug → "movie/949" or "tv/87108"
	status   map[string]int    // path → an error status to answer with (403: a refusal)
	answer   func()            // called before each answer: a test's slow Letterboxd advances its clock here
	requests []string
}

func newFakeLetterboxd(t *testing.T) *fakeLetterboxd {
	lb := &fakeLetterboxd{pages: map[string][]byte{}, redirect: map[string]string{}, films: map[string]string{}, status: map[string]int{}}
	lb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lb.mu.Lock()
		defer lb.mu.Unlock()
		lb.requests = append(lb.requests, r.URL.Path)
		if lb.answer != nil {
			lb.answer()
		}
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no agent", http.StatusForbidden)
			return
		}
		if code, ok := lb.status[r.URL.Path]; ok {
			http.Error(w, "refused", code)
			return
		}
		if p, ok := lb.redirect[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			http.Redirect(w, r, lb.srv.URL+p, http.StatusFound)
			return
		}
		if slug, ok := strings.CutPrefix(r.URL.Path, "/film/"); ok {
			if id, ok := lb.films[strings.TrimSuffix(slug, "/")]; ok {
				fmt.Fprintf(w, `<body class="film backdropped" data-type="film" data-tmdb-type="movie" data-tmdb-id=""><a href="https://www.themoviedb.org/%s/" class="micro-button track-event" data-track-action="TMDB" target="_blank" >TMDB</a></body>`, id)
				return
			}
		}
		if b, ok := lb.pages[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(lb.srv.Close)
	return lb
}

func (lb *fakeLetterboxd) count() int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return len(lb.requests)
}
