package tonight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"projectionist/internal/fake"
	"projectionist/internal/lbpace"
	"projectionist/internal/stage"
	"projectionist/internal/watch"
)

// ---- decision tables ----

// Catches: re-picking at UTC midnight (5 p.m. his time) or before pick_after, instead of once each local morning.
func TestPickDay(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ utc, want string }{
		{"2026-10-05T11:59:00Z", "2026-10-04"}, // 04:59 PDT: yesterday's picks still show
		{"2026-10-05T12:00:00Z", "2026-10-05"}, // 05:00 PDT
		{"2026-10-06T06:30:00Z", "2026-10-05"}, // 23:30 PDT, already the 6th in UTC
	} {
		now, _ := time.Parse(time.RFC3339, c.utc)
		if got := pickDay(now, la, 5*time.Hour); got != c.want {
			t.Errorf("pickDay(%s) = %s, want %s", c.utc, got, c.want)
		}
	}
}

// Catches: recommending a film he logged under a differently spelt title, TMDB's title rather than the folder's, or
// a year off by one; and the reverse, hiding a different film that shares a title.
func TestSeen(t *testing.T) {
	w := newWatched()
	w.add("owner", []entry{{Name: "Mission: Impossible", Year: 1996}, {Name: "Amélie", Year: 2001}, {Name: "Dune", Year: 2021},
		{Name: "Se7en", Year: 1995}},
		map[string]entry{"g": {Name: "Ad Astra", Year: 2019, TMDB: 419704}})
	for _, c := range []struct {
		f    film
		want bool
	}{
		{film{TMDB: 419704, Title: "Ad Astra", Year: 2020}, true}, // the feed's TMDB id decides, whatever the year
		{film{TMDB: 954, Title: "Mission - Impossible", Year: 1996}, true},
		{film{TMDB: 194, Title: "Amelie", Year: 2001}, true},
		{film{TMDB: 1, Title: "The Fabulous Destiny", OriginalTitle: "Amélie", Year: 2001}, true},
		{film{TMDB: 438631, Title: "Dune", Year: 2022}, true},
		{film{TMDB: 807, Title: "Seven", TMDBTitle: "Se7en", Year: 1995}, true}, // a 5-star film offered as unseen on 2026-10-05
		{film{TMDB: 841, Title: "Dune", Year: 1984}, false},
		{film{TMDB: 949, Title: "Heat", Year: 1995}, false},
	} {
		if got := w.seen(c.f); got != c.want {
			t.Errorf("seen(%s %d) = %v, want %v", c.f.Title, c.f.Year, got, c.want)
		}
	}
}

// Catches a pick that fails the owner's bars (Letterboxd 3.5 stars or less; critics 75 % or less where there is a
// critics score), a film with no critics score kept out although its Letterboxd average passes, a film without ratings
// or one TMDB no longer knows picked, a film the person has seen picked, a recent pick repeated while enough others
// pass (and none at all once too few do), and picks that change between two runs of one day or stay the same the
// next day, or are the same for two people on one day.
func TestShuffledFollowsTheFilter(t *testing.T) {
	lib := map[int]film{}
	for id := 1; id <= 12; id++ {
		lib[id] = film{TMDB: id, Title: fmt.Sprintf("Film %d", id), Year: 2000}
	}
	lib[13] = film{TMDB: 13, Missing: true}
	ratings := map[int]rating{1: {80, 90}, 2: {80, -1}, 3: {70, 90}, 4: {80, 75}, 5: {90, 76}, 6: {72, 100}, 7: {80, 90}, 8: {80, 90},
		9: {80, 90}, 10: {80, 90}, 11: {80, 90}, 13: {95, 99}} // 12 has no ratings
	pass := []int{1, 2, 5, 6, 7, 8, 9, 10, 11}
	opt := defaults()
	w := newWatched()
	w.IDs[11] = "server"
	recent := map[int]string{7: "2026-10-04", 8: "2026-10-04"}
	ids := func(ps []pick) []int { return slices.Sorted(slices.Values(tmdbIDs(ps))) }
	got := shuffled(lib, ratings, w, recent, opt, "2026-10-05", "owner")
	fresh := []int{1, 2, 5, 6, 9, 10} // over the bars, unseen, not picked lately
	if len(got) != 5 || slices.ContainsFunc(got, func(p pick) bool { return !slices.Contains(fresh, p.TMDB) }) {
		t.Errorf("picks %v; want 5 of %v (over the bars, unseen, not picked in the last two weeks)", ids(got), fresh)
	}
	for _, p := range got {
		r := ratings[p.TMDB]
		want := fmt.Sprintf("Letterboxd ★%.1f · Rotten Tomatoes %d%%", float64(r.Letterboxd)/20, r.Tomatoes)
		if r.Tomatoes < 0 {
			want = fmt.Sprintf("Letterboxd ★%.1f", float64(r.Letterboxd)/20)
		}
		if p.Reason != want || p.Title != lib[p.TMDB].Title {
			t.Errorf("pick %d: reason %q, title %q; want %q", p.TMDB, p.Reason, p.Title, want)
		}
	}
	if again := shuffled(lib, ratings, w, recent, opt, "2026-10-05", "owner"); !slices.Equal(tmdbIDs(again), tmdbIDs(got)) {
		t.Errorf("a second run of the day picked %v, the first %v", tmdbIDs(again), tmdbIDs(got))
	}
	differ := func(a, b []pick) bool { return !slices.Equal(tmdbIDs(a), tmdbIDs(b)) }
	if !differ(shuffled(lib, ratings, w, recent, opt, "2026-10-06", "owner"), got) ||
		!differ(shuffled(lib, ratings, w, recent, opt, "2026-10-05", "viewer-b"), got) {
		t.Error("the next day, or another person, got the same picks in the same order")
	}
	many := map[int]string{}
	for _, id := range pass {
		many[id] = "2026-10-04"
	}
	delete(many, 1)
	if got := ids(shuffled(lib, ratings, w, many, opt, "2026-10-05", "owner")); len(got) != 5 || !slices.Contains(got, 1) ||
		slices.ContainsFunc(got, func(id int) bool { return !slices.Contains(pass, id) || id == 11 }) {
		t.Errorf("with one fresh film over the bars: picks %v; want it and 4 recent ones, none seen", got)
	}
	if got := ids(shuffled(lib, map[int]rating{2: {80, -1}, 3: {70, 90}}, w, nil, opt, "2026-10-05", "owner")); !slices.Equal(got, []int{2}) {
		t.Errorf("a film with no critics score and Letterboxd 4.0: picks %v; want it, Letterboxd alone deciding", got)
	}
	if got := shuffled(lib, map[int]rating{3: {70, 90}}, w, nil, opt, "2026-10-05", "owner"); len(got) != 0 {
		t.Errorf("nothing over the bars: picks %v", ids(got))
	}
}

// ---- the stage against fake Plex, plex.tv, Jellyfin, MDBList, TMDB and Letterboxd ----

type libFilm struct {
	tmdb        int
	title       string
	year        int
	director    string
	runtime     int
	plexViewed  bool
	jellyPlayed bool
}

// The owner has seen every film below 13 (export, feed, Plex, Jellyfin); the first seven are unseen.
var films = []libFilm{
	{389, "12 Angry Men", 1957, "Sidney Lumet", 97, false, false},
	{346, "Seven Samurai", 1954, "Akira Kurosawa", 207, false, false},
	{655, "Paris, Texas", 1984, "Wim Wenders", 147, false, false},
	{1398, "Stalker", 1979, "Andrei Tarkovsky", 162, false, false},
	{949, "Heat", 1995, "Michael Mann", 170, false, false},
	{18148, "Tokyo Story", 1953, "Yasujirō Ozu", 136, false, false},
	{843, "In the Mood for Love", 2000, "Wong Kar-wai", 98, false, false},
	{559907, "The Green Knight", 2021, "David Lowery", 130, false, false},       // export watched.csv
	{954, "Mission - Impossible", 1996, "Brian De Palma", 110, false, false},    // export, punctuation differs
	{419704, "Ad Astra", 2019, "James Gray", 123, false, false},                 // feed
	{10948, "The Fox and the Hound", 1981, "Ted Berman", 83, false, false},      // feed
	{718032, "Licorice Pizza", 2021, "Paul Thomas Anderson", 133, false, false}, // export diary.csv
	{680, "Pulp Fiction", 1994, "Quentin Tarantino", 154, true, false},
	{13, "Forrest Gump", 1994, "Robert Zemeckis", 142, false, true},
}

func rk(id int) string   { return strconv.Itoa(100000 + id) }
func jfID(id int) string { return fmt.Sprintf("jf%d", id) }

type world struct {
	base    string // the options every run carries; options() adds a test's own
	h       *fake.Harness
	logs    *bytes.Buffer
	mdb     *mdbFake
	plex    *plexFake
	jf      *jfFake
	px      *plexPlaylists
	jp      *jfPlaylists
	tv      *plexTVFake
	lbHits  []string        // the path of each Letterboxd request
	lbWaits []time.Duration // what each Letterboxd request waited on the limiter, which runs on a frozen clock
	feeds   map[string][]lbItem
	films   map[string][]lbItem // each user's films page
	down    map[string]bool     // users whose feed and films page answer 503
	mu      sync.Mutex
}

// household is the three of them: the owner with his export, Viewer B (jfsync's viewer-b) and Viewer-a, each with only a
// feed and a films page.
const household = `people = [
  { person = "owner", user = "owner-lb", export_dir = "testdata/export" },
  { person = "viewer-b", user = "viewer-b-lb" },
  { person = "viewer-a", user = "viewer-a-lb" },
]
`

// viewerBReview is long enough that 6 of its words make a quote.
const viewerBReview = "Quiet buildings and quieter people, I could live inside this film forever."

// lbItem is one entry of a fake Letterboxd feed (all fields) or films page (title, year, stars).
type lbItem struct {
	id     int
	tmdb   int
	title  string
	year   int
	stars  float64
	review string
}

func slug(title string) string {
	return strings.ToLower(strings.NewReplacer(" ", "-", ",", "").Replace(title))
}

// feedXML is a feed in Letterboxd's markup, as the letterboxd stage's parser reads it.
func feedXML(user string, items []lbItem) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0" xmlns:letterboxd="https://letterboxd.com" ` +
		`xmlns:tmdb="https://themoviedb.org"><channel>`)
	for _, it := range items {
		desc := "<p>Watched on Saturday October 3, 2026.</p>"
		if it.review != "" {
			desc = "<p>" + html.EscapeString(it.review) + "</p>"
		}
		fmt.Fprintf(&b, `<item><link>https://letterboxd.com/%s/film/%s/</link><guid isPermaLink="false">letterboxd-watch-%d</guid>`+
			`<pubDate>Sun, 4 Oct 2026 19:00:38 +1300</pubDate><letterboxd:watchedDate>2026-10-03</letterboxd:watchedDate>`+
			`<letterboxd:filmTitle>%s</letterboxd:filmTitle><letterboxd:filmYear>%d</letterboxd:filmYear>`+
			`<letterboxd:memberRating>%.1f</letterboxd:memberRating><tmdb:movieId>%d</tmdb:movieId>`+
			`<description><![CDATA[%s]]></description></item>`, user, slug(it.title), it.id, html.EscapeString(it.title), it.year, it.stars, it.tmdb, desc)
	}
	return b.String() + `</channel></rss>`
}

// filmsHTML is a films page in Letterboxd's grid markup.
func filmsHTML(items []lbItem) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><body><ul class="grid">`)
	for _, it := range items {
		fmt.Fprintf(&b, `<li class="griditem"><div class="react-component" data-item-name="%s (%d)" data-item-slug="%s"></div>`+
			`<p class="poster-viewingdata"><span class="rating -micro -darker rated-%d">★</span></p></li>`,
			html.EscapeString(it.title), it.year, slug(it.title), int(it.stars*2))
	}
	return b.String() + `</ul></body></html>`
}

// newWorld adds fillers unseen films to the library (TMDB knows them; Plex and Jellyfin do not), for tests that
// need the real library's ~190 candidate lines.
func newWorld(t *testing.T, options string, fillers int) *world {
	t.Helper()
	w := &world{h: fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "machine1", "PlexOnlineUsername": "owner-plex"}),
		logs: &bytes.Buffer{}}
	w.h.Env.Log = slog.New(slog.NewTextHandler(w.logs, nil))
	la, _ := time.LoadLocation("America/Los_Angeles")
	w.h.Env.Loc = la
	w.h.Now = time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC) // 08:00 PDT
	shared, frozen := limiter, w.h.Now
	limiter = lbpace.New(func() time.Time { return frozen }, func(_ context.Context, d time.Duration) error {
		w.lbWaits = append(w.lbWaits, d)
		return nil
	})
	t.Cleanup(func() { limiter = shared })

	movies := t.TempDir()
	lib := slices.Clone(films)
	for i := range fillers {
		lib = append(lib, libFilm{tmdb: 700000 + i, title: fmt.Sprintf("Filler %d", i), year: 2000, director: "Filler Director", runtime: 100})
	}
	for _, f := range lib {
		name := fmt.Sprintf("%s (%d) {tmdb-%d}", f.title, f.year, f.tmdb)
		write(t, filepath.Join(movies, name, name+".mkv"), "")
		details, _ := json.Marshal(map[string]any{"id": f.tmdb, "title": f.title, "original_title": f.title, "runtime": f.runtime,
			"original_language": "en", "overview": "First sentence. Second sentence.", "poster_path": fmt.Sprintf("/p%d.jpg", f.tmdb),
			"vote_average": 8.0, "vote_count": 5000, "genres": []map[string]string{{"name": "Drama"}},
			"production_countries": []map[string]string{{"iso_3166_1": "US"}}})
		w.h.TMDB.Handle("GET", fmt.Sprintf("/movie/%d", f.tmdb), 200, string(details))
		w.h.TMDB.Handle("GET", fmt.Sprintf("/movie/%d/credits", f.tmdb), 200,
			fmt.Sprintf(`{"crew":[{"job":"Writer","name":"Someone"},{"job":"Director","name":%q}]}`, f.director))
	}
	write(t, filepath.Join(movies, "Some.Film.2020.1080p.WEB", "a.mkv"), "") // outside the naming scheme: ignored

	thumb := &bytes.Buffer{}
	if err := jpeg.Encode(thumb, image.NewRGBA(image.Rect(0, 0, 34, 51)), nil); err != nil {
		t.Fatal(err)
	}
	// Viewer B and Viewer-a log films outside the unseen seven by default: each test adds what it needs.
	w.feeds = map[string][]lbItem{
		"viewer-b-lb": {{id: 11, tmdb: 429191, title: "Columbus", year: 2017, stars: 5, review: viewerBReview},
			{id: 12, tmdb: 680, title: "Pulp Fiction", year: 1994, stars: 4}},
		"viewer-a-lb": {{id: 21, tmdb: 603, title: "The Matrix", year: 1999, stars: 4.5, review: "the lobby scene still rules"}},
	}
	w.films = map[string][]lbItem{"viewer-b-lb": {{title: "Columbus", year: 2017, stars: 5}},
		"viewer-a-lb": {{title: "Spirited Away", year: 2001, stars: 5}}}
	w.down = map[string]bool{}
	ext := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		user, page, _ := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case page == "rss" || page == "films":
			w.lbHits = append(w.lbHits, r.URL.Path)
			if w.down[user] {
				http.Error(rw, "unavailable", http.StatusServiceUnavailable)
				return
			}
			var body []byte
			switch {
			case r.URL.Path == "/owner-lb/rss/":
				body, _ = os.ReadFile("testdata/rss.xml")
			case page == "rss":
				body = []byte(feedXML(user, w.feeds[user]))
			default:
				body = []byte(filmsHTML(w.films[user]))
			}
			etag := fmt.Sprintf(`"%d-%d"`, len(body), crc32.ChecksumIEEE(body))
			if r.Header.Get("If-None-Match") == etag {
				rw.WriteHeader(http.StatusNotModified)
				return
			}
			rw.Header().Set("ETag", etag)
			rw.Write(body)
		case strings.HasPrefix(r.URL.Path, "/img/w342/"):
			rw.Write(thumb.Bytes())
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(ext.Close)

	w.mdb = newMDBFake(t, lib)
	w.plex = newPlexFake(w.h.Plex)
	w.jf = newJFFake(w.h.Jellyfin)
	w.px, w.jp, w.tv = newPlexPlaylists(w.h.Plex), newJFPlaylists(w.h.Jellyfin, w.jf), newPlexTV(t)
	key := filepath.Join(t.TempDir(), "mdblist_api_key")
	write(t, key, "test-mdb-key\n")
	w.base = fmt.Sprintf(`
picks = 3
movies_dir = %q
letterboxd_url = %q
image_base = %q
plex_tv = %q
mdblist_url = %q
mdblist_key_file = %q
`, movies, ext.URL, ext.URL+"/img", w.tv.srv.URL, w.mdb.srv.URL, key)
	w.h.Env.PeerOptions = func(stage string, v any) error {
		if stage != "jfsync" {
			return nil
		}
		_, err := toml.Decode(jfsyncUsers, v)
		return err
	}
	w.options(options)
	return w
}

// options sets what the next runs read: the world's base, then text. Unless text names them, the people are the owner
// alone, with his export.
func (w *world) options(text string) {
	if !strings.Contains(text, "people") {
		text += `people = [{ person = "owner", user = "owner-lb", plex_user = "owner-plex", export_dir = "testdata/export" }]` + "\n"
	}
	w.h.SetOptions(w.base + text)
}

// record adds one event to the watch record, as jfsync does for a play (Finished) or a rating (Rated).
func (w *world) record(t *testing.T, person string, tmdb int, kind watch.Kind, rating float64) {
	t.Helper()
	ctx, db := context.Background(), w.h.Env.Store.DB()
	e := watch.Event{Person: person, Item: fmt.Sprintf("movie/tmdb-%d", tmdb), Ref: fmt.Sprintf("plex:%d", tmdb), Source: "plex-webhook",
		At: w.h.Now, Kind: kind}
	if kind == watch.Rated {
		e.Rating = &rating
	}
	if err := watch.Open(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := watch.Add(ctx, db, e); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *world) run(t *testing.T) *stage.Report {
	t.Helper()
	rep := &stage.Report{}
	if err := (Stage{}).Run(context.Background(), w.h.Env, rep); err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, w.logs)
	}
	return rep
}

func tmdbIDs(picks []pick) []int {
	ids := make([]int, len(picks))
	for i, p := range picks {
		ids[i] = p.TMDB
	}
	return ids
}

func (w *world) state(t *testing.T) state {
	t.Helper()
	var st state
	if _, err := w.h.Env.Store.GetJSON(context.Background(), name, "state", &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// ---- fake MDBList: every library film over the bars unless a test rates it otherwise ----

type mdbFake struct {
	srv     *httptest.Server
	mu      sync.Mutex
	ratings map[int][2]int // tmdb → Letterboxd, critics scores (-1: none); a film absent is one MDBList does not know
	asked   [][]int        // each request's ids
	quota   bool           // answers 429
}

func newMDBFake(t *testing.T, lib []libFilm) *mdbFake {
	m := &mdbFake{ratings: map[int][2]int{}}
	for _, f := range lib {
		m.ratings[f.tmdb] = [2]int{80, 90}
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.quota || r.URL.Query().Get("apikey") != "test-mdb-key" {
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var body struct{ IDs []int }
		json.NewDecoder(r.Body).Decode(&body)
		m.asked = append(m.asked, body.IDs)
		out := []map[string]any{}
		for _, id := range body.IDs {
			if s, ok := m.ratings[id]; ok {
				out = append(out, map[string]any{"ids": map[string]int{"tmdb": id}, "ratings": []map[string]any{
					{"source": "letterboxd", "score": s[0]}, {"source": "tomatoes", "score": s[1]}}})
			}
		}
		json.NewEncoder(rw).Encode(out)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// rate gives films these scores; a test calls it before the run that first asks about them (they are then kept).
func (m *mdbFake) rate(lb, rt int, ids ...int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.ratings[id] = [2]int{lb, rt}
	}
}

// onlyPass leaves ids alone over the bars among films.
func (m *mdbFake) onlyPass(ids ...int) {
	for _, f := range films {
		if !slices.Contains(ids, f.tmdb) {
			m.rate(60, 90, f.tmdb)
		}
	}
}

func (m *mdbFake) requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asked)
}

// ---- fake Plex: one Movies section with the films above, and the shelves the stage makes ----

// Every collection the stage makes is a shelf: the first is 900, the next 901, and so on. Its fault switches (a lost
// create answer, a failed read-back, poster or hub write, a stop inside a member's removal) act on any shelf.
type plexFake struct {
	srv     *fake.Server
	mu      sync.Mutex
	viewed  map[string]bool // ratingKey → played
	hubs    []plexHub
	shelves []*fakeShelf
	foreign []map[string]any // collections not ours beside 777, listed and readable, with no route that writes them

	dropCreate   bool           // POST /library/collections makes the shelf, then hangs up: its answer is lost
	failReadBack int            // GET /library/metadata/<shelf> answers 503 this many times first
	onCreate     func()         // runs inside the create, e.g. to stop the run there
	fail         map[string]int // "poster" | "hub" (the hub's manage PUT): answers 503 this many times first
	stopInRemove func()         // a member DELETE takes the film out, runs this (e.g. to stop the run), then hangs up
}

// fakeShelf is one collection the stage made.
type fakeShelf struct {
	rk, title, summary string
	index              int
	labels, items      []string // labels as sent; Plex answers each with its first letter upper-cased
	home               []string // the members its Home row lists: Plex's cached hub, refreshed only by a manage call on it
	posters            [][]byte
	gone               bool // deleted
}

func (s *fakeShelf) metadata() map[string]any {
	var labels []map[string]string
	for _, l := range s.labels {
		labels = append(labels, map[string]string{"tag": strings.ToUpper(l[:1]) + l[1:]})
	}
	return map[string]any{"ratingKey": s.rk, "index": s.index, "type": "collection", "title": s.title, "summary": s.summary, "Label": labels}
}

// failing reports whether the write named key fails this time; the caller holds p.mu.
func (p *plexFake) failing(rw http.ResponseWriter, key string) bool {
	if p.fail[key] == 0 {
		return false
	}
	p.fail[key]--
	http.Error(rw, "busy", http.StatusServiceUnavailable)
	return true
}

// ownerShelf is the owner's shelf title, which his own collection 777 also carries (no label: never written).
const ownerShelf = "Tonight · Owner"

func newPlexFake(srv *fake.Server) *plexFake {
	p := &plexFake{srv: srv, viewed: map[string]bool{}, fail: map[string]int{},
		hubs: []plexHub{{Identifier: "movie.recentlyadded", PromotedToOwnHome: true}}}
	mc := func(rw http.ResponseWriter, v any) {
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": v})
	}
	var all []map[string]any
	for _, f := range films {
		item := map[string]any{"ratingKey": rk(f.tmdb), "guid": "plex://movie/5d77", // both keys, as Plex sends them
			"Guid": []map[string]string{{"id": "imdb://tt1"}, {"id": fmt.Sprintf("tmdb://%d", f.tmdb)}}}
		if f.plexViewed {
			item["viewCount"] = 1
		}
		all = append(all, item)
	}
	// One listing serves the daily map, a shelf's members (filtered by its tag index) and the picks' play state
	// (filtered by id): Plex's per-item endpoints are too slow under load to use.
	srv.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		q := r.URL.Query()
		var items []map[string]any
		for _, it := range all {
			k := it["ratingKey"].(string)
			switch c := q.Get("collection"); {
			case q.Has("id") && !slices.Contains(strings.Split(q.Get("id"), ","), k),
				c != "" && !slices.ContainsFunc(p.shelves, func(s *fakeShelf) bool {
					return strconv.Itoa(s.index) == c && !s.gone && slices.Contains(s.items, k)
				}):
				continue
			}
			item := maps.Clone(it)
			if p.viewed[k] {
				item["viewCount"] = 1
			}
			items = append(items, item)
		}
		mc(rw, map[string]any{"totalSize": len(items), "Metadata": items})
	})
	members := func(r *http.Request) []string {
		u := r.URL.Query().Get("uri")
		return strings.Split(u[strings.LastIndex(u, "/")+1:], ",")
	}
	srv.HandleFunc("POST", "/library/collections", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		s := p.addShelf(r.URL.Query().Get("title"), members(r))
		if p.onCreate != nil {
			p.onCreate()
		}
		if p.dropCreate {
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		mc(rw, map[string]any{"Metadata": []map[string]string{{"ratingKey": s.rk}}})
	})
	his := map[string]any{"ratingKey": "777", "index": 77700, "type": "collection", "title": ownerShelf, "summary": "his"}
	srv.HandleFunc("GET", "/library/metadata/777", func(rw http.ResponseWriter, _ *http.Request) {
		mc(rw, map[string]any{"Metadata": []any{his}})
	})
	srv.HandleFunc("GET", "/library/sections/4/collections", func(rw http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		list := []map[string]any{his}
		for _, s := range p.shelves {
			if !s.gone {
				list = append(list, s.metadata())
			}
		}
		mc(rw, map[string]any{"Metadata": append(list, p.foreign...)})
	})
	srv.HandleFunc("PUT", "/library/sections/4/all", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		q := r.URL.Query()
		for _, s := range p.shelves {
			if q.Get("type") != "18" || q.Get("id") != s.rk || s.gone {
				continue
			}
			if q.Has("summary.value") {
				s.summary = q.Get("summary.value")
			}
			if q.Has("title.value") {
				s.title = q.Get("title.value")
			}
			if q.Has("label[0].tag.tag") {
				s.labels = nil
				for i := 0; q.Has(fmt.Sprintf("label[%d].tag.tag", i)); i++ {
					s.labels = append(s.labels, q.Get(fmt.Sprintf("label[%d].tag.tag", i)))
				}
			}
		}
	})
	srv.HandleFunc("GET", "/hubs/sections/4/manage", func(rw http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		mc(rw, map[string]any{"Hub": p.hubs})
	})
	srv.HandleFunc("POST", "/hubs/sections/4/manage", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		q := r.URL.Query()
		for _, s := range p.shelves {
			if s.rk == q.Get("metadataItemId") && !s.gone {
				p.hubs = append(p.hubs, hubFlags("custom.collection.4."+s.rk, q))
				s.home = slices.Clone(s.items)
			}
		}
	})
	return p
}

// addShelf makes a shelf and its routes; the caller holds p.mu.
func (p *plexFake) addShelf(title string, items []string) *fakeShelf {
	n := len(p.shelves)
	s := &fakeShelf{rk: strconv.Itoa(900 + n), index: 90000 + 100*n, title: title, items: items}
	p.shelves = append(p.shelves, s)
	mc := func(rw http.ResponseWriter, v any) { json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": v}) }
	live := func(rw http.ResponseWriter, r *http.Request) bool {
		if s.gone {
			http.NotFound(rw, r)
		}
		return !s.gone
	}
	p.srv.HandleFunc("GET", "/library/metadata/"+s.rk, func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failReadBack > 0 {
			p.failReadBack--
			http.Error(rw, "busy", http.StatusServiceUnavailable)
			return
		}
		if live(rw, r) {
			mc(rw, map[string]any{"Metadata": []any{s.metadata()}})
		}
	})
	p.srv.HandleFunc("PUT", "/library/collections/"+s.rk+"/items", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if live(rw, r) {
			u := r.URL.Query().Get("uri")
			s.items = append(s.items, strings.Split(u[strings.LastIndex(u, "/")+1:], ",")...)
		}
	})
	for _, f := range films {
		key := rk(f.tmdb)
		p.srv.HandleFunc("DELETE", "/library/collections/"+s.rk+"/items/"+key, func(rw http.ResponseWriter, r *http.Request) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if !live(rw, r) {
				return
			}
			s.items = slices.DeleteFunc(s.items, func(k string) bool { return k == key })
			if p.stopInRemove != nil {
				p.stopInRemove()
				conn, _, _ := rw.(http.Hijacker).Hijack()
				conn.Close()
			}
		})
	}
	p.srv.HandleFunc("POST", "/library/metadata/"+s.rk+"/posters", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failing(rw, "poster") || !live(rw, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.posters = append(s.posters, body)
	})
	ident := "custom.collection.4." + s.rk
	p.srv.HandleFunc("PUT", "/hubs/sections/4/manage/"+ident, func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failing(rw, "hub") {
			return
		}
		if i := slices.IndexFunc(p.hubs, func(h plexHub) bool { return h.Identifier == ident }); i >= 0 {
			p.hubs[i] = hubFlags(ident, r.URL.Query())
			s.home = slices.Clone(s.items)
		}
	})
	p.srv.HandleFunc("PUT", "/hubs/sections/4/manage/"+ident+"/move", p.move(ident))
	p.srv.HandleFunc("DELETE", "/library/collections/"+s.rk, func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if live(rw, r) {
			s.gone = true
			p.hubs = slices.DeleteFunc(p.hubs, func(h plexHub) bool { return h.Identifier == ident })
		}
	})
	return s
}

// move serves a hub's move: to the top of the section's order, or after the hub its "after" names.
func (p *plexFake) move(ident string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		find := func(id string) int {
			return slices.IndexFunc(p.hubs, func(h plexHub) bool { return h.Identifier == id })
		}
		i := find(ident)
		if i < 0 {
			http.NotFound(rw, r)
			return
		}
		ours := p.hubs[i]
		p.hubs = slices.Delete(p.hubs, i, i+1)
		at := 0
		if after := r.URL.Query().Get("after"); after != "" {
			if at = find(after) + 1; at == 0 {
				p.hubs = slices.Insert(p.hubs, i, ours)
				http.Error(rw, "no hub "+after, http.StatusBadRequest)
				return
			}
		}
		p.hubs = slices.Insert(p.hubs, at, ours)
	}
}

// addForeign adds a collection that is not ours (no label), listed and readable; no route writes it.
func (p *plexFake) addForeign(key, title string) {
	c := map[string]any{"ratingKey": key, "index": 77800, "type": "collection", "title": title, "summary": "hers"}
	p.foreign = append(p.foreign, c)
	p.srv.HandleFunc("GET", "/library/metadata/"+key, func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{c}}})
	})
}

// shelf is a copy of the live shelf titled title, nil when there is none; hub is its hub's flags and place in the
// section's order (-1 when it has none).
func (p *plexFake) shelf(title string) (s *fakeShelf, hub plexHub, at int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.shelves {
		if x.title == title && !x.gone {
			c := *x
			c.items = slices.Sorted(slices.Values(x.items))
			c.home = slices.Sorted(slices.Values(x.home))
			at = slices.IndexFunc(p.hubs, func(h plexHub) bool { return h.Identifier == "custom.collection.4."+x.rk })
			if at >= 0 {
				hub = p.hubs[at]
			}
			return &c, hub, at
		}
	}
	return nil, plexHub{}, -1
}

func hubFlags(ident string, q url.Values) plexHub {
	return plexHub{Identifier: ident, PromotedToOwnHome: q.Get("promotedToOwnHome") == "1",
		PromotedToSharedHome: q.Get("promotedToSharedHome") == "1", PromotedToRecommended: q.Get("promotedToRecommended") == "1"}
}

// ---- fake Jellyfin: users "owner", "viewer-b" and "viewer-a", the films above, and what the stage deletes ----

type jfFake struct {
	mu        sync.Mutex
	playlists func(q map[string][]string) ([]map[string]any, bool) // answers a playlist query first, when set
	deleted   []string                                             // the ids of DELETE /Items/<id>
}

func newJFFake(srv *fake.Server) *jfFake {
	j := &jfFake{}
	srv.Handle("GET", "/Users", 200, `[{"Name":"guest","Id":"u2"},{"Name":"owner","Id":"u1"},{"Name":"viewer-b","Id":"u3"},{"Name":"viewer-a","Id":"u4"}]`)
	item := func(f libFilm) map[string]any {
		return map[string]any{"Id": jfID(f.tmdb), "Name": f.title, "Type": "Movie",
			"ProviderIds": map[string]string{"Tmdb": strconv.Itoa(f.tmdb)}, "UserData": map[string]bool{"Played": f.jellyPlayed}}
	}
	srv.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		q := r.URL.Query()
		if j.playlists != nil {
			if out, ok := j.playlists(q); ok {
				json.NewEncoder(rw).Encode(map[string]any{"Items": out})
				return
			}
		}
		var out []map[string]any // all movies, the played ones, or the given Ids
		ids := strings.Split(q.Get("Ids"), ",")
		for _, f := range films {
			if (q.Get("Filters") != "IsPlayed" || f.jellyPlayed) && (q.Get("Ids") == "" || slices.Contains(ids, jfID(f.tmdb))) {
				out = append(out, item(f))
			}
		}
		json.NewEncoder(rw).Encode(map[string]any{"Items": out})
	})
	srv.HandleFunc("DELETE", "/Items/box1", func(rw http.ResponseWriter, _ *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.deleted = append(j.deleted, "box1")
		rw.WriteHeader(http.StatusNoContent)
	})
	return j
}

// Catches: the movie listing left to collapse collections. Jellyfin then answers each film in a collection with the
// collection, so a pick in one was left out of Jellyfin's Tonight, and a collection's TMDB id was read as a film's.
func TestJellyfinMapListsFilmsInCollections(t *testing.T) {
	h := fake.NewEnv(t, nil)
	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"owner","Id":"u1"}]`)
	h.Jellyfin.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		item := `{"Id":"j438631","ProviderIds":{"Tmdb":"438631"}}`
		if r.URL.Query().Get("collapseBoxSetItems") != "false" { // Jellyfin's default for a movie query
			item = `{"Id":"box2","Type":"BoxSet","ProviderIds":{"Tmdb":"726871"}}`
		}
		io.WriteString(rw, `{"Items":[`+item+`]}`)
	})
	m, err := jellyfinMap(context.Background(), h.Env.Jellyfin, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]string{438631: "j438631"}; !maps.Equal(m.Items, want) {
		t.Fatalf("item map = %v, want %v (Dune is in a Jellyfin collection)", m.Items, want)
	}
}

// ---- runs ----

// picksOf is who's picks in the state, sorted.
func (w *world) picksOf(t *testing.T, who string) []int {
	t.Helper()
	o := w.state(t).Persons[who]
	if o == nil {
		return nil
	}
	return slices.Sorted(slices.Values(tmdbIDs(o.Picks)))
}

// Catches, across four runs of the owner's shelf: a pick he has seen (export, feed, Plex, Jellyfin) or under the bars;
// a second pick on the same day, or ratings asked for again within ratings_every; a watched pick left on the shelf (in
// Plex's cached Home hub too) or a poster uploaded for its removal; yesterday's picks repeated while others pass; the
// shelf missing from his Home, shown to others, or on the Recommended tab (where an earlier build put rows); his own
// collection with the shelf's title adopted and edited; an RSS read that skips the process-wide Letterboxd limiter
// (lbpace.Shared, at most one request per 2 s across stages).
func TestRunAcrossDays(t *testing.T) {
	w := newWorld(t, "", 0)
	six := []int{389, 346, 655, 1398, 949, 18148}
	w.mdb.onlyPass(six...)
	w.mdb.rate(80, -1, 949) // no critics score: Letterboxd alone decides
	rep := w.run(t)
	if len(rep.Alerts) != 0 || rep.Counts["owner.plex_retry"]+rep.Counts["owner.jellyfin_retry"] != 0 {
		t.Fatalf("run 1: alerts %q counts %v\n%s", rep.Alerts, rep.Counts, w.logs)
	}
	picks1 := w.picksOf(t, "owner")
	if w.state(t).Persons["owner"].Day != "2026-10-05" || len(picks1) != 3 || slices.ContainsFunc(picks1, func(id int) bool { return !slices.Contains(six, id) }) {
		t.Fatalf("run 1 picks %v; want 3 of %v", picks1, six)
	}
	s, _, _ := w.plex.shelf(ownerShelf)
	if s == nil || strings.Join(s.items, ",") != plexKeys(picks1...) || !slices.Equal(s.labels, []string{"projectionist", "Tonight-owner"}) ||
		len(s.posters) != 1 || !bytes.HasPrefix(s.posters[0], []byte{0xFF, 0xD8}) || strings.Count(s.summary, "\n") != 2 ||
		!strings.Contains(s.summary, "Letterboxd ★4.0") {
		t.Fatalf("his shelf %+v; want %s, labelled, one poster, a summary of the picks' scores", s, plexKeys(picks1...))
	}
	own := plexHub{Identifier: "custom.collection.4.900", PromotedToOwnHome: true}
	if w.plex.hubs[0] != own {
		t.Fatalf("hubs %+v: his shelf is not first on his own Home alone", w.plex.hubs)
	}
	if got := w.jp.owned("u1"); !slices.Equal(got, []string{"Tonight: " + jfKeys(picks1...)}) {
		t.Fatalf("his Jellyfin playlists %q", got)
	}
	if w.mdb.requests() != 1 {
		t.Fatalf("MDBList asked %d times for 14 films, want once", w.mdb.requests())
	}

	// 30 min later: nothing new to do, except to take the shelf off the Recommended tab, where an earlier build put rows.
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.plex.mu.Lock()
	w.plex.hubs[0].PromotedToRecommended = true
	w.plex.mu.Unlock()
	if rep := w.run(t); len(rep.Alerts) != 0 || !slices.Equal(w.picksOf(t, "owner"), picks1) {
		t.Fatalf("run 2: alerts %q, picks %v: a same-day run picked again", rep.Alerts, w.picksOf(t, "owner"))
	}
	if s, _, _ := w.plex.shelf(ownerShelf); len(s.posters) != 1 || len(w.h.Plex.Calls("POST", "/library/collections")) != 1 || w.plex.hubs[0] != own {
		t.Fatalf("run 2: posters %d, creates %d, hubs %+v", len(s.posters), len(w.h.Plex.Calls("POST", "/library/collections")), w.plex.hubs)
	}
	labels := 0
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		if strings.Contains(c.RawQuery, "label") {
			labels++
		}
		if strings.Contains(c.RawQuery, "id=777") {
			t.Fatalf("his collection was edited: %s", c.RawQuery)
		}
	}
	if labels != 1 {
		t.Fatalf("run 2: %d label writes, want the one from run 1", labels)
	}

	// He watches one pick on Plex: it leaves the shelf, and his cached Home row, at once, without a new pick.
	gone := picks1[0]
	w.plex.mu.Lock()
	w.plex.viewed[rk(gone)] = true
	w.plex.mu.Unlock()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	left := picks1[1:]
	if s, _, _ := w.plex.shelf(ownerShelf); strings.Join(s.items, ",") != plexKeys(left...) || !slices.Equal(s.home, s.items) || len(s.posters) != 1 {
		t.Fatalf("run 3: shelf %v, Home row %v, posters %d; want %s on both, no new poster", s.items, s.home, len(s.posters), plexKeys(left...))
	}
	if got := w.jp.owned("u1"); !slices.Equal(got, []string{"Tonight: " + jfKeys(left...)}) {
		t.Fatalf("run 3: his Jellyfin playlists %q", got)
	}

	// Next morning: the other three of the six, none of yesterday's, the ratings from the cache.
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	picks2 := w.picksOf(t, "owner")
	rest := slices.DeleteFunc(slices.Clone(six), func(id int) bool { return slices.Contains(picks1, id) })
	if !slices.Equal(picks2, slices.Sorted(slices.Values(rest))) {
		t.Fatalf("run 4 picks %v; want the three not picked yesterday, %v", picks2, rest)
	}
	if s, _, _ := w.plex.shelf(ownerShelf); strings.Join(s.items, ",") != plexKeys(picks2...) || !slices.Equal(s.home, s.items) || len(s.posters) != 2 {
		t.Fatalf("run 4: shelf %v, Home row %v, posters %d", s.items, s.home, len(s.posters))
	}
	if w.mdb.requests() != 1 {
		t.Fatalf("MDBList asked %d times; the cached ratings were not used", w.mdb.requests())
	}
	if len(w.lbHits) != 4 || !slices.Equal(w.lbWaits, []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second}) {
		t.Fatalf("letterboxd requests = %q (want his feed once per run), limiter waits %v (want each queued behind the last)", w.lbHits, w.lbWaits)
	}
}

// Catches MDBList asked one request a film, or for more than 200 films a request; asked again within ratings_every, or
// never after it; and a spent quota costing the day its picks: with 450 films beside the library's 14 (one MDBList
// does not know), the first pick asks 3 requests that name each film once, the next day none, and once ratings_every
// has passed a 429 leaves the cached ratings to pick from, without an alert.
func TestRatingsAreCachedAndAskedInBatches(t *testing.T) {
	w := newWorld(t, "", 450)
	w.mdb.mu.Lock()
	delete(w.mdb.ratings, 700000)
	w.mdb.mu.Unlock()
	w.run(t)
	seen := map[int]int{}
	for _, ids := range w.mdb.asked {
		if len(ids) > 200 {
			t.Errorf("a request named %d films", len(ids))
		}
		for _, id := range ids {
			seen[id]++
		}
	}
	if len(w.mdb.asked) != 3 || len(seen) != 464 || slices.Max(slices.Collect(maps.Values(seen))) != 1 {
		t.Fatalf("%d requests naming %d films (most asked %d times); want 3, each of the 464 once", len(w.mdb.asked), len(seen),
			slices.Max(slices.Collect(maps.Values(seen))))
	}
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	if w.mdb.requests() != 3 {
		t.Fatalf("the next day: %d requests, want none more", w.mdb.requests())
	}
	w.mdb.mu.Lock()
	w.mdb.quota = true
	w.mdb.mu.Unlock()
	w.h.Now = time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC)
	if rep := w.run(t); len(rep.Alerts) != 0 || len(w.picksOf(t, "owner")) != 3 || w.state(t).Persons["owner"].Day != "2026-11-07" {
		t.Fatalf("a spent quota: alerts %q, picks %v for %s; want the day's picks from the cache", rep.Alerts, w.picksOf(t, "owner"),
			w.state(t).Persons["owner"].Day)
	}
}

// Catches the household row an earlier build kept left in place (its Plex collection, and with it its Home row, and its
// Jellyfin BoxSet), or taken down again on a later run: the state names them, the first run deletes both and forgets
// them, and the next deletes nothing.
func TestHouseholdRowIsTakenDown(t *testing.T) {
	w := newWorld(t, "", 0)
	if err := w.h.Env.Store.PutJSON(context.Background(), name, "state", state{Plex: sinkState{ID: "555"}, Jellyfin: sinkState{ID: "box1"}}); err != nil {
		t.Fatal(err)
	}
	w.h.Plex.Handle("DELETE", "/library/collections/555", 200, "")
	w.run(t)
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	st := w.state(t)
	if n := len(w.h.Plex.Calls("DELETE", "/library/collections/555")); n != 1 || !slices.Equal(w.jf.deleted, []string{"box1"}) ||
		st.Plex.ID != "" || st.Jellyfin.ID != "" {
		t.Fatalf("Plex deletes %d, Jellyfin deletes %v, state plex %+v jellyfin %+v; want each deleted once and forgotten",
			n, w.jf.deleted, st.Plex, st.Jellyfin)
	}
}

// Catches a second shelf made after a create whose answer was lost (Plex made it, the stage never heard its id), whose
// read-back failed, or inside which the service stopped: the next run must adopt the one made, not make another that
// nothing would ever take away.
func TestCreateNeverDuplicates(t *testing.T) {
	for _, c := range []struct {
		name  string
		fault func(*world, context.CancelFunc)
	}{
		{"create answer lost", func(w *world, _ context.CancelFunc) { w.plex.dropCreate = true }},
		{"read-back fails", func(w *world, _ context.CancelFunc) { w.plex.failReadBack = 1 }},
		{"stopped inside the create", func(w *world, stop context.CancelFunc) { w.plex.dropCreate, w.plex.onCreate = true, stop }},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "", 0)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			c.fault(w, stop)
			rep := &stage.Report{}
			err := (Stage{}).Run(ctx, w.h.Env, rep)
			if stopped := ctx.Err() != nil; stopped != (err != nil) || (!stopped && rep.Counts["owner.plex_retry"] != 1) {
				t.Fatalf("run 1: err %v, counts %v\n%s", err, rep.Counts, w.logs)
			}
			if st := w.state(t).Persons["owner"]; c.name == "read-back fails" && (st.Plex.ID != "900" || st.Plex.Pending != nil) {
				t.Fatalf("after the create answered 900: plex state %+v", st.Plex)
			}
			w.plex.mu.Lock()
			w.plex.dropCreate, w.plex.onCreate = false, nil
			w.plex.mu.Unlock()
			w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
			if rep := w.run(t); rep.Counts["owner.plex_synced"] != 1 {
				t.Fatalf("run 2: counts %v\n%s", rep.Counts, w.logs)
			}
			s, _, _ := w.plex.shelf(ownerShelf)
			st := w.state(t).Persons["owner"]
			if n := len(w.h.Plex.Calls("POST", "/library/collections")); n != 1 || s == nil || !slices.Contains(s.labels, "projectionist") ||
				st.Plex.ID != "900" || st.Plex.Pending != nil || strings.Join(s.items, ",") != plexKeys(w.picksOf(t, "owner")...) {
				t.Fatalf("plex creates %d, shelf %+v, state %+v; want one shelf, ours, holding the picks", n, s, st.Plex)
			}
		})
	}
}

// Catches his Home row listing films his shelf no longer holds after a Plex write following a member edit fails: the
// next run finds the members right, so only a saved mark refreshes the hub Plex caches. Row 1 is a reviewer's repro
// (day 2's poster answers 503; 16 min later his Home still listed day 1's picks); in row 2 the refresh itself fails,
// so the mark must outlive it; row 3 stops the service inside a watched pick's removal after Plex made it, where the
// run's closing save fails and only a mark saved before the edit survives.
func TestHomeRowRefreshedAfterAFailedWrite(t *testing.T) {
	nextDay := func(t *testing.T, w *world) { w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC) }
	watchedOne := func(t *testing.T, w *world) {
		w.plex.viewed[rk(w.picksOf(t, "owner")[0])], w.h.Now = true, w.h.Now.Add(30*time.Minute)
	}
	for _, c := range []struct {
		name   string
		change func(*testing.T, *world)
		fault  func(*plexFake, context.CancelFunc)
	}{
		{"the poster fails after a new day's members", nextDay, func(p *plexFake, _ context.CancelFunc) { p.fail["poster"] = 1 }},
		{"the hub refresh fails after a new day's members", nextDay, func(p *plexFake, _ context.CancelFunc) { p.fail["hub"] = 1 }},
		{"stopped inside a watched pick's removal", watchedOne, func(p *plexFake, stop context.CancelFunc) { p.stopInRemove = stop }},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "", 0)
			w.run(t)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			c.change(t, w)
			w.plex.mu.Lock()
			c.fault(w.plex, stop)
			w.plex.mu.Unlock()
			rep := &stage.Report{}
			err := (Stage{}).Run(ctx, w.h.Env, rep)
			if stopped := ctx.Err() != nil; stopped != (err != nil) || (!stopped && rep.Counts["owner.plex_retry"] != 1) {
				t.Fatalf("run A: err %v, counts %v\n%s", err, rep.Counts, w.logs)
			}
			w.plex.mu.Lock()
			w.plex.stopInRemove = nil
			w.plex.mu.Unlock()
			w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
			if rep := w.run(t); rep.Counts["owner.plex_synced"] != 1 {
				t.Fatalf("run B: counts %v\n%s", rep.Counts, w.logs)
			}
			if s, _, _ := w.plex.shelf(ownerShelf); !slices.Equal(s.home, s.items) || len(s.items) == 0 {
				t.Fatalf("STALE: his Home row lists %v while the shelf holds %v", s.home, s.items)
			}
		})
	}
}

// Catches: a Plex outage failing the run, raising an alert on the first miss, retrying every run instead of backing
// off, or holding up Jellyfin.
func TestPlexOutageNeverFailsTheRun(t *testing.T) {
	w := newWorld(t, "", 0)
	w.h.Plex.Handle("GET", "/library/sections/4/all", http.StatusServiceUnavailable, "scanning")
	rep := w.run(t)
	if len(rep.Alerts) != 0 || rep.Counts["owner.plex_retry"] != 1 || len(w.jp.owned("u1")) != 1 {
		t.Fatalf("run 1: alerts %q counts %v jellyfin %v", rep.Alerts, rep.Counts, w.jp.owned("u1"))
	}
	w.h.Now = w.h.Now.Add(10 * time.Minute) // inside the 15 min backoff
	if rep := w.run(t); rep.Counts["owner.plex_deferred"] != 1 {
		t.Fatalf("run 2 counts %v, want owner.plex_deferred", rep.Counts)
	}
	st := w.state(t)
	o := st.Persons["owner"]
	o.Plex.FailingSince, o.Plex.RetryAt = w.h.Now.Add(-25*time.Hour), time.Time{}
	if err := w.h.Env.Store.PutJSON(context.Background(), name, "state", st); err != nil {
		t.Fatal(err)
	}
	if rep := w.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "owner.plex has not taken the picks for 25h") {
		t.Fatalf("after a day failing: alerts %q", rep.Alerts)
	}
}

// Catches a film a person has seen picked for them, one way each row (her feed's TMDB id, his films page's title and
// year, her Plex play or his rating in the watch record, an export of his own), or hidden from the others, who have not
// seen it; and a play by someone outside the people with shelves (the watch record holds every jfsync user) hiding a
// film. Four films pass: the one seen and three others, so the one who saw it gets exactly the three.
func TestFilmSeenByThePersonIsExcluded(t *testing.T) {
	ownExport := func(t *testing.T) string {
		dir := t.TempDir()
		mine := filepath.Join(dir, "letterboxd-viewer-a-lb-2026-09-01-10-00-utc")
		write(t, filepath.Join(mine, "watched.csv"), "Date,Name,Year,Letterboxd URI\n2026-08-01,\"Paris, Texas\",1984,https://boxd.it/pt\n")
		for _, f := range []string{"diary.csv", "ratings.csv", "watchlist.csv"} {
			write(t, filepath.Join(mine, f), "Date,Name,Year,Letterboxd URI\n")
		}
		return strings.Replace(threeWithPlex, `{ person = "viewer-a", user = "viewer-a-lb", plex_user = "viewer-a-plex" }`,
			fmt.Sprintf(`{ person = "viewer-a", user = "viewer-a-lb", plex_user = "viewer-a-plex", export_dir = %q }`, dir), 1)
	}
	for _, c := range []struct {
		name    string
		options func(*testing.T) string // nil: the three
		see     func(*testing.T, *world)
		who     string // who saw it ("" = no one with a shelf)
		gone    int
	}{
		{"her feed names it", nil, func(_ *testing.T, w *world) {
			w.feeds["viewer-b-lb"] = append(w.feeds["viewer-b-lb"], lbItem{id: 13, tmdb: 949, title: "Heat", year: 1995, stars: 3})
		}, "viewer-b", 949},
		{"his films page lists it", nil, func(_ *testing.T, w *world) {
			w.films["viewer-a-lb"] = append(w.films["viewer-a-lb"], lbItem{title: "Stalker", year: 1979, stars: 4})
		}, "viewer-a", 1398},
		{"her Plex play is in the watch record", nil, func(t *testing.T, w *world) { w.record(t, "viewer-b", 18148, watch.Finished, 0) }, "viewer-b", 18148},
		{"his rating is in the watch record", nil, func(t *testing.T, w *world) { w.record(t, "viewer-a", 843, watch.Rated, 8) }, "viewer-a", 843},
		{"an export of his own", ownExport, func(*testing.T, *world) {}, "viewer-a", 655},
		{"a play outside the people", nil, func(t *testing.T, w *world) { w.record(t, "guest", 389, watch.Finished, 0) }, "", 389},
	} {
		t.Run(c.name, func(t *testing.T) {
			options := threeWithPlex
			if c.options != nil {
				options = c.options(t)
			}
			w := newWorld(t, options, 0)
			c.see(t, w)
			others := slices.DeleteFunc([]int{389, 346, 1398, 949}, func(id int) bool { return id == c.gone })[:3]
			pass := append(slices.Clone(others), c.gone)
			if c.who == "" {
				pass = others // the film no one with a shelf saw must reach all three
				pass[0] = c.gone
			}
			w.mdb.onlyPass(pass...)
			w.run(t)
			for _, who := range []string{"owner", "viewer-b", "viewer-a"} {
				got := w.picksOf(t, who)
				switch {
				case who == c.who && !slices.Equal(got, slices.Sorted(slices.Values(others))):
					t.Errorf("%s saw %d; their picks %v, want %v", who, c.gone, got, others)
				case c.who == "" && !slices.Contains(got, c.gone):
					t.Errorf("%s's picks %v lack %d, which only a guest played", who, got, c.gone)
				case len(got) != 3:
					t.Errorf("%s's picks %v; want 3", who, got)
				}
			}
		})
	}
}

// Catches a person whose Letterboxd does not answer costing them their picks, failing the run, or raising an alert per
// failed request (her feed and her films page both fail) or per run; and the reverse, an alert for a feed that failed
// now but answered within feedStale. Rows: her feed never answered (alerted); it answered yesterday (no alert); it last
// answered three days ago (one alert dated by that read).
func TestUnreachableFeedDegrades(t *testing.T) {
	for _, c := range []struct {
		name  string
		upAgo time.Duration // when her feed last answered, before the failing pick; 0 = never
		alert string        // "" = no alert
	}{
		{"never answered", 0, "viewer-b (Letterboxd viewer-b-lb: feed unanswered since it was first asked, latest error " +
			`"letterboxd: challenged: GET `},
		{"answered yesterday", 24 * time.Hour, ""},
		{"last answered three days ago", 72 * time.Hour, "feed unanswered since 2026-10-02T15:00:00Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, threeWithPlex, 0)
			if c.upAgo > 0 {
				w.h.Now = w.h.Now.Add(-c.upAgo)
				w.run(t)
				w.h.Now = w.h.Now.Add(c.upAgo)
			}
			w.mu.Lock()
			w.down["viewer-b-lb"] = true
			w.mu.Unlock()
			rep := w.run(t)
			if got := w.picksOf(t, "viewer-b"); len(got) != 3 || w.state(t).Persons["viewer-b"].Day != "2026-10-05" {
				t.Fatalf("her picks %v, alerts %q\n%s", got, rep.Alerts, w.logs)
			}
			switch {
			case c.alert == "" && len(rep.Alerts) != 0,
				c.alert != "" && (len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], c.alert) || strings.Contains(rep.Alerts[0], "viewer-a")):
				t.Fatalf("alerts %q; want %q alone", rep.Alerts, c.alert)
			}
			w.h.Now = w.h.Now.Add(30 * time.Minute)
			if rep := w.run(t); len(rep.Alerts) != 0 {
				t.Fatalf("30 min later: alerts %q; want the day's one alert only", rep.Alerts)
			}
		})
	}
}

// Catches a pick a person has since watched staying on their shelf, because only the owner's sources are checked (her
// feed logs it, or his play reaches the watch record), or leaving the others' shelves, who have not watched it.
func TestPickWatchedLeavesOnlyTheirShelf(t *testing.T) {
	for _, c := range []struct {
		name, who, shelf, user string
		watch                  func(*testing.T, *world)
	}{
		{"her feed logs it", "viewer-b", "Tonight · Viewer B", "u3", func(_ *testing.T, w *world) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.feeds["viewer-b-lb"] = append(w.feeds["viewer-b-lb"], lbItem{id: 14, tmdb: 346, title: "Seven Samurai", year: 1954, stars: 4.5})
		}},
		{"his play reaches the watch record", "viewer-a", "Tonight · Viewer-a", "u4", func(t *testing.T, w *world) {
			w.record(t, "viewer-a", 346, watch.Finished, 0)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, threeWithPlex, 0)
			w.mdb.onlyPass(389, 346, 655)
			w.run(t)
			c.watch(t, w)
			w.h.Now = w.h.Now.Add(30 * time.Minute)
			w.run(t)
			s, _, _ := w.plex.shelf(c.shelf)
			if s == nil || strings.Join(s.items, ",") != plexKeys(389, 655) || !slices.Equal(w.jp.owned(c.user), []string{"Tonight: " + jfKeys(389, 655)}) {
				t.Fatalf("%s's shelf %v, Jellyfin %q; want Seven Samurai gone without a new pick", c.who, s, w.jp.owned(c.user))
			}
			if o, _, _ := w.plex.shelf(ownerShelf); strings.Join(o.items, ",") != plexKeys(389, 346, 655) {
				t.Fatalf("his shelf %v; want Seven Samurai kept, he has not seen it", o.items)
			}
		})
	}
}
