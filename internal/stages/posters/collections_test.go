package posters

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
)

func init() { CommonsPace = func(bool) time.Duration { return 0 } }

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
 "belongs_to_collection":{"id":4246,"name":"Scary Movie Collection"}}`,
		"/collection/4246/images": `{"posters":[` + img("red", "en") + `,` + img("yellow", "de") + `]}`,
		"/movie/764": `{"id":764,"title":"The Evil Dead","original_language":"en","vote_average":7.3,"vote_count":3000,
 "belongs_to_collection":{"id":1960,"name":"Evil Dead Collection"}}`,
		"/movie/764/images":           `{"posters":[` + img("green", "en") + `]}`,
		"/movie/2502/credits":         `{"crew":[{"id":25598,"name":"Paul Greengrass","job":"Director"},{"id":7,"name":"Tony Gilroy","job":"Writer"}]}`,
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
		return fmt.Sprintf(`{"title":%q,"imageinfo":[{"url":"x","thumburl":"%s/t/p/1920px-/%s.jpg","mime":"image/jpeg","width":%d,"height":%d,
 "extmetadata":{"ImageDescription":{"value":%q},"LicenseShortName":{"value":"CC BY-SA 4.0"}}}]}`, title, w.api.URL, colour, wd, ht, desc)
	}
	w.set(func() {
		w.commons = func(rw http.ResponseWriter, r *http.Request) {
			q, pages := r.URL.Query(), ""
			switch search := q.Get("gsrsearch"); {
			case q.Get("prop") == "pageprops" && q.Get("titles") == "Category:Paul Greengrass":
				pages = `{"pageprops":{"wikibase_item":"Q356275"}}`
			case strings.HasPrefix(search, "haswbstatement:P180=Q356275"):
				pages = file("File:Paul Greengrass filming United 93.jpg", "redwide", 3000, 1500, "<b>Paul Greengrass</b> on the set") + "," +
					file("File:Greengrass small.jpg", "yellow", 300, 200, "Paul Greengrass")
			case strings.HasPrefix(search, `incategory:"Paul Greengrass"`):
				pages = file("File:Greengrass at TIFF.jpg", "green", 2000, 3000, "")
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
	for _, rk := range []string{"500", "601", "602", "603"} {
		w.h.Plex.Handle("POST", "/library/metadata/"+rk+"/posters", 200, "")
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
// a Commons file kept though it is tiny or never names the director; a collection poster sent to the wrong app item
// or left unlocked in Plex; a director's poster not composed to 2:3 around the judge's box; a collection opened by an
// arrival, or made in an app after the first listing, never getting its poster; and a rerun paying or uploading again.
func TestCollectionPosters(t *testing.T) {
	w := collectionWorld(t, "", false)
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
	w.run(t)
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
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/500/posters")) + len(w.h.Jellyfin.Calls("POST", "/Items/jb0/Images/Primary")); n != 0 ||
		w.pool("collection:alien") != nil || w.verdict(t, "collection:alien").Rule != "" {
		t.Fatalf("the owner's collection Alien got %d uploads or a pool; his posters are never touched", n)
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
	if want := []string{"commons redwide.jpg", "tmdb-profile blue.jpg", "commons green.jpg"}; !slices.Equal(photos, want) {
		t.Fatalf("Greengrass pool %v, want %v: on-set Commons first, turns with TMDB, no tiny file, no text hit that never names him", photos, want)
	}
	jf := w.h.Jellyfin.Calls("POST", "/Items/jb2/Images/Primary")
	if len(jf) != 1 {
		t.Fatalf("Jellyfin BoxSet jb2 got %d uploads, want 1", len(jf))
	}
	body, _ := base64.StdEncoding.DecodeString(string(jf[0].Body))
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(body)); err != nil || cfg.Width != posterW || cfg.Height != posterH || scoreOf(body) != 9 {
		t.Fatalf("Greengrass poster %dx%d (err %v), score %d; want %dx%d around the red director in the photo's left quarter",
			cfg.Width, cfg.Height, err, scoreOf(body), posterW, posterH)
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
}

// Catches: collection judging stopped by what the titles spent, titles stopped by what collections spent, and
// collection calls past their own budget.
func TestCollectionBudgetApart(t *testing.T) {
	for _, c := range []struct {
		name, item    string
		cost          float64
		titles, colls bool // judged this run
		alertMentions string
	}{{"collections spent", "collection:x", 4.999, true, false, "collections"}, {"titles spent", "movie:tmdb-9", 11.999, false, true, "titles"}} {
		w := collectionWorld(t, "dry_run = true\n", true)
		db := w.h.Env.Store.DB()
		db.Exec(schema)
		db.Exec(`INSERT INTO posters_calls(at, item, of, model, mode, batch_id, state, input_tokens, output_tokens, reserved_usd,
			cost_usd, llm_ms, error) VALUES (?, ?, 'x', 'claude-sonnet-5-5', 'sync', '', 'done', 0, 0, 0, ?, 0, '')`, stamp(w.h.Now), c.item, c.cost)
		rep := w.run(t)
		titles, colls := w.verdict(t, "movie:tmdb-1").Rule == "judged", w.verdict(t, "collection:scary movie").Rule == "judged"
		if titles != c.titles || colls != c.colls || len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], c.alertMentions) {
			t.Errorf("%s: titles judged %v (want %v), collections judged %v (want %v), alerts %v", c.name, titles, c.titles, colls, c.colls, rep.Alerts)
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
	if out.Bounds() != image.Rect(0, 0, posterW, posterH) || red < 300*200/10 {
		t.Fatalf("poster %v with %d red samples of %d; want %dx%d holding the director (about a quarter of it)", out.Bounds(), red, 300*200, posterW, posterH)
	}
}
