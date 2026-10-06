package posters

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/llm"
	"projectionist/internal/llm/llmtest"
)

func init() { WikimediaPace = func(bool) time.Duration { return 0 } }

// collectionWorld adds to newWorld the owner's collection Alien (his poster: never touched), two approved franchises
// and what TMDB, fanart.tv, Wikimedia Commons, Plex and Jellyfin say about them and about Paul Greengrass. Unless
// keepTitles, the library holds no movie or show, so only collections are judged.
func collectionWorld(t *testing.T, opts string, keepTitles bool) *world {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "owner.json"), []byte(`[{"ratingKey":"500","title":"Alien","titleSort":null,
 "members":[{"ids":["tmdb://348"]}]}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "approved.json"), []byte(`[
 {"name":"Scary Movie","kind":"franchise","sort_title":null,"tmdb_collection":["Scary Movie Collection"],"members":[{"tmdb":4247},{"tmdb":4248}]},
 {"name":"Evil Dead","kind":"franchise","sort_title":null,"tmdb_collection":["Evil Dead Collection"],"members":[{"tmdb":764}]}]`), 0o644)
	w := newWorld(t, fmt.Sprintf("collections_owner_file = %q\ncollections_approved_file = %q\n", filepath.Join(dir, "owner.json"),
		filepath.Join(dir, "approved.json"))+opts)
	w.ownerFile = filepath.Join(dir, "owner.json")
	w.opts += fmt.Sprintf("commons_api = %q\n", w.api.URL+"/w/api.php")
	w.h.SetOptions(w.opts)
	if !keepTitles {
		for _, d := range []string{"Movies", "TV"} {
			os.RemoveAll(filepath.Join(w.lib, d))
			os.MkdirAll(filepath.Join(w.lib, d), 0o755)
		}
	}
	img := func(colour, lang string) string {
		return fmt.Sprintf(`{"file_path":"/%s.jpg","iso_639_1":"%s","width":1000,"height":1500,"vote_average":6,"vote_count":3}`, colour, lang)
	}
	for path, body := range map[string]string{
		"/movie/4247": `{"id":4247,"title":"Scary Movie","release_date":"2000-07-07","original_language":"en","vote_average":6.3,
 "vote_count":5000,"belongs_to_collection":{"id":4246,"name":"Scary Movie Collection"}}`,
		"/movie/4248": `{"id":4248,"title":"Scary Movie 2","original_language":"en","vote_average":5.6,"vote_count":3000,
 "belongs_to_collection":{"id":999,"name":"Scary Movie Spinoffs"}}`,
		"/collection/999/images":  `{"posters":[` + img("yellow", "en") + `]}`,
		"/collection/4246/images": `{"posters":[` + img("red", "en") + `,` + img("yellow", "de") + `]}`,
		"/movie/764": `{"id":764,"title":"The Evil Dead","original_language":"en","vote_average":7.3,"vote_count":3000,
 "belongs_to_collection":{"id":1960,"name":"Evil Dead Collection"}}`,
		"/movie/764/images": `{"posters":[` + img("green", "en") + `]}`,
		"/movie/2502/credits": `{"crew":[{"id":25598,"name":"Paul Greengrass","job":"Director"},{"id":8,"name":"Co Director","job":"Director"},
 {"id":7,"name":"Tony Gilroy","job":"Writer"}]}`,
		"/person/8/images":            `{"profiles":[{"file_path":"/yellow.jpg","width":1000,"height":1500}]}`,
		"/movie/9829/credits":         `{"crew":[{"id":25598,"name":"Paul Greengrass","job":"Director"}]}`,
		"/person/25598/external_ids":  `{"wikidata_id":"Q356275"}`,
		"/person/25598/images":        `{"profiles":[{"file_path":"/blue.jpg","width":1000,"height":1500}]}`,
		"/person/25598/tagged_images": `{"results":[]}`,
	} {
		w.h.TMDB.Handle("GET", path, 200, body)
	}
	w.h.Fanart.Handle("GET", "/movies/4246", 200, `{"name":"Scary Movie Collection","movieposter":[{"id":"1","url":"`+w.api.URL+`/t/p/fanart/blue.jpg","lang":"en","likes":"5"}]}`)
	w.h.Fanart.Handle("GET", "/movies/1960", 200, `{"name":"The Evil Dead","movieposter":[{"id":"2","url":"`+w.api.URL+`/t/p/fanart/red.jpg","lang":"en","likes":"9"}]}`)
	file := func(title, colour string, wd, ht int, desc string) string {
		return fmt.Sprintf(`{"title":%q,"imageinfo":[{"url":"%[2]s/original/%[3]s.jpg","thumburl":"%[2]s/t/p/c/500px-%[3]s.jpg","mime":"image/jpeg",
 "width":%[4]d,"height":%[5]d,"extmetadata":{"ImageDescription":{"value":%[6]q},"LicenseShortName":{"value":"CC BY-SA 4.0"}}}]}`,
			title, w.api.URL, colour, wd, ht, desc)
	}
	w.set(func() {
		w.commons = func(rw http.ResponseWriter, r *http.Request) {
			q, pages := r.URL.Query(), ""
			switch search := q.Get("gsrsearch"); {
			case q.Get("prop") == "pageprops" && q.Get("titles") == "Category:Paul Greengrass":
				pages = `{"pageprops":{"wikibase_item":"Q99"}}` // a namesake's category
			case strings.HasPrefix(search, "haswbstatement:P180=Q356275"):
				pages = file("File:Paul Greengrass filming United 93.jpg", "redwide", 3000, 1500, "<b>Paul Greengrass</b> on the set") + "," +
					file("File:Greengrass small.jpg", "yellow", 300, 200, "Paul Greengrass") + "," + file("File:Greengrass at TIFF.jpg", "green", 1000, 1500, "")
			case strings.HasPrefix(search, `incategory:"Paul Greengrass"`):
				pages = file("File:Paul Greengrass the bowler.jpg", "yellow", 2000, 3000, "")
			case strings.HasPrefix(search, `"Paul Greengrass" filming`):
				pages = file("File:Sacramento skyline.jpg", "yellow", 4000, 2000, "The skyline of downtown Sacramento")
			}
			reply(rw, `{"query":{"pages":[`+pages+`]}}`)
		}
	})
	w.h.Plex.Handle("GET", "/library/sections/4/collections", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"601","title":"Scary Movie"},
 {"ratingKey":"603","title":"Evil Dead"},{"ratingKey":"500","title":"Alien"}]}}`)
	w.jellyfinLists(jellyfinItems + `, {"Id":"jb1","Type":"BoxSet","Name":"Scary Movie"}, {"Id":"jb3","Type":"BoxSet","Name":"Evil Dead"},
 {"Id":"jb2","Type":"BoxSet","Name":"Paul Greengrass"}, {"Id":"jb0","Type":"BoxSet","Name":"Alien"}`)
	for _, rk := range []string{"500", "601", "602", "603", "6011"} {
		w.h.Plex.Handle("POST", "/library/metadata/"+rk+"/posters", 200, "")
		w.h.Plex.Handle("GET", "/library/metadata/"+rk, 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"`+rk+`","Field":[{"locked":true,"name":"title"}]}]}}`)
	}
	for _, id := range []string{"jb0", "jb1", "jb2", "jb3"} {
		w.h.Jellyfin.Handle("POST", "/Items/"+id+"/Images/Primary", 204, "")
	}
	return w
}

func (w *world) pool(key string) []Candidate {
	var raw string
	var cands []Candidate
	w.h.Env.Store.DB().QueryRow(`SELECT coalesce(cands, '') FROM posters_items WHERE key=?`, key).Scan(&raw)
	json.Unmarshal([]byte(raw), &cands)
	return cands
}

// Catches, end to end: collection posters uploaded before collection_dry_run is switched off; the owner's collection
// gathered, judged or given a poster; a franchise pool missing its TMDB collection's, fanart.tv's or its film's
// poster (the film's as this stage chose it), or holding fanart.tv art filed under the collection's number for a film;
// a member film's other TMDB collection feeding the pool; a Commons file kept though it is tiny, never names the
// director or sits in a namesake's category; a Commons original downloaded (Wikimedia throttles them) or a smaller
// rendering than the photo allows; a co-director of one film taken for the director; a title's poster sent
// while titles are dry and collections live; a collection poster sent to the wrong app item
// or left unlocked in Plex; a director's poster not composed to 2:3 around the judge's box; a collection opened by an
// arrival, or made in an app after the first listing, never getting its poster; a wait for the app counted as a failed
// attempt (its backoff grows); Plex uploads and locks missing from the library changes health's optimize reads; the
// stage's own Plex lock taken for someone else's; and a rerun paying or uploading again.
func TestCollectionPosters(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\n", false)
	film := `[{"url":"` + w.api.URL + `/t/p/original/green.jpg","thumb":"` + w.api.URL + `/t/p/w500/green.jpg","source":"tmdb"}]`
	w.h.Env.Store.DB().Exec(schema)
	w.h.Env.Store.DB().Exec(`INSERT INTO posters_items(key, title, cands, sig, verdict, updated_at) VALUES ('movie:tmdb-4247', '{}', ?, '', ?, '')`,
		film, `{"choice":"`+w.api.URL+`/t/p/original/green.jpg","rule":"judged"}`)
	w.run(t)
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/601/posters")) + len(w.h.Jellyfin.Calls("POST", "/Items/jb1/Images/Primary")); n != 0 {
		t.Fatalf("%d collection poster uploads with collection_dry_run unset; it defaults to true", n)
	}
	var sources []string
	for _, c := range w.pool("collection:scary movie") {
		sources = append(sources, c.Source+" "+filepath.Base(c.URL))
	}
	if want := []string{"tmdb-collection red.jpg", "fanart-collection blue.jpg", "film green.jpg"}; !slices.Equal(sources, want) {
		t.Fatalf("Scary Movie pool %v, want %v (the German poster stays out; the film's poster is the one this stage chose)", sources, want)
	}
	if v := w.verdict(t, "collection:evil dead"); !strings.HasSuffix(v.Choice, "/green.jpg") || v.Rule != "single" {
		t.Fatalf("Evil Dead: %s %s; fanart.tv's answer for its number names a film, so its film's poster is the only candidate", v.Rule, v.Choice)
	}

	w.h.SetOptions(w.opts + "collection_dry_run = false\n")
	w.h.Env.Store.DB().Exec(`INSERT INTO posters_sinks VALUES ('movie:tmdb-1', 'plex', ?, 'pending', '', 0, '', '')`, w.api.URL+"/t/p/original/red.jpg")
	w.run(t)
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/101/posters")); n != 0 {
		t.Fatalf("a title's pending poster went to Plex %d times while titles are dry", n)
	}
	up := w.h.Plex.Calls("POST", "/library/metadata/601/posters")
	if len(up) != 1 || scoreOf(up[0].Body) != 9 || !bytes.Contains(up[0].Body, []byte("poster-curator 2026-10")) {
		t.Fatalf("Plex collection 601 (Scary Movie): %d uploads; want one red poster carrying the stage's mark", len(up))
	}
	locked := func(rk string) bool {
		return slices.ContainsFunc(w.h.Plex.Calls("PUT", "/library/sections/4/all"), func(c fake.Call) bool { return c.RawQuery == "type=18&id="+rk+"&thumb.locked=1" })
	}
	if !locked("601") || len(w.h.Jellyfin.Calls("POST", "/Items/jb1/Images/Primary")) != 1 || len(w.h.Plex.Calls("POST", "/library/metadata/603/posters")) != 1 {
		t.Fatal("Scary Movie must be locked in Plex (type 18) and uploaded to Jellyfin's BoxSet jb1; Evil Dead uploaded to Plex 603")
	}
	var alien int
	w.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM posters_items WHERE key='collection:alien'`).Scan(&alien)
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/500/posters")) + len(w.h.Jellyfin.Calls("POST", "/Items/jb0/Images/Primary")); n != 0 || alien != 0 {
		t.Fatalf("the owner's collection Alien: %d uploads, %d rows; his posters are never touched", n, alien)
	}
	if n, _ := w.h.Env.Store.LibraryChanges(context.Background()); n != 4 {
		t.Fatalf("library changes counted %d, want 4 (two Plex uploads and two locks), so health's optimize sees them", n)
	}

	// An arrival opens the director collection Paul Greengrass; Jellyfin already lists it, Plex only from tomorrow.
	db := w.h.Env.Store.DB()
	db.Exec(`INSERT INTO collections_new(name, sort_title, at) VALUES ('Paul Greengrass', 'zzPaul Greengrass', '2026-10-05T06:00:00Z')`)
	db.Exec(`INSERT INTO collections_joins(collection, tmdb, reason, at) VALUES ('Paul Greengrass', 2502, 'x', ''), ('Paul Greengrass', 9829, 'x', '')`)
	w.run(t)
	var photos []string
	for _, c := range w.pool("collection:paul greengrass") {
		photos = append(photos, c.Source+" "+filepath.Base(c.URL))
	}
	if want := []string{"commons 1920px-redwide.jpg", "tmdb-profile blue.jpg", "commons 960px-green.jpg"}; !slices.Equal(photos, want) {
		t.Fatalf("Greengrass pool %v, want %v: on-set Commons first, turns with TMDB; no tiny file, no namesake's category, no text hit "+
			"that never names him, no co-director", photos, want)
	}
	var attempts int
	db.QueryRow(`SELECT attempts FROM posters_sinks WHERE item='collection:paul greengrass' AND sink='plex'`).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("Greengrass, not in Plex yet, counted %d failed attempts; waiting for the app is no failure", attempts)
	}
	jf := w.h.Jellyfin.Calls("POST", "/Items/jb2/Images/Primary")
	if len(jf) != 1 {
		t.Fatalf("Jellyfin BoxSet jb2 got %d uploads, want 1", len(jf))
	}
	body, _ := base64.StdEncoding.DecodeString(string(jf[0].Body))
	if archived, _ := os.ReadFile(filepath.Join(w.archive, "director", "paul_greengrass", "chosen.jpg")); len(archived) < 1000 || !bytes.HasSuffix(body, archived[2:]) {
		t.Fatal("Greengrass's archived chosen.jpg must be the poster as composed for the apps (without the stage's mark)")
	}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(body)); err != nil || cfg.Width != 1000 || cfg.Height != 1500 || scoreOf(body) != 9 {
		t.Fatalf("Greengrass poster %dx%d (err %v), score %d; want 1000x1500 around the red director in the photo's left quarter",
			cfg.Width, cfg.Height, err, scoreOf(body))
	}

	w.h.Plex.Handle("GET", "/library/sections/4/collections", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"601","title":"Scary Movie"},
 {"ratingKey":"603","title":"Evil Dead"},{"ratingKey":"602","title":"\u200ePaul Greengrass"},{"ratingKey":"500","title":"Alien"}]}}`)
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t)
	w.run(t)
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/602/posters")); n != 1 || !locked("602") || w.messages != 2 ||
		len(w.h.Plex.Calls("POST", "/library/metadata/601/posters")) != 1 || len(w.h.Jellyfin.Calls("POST", "/Items/jb2/Images/Primary")) != 1 {
		t.Fatalf("Greengrass in Plex the next day: %d uploads (want 1, locked); %d judge calls in all (want 2: Scary Movie, Greengrass); reruns change nothing",
			n, w.messages)
	}

	// Plex shows the lock the stage set on 601; Scary Movie's row is sent again: that lock is no one else's.
	w.h.Plex.Handle("GET", "/library/metadata/601", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"601","Field":[{"locked":true,"name":"thumb"}]}]}}`)
	db.Exec(`DELETE FROM posters_sinks WHERE item='collection:scary movie' AND sink='plex'`)
	if rep := w.run(t); len(w.h.Plex.Calls("POST", "/library/metadata/601/posters")) != 2 || len(rep.Alerts) != 0 {
		t.Fatalf("Scary Movie sent again to the 601 the stage locked: %d uploads (want 2), alerts %v (want none)",
			len(w.h.Plex.Calls("POST", "/library/metadata/601/posters")), rep.Alerts)
	}
}

// Catches: collection judging stopped by what the titles spent, titles stopped by what collections spent, collection
// calls past their own budget, and one spent budget's alert silencing the other's.
func TestCollectionBudgetApart(t *testing.T) {
	for _, c := range []struct {
		name          string
		spent         map[string]float64
		titles, colls bool     // judged this run
		alerts        []string // each alert names one budget
	}{
		{"collections spent", map[string]float64{"collection:x": 4.999}, true, false, []string{"collections"}},
		{"titles spent", map[string]float64{"movie:tmdb-9": 11.999}, false, true, []string{"titles"}},
		{"both spent", map[string]float64{"collection:x": 4.999, "movie:tmdb-9": 11.999}, false, false, []string{"collections", "titles"}},
	} {
		w := collectionWorld(t, "dry_run = true\n", true)
		db := w.h.Env.Store.DB()
		db.Exec(schema)
		for item, cost := range c.spent {
			db.Exec(`INSERT INTO posters_calls(at, item, of, model, mode, batch_id, state, input_tokens, output_tokens, reserved_usd,
				cost_usd, llm_ms, error) VALUES (?, ?, 'x', 'claude-sonnet-5-5', 'sync', '', 'done', 0, 0, 0, ?, 0, '')`, stamp(w.h.Now), item, cost)
		}
		rep := w.run(t)
		titles, colls := w.verdict(t, "movie:tmdb-1").Rule == "judged", w.verdict(t, "collection:scary movie").Rule == "judged"
		var named []string
		for _, a := range rep.Alerts {
			for _, b := range []string{"collections", "titles"} {
				if strings.Contains(a, "("+b+")") {
					named = append(named, b)
				}
			}
		}
		slices.Sort(named)
		if titles != c.titles || colls != c.colls || !slices.Equal(named, c.alerts) {
			t.Errorf("%s: titles judged %v (want %v), collections judged %v (want %v), alerts %v (want one per budget %v)",
				c.name, titles, c.titles, colls, c.colls, rep.Alerts, c.alerts)
		}
	}
}

// Catches: a wide photo cropped in its middle, whatever the judge said about where the director stands, which loses a
// director standing at one side; and a poster that is not 2:3.
func TestComposeFramesTheDirector(t *testing.T) {
	photo := image.NewRGBA(image.Rect(0, 0, 1600, 900))
	for y := 0; y < 900; y++ {
		for x := 0; x < 1600; x++ {
			c := color.RGBA{90, 90, 90, 255}
			if x >= 80 && x < 400 && y >= 150 && y < 850 {
				c = color.RGBA{220, 30, 30, 255}
			}
			photo.Set(x, y, c)
		}
	}
	out := compose(photo, []float64{0.05, 0.17, 0.25, 0.94})
	red := 0
	for y := 0; y < out.Bounds().Dy(); y += 5 {
		for x := 0; x < out.Bounds().Dx(); x += 5 {
			if c := out.RGBAAt(x, y); c.R > 150 && c.G < 80 {
				red++
			}
		}
	}
	if out.Bounds() != image.Rect(0, 0, 1000, 1500) || red < 300*200/10 {
		t.Fatalf("poster %v with %d red samples of %d; want 1000x1500 holding the director (about a quarter of it)", out.Bounds(), red, 300*200)
	}
}

// Catches collections judged on the posters task's model, or titles on the collections task's: each task's model is
// configured on its own.
func TestCollectionsJudgeOnTheirTask(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\n", true)
	w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, w.h.Env.Now, llm.Config{Tasks: map[string]llm.Task{
		"posters": {Model: "google/gemma-4-31b-it"}, "collections": {Model: "zai/glm-5.3-flash"}}})
	w.run(t)
	asked := map[string][]string{}
	for _, c := range w.gateway.Calls() {
		collection := c.Messages[0].Content[0].Text != systemPrompt
		asked[c.Model] = append(asked[c.Model], fmt.Sprint(collection))
	}
	if g, z := asked["google/gemma-4-31b-it"], asked["zai/glm-5.3-flash"]; len(g) == 0 || len(z) == 0 || slices.Contains(g, "true") || slices.Contains(z, "false") {
		t.Fatalf("collection calls by model: %v; want titles on gemma and collections on glm only", asked)
	}
}

// Catches: a poster sent to one of the owner's Plex collections because he renamed it to an approved name, or to his
// Jellyfin BoxSet (the one the collections stage gave his poster) under such a name; and a first upload replacing a
// Plex collection poster someone else locked.
func TestOwnersAppCollectionsRefused(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\ncollection_dry_run = false\n", false)
	w.h.Plex.Handle("GET", "/library/sections/4/collections", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"500","title":"Scary Movie"},
 {"ratingKey":"603","title":"Evil Dead"}]}}`)
	w.h.Plex.Handle("GET", "/library/metadata/603", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"603","Field":[{"locked":true,"name":"thumb"}]}]}}`)
	w.jellyfinLists(jellyfinItems + `, {"Id":"jb0","Type":"BoxSet","Name":"Scary Movie"}, {"Id":"jb3","Type":"BoxSet","Name":"Evil Dead"}`)
	w.h.Env.Store.PutJSON(context.Background(), "collections", "poster/jellyfin/jb0", "/config/collection-posters/500.jpg")
	rep := w.run(t)
	var locks []string
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		locks = append(locks, c.RawQuery)
	}
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/500/posters")) + len(w.h.Plex.Calls("POST", "/library/metadata/603/posters")) +
		len(w.h.Jellyfin.Calls("POST", "/Items/jb0/Images/Primary")); n != 0 || len(locks) != 0 {
		t.Fatalf("%d uploads to the owner's Plex 500 or Jellyfin jb0 or the locked Plex 603, locks %v; want none", n, locks)
	}
	if len(w.h.Jellyfin.Calls("POST", "/Items/jb3/Images/Primary")) != 1 || !slices.ContainsFunc(rep.Alerts, func(a string) bool { return strings.Contains(a, "603") }) {
		t.Fatalf("Evil Dead must still reach Jellyfin, and the locked Plex 603 raise an alert; alerts %v", rep.Alerts)
	}
}

// Catches: a collection that left the set (the owner adopted it: it is in his file now) getting a poster from an
// outbox row this run did not ask for: a pending one, or one sent again because an app re-created the collection.
func TestCollectionLeavingTheSetNeverUploads(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\ncollection_dry_run = false\n", false)
	w.jellyfinLists(jellyfinItems + `, {"Id":"jb3","Type":"BoxSet","Name":"Evil Dead"}`) // no Scary Movie in Jellyfin yet
	w.run(t)
	w.jellyfinLists(jellyfinItems + `, {"Id":"jb1","Type":"BoxSet","Name":"Scary Movie"}, {"Id":"jb3","Type":"BoxSet","Name":"Evil Dead"}`)
	os.WriteFile(w.ownerFile, []byte(`[{"ratingKey":"500","title":"Alien","titleSort":null,"members":[{"ids":["tmdb://348"]}]},
 {"ratingKey":"700","title":"Scary Movie","titleSort":null,"members":[{"ids":["tmdb://4247"]}]}]`), 0o644)
	w.h.Plex.Handle("GET", "/library/sections/4/collections", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"6011","title":"Scary Movie"},
 {"ratingKey":"603","title":"Evil Dead"}]}}`)
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t)
	if n, m := len(w.h.Plex.Calls("POST", "/library/metadata/6011/posters")), len(w.h.Jellyfin.Calls("POST", "/Items/jb1/Images/Primary")); n+m != 0 {
		t.Fatalf("Scary Movie, the owner's now: %d uploads to Plex 6011 (re-created), %d to Jellyfin jb1 (pending); want none", n, m)
	}
}

// Catches: a director's poster from a photo the judge scored below director_min_score, or chosen unjudged because it
// was the only candidate though no source vouches for who it shows (a Commons text hit); a director left without a
// poster for that with no alert; and a later run deciding it again from its stored scores without the bar.
func TestDirectorBelowTheBar(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\ncollection_dry_run = false\n", false)
	w.run(t)
	for path, body := range map[string]string{
		"/movie/111/credits": `{"crew":[{"id":99,"name":"Kathryn Bigelow","job":"Director"}]}`,
		"/movie/121/credits": `{"crew":[{"id":98,"name":"Ang Lee","job":"Director"}]}`,
		"/person/98/images":  `{"profiles":[{"file_path":"/blue.jpg","width":1000,"height":1500}]}`,
	} {
		w.h.TMDB.Handle("GET", path, 200, body)
	}
	commons := w.commons
	w.set(func() {
		w.commons = func(rw http.ResponseWriter, r *http.Request) {
			for q, colour := range map[string]string{`"Kathryn Bigelow" filming`: "redwide", `"Ang Lee" filming`: "bluewide"} {
				if strings.HasPrefix(r.URL.Query().Get("gsrsearch"), q) {
					name := strings.Trim(strings.TrimSuffix(q, " filming"), `"`)
					reply(rw, fmt.Sprintf(`{"query":{"pages":[{"title":"File:%s filming.jpg","imageinfo":[{"url":"x","thumburl":"%s/t/p/c/500px-%s.jpg",
 "mime":"image/jpeg","width":3000,"height":1500,"extmetadata":{"ImageDescription":{"value":"%s directing"}}}]}]}}`, name, w.api.URL, colour, name))
					return
				}
			}
			commons(rw, r)
		}
	})
	db := w.h.Env.Store.DB()
	db.Exec(`INSERT INTO collections_new(name, sort_title, at) VALUES ('Kathryn Bigelow', 'zzKathryn Bigelow', ''), ('Ang Lee', 'zzAng Lee', '')`)
	db.Exec(`INSERT INTO collections_joins(collection, tmdb, reason, at) VALUES ('Kathryn Bigelow', 111, 'x', ''), ('Ang Lee', 121, 'x', '')`)
	before := w.messages
	rep := w.run(t)
	bigelow, lee := w.verdict(t, "collection:kathryn bigelow"), w.verdict(t, "collection:ang lee")
	var queued int
	db.QueryRow(`SELECT count(*) FROM posters_sinks WHERE item='collection:ang lee'`).Scan(&queued)
	if bigelow.Rule != "judged" || w.messages-before != 2 {
		t.Fatalf("Bigelow's lone Commons text hit: rule %q after %d judge calls; it must be judged, not taken unseen", bigelow.Rule, w.messages-before)
	}
	if lee.Choice != "" || queued != 0 || !slices.ContainsFunc(rep.Alerts, func(a string) bool { return strings.Contains(a, "Ang Lee") }) {
		t.Fatalf("Ang Lee's best photo scored 3: choice %q, %d outbox rows, alerts %v; want no poster and an alert naming him", lee.Choice, queued, rep.Alerts)
	}
	w.run(t)
	if lee = w.verdict(t, "collection:ang lee"); lee.Choice != "" || lee.Rule != "below_bar" {
		t.Fatalf("Ang Lee on the next run: rule %q choice %q; his below-bar verdict must stand", lee.Rule, lee.Choice)
	}
}

// Catches: the owner's-taste backfill missing a title or franchise judged under the older prompts (its stored verdict
// redecided instead), paying again for a director (his prompt has not changed), paying again on every later run (the
// verdict saved without the prompts it was judged under), or calling for a pool no rule needs a judge for.
func TestOlderPromptsJudgedAgain(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\n", true)
	w.h.TMDB.Handle("GET", "/movie/111/credits", 200, `{"crew":[{"id":99,"name":"Kathryn Bigelow","job":"Director"}]}`)
	w.set(func() {
		commons := w.commons
		w.commons = func(rw http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Query().Get("gsrsearch"), `"Kathryn Bigelow" filming`) {
				reply(rw, `{"query":{"pages":[{"title":"File:Kathryn Bigelow filming.jpg","imageinfo":[{"url":"x","thumburl":"`+w.api.URL+
					`/t/p/c/500px-redwide.jpg","mime":"image/jpeg","width":3000,"height":1500,"extmetadata":{}}]}]}}`)
				return
			}
			commons(rw, r)
		}
	})
	asked := func() []string {
		var heads []string
		for _, c := range w.gateway.Calls() {
			heads = append(heads, strings.SplitN(c.Messages[len(c.Messages)-1].Content[0].Text, ":", 2)[1][1:4])
		}
		slices.Sort(heads)
		w.gateway = llmtest.New(w.gateway.Reply)
		w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, w.h.Env.Now, llm.Config{})
		return heads
	}
	w.run(t)
	db := w.h.Env.Store.DB()
	db.Exec(`INSERT INTO collections_new(name, sort_title, at) VALUES ('Kathryn Bigelow', 'zzKathryn Bigelow', '')`)
	db.Exec(`INSERT INTO collections_joins(collection, tmdb, reason, at) VALUES ('Kathryn Bigelow', 111, 'x', '')`)
	w.run(t)
	if got := asked(); !slices.Equal(got, []string{"Alp", "Gam", "Kat", "Sca"}) {
		t.Fatalf("judged %v; want Alpha, Gamma, Kathryn Bigelow and Scary Movie", got)
	}
	db.Exec(`UPDATE posters_items SET verdict = json_remove(verdict, '$.prompt') WHERE verdict IS NOT NULL`) // as saved before
	w.run(t)
	if got := asked(); !slices.Equal(got, []string{"Alp", "Gam", "Sca"}) {
		t.Fatalf("verdicts from the older prompts: judged %v again; want Alpha, Gamma and Scary Movie, not the director", got)
	}
	w.run(t)
	if got := asked(); len(got) != 0 {
		t.Fatalf("a third run judged %v again; want nothing", got)
	}
}

// Catches: a collections file that cannot be read failing the whole run, so no title is judged either.
func TestBadCollectionFilesSkipOnlyCollections(t *testing.T) {
	w := newWorld(t, "dry_run = true\ncollections_owner_file = \"/nonexistent/owner.json\"\ncollections_approved_file = \"/nonexistent/approved.json\"\n")
	rep := w.run(t)
	if w.verdict(t, "movie:tmdb-1").Rule != "judged" || len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "owner.json") {
		t.Fatalf("Alpha rule %q, alerts %v; want titles judged and one alert naming the unreadable file", w.verdict(t, "movie:tmdb-1").Rule, rep.Alerts)
	}
}

// Catches: a 429's Retry-After pausing only the request that got it, while the run's other Wikimedia requests go on
// at the usual pace and are refused in turn.
func TestRetryAfterPausesEveryWikimediaRequest(t *testing.T) {
	h := fake.NewEnv(t, nil)
	pace := WikimediaPace
	t.Cleanup(func() { WikimediaPace = pace })
	WikimediaPace = func(bool) time.Duration { return 6500 * time.Millisecond }
	var at []time.Duration // when each request reached the server, on the fake clock
	start := h.Now
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		at = append(at, h.Now.Sub(start))
		if strings.HasSuffix(r.URL.Path, "/a.jpg") {
			rw.Header().Set("Retry-After", "30")
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.Write(poster("red"))
	}))
	t.Cleanup(srv.Close)
	to, _ := url.Parse(srv.URL)
	r := &run{env: h.Env, http: &http.Client{Transport: rewrite{to}}}
	sleep := wikiSleep
	t.Cleanup(func() { wikiSleep = sleep })
	wikiSleep = func(_ context.Context, d time.Duration) error { h.Now = h.Now.Add(d); return nil }
	if _, err := r.fetch(context.Background(), "https://upload.wikimedia.org/a.jpg"); err == nil {
		t.Fatal("a.jpg answered 429 every time; want an error")
	}
	if _, err := r.fetch(context.Background(), "https://upload.wikimedia.org/b.jpg"); err != nil {
		t.Fatal(err)
	}
	if n := len(at); n != 4 || at[3]-at[2] < 30*time.Second {
		t.Fatalf("requests at %v; b.jpg must wait out a.jpg's last Retry-After (30 s), not just the 6.5 s pace", at)
	}
}

// rewrite sends every request to one test server, whatever host it names.
type rewrite struct{ to *url.URL }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rw.to.Scheme, rw.to.Host
	return http.DefaultTransport.RoundTrip(r)
}
