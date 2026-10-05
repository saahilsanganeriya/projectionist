package lists

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
	"projectionist/internal/stage"
)

type world struct {
	h       *fake.Harness
	clock   *fakeClock
	lb      *fakeLetterboxd
	px      *fakePlex
	jf      *fakeJellyfin
	missing string
	top     string // the canon list's URL
	opts    string // the stage options, without the canon
}

// newWorld: one canon list (7 films over two pages) and one own list ("nolan", found through the export's code),
// seven of the films on the NAS, a hand-made Plex collection that shares the own list's title.
func newWorld(t *testing.T) *world {
	h := fake.NewEnv(t, nil)
	w := &world{h: h, clock: &fakeClock{now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}}
	h.Env.Now = w.clock.Now
	oldSleep, oldPage := sleep, plexPage
	sleep, plexPage = w.clock.Sleep, 3 // three listing pages for seven movies
	t.Cleanup(func() { sleep, plexPage = oldSleep, oldPage })

	w.lb = newFakeLetterboxd(t)
	w.top = w.lb.srv.URL + "/official/list/letterboxds-top-500-films/"
	w.lb.pages["/official/list/letterboxds-top-500-films/"] = fixture(t, "list_top500_p1.html")
	w.lb.pages["/official/list/letterboxds-top-500-films/page/2/"] = fixture(t, "list_top500_p2.html")
	w.lb.pages["/saahilsan/lists/"] = fixture(t, "lists_index.html") // both lists on it answer 404: private
	w.lb.pages["/saahilsan/list/nolan/"] = fixture(t, "list_nolan.html")
	w.lb.pages["/saahilsan/films/"] = fixture(t, "films_grid.html")
	w.lb.pages["/saahilsan/rss/"] = fixture(t, "rss.xml")
	w.lb.redirect["fQbG8"] = "/saahilsan/list/nolan/"
	for slug, id := range map[string]string{"harakiri": "movie/14537", "the-human-condition-iii-a-soldiers-prayer": "movie/31414",
		"12-angry-men": "movie/389", "come-and-see": "movie/25237", "oldboy": "movie/670", "sherlock-jr": "movie/992",
		"chainsaw-man-the-movie-reze-arc": "tv/389", // a TV id equal to an owned film's
		"oppenheimer-2023":                "movie/872585", "interstellar": "movie/157336", "memento": "movie/77", "inception": "movie/27205"} {
		w.lb.films[slug] = id
	}

	dir := t.TempDir()
	movies := filepath.Join(dir, "Movies")
	for _, f := range []string{"Harakiri (1962) {tmdb-14537}", "12 Angry Men (1957) {tmdb-389}", "Oldboy (2003) {tmdb-670}",
		"Come and See (1985) {tmdb-25237}", "Interstellar (2014) {tmdb-157336}", "Memento (2000) {tmdb-77}", "Inception (2010) {tmdb-27205}"} {
		if err := os.MkdirAll(filepath.Join(movies, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	export := filepath.Join(dir, "export", "letterboxd-test-export")
	os.MkdirAll(filepath.Join(export, "lists"), 0o755)
	os.WriteFile(filepath.Join(export, "lists", "nolan.csv"), []byte("Letterboxd list export v7\nDate,Name,Tags,URL,Description\n"+
		"2022-03-17,nolan,,https://boxd.it/fQbG8,\n\nPosition,Name,Year,URL,Description\n1,Oppenheimer,2023,https://boxd.it/wUow,\n"), 0o644)
	os.WriteFile(filepath.Join(export, "watched.csv"), []byte("Date,Name,Year,Letterboxd URI\n2021-08-28,Harakiri,1962,https://boxd.it/1NTU\n"), 0o644)
	w.missing = filepath.Join(dir, "_lists-missing.txt")

	w.px = newFakePlex(t, map[string]int{"k14537": 14537, "k389": 389, "k670": 670, "k25237": 25237, "k157336": 157336,
		"k77": 77, "k11": 11}, func() int64 { return w.clock.Now().Unix() }) // Inception (27205) not scanned by Plex yet
	w.px.watched["k25237"] = true
	w.px.colls = []*plexColl0{{key: "500", title: "nolan", summary: "my favourite Nolans", items: []string{"k77"}}}
	h.Env.Plex = plex.New(w.px.srv.URL, h.PrefsPath, 5*time.Second)

	w.jf = newFakeJellyfin(t, map[string]int{"j14537": 14537, "j389": 389, "j670": 670, "j25237": 25237, "j157336": 157336,
		"j77": 77, "j27205": 27205})
	w.jf.played["j157336"] = true
	key := filepath.Join(dir, "jellyfin_key")
	os.WriteFile(key, []byte(fake.JellyfinKey), 0o600)
	h.Env.Jellyfin = jellyfin.New(w.jf.srv.URL, key, 5*time.Second)

	w.opts = fmt.Sprintf(`
letterboxd_url = %q
shortlink_url  = %q
export_dir     = %q
movies_dir     = %q
missing_file   = %q
`, w.lb.srv.URL, w.lb.srv.URL, filepath.Dir(export), movies, w.missing)
	h.SetOptions(w.opts + fmt.Sprintf("home = [\"Top\"]\n[[canon]]\nname = \"Top\"\nurl = %q\n", w.top))
	return w
}

func (w *world) run(t *testing.T) *stage.Report {
	t.Helper()
	rep := &stage.Report{}
	if err := (Stage{}).Run(context.Background(), w.h.Env, rep); err != nil {
		t.Fatalf("run: %v", err)
	}
	return rep
}

func (w *world) plexColl(t *testing.T, title string) *plexColl0 {
	t.Helper()
	for _, c := range w.px.colls {
		if c.title == title {
			return c
		}
	}
	t.Fatalf("no Plex collection %q", title)
	return nil
}

func (w *world) boxSet(t *testing.T, name string) *boxSet0 {
	t.Helper()
	for _, s := range w.jf.sets {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no Jellyfin BoxSet %q", name)
	return nil
}

// jfOrder is what Jellyfin shows: insertion order once DisplayOrder is Default.
func jfOrder(s *boxSet0) []string {
	if s.order != "Default" {
		return nil
	}
	return s.items
}

// Catches, end to end: owned films shelved out of list order (Plex reorders a new custom collection; Jellyfin
// sorts by premiere date unless told otherwise), a summary or sort title off the owner's format, a missing poster
// or Home row, a private list shelved, a hand-made collection with the same title touched, and — on the second run
// — any write at all or any Letterboxd request inside the refresh windows (the reconciler's "nothing to do").
func TestShelvesAndRerunChangesNothing(t *testing.T) {
	w := newWorld(t)
	rep := w.run(t)
	if len(rep.Alerts) > 0 {
		t.Fatalf("alerts: %v", rep.Alerts)
	}

	top := w.plexColl(t, "Top")
	if want := []string{"k14537", "k389", "k25237", "k670"}; !reflect.DeepEqual(top.items, want) {
		t.Errorf("plex Top = %v, want %v", top.items, want)
	}
	wantSummary := "On the NAS: 4 of 7 · seen 2 · " + w.top // seen: Harakiri (export), Come and See (Plex)
	if top.summary != wantSummary || top.titleSort != "~ Canon 01 Top" || top.sort != "2" || top.mode != "0" {
		t.Errorf("plex Top: summary=%q sort title=%q sort=%q mode=%q", top.summary, top.titleSort, top.sort, top.mode)
	}
	if img, err := png.Decode(bytes.NewReader(top.poster)); err != nil || img.Bounds().Dx() != posterW {
		t.Errorf("plex Top poster: %v", err)
	}
	var hubs []string
	for _, h := range w.px.hubs {
		hubs = append(hubs, h["identifier"].(string))
	}
	if want := []string{"movie.recentlyreleased", "movie.recentlyadded", "custom.collection.4." + top.key, "movie.curated"}; !reflect.DeepEqual(hubs, want) {
		t.Errorf("home rows = %v", hubs)
	}
	if h := w.px.hubs[2]; h["promotedToOwnHome"] != true || h["promotedToSharedHome"] != false {
		t.Errorf("home row flags = %v", h)
	}
	if hand := w.px.coll("500"); hand.title != "nolan" || hand.summary != "my favourite Nolans" || !reflect.DeepEqual(hand.items, []string{"k77"}) {
		t.Errorf("hand-made collection changed: %+v", hand)
	}
	for _, c := range w.px.writes() {
		if strings.Contains(c, "/500/") || strings.Contains(c, "/500?") || strings.Contains(c, "id=500") || strings.Contains(c, "Item=500") {
			t.Errorf("write to the hand-made collection: %s", c)
		}
	}

	bsTop, bsNolan := w.boxSet(t, "Top"), w.boxSet(t, "nolan")
	if want := []string{"j14537", "j389", "j25237", "j670"}; !reflect.DeepEqual(jfOrder(bsTop), want) {
		t.Errorf("jellyfin Top = %v (order %s), want %v", bsTop.items, bsTop.order, want)
	}
	if want := []string{"j157336", "j77", "j27205"}; !reflect.DeepEqual(jfOrder(bsNolan), want) {
		t.Errorf("jellyfin nolan = %v, want %v", bsNolan.items, want)
	}
	nolanURL := w.lb.srv.URL + "/saahilsan/list/nolan/"
	if bsNolan.overview != "On the NAS: 3 of 4 · seen 1 · "+nolanURL || bsNolan.forcedSort != "~ Mine nolan" || len(bsNolan.poster) == 0 {
		t.Errorf("jellyfin nolan: overview=%q sort=%q poster=%d bytes", bsNolan.overview, bsNolan.forcedSort, len(bsNolan.poster))
	}
	if len(w.jf.sets) != 2 {
		t.Errorf("jellyfin sets = %d, want 2 (private lists stay off)", len(w.jf.sets))
	}
	text, err := os.ReadFile(w.missing)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"== Top · 3 of 7 not on the NAS · " + w.top, "    2. The Human Condition III: A Soldier's Prayer (1961)",
		"    7. Chainsaw Man – The Movie: Reze Arc (2025)  (TV)", "== nolan · 1 of 4 not on the NAS"} {
		if !strings.Contains(string(text), line) {
			t.Errorf("missing file lacks %q:\n%s", line, text)
		}
	}
	info, _ := os.Stat(w.missing)

	// second run, an hour later: everything is fresh and in place
	w.clock.Sleep(context.Background(), time.Hour)
	pxWrites, jfWrites, lbRequests := len(w.px.writes()), len(w.jf.writes()), w.lb.count()
	rep = w.run(t)
	if got := w.px.writes()[pxWrites:]; len(got) > 0 {
		t.Errorf("second run wrote to Plex: %v", got)
	}
	if got := w.jf.writes()[jfWrites:]; len(got) > 0 {
		t.Errorf("second run wrote to Jellyfin: %v", got)
	}
	if n := w.lb.count() - lbRequests; n > 0 {
		t.Errorf("second run sent %d Letterboxd requests", n)
	}
	if again, _ := os.Stat(w.missing); !again.ModTime().Equal(info.ModTime()) {
		t.Errorf("missing file rewritten with the same text")
	}
	if rep.Counts["plex_writes"] != 0 || rep.Counts["jellyfin_writes"] != 0 {
		t.Errorf("counts = %v", rep.Counts)
	}
}

// Catches: a changed list (a film dropped, another moved to the top) not followed, or followed by rebuilding the
// Plex collection instead of the few calls the change needs.
func TestFollowsListChanges(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	p1 := string(w.lb.pages["/official/list/letterboxds-top-500-films/"])
	first, fourth := posterLI(p1, "harakiri"), posterLI(p1, "come-and-see")
	p1 = strings.Replace(strings.Replace(p1, first, "@@1", 1), fourth, first, 1)
	w.lb.pages["/official/list/letterboxds-top-500-films/"] = []byte(strings.Replace(p1, "@@1", fourth, 1))
	p2 := string(w.lb.pages["/official/list/letterboxds-top-500-films/page/2/"])
	w.lb.pages["/official/list/letterboxds-top-500-films/page/2/"] = []byte(strings.Replace(p2, posterLI(p2, "oldboy"), "", 1))

	w.clock.Sleep(context.Background(), 8*24*time.Hour) // past canon_refresh
	before := len(w.px.writes())
	w.run(t)
	top := w.plexColl(t, "Top")
	if want := []string{"k25237", "k389", "k14537"}; !reflect.DeepEqual(top.items, want) {
		t.Errorf("plex Top = %v, want %v", top.items, want)
	}
	if want := []string{"j25237", "j389", "j14537"}; !reflect.DeepEqual(jfOrder(w.boxSet(t, "Top")), want) {
		t.Errorf("jellyfin Top = %v, want %v", w.boxSet(t, "Top").items, want)
	}
	// drop Oldboy, move Come and See to the front, swap Harakiri back: 1 drop + 2 moves, then the summary
	var topWrites []string
	for _, c := range w.px.writes()[before:] {
		if strings.Contains(c, "/collections/"+top.key+"/") || strings.Contains(c, "id="+top.key) {
			topWrites = append(topWrites, c)
		}
	}
	if len(topWrites) != 4 {
		t.Errorf("plex writes for the change = %d, want 4: %v", len(topWrites), topWrites)
	}
	if !strings.HasPrefix(top.summary, "On the NAS: 3 of 6 · ") {
		t.Errorf("summary = %q", top.summary)
	}
}

func posterLI(page, slug string) string {
	i := strings.Index(page, `data-item-slug="`+slug+`"`)
	start := strings.LastIndex(page[:i], `<li class="posteritem">`)
	end := strings.Index(page[i:], "</li>") + i + len("</li>")
	return page[start:end]
}

// Catches: a failing Plex failing the run or holding up Jellyfin, and — once Plex is back, with the state of the
// collections lost — a duplicate collection instead of adopting the one made earlier (its summary marks it ours).
func TestPlexDownNeverFailsAndStateLossAdopts(t *testing.T) {
	w := newWorld(t)
	w.px.fail = true
	rep := w.run(t)
	if len(w.jf.sets) != 2 {
		t.Fatalf("jellyfin sets = %d with Plex down, want 2", len(w.jf.sets))
	}
	if !strings.Contains(rep.Summary, "plex 0 writes (rest deferred") {
		t.Errorf("summary = %q", rep.Summary)
	}

	w.px.fail = false
	w.clock.Sleep(context.Background(), 25*time.Hour) // past plex_refresh: the map is read now that Plex answers
	w.run(t)
	created, top := len(w.px.colls), w.plexColl(t, "Top").key
	if _, err := w.h.Env.Store.DB().Exec(`DELETE FROM lists_sinks`); err != nil {
		t.Fatal(err)
	}
	w.run(t)
	if len(w.px.colls) != created || len(w.jf.sets) != 2 {
		t.Errorf("after losing state: %d Plex collections (was %d), %d BoxSets (was 2)", len(w.px.colls), created, len(w.jf.sets))
	}
	var id string
	w.h.Env.Store.DB().QueryRow(`SELECT id FROM lists_sinks WHERE app='plex' AND list=?`, w.top).Scan(&id)
	if id != top {
		t.Errorf("after losing state the Top collection was not adopted: state id %q, collection %q", id, top)
	}
}

// Catches: limit ignored (the Top 250 would shelve all 500) or applied after reversing (it would keep the
// list's last films), and reverse ignored (the Criterion list runs newest-first, its shelf runs by spine).
func TestLimitAndReverse(t *testing.T) {
	w := newWorld(t)
	w.h.SetOptions(w.opts + fmt.Sprintf("home = []\n[[canon]]\nname = \"Top 4 reversed\"\nurl = %q\nlimit = 4\nreverse = true\n", w.top))
	w.run(t)
	// the first four: Harakiri, The Human Condition III (not owned), 12 Angry Men, Come and See — reversed
	if want := []string{"k25237", "k389", "k14537"}; !reflect.DeepEqual(w.plexColl(t, "Top 4 reversed").items, want) {
		t.Errorf("plex = %v, want %v", w.plexColl(t, "Top 4 reversed").items, want)
	}
}

// Catches: a renamed own list shelved twice — once from the lists page (new path), once from the export's code,
// whose cached path is the old one (Letterboxd redirects it) — two shelves fighting over one collection.
func TestRenamedOwnListShelvedOnce(t *testing.T) {
	w := newWorld(t)
	w.lb.redirect["fQbG8"] = "/saahilsan/list/nolan-old/"
	w.lb.redirect["saahilsan/list/nolan-old/"] = "/saahilsan/list/nolan/"
	index := string(w.lb.pages["/saahilsan/lists/"])
	w.lb.pages["/saahilsan/lists/"] = []byte(strings.Replace(index, `<div class="list-summary-list">`,
		`<div class="list-summary-list"><h2 class="name prettify"><a href="/saahilsan/list/nolan/">nolan</a></h2>`, 1))
	w.run(t)
	n := 0
	for _, s := range w.jf.sets {
		if strings.HasPrefix(s.name, "nolan") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("nolan shelved %d times, want once", n)
	}
}

// Catches: a partial section listing (Plex too slow for some pages) replacing the map, after which the films it
// missed would be dropped from their shelves and re-added, out of order, on a later run.
func TestPartialPlexListingNeverDrops(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	w.clock.Sleep(context.Background(), 25*time.Hour) // past plex_refresh: the listing runs again
	w.px.failAt = 3                                   // its second and third pages fail
	before := len(w.px.writes())
	w.run(t)
	for _, c := range w.px.writes()[before:] {
		if strings.HasPrefix(c, "DELETE ") {
			t.Errorf("partial listing dropped a member: %s", c)
		}
	}
	if want := []string{"k14537", "k389", "k25237", "k670"}; !reflect.DeepEqual(w.plexColl(t, "Top").items, want) {
		t.Errorf("plex Top = %v, want %v", w.plexColl(t, "Top").items, want)
	}
}

// Catches: BoxSets rebuilt from a listing Jellyfin gives while it scans its movie library (movies drop out and come
// back): members dropped and re-added, out of order, until the scan ends; and that partial listing replacing the
// last good map, which is what the next run falls back on if Jellyfin does not answer.
func TestJellyfinScanDefersBoxSets(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	w.jf.scanning = true
	before := len(w.jf.writes())
	rep := w.run(t)
	if got := w.jf.writes()[before:]; len(got) > 0 {
		t.Errorf("writes while Jellyfin scans: %v", got)
	}
	if !strings.Contains(rep.Summary, "jellyfin 0 writes (rest deferred: Jellyfin is scanning") {
		t.Errorf("summary = %q", rep.Summary)
	}
	if want := []string{"j14537", "j389", "j25237", "j670"}; !reflect.DeepEqual(jfOrder(w.boxSet(t, "Top")), want) {
		t.Errorf("jellyfin Top = %v, want %v", w.boxSet(t, "Top").items, want)
	}
	var kept jellyfinMap
	w.h.Env.Store.GetJSON(context.Background(), name, "jellyfin_map", &kept)
	if len(kept.Items) != len(w.jf.movies) {
		t.Errorf("map kept through the scan has %d movies, want %d", len(kept.Items), len(w.jf.movies))
	}
}
