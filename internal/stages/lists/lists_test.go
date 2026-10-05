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
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/jellyfin"
	"projectionist/internal/lbpace"
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
// seven of the films on the NAS, a hand-made Plex collection whose title differs from the own list's only in case.
func newWorld(t *testing.T) *world {
	h := fake.NewEnv(t, nil)
	w := &world{h: h, clock: &fakeClock{now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}}
	h.Env.Now = w.clock.Now
	oldSleep, oldPage, oldLimiter := sleep, plexPage, limiter
	sleep, plexPage, limiter = w.clock.Sleep, 3, lbpace.New(w.clock.Now, w.clock.Sleep) // three listing pages for seven movies
	t.Cleanup(func() { sleep, plexPage, limiter = oldSleep, oldPage, oldLimiter })

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
	w.px.colls = []*plexColl0{{key: "500", title: "Nolan", summary: "my favourite Nolans", items: []string{"k77"}}}
	h.Env.Plex = plex.New(w.px.srv.URL, h.PrefsPath, 5*time.Second)

	w.jf = newFakeJellyfin(t, map[string]int{"j14537": 14537, "j389": 389, "j670": 670, "j25237": 25237, "j157336": 157336,
		"j77": 77, "j27205": 27205}, w.clock.Now)
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
// or Home row, a Home row on Recommended or friends' Homes, a private list shelved, a hand-made collection whose
// title differs only in case touched (Plex files a new collection with that title into it) or the two apps naming
// the shelf differently, and — on the second run — any write at all or any Letterboxd request inside the refresh
// windows (the reconciler's "nothing to do").
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
	if h := w.px.hubs[2]; h["promotedToOwnHome"] != true || h["promotedToRecommended"] != false || h["promotedToSharedHome"] != false {
		t.Errorf("home row flags = %v", h)
	}
	if hand := w.px.coll("500"); hand.title != "Nolan" || hand.summary != "my favourite Nolans" || !reflect.DeepEqual(hand.items, []string{"k77"}) {
		t.Errorf("hand-made collection changed: %+v", hand)
	}
	if want := []string{"k157336", "k77"}; !reflect.DeepEqual(w.plexColl(t, "nolan · Letterboxd").items, want) {
		t.Errorf("plex nolan = %v, want %v", w.plexColl(t, "nolan · Letterboxd").items, want)
	}
	for _, c := range w.px.writes() {
		if strings.Contains(c, "/500/") || strings.Contains(c, "/500?") || strings.Contains(c, "id=500") || strings.Contains(c, "Item=500") {
			t.Errorf("write to the hand-made collection: %s", c)
		}
	}

	bsTop, bsNolan := w.boxSet(t, "Top"), w.boxSet(t, "nolan · Letterboxd")
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

	w.px.hubs[2]["promotedToRecommended"] = true // as an earlier build left the gate's rows
	w.run(t)
	if h := w.px.hubs[2]; h["promotedToOwnHome"] != true || h["promotedToRecommended"] != false {
		t.Errorf("home row left on Recommended: %v", h)
	}
}

// Catches: a collection the owner makes after the stage's last look at an app, with a title equal to a new shelf's
// but for case — adopted as the shelf's, or created over (Plex files the shelf's films into it) instead of the
// shelf waiting a run — and, once each app has reported it, the shelf not renamed in both apps (each app's owner
// titles count for both, so the collections stage, which matches titles ignoring case, never writes into a shelf).
func TestOwnerCollectionsKeepTheirTitles(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	w.h.SetOptions(w.opts + fmt.Sprintf("home = []\n[[canon]]\nname = \"Top\"\nurl = %q\n"+
		"[[canon]]\nname = \"Top 3\"\nurl = %q\nlimit = 3\n[[canon]]\nname = \"Top 4\"\nurl = %q\nlimit = 4\n", w.top, w.top, w.top))
	w.px.colls = append(w.px.colls, &plexColl0{key: "600", title: "top 3", summary: "mine", items: []string{"k11"}})
	w.jf.sets = append(w.jf.sets, &boxSet0{id: "mine4", name: "TOP 4", overview: "mine", items: []string{"j77"}})
	for run := 2; run <= 3; run++ {
		w.clock.Sleep(context.Background(), time.Hour)
		w.run(t)
	}
	for _, title := range []string{"Top 3 · Letterboxd", "Top 4 · Letterboxd"} {
		w.plexColl(t, title)
		w.boxSet(t, title)
	}
	if c := w.px.coll("600"); c.title != "top 3" || c.summary != "mine" || !reflect.DeepEqual(c.items, []string{"k11"}) {
		t.Errorf("the owner's Plex collection changed: %+v", c)
	}
	if b := w.jf.set("mine4"); b.name != "TOP 4" || b.overview != "mine" || !reflect.DeepEqual(b.items, []string{"j77"}) {
		t.Errorf("the owner's BoxSet changed: %+v", b)
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

// Catches: a failing Plex failing the run or stopping Jellyfin, and — once Plex is back, with the state of the
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

// Catches: limit ignored (the Top 250 would shelve all 500), and a limited shelf's summary linking the whole list as
// if it were the shelf's (Letterboxd has no Top 250 list of its own any more).
func TestLimit(t *testing.T) {
	w := newWorld(t)
	w.h.SetOptions(w.opts + fmt.Sprintf("home = []\n[[canon]]\nname = \"Top 4\"\nurl = %q\nlimit = 4\n", w.top))
	w.run(t)
	// the first four: Harakiri, The Human Condition III (not owned), 12 Angry Men, Come and See
	top4 := w.plexColl(t, "Top 4")
	if want := []string{"k14537", "k389", "k25237"}; !reflect.DeepEqual(top4.items, want) {
		t.Errorf("plex = %v, want %v", top4.items, want)
	}
	if want := "On the NAS: 3 of 4 · seen 2 · the first 4 of " + w.top; top4.summary != want {
		t.Errorf("summary = %q, want %q", top4.summary, want)
	}
}

// Catches: a default canon shelf out of its list's published order — the Criterion shelf reversed, as it once was:
// the list runs newest spine first and ends with box-set films that have no spine, so its reverse opened on Full
// Metal Jacket rather than on the newest release.
func TestDefaultCanonKeepsPublishedOrder(t *testing.T) {
	w := newWorld(t)
	w.h.SetOptions(w.opts) // the default canon and Home rows
	w.lb.pages["/jbutts15/list/the-complete-criterion-collection/"] = w.lb.pages["/official/list/letterboxds-top-500-films/"]
	w.run(t)
	if want := []string{"k14537", "k389", "k25237", "k670"}; !reflect.DeepEqual(w.plexColl(t, "Criterion Collection").items, want) {
		t.Errorf("plex Criterion = %v, want %v", w.plexColl(t, "Criterion Collection").items, want)
	}
	if want := []string{"j14537", "j389", "j25237", "j670"}; !reflect.DeepEqual(jfOrder(w.boxSet(t, "Criterion Collection")), want) {
		t.Errorf("jellyfin Criterion = %v, want %v", w.boxSet(t, "Criterion Collection").items, want)
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

// Catches: BoxSets rebuilt from listings Jellyfin gives while it scans (movies drop out of the library and of each
// BoxSet, and come back): members dropped and re-added, out of order, every run until the scan ends — whether the
// Movies library reports the scan, or only the scan task does (measured: the task runs on after the library says
// Idle), or Jellyfin does not say; that partial listing replacing the last good map, which the next run falls back
// on; and a scan that never ends stopping BoxSet upkeep without telling the owner.
func TestJellyfinScanDefersBoxSets(t *testing.T) {
	for _, c := range []struct{ name, scan, failOn string }{
		{"library scanning", "library", ""},
		{"only the scan task running", "task", ""},
		{"scan state not answered", "task", "GET /ScheduledTasks"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.run(t)
			w.jf.scanning, w.jf.failOn = c.scan, c.failOn
			before := len(w.jf.writes())
			for run := 1; run <= 3; run++ {
				rep := w.run(t)
				if !strings.Contains(rep.Summary, "jellyfin 0 writes (rest deferred: Jellyfin is scanning") {
					t.Errorf("run %d: summary = %q", run, rep.Summary)
				}
				if alerted := len(rep.Alerts) > 0; alerted != (run == 3) {
					t.Errorf("run %d of the scan: alerts %v, want one only from the third", run, rep.Alerts)
				}
			}
			if got := w.jf.writes()[before:]; len(got) > 0 {
				t.Errorf("writes while Jellyfin scans: %v", got)
			}
			if want := []string{"j14537", "j389", "j25237", "j670"}; !reflect.DeepEqual(w.boxSet(t, "Top").items, want) {
				t.Errorf("jellyfin Top = %v, want %v", w.boxSet(t, "Top").items, want)
			}
			var kept jellyfinMap
			w.h.Env.Store.GetJSON(context.Background(), name, "jellyfin_map", &kept)
			if len(kept.Items) != len(w.jf.movies) {
				t.Errorf("map kept through the scan has %d movies, want %d", len(kept.Items), len(w.jf.movies))
			}
		})
	}
}

// Catches: one of the stage's own collections counted as an owner's title because a failed write left it without
// the stage's summary — the shelf would be renamed "… · Letterboxd" on a later run, and back once the summary is in.
func TestOwnShelfNeverTakesItsOwnTitle(t *testing.T) {
	w := newWorld(t)
	w.px.failOn = "PUT /library/sections/4/all" // Top is made in both apps, but its summary write fails
	w.jf.failOn = "POST /Items/"
	w.run(t)
	w.px.failOn, w.jf.failOn = "", ""
	for run := 2; run <= 3; run++ {
		w.clock.Sleep(context.Background(), time.Hour)
		w.run(t)
	}
	if top := w.plexColl(t, "Top"); !strings.HasPrefix(top.summary, summaryMarker) {
		t.Errorf("plex Top summary = %q", top.summary)
	}
	if top := w.boxSet(t, "Top"); !strings.HasPrefix(top.overview, summaryMarker) {
		t.Errorf("jellyfin Top overview = %q", top.overview)
	}
}

// Catches: a shelf left in both apps (and on Home) after its list is gone — made private on Letterboxd (also one
// found only on his lists page, which drops it), left out by skip_own, dropped from the canon config — so a private
// list stays visible to every account sharing the library; a shelf deleted because its list was merely unreadable
// this run (a refusal, a state read that fails); and a shelf deleted after the owner made it his (rewrote its summary).
func TestRetiresShelvesOfGoneLists(t *testing.T) {
	nolan := "/saahilsan/list/nolan/"
	canonTop := func(w *world, extra string) {
		w.h.SetOptions(w.opts + extra + fmt.Sprintf("home = [\"Top\"]\n[[canon]]\nname = \"Top\"\nurl = %q\n", w.top))
	}
	for _, c := range []struct {
		name         string
		before       func(w *world) // before the first run
		change       func(w *world)
		goneInPlex   string // the shelf that must be gone; "" = none
		goneInJF     string
		retiredCount int
	}{
		{"made private", nil, func(w *world) { delete(w.lb.pages, nolan) }, "nolan · Letterboxd", "nolan · Letterboxd", 2},
		{"made private, found only on his lists page", func(w *world) {
			os.RemoveAll(filepath.Join(filepath.Dir(w.missing), "export"))
			w.lb.pages["/saahilsan/lists/"] = []byte(strings.Replace(string(w.lb.pages["/saahilsan/lists/"]), `<div class="list-summary-list">`,
				`<div class="list-summary-list"><h2 class="name prettify"><a href="/saahilsan/list/nolan/">nolan</a></h2>`, 1))
		}, func(w *world) {
			delete(w.lb.pages, nolan)
			w.lb.pages["/saahilsan/lists/"] = fixture(t, "lists_index.html")
		}, "nolan · Letterboxd", "nolan · Letterboxd", 2},
		{"left out by skip_own", nil, func(w *world) { canonTop(w, "skip_own = [\"nolan\"]\n") }, "nolan · Letterboxd", "nolan · Letterboxd", 2},
		{"dropped from the canon", nil, func(w *world) { w.h.SetOptions(w.opts + "canon = []\n") }, "Top", "Top", 2},
		{"refused this run", nil, func(w *world) { w.lb.status[nolan] = 403 }, "", "", 0},
		{"unreadable in the state", nil, func(w *world) {
			if _, err := w.h.Env.Store.DB().Exec(`UPDATE lists_pages SET page='{' WHERE path=?`, nolan); err != nil {
				t.Fatal(err)
			}
		}, "", "", 0},
		{"made his own, then dropped", nil, func(w *world) {
			w.plexColl(t, "Top").summary = "my favourites from the Top 500"
			w.h.SetOptions(w.opts + "canon = []\n")
		}, "", "Top", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			if c.before != nil {
				c.before(w)
			}
			w.run(t)
			c.change(w)
			w.clock.Sleep(context.Background(), 21*time.Hour) // past own_refresh
			rep := w.run(t)
			for _, title := range []string{"Top", "nolan · Letterboxd"} {
				inPlex, inJellyfin := false, false
				for _, x := range w.px.colls {
					inPlex = inPlex || x.title == title
				}
				for _, x := range w.jf.sets {
					inJellyfin = inJellyfin || x.name == title
				}
				if inPlex == (title == c.goneInPlex) || inJellyfin == (title == c.goneInJF) {
					t.Errorf("%q: in Plex %v (gone: %q), in Jellyfin %v (gone: %q)", title, inPlex, c.goneInPlex, inJellyfin, c.goneInJF)
				}
			}
			for _, h := range w.px.hubs {
				if id := h["identifier"].(string); strings.HasPrefix(id, "custom.collection.") && c.goneInPlex == "Top" {
					t.Errorf("home row of a retired shelf left: %s", id)
				}
			}
			if hand := w.px.coll("500"); hand == nil || hand.title != "Nolan" {
				t.Errorf("hand-made collection touched: %+v", hand)
			}
			if rep.Counts["shelves_retired"] != c.retiredCount {
				t.Errorf("shelves_retired = %d, want %d", rep.Counts["shelves_retired"], c.retiredCount)
			}
		})
	}
}

// Catches: a slow Plex holding up Jellyfin. Every Plex call here waits until Jellyfin has written the list change,
// so a run whose Jellyfin lane waits for Plex never gets there and the test hangs instead of passing.
func TestSlowPlexNeverHoldsUpJellyfin(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	p2 := "/official/list/letterboxds-top-500-films/page/2/"
	w.lb.pages[p2] = []byte(strings.Replace(string(w.lb.pages[p2]), posterLI(string(w.lb.pages[p2]), "oldboy"), "", 1))
	w.clock.Sleep(context.Background(), 8*24*time.Hour) // past canon_refresh and plex_refresh

	w.px.hold = make(chan struct{})
	w.h.Env.Plex = plex.New(w.px.srv.URL, w.h.PrefsPath, time.Hour) // a held call never times out into a pass
	var once sync.Once
	w.jf.onWrite = func(call string) {
		if strings.HasPrefix(call, "DELETE /Collections/") { // Oldboy leaves the Top BoxSet
			once.Do(func() { close(w.px.hold) })
		}
	}
	t.Cleanup(func() { once.Do(func() { close(w.px.hold) }) })
	w.run(t)
	if want := []string{"j14537", "j389", "j25237"}; !reflect.DeepEqual(jfOrder(w.boxSet(t, "Top")), want) {
		t.Errorf("jellyfin Top = %v, want %v", w.boxSet(t, "Top").items, want)
	}
	if want := []string{"k14537", "k389", "k25237"}; !reflect.DeepEqual(w.plexColl(t, "Top").items, want) {
		t.Errorf("plex Top = %v, want %v (Plex catches up once it answers)", w.plexColl(t, "Top").items, want)
	}
}

// Catches: a page that is not what it should be taken at face value — a film page that is a challenge answered with
// HTTP 200 cached as "no TMDB link" for 30 days (the film missing from every shelf), and a list page that parses
// to no films (a challenge or a new layout) taken as an empty list, which would empty the shelf in both apps.
func TestUnreadablePagesChangeNothing(t *testing.T) {
	w := newWorld(t)
	delete(w.lb.films, "oldboy")
	w.lb.pages["/film/oldboy/"] = fixture(t, "challenge.html")
	w.run(t)
	w.lb.films["oldboy"] = "movie/670"
	w.clock.Sleep(context.Background(), time.Hour)
	w.run(t)
	want := []string{"k14537", "k389", "k25237", "k670"}
	if got := w.plexColl(t, "Top").items; !reflect.DeepEqual(got, want) {
		t.Fatalf("plex Top = %v, want %v (Oldboy's film page read again once it answers)", got, want)
	}

	w.lb.pages["/official/list/letterboxds-top-500-films/"] = fixture(t, "challenge.html")
	w.clock.Sleep(context.Background(), 8*24*time.Hour) // past canon_refresh
	pxBefore, jfBefore := len(w.px.writes()), len(w.jf.writes())
	w.run(t)
	for _, c := range append(w.px.writes()[pxBefore:], w.jf.writes()[jfBefore:]...) {
		if strings.HasPrefix(c, "DELETE ") {
			t.Errorf("an unreadable list page dropped a member: %s", c)
		}
	}
	if got := w.plexColl(t, "Top").items; !reflect.DeepEqual(got, want) {
		t.Errorf("plex Top = %v, want %v", got, want)
	}
}

// Catches: a retired shelf's record forgotten while its collection still exists — because this run's listing left it
// out (Plex), or because Jellyfin, mid-scan, answered 404 for a BoxSet it still holds — after which nothing deletes
// the orphan, and a list gone private stays on a library every shared account sees; and the record of a create
// never confirmed forgotten, though nothing confirmed its result gone.
func TestRetireForgetsOnlyConfirmedGone(t *testing.T) {
	w := newWorld(t)
	w.run(t)
	px, jf := w.plexColl(t, "nolan · Letterboxd"), w.boxSet(t, "nolan · Letterboxd")
	delete(w.lb.pages, "/saahilsan/list/nolan/") // made private
	// a canon list dropped from the config whose create was refused: no collection, no id to confirm anything by
	const unconfirmed = "https://letterboxd.com/x/list/dropped/"
	for _, app := range []string{"plex", "jellyfin"} {
		if err := putRow(context.Background(), w.h.Env.Store.DB(), app, unconfirmed, sinkRow{IntentAt: w.clock.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	w.px.hidden[px.key] = true
	w.jf.hidden[jf.id], w.jf.scanning = true, "task"
	w.clock.Sleep(context.Background(), 21*time.Hour) // past own_refresh
	w.run(t)
	w.px.hidden, w.jf.hidden, w.jf.scanning = map[string]bool{}, map[string]bool{}, ""
	w.clock.Sleep(context.Background(), time.Hour)
	w.run(t)
	if c := w.px.coll(px.key); c != nil {
		t.Errorf("plex collection of a private list left: %+v", c)
	}
	if b := w.jf.set(jf.id); b != nil {
		t.Errorf("jellyfin BoxSet of a private list left: %+v", b)
	}
	var n int
	w.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM lists_sinks WHERE list=?`, unconfirmed).Scan(&n)
	if n != 2 {
		t.Errorf("records of a create never confirmed: %d left, want 2 (kept)", n)
	}
}

// Catches: adoption by intent that is unbounded or skips the title check — a collection the owner (or the
// collections stage) makes with a shelf's title a day after a failed create adopted and rewritten (members, summary,
// poster, lock), or a BoxSet adopted while another BoxSet holds the title — and, for a create whose answer was lost
// but which went through, the result not adopted on the next run, or reported as an owner's title (the shelf would
// be renamed "… · Letterboxd" the run after it is adopted).
func TestAdoptsOnlyItsOwnCreate(t *testing.T) {
	ctx := context.Background()
	t.Run("lost answer adopted on the next run", func(t *testing.T) {
		w := newWorld(t)
		// the first create in each app, Top's: the app makes it, the answer never arrives
		w.px.loseOn, w.jf.loseOn = "POST /library/collections", "POST /Collections"
		w.run(t)
		w.px.loseOn, w.jf.loseOn = "", ""
		w.clock.Sleep(ctx, 24*time.Hour)
		w.run(t)
		w.clock.Sleep(ctx, time.Hour)
		pxBefore, jfBefore := len(w.px.writes()), len(w.jf.writes())
		w.run(t)
		if got := append(w.px.writes()[pxBefore:], w.jf.writes()[jfBefore:]...); len(got) > 0 {
			t.Errorf("third run wrote: %v", got)
		}
		px, jf := 0, 0
		for _, c := range w.px.colls {
			if strings.HasPrefix(strings.ToLower(c.title), "top") {
				px++
			}
		}
		for _, s := range w.jf.sets {
			if strings.HasPrefix(strings.ToLower(s.name), "top") {
				jf++
			}
		}
		if top := w.plexColl(t, "Top"); px != 1 || !strings.HasPrefix(top.summary, summaryMarker) {
			t.Errorf("%d Top collections; Top summary %q", px, top.summary)
		}
		if top := w.boxSet(t, "Top"); jf != 1 || !strings.HasPrefix(top.overview, summaryMarker) {
			t.Errorf("%d Top BoxSets; Top overview %q", jf, top.overview)
		}
	})
	t.Run("a collection made a day later is never adopted", func(t *testing.T) {
		w := newWorld(t)
		w.px.failOn = "POST /library/collections" // refused: no collection
		w.jf.loseOn = "POST /Collections"         // made, answer lost
		w.run(t)
		w.px.failOn, w.jf.loseOn = "", ""
		w.clock.Sleep(ctx, 24*time.Hour)
		w.px.colls = append(w.px.colls, &plexColl0{key: "700", title: "top", summary: "mine", added: w.clock.Now().Unix(), items: []string{"k11"}})
		w.jf.sets = append(w.jf.sets, &boxSet0{id: "mine", name: "TOP", overview: "mine", created: w.clock.Now(), items: []string{"j77"}})
		orphan := w.jf.sets[0]
		pxBefore, jfBefore := len(w.px.writes()), len(w.jf.writes())
		rep := w.run(t)
		if c := w.px.coll("700"); c.title != "top" || c.summary != "mine" || !reflect.DeepEqual(c.items, []string{"k11"}) {
			t.Errorf("the owner's Plex collection changed: %+v", c)
		}
		for _, c := range w.px.writes()[pxBefore:] {
			if strings.Contains(c, "/700/") || strings.Contains(c, "/700?") || strings.Contains(c, "id=700") {
				t.Errorf("write to the owner's Plex collection: %s", c)
			}
		}
		for _, c := range w.jf.writes()[jfBefore:] {
			if strings.Contains(c, "/mine") || strings.Contains(c, "/"+orphan.id) {
				t.Errorf("write to a BoxSet holding the title %q: %s", "Top", c)
			}
		}
		if rep.Counts["name_taken"] != 2 {
			t.Errorf("name_taken = %d, want 2 (both apps hold Top back)", rep.Counts["name_taken"])
		}
	})
}

// Catches: one of the owner's lists named like a canon shelf but for case ("TOP" beside "Top") given the canon
// shelf's title: Plex files both under one tag, and the two shelves adopt it by its summary and fight every run.
func TestOwnListNamedLikeACanonShelf(t *testing.T) {
	w := newWorld(t)
	w.lb.pages["/saahilsan/list/nolan/"] = []byte(strings.Replace(string(fixture(t, "list_nolan.html")), `content="nolan"`, `content="TOP"`, 1))
	w.run(t)
	if want := []string{"k14537", "k389", "k25237", "k670"}; !reflect.DeepEqual(w.plexColl(t, "Top").items, want) {
		t.Errorf("plex Top = %v, want %v", w.plexColl(t, "Top").items, want)
	}
	if want := []string{"k157336", "k77"}; !reflect.DeepEqual(w.plexColl(t, "TOP · my list").items, want) {
		t.Errorf("plex TOP · my list = %v, want %v", w.plexColl(t, "TOP · my list").items, want)
	}
	w.boxSet(t, "TOP · my list")
}

// Catches: a list the owner keeps for himself ("private reviews and notes", public on Letterboxd) shelved on a
// library every shared account sees, and skip_own names matched as substrings ("nolan" would leave out "nolan
// ranked") or with case.
func TestPrivateOwnListsStayOff(t *testing.T) {
	w := newWorld(t)
	w.lb.pages["/saahilsan/list/nolan/"] = []byte(strings.Replace(string(fixture(t, "list_nolan.html")), `content="nolan"`,
		`content="Private Reviews and Notes"`, 1))
	w.run(t)
	for _, c := range w.px.colls {
		if strings.Contains(strings.ToLower(c.title), "private") {
			t.Errorf("plex shelf %q", c.title)
		}
	}
	for _, s := range w.jf.sets {
		if strings.Contains(strings.ToLower(s.name), "private") {
			t.Errorf("jellyfin shelf %q", s.name)
		}
	}
	for _, c := range []struct {
		pattern, name string
		skip          bool
	}{{"nolan", "NOLAN", true}, {"nolan", "nolan ranked", false}, {"*private*", "my private notes", true}, {"a.c", "abc", false}} {
		if got := skipped([]string{c.pattern}, c.name); got != c.skip {
			t.Errorf("skip_own %q on %q = %v, want %v", c.pattern, c.name, got, c.skip)
		}
	}
}
