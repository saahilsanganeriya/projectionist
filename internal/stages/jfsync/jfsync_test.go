package jfsync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
	"projectionist/internal/watch"
)

const (
	movies = "/lib/Movies/"
	heat   = movies + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv"
	ran    = movies + "Ran (1985) {tmdb-11645}/Ran (1985) {tmdb-11645}.mkv"
	alien  = movies + "Alien (1979) {tmdb-348}/Alien (1979) {tmdb-348} - 2160p.mkv" // not in Jellyfin yet
	clerks = movies + "Clerks (1994) {tmdb-2292}/Clerks (1994) {tmdb-2292}.mkv"
	dune   = movies + "Dune (2021) {tmdb-438631}/Dune (2021) {tmdb-438631}.mkv"
	fargo  = movies + "Fargo (1996) {tmdb-275}/Fargo (1996) {tmdb-275}.mkv"
	wire   = "/lib/TV/The Wire (2002) {tmdb-1438}/Season 01/The Wire (2002) - S01E01-E02 - The Target.mkv"
)

// writeBackup makes a Plex DB backup copy holding the given watch rows: account, metadata id, index, file,
// view count, view offset (ms), last viewed (unix s).
func writeBackup(t *testing.T, path string, rows [][]any) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE accounts (id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, guid TEXT, metadata_type INTEGER, library_section_id INTEGER, "index" INTEGER, deleted_at INTEGER);
		CREATE TABLE metadata_item_settings (id INTEGER PRIMARY KEY, account_id INTEGER, guid TEXT, rating REAL, view_offset INTEGER, view_count INTEGER, last_viewed_at INTEGER, updated_at INTEGER);
		CREATE TABLE media_items (id INTEGER PRIMARY KEY, metadata_item_id INTEGER, deleted_at INTEGER);
		CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT);
		INSERT INTO accounts VALUES (1, 'owner-plex'), (578, 'viewer-b'), (579, 'viewer-b-tv')`)
	for _, r := range rows {
		typ, section := 1, 4
		if strings.Contains(r[3].(string), "/TV/") {
			typ, section = 4, 2
		}
		guid := "plex://x/" + r[3].(string)
		exec(`INSERT OR IGNORE INTO metadata_items VALUES (?, ?, ?, ?, ?, NULL)`, r[1], guid, typ, section, r[2])
		exec(`INSERT OR IGNORE INTO media_items VALUES (?, ?, NULL)`, r[1], r[1])
		exec(`INSERT OR IGNORE INTO media_parts VALUES (?, ?, ?)`, r[1], r[1], r[3])
		exec(`INSERT INTO metadata_item_settings (account_id, guid, view_count, view_offset, last_viewed_at, updated_at)
			VALUES ((SELECT id FROM accounts WHERE name = ?), ?, ?, ?, ?, ?)`, r[0], guid, r[4], r[5], r[6], r[6])
	}
}

// writes records the apps' write calls (and anything a test adds) in one order across both apps.
type writes struct {
	sync.Mutex
	calls  []string                       // "<owner|friend> <path?query>" for Plex, "<path?query> <json>" for Jellyfin
	status map[string]int                 // path → the status a write to it answers (default 200)
	during map[string]func(*http.Request) // path → called while a write to it is being answered
}

func (w *writes) add(rw http.ResponseWriter, r *http.Request, call string) {
	w.Lock()
	w.calls = append(w.calls, call)
	status, during := w.status[r.URL.Path], w.during[r.URL.Path]
	w.Unlock()
	if during != nil {
		during(r)
	}
	if status != 0 {
		rw.WriteHeader(status)
	}
}

func (w *writes) since(n int) []string {
	w.Lock()
	defer w.Unlock()
	return append([]string(nil), w.calls[n:]...)
}

// plexPages answers a Plex section listing with the page its query asks for.
func plexPages(items ...string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
		start = min(start, len(items))
		rw.Write([]byte(`{"MediaContainer":{"Metadata":[` + strings.Join(items[start:min(start+size, len(items))], ",") + `]}}`))
	}
}

// newHarness: Plex holds Heat (re-created after the backup: ratingKey 13, was 10), Ran, Alien, Clerks, Dune, Fargo
// and both episodes of a two-episode Wire file; Jellyfin holds all but Alien, the Wire file as one item. History:
// owner watched Heat (twice), Alien, Clerks, Dune, Fargo in Plex, is 30 min into Ran in Jellyfin and 20 min into a
// rewatch of Clerks there; viewer-b watched Wire E02 in Plex and Ran in Jellyfin, rating it 7.
func newHarness(t *testing.T) (h *fake.Harness, w *writes, options, backups string) {
	h = fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	dir := t.TempDir()
	backups = filepath.Join(dir, "backups")
	os.MkdirAll(backups, 0o755)
	T1, T2 := time.Date(2024, 8, 9, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	writeBackup(t, filepath.Join(backups, "pre-x.db"), nil) // older copy: ignored
	writeBackup(t, filepath.Join(backups, "pre-y.db"), [][]any{
		{"owner-plex", 10, 0, heat, 2, 0, T1}, {"owner-plex", 12, 0, alien, 1, 0, T1},
		{"owner-plex", 14, 0, clerks, 1, 0, T1}, {"owner-plex", 15, 0, dune, 1, 0, T1},
		{"owner-plex", 16, 0, fargo, 1, 0, T1},
		{"viewer-b", 21, 2, wire, 1, 0, T2},
	})
	os.Chtimes(filepath.Join(backups, "pre-x.db"), time.Unix(T1, 0), time.Unix(T1, 0))
	os.Chtimes(filepath.Join(backups, "pre-y.db"), h.Now.Add(-10*time.Minute), h.Now.Add(-10*time.Minute))
	options = `plex_backups = "` + backups + `"
plex_tv = "` + h.Plex.URL + `"
plan_file = "` + filepath.Join(dir, "plan.tsv") + `"
users = [{ name = "owner", plex = "owner-plex", jellyfin = "owner" }, { name = "viewer-b", plex = "viewer-b", jellyfin = "viewer-b" }]
`
	h.SetOptions(options)

	listPage = 4 // the movie listing takes two pages
	part := func(rk string, index int, file string) string {
		return `{"ratingKey":"` + rk + `","index":` + string(rune('0'+index)) + `,"Media":[{"Part":[{"file":"` + file + `"}]}]}`
	}
	h.Plex.Handle("GET", "/accounts", 200, `{"MediaContainer":{"Account":[{"id":1,"name":"owner-plex"},{"id":578,"name":"viewer-b"},{"id":9,"name":""}]}}`)
	h.Plex.HandleFunc("GET", "/library/sections/4/all", plexPages(part("13", 0, heat), part("11", 0, ran), part("12", 0, alien),
		part("14", 0, clerks), part("15", 0, dune), part("16", 0, fargo)))
	h.Plex.HandleFunc("GET", "/library/sections/2/all", plexPages(part("20", 1, wire), part("21", 2, wire)))
	h.Plex.Handle("GET", "/api/v2/user/webhooks", 200, `<webhooks><webhook url="http://tautulli:8181/hook"/></webhooks>`)
	h.Plex.Handle("POST", "/api/v2/user/webhooks", 201, ``)
	h.Plex.Handle("GET", "/api/servers/mid/shared_servers", 200,
		`<MediaContainer><SharedServer userID="578" accessToken="`+fake.PlexFriendToken+`"/><SharedServer userID="9" accessToken="other"/></MediaContainer>`)
	w = &writes{status: map[string]int{}, during: map[string]func(*http.Request){}}
	for _, p := range []string{"GET /:/scrobble", "GET /:/unscrobble", "GET /:/progress", "PUT /:/rate"} {
		method, path, _ := strings.Cut(p, " ")
		h.Plex.HandleFunc(method, path, func(rw http.ResponseWriter, r *http.Request) {
			who := "owner"
			if r.Header.Get("X-Plex-Token") == fake.PlexFriendToken {
				who = "friend"
			}
			w.add(rw, r, who+" "+r.URL.Path+"?"+r.URL.RawQuery)
		})
	}

	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"owner","Id":"u1"},{"Name":"viewer-b","Id":"u2"}]`)
	// Like Jellyfin 12.1 on the NAS, user-scoped item queries leave items out; each item's own UserData is right.
	h.Jellyfin.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("userId") != "" {
			rw.Write([]byte(`{"Items":[]}`))
			return
		}
		rw.Write([]byte(`{"Items":[{"Id":"a1","Path":"` + heat + `"},{"Id":"b1","Path":"` + ran + `"},{"Id":"c1","Path":"` +
			clerks + `"},{"Id":"d1","Path":"` + dune + `"},{"Id":"e1","Path":"` + wire + `"},{"Id":"f1","Path":"` + fargo +
			`"},{"Id":"x1","Path":"/lib/Home Videos/x.mkv"}]}`))
	})
	userData := map[string]string{
		"u1/b1": `"PlaybackPositionTicks":18000000000,"LastPlayedDate":"2025-01-01T00:00:00.0000000Z"`,
		"u1/c1": `"PlaybackPositionTicks":12000000000,"LastPlayedDate":"2025-03-01T00:00:00Z"`,
		"u2/b1": `"Played":true,"PlayCount":1,"Rating":7,"LastPlayedDate":"2025-02-01T00:00:00Z"`,
	}
	for _, id := range []string{"a1", "b1", "c1", "d1", "e1", "f1", "a2"} { // a2: Heat, if Jellyfin re-creates it
		h.Jellyfin.HandleFunc("GET", "/UserItems/"+id+"/UserData", func(rw http.ResponseWriter, r *http.Request) {
			rw.Write([]byte(`{` + userData[r.URL.Query().Get("userId")+"/"+id] + `}`))
		})
		h.Jellyfin.HandleFunc("POST", "/UserItems/"+id+"/UserData", func(rw http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.add(rw, r, r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		})
	}
	return h, w, options, backups
}

func run(t *testing.T, h *fake.Harness) *stage.Report {
	t.Helper()
	rep := &stage.Report{}
	if err := (Stage{}).Run(context.Background(), h.Env, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// hookRequest is what a Plex (multipart, a "payload" field) or a Jellyfin (JSON) webhook posts.
func hookRequest(app, payload string) *http.Request {
	if app != plexApp {
		return httptest.NewRequest("POST", "/hooks/"+app, strings.NewReader(payload))
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("payload", payload)
	mw.Close()
	req := httptest.NewRequest("POST", "/hooks/plex", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// push sends a webhook to the stage and returns the status it answered.
func push(h *fake.Harness, app, payload string) int {
	rec := httptest.NewRecorder()
	(Stage{}).Hooks(h.Env)["POST /hooks/"+app].ServeHTTP(rec, hookRequest(app, payload))
	return rec.Code
}

func plexPlay(event, account, key, extra string) string {
	return `{"event":"` + event + `","Account":{"title":"` + account + `"},"Metadata":{"ratingKey":"` + key +
		`","type":"movie","librarySectionID":4` + extra + `}}`
}

// Catches: a dry run that writes; a state written back to the app it came from (Heat's re-created Plex item would
// be scrobbled again, adding a play); the Plex history not counted as held (Clerks would be scrobbled again); a
// field an app never held left out because the newest change came from that app (Clerks' Plex play never reaching
// Jellyfin, where he stopped a rewatch); what Plex holds lost when it re-creates an item under a new id (a Jellyfin
// stop on Heat would scrobble 13, a view Plex's API cannot remove); a friend's write sent with the owner's token; an existing plex.tv webhook dropped when ours is added; a rerun that
// writes again; a pushed play not reaching the other app, or not counted as held by the app that pushed it; the
// owner's pushed play lost because its payload names him by his plex.tv title; a state that knows only a resume
// point sent to Jellyfin as played=false, playcount=0 (it would unmark a play there); a Plex listing that reads only
// its first page; and a rating sent to Jellyfin with the play it already holds, or setting LastPlayedDate to the time of
// the rating rather than of the play (it reorders "recently played").
func TestRunTwoWay(t *testing.T) {
	h, w, options, _ := newHarness(t)
	h.SetOptions(options + "apply = false\n")
	rep := run(t, h)
	if len(w.since(0)) != 0 || len(h.Plex.Calls("POST", "/api/v2/user/webhooks")) != 0 {
		t.Fatalf("dry run wrote: %v", w.since(0))
	}
	if rep.Counts["pending.jellyfin.owner"] != 1 || rep.Counts["to_jellyfin.owner"] != 4 || rep.Counts["to_jellyfin.viewer-b"] != 1 ||
		rep.Counts["to_plex.owner"] != 2 || rep.Counts["to_plex.viewer-b"] != 1 || len(rep.Alerts) != 0 {
		t.Fatalf("plan: counts %v alerts %v", rep.Counts, rep.Alerts)
	}

	h.SetOptions(options)
	run(t, h)
	got := strings.Join(w.since(0), "\n")
	for _, want := range []string{
		`/UserItems/a1/UserData?userId=u1 {"LastPlayedDate":"2024-08-09T00:00:00Z","PlayCount":2,"PlaybackPositionTicks":0,"Played":true}`,
		`/UserItems/e1/UserData?userId=u2 {"LastPlayedDate":"2024-09-01T00:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}`,
		`/UserItems/c1/UserData?userId=u1 {"LastPlayedDate":"2025-03-01T00:00:00Z","PlayCount":1,"Played":true}`,
		"owner /:/progress?identifier=com.plexapp.plugins.library&key=11&time=1800000&state=stopped",
		"owner /:/progress?identifier=com.plexapp.plugins.library&key=14&time=1200000&state=stopped", // Clerks stays watched
		"friend /:/scrobble?identifier=com.plexapp.plugins.library&key=11",
		"friend /:/rate?identifier=com.plexapp.plugins.library&key=11&rating=7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing write %s in\n%s", want, got)
		}
	}
	if n := len(w.since(0)); n != 9 || strings.Contains(got, "key=13") || strings.Contains(got, "scrobble?identifier=com.plexapp.plugins.library&key=14") {
		t.Errorf("%d writes:\n%s", n, got)
	}
	both := url.Values{"urls[]": {"http://tautulli:8181/hook", "http://127.0.0.1:8097/hooks/plex"}}
	if posts := h.Plex.Calls("POST", "/api/v2/user/webhooks"); len(posts) != 1 || string(posts[0].Body) != both.Encode() {
		t.Errorf("webhook registration %v", posts)
	}

	done := len(w.since(0))
	if run(t, h); len(w.since(done)) != 0 {
		t.Fatalf("rerun wrote %v", w.since(done))
	}

	h.Now = h.Now.Add(time.Hour)
	// The owner's payload carries his plex.tv title, not his account name on the server: the id maps him.
	push(h, plexApp, strings.Replace(plexPlay("media.scrobble", "Owner S", "11", ""), `"title"`, `"id":1,"title"`, 1))
	push(h, plexApp, plexPlay("media.stop", "viewer-b", "15", `,"viewOffset":600000,"duration":6000000`))
	run(t, h)
	if got := strings.Join(w.since(done), "\n"); got != `/UserItems/b1/UserData?userId=u1 {"LastPlayedDate":"2026-10-05T07:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}`+"\n"+
		`/UserItems/d1/UserData?userId=u2 {"LastPlayedDate":"2026-10-05T07:00:00Z","PlaybackPositionTicks":6000000000}` {
		t.Fatalf("after the Plex play and viewer-b's stop:\n%s", got)
	}

	// Rewatches stopped 10 min in on Jellyfin: Plex holds both plays (Ran's it pushed; Heat's from the backup, under
	// the ratingKey Plex has since replaced), so only the positions go.
	h.Now = h.Now.Add(10 * time.Minute)
	for _, id := range []string{"b1", "a1"} {
		push(h, jellyfinApp, `{"NotificationType":"UserDataSaved","NotificationUsername":"owner","ItemId":"`+id+`","ItemType":"Movie","SaveReason":"PlaybackFinished","Played":true,"PlayCount":1,"PlaybackPositionTicks":6000000000}`)
	}
	run(t, h)
	if got := strings.Join(w.since(done+2), "\n"); got != "owner /:/progress?identifier=com.plexapp.plugins.library&key=11&time=600000&state=stopped\n"+
		"owner /:/progress?identifier=com.plexapp.plugins.library&key=13&time=600000&state=stopped" {
		t.Fatalf("after the Jellyfin rewatches:\n%s", got)
	}

	done = len(w.since(0))
	h.Now = h.Now.Add(10 * time.Minute)
	push(h, plexApp, plexPlay("media.rate", "owner-plex", "13", `,"userRating":8`)) // Jellyfin holds the play
	push(h, plexApp, plexPlay("media.scrobble", "viewer-b", "15", ""))
	h.Now = h.Now.Add(time.Minute)
	push(h, plexApp, plexPlay("media.rate", "viewer-b", "15", `,"userRating":6`))
	run(t, h)
	if got := strings.Join(w.since(done), "\n"); got != `/UserItems/a1/UserData?userId=u1 {"Rating":8}`+"\n"+
		`/UserItems/d1/UserData?userId=u2 {"LastPlayedDate":"2026-10-05T07:20:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true,"Rating":6}` {
		t.Fatalf("after the ratings:\n%s", got)
	}
}

// Catches: a push that crosses this stage's write to the same field lost for good. Owner marks Heat unplayed in
// Jellyfin while the run writes Heat's Plex play there; the next run would skip the item (its newest state came
// from Jellyfin) and both apps would keep the play. Likewise viewer-b's Plex stop of a Ran rewatch crossing the
// stage's scrobble, which clears the resume point. Also: Heat's re-created Plex item, which nothing is known to
// hold, not unscrobbled.
func TestPushCrossingAWrite(t *testing.T) {
	h, w, _, _ := newHarness(t)
	code := 0
	w.during["/UserItems/a1/UserData"] = func(*http.Request) {
		code = push(h, jellyfinApp, `{"NotificationType":"UserDataSaved","NotificationUsername":"owner","ItemId":"a1","ItemType":"Movie","SaveReason":"TogglePlayed","Played":false}`)
	}
	w.during["/:/scrobble"] = func(*http.Request) {
		push(h, plexApp, plexPlay("media.stop", "viewer-b", "11", `,"viewOffset":600000,"duration":6000000`))
	}
	run(t, h)
	w.during = map[string]func(*http.Request){}
	done := len(w.since(0))
	h.Now = h.Now.Add(5 * time.Minute)
	run(t, h)
	got := strings.Join(w.since(done), "\n")
	if code != http.StatusNoContent || !strings.Contains(got, `/UserItems/a1/UserData?userId=u1 {"PlayCount":0,"Played":false}`) ||
		!strings.Contains(got, "owner /:/unscrobble?identifier=com.plexapp.plugins.library&key=13") ||
		!strings.Contains(got, "friend /:/progress?identifier=com.plexapp.plugins.library&key=11&time=600000&state=stopped") {
		t.Fatalf("after the crossing push (hook %d):\n%s", code, got)
	}
	done = len(w.since(0))
	if run(t, h); len(w.since(done)) != 0 {
		t.Fatalf("third run wrote %v", w.since(done))
	}
}

// Catches: Plex's echo of this stage's own write read as a push that crossed it. PUT /:/rate comes back as media.rate
// (as any client's rating does); the sink then forgot what Plex holds, and every later run scrobbled Ran again for
// viewer-b (a view nobody can remove) and rated it again, which echoed again. Also: a real Plex scrobble crossing the
// stage's progress write (owner) or its scrobble (viewer-b; Plex's play count is nobody's to set) making it forget the
// play Plex then holds, so the next run scrobbles again; and viewer-b's own rating crossing the stage's lost, though
// Plex may hold either.
func TestPlexPushesCrossingWrites(t *testing.T) {
	h, w, _, _ := newHarness(t)
	first := map[string]bool{"/:/progress": true, "/:/scrobble": true, "/:/rate": true}
	cross := func(r *http.Request, account, event, extra string) { // once per path, on Ran
		if first[r.URL.Path] && r.URL.Query().Get("key") == "11" {
			first[r.URL.Path] = false
			push(h, plexApp, plexPlay(event, account, "11", extra))
		}
	}
	w.during["/:/progress"] = func(r *http.Request) { cross(r, "owner-plex", "media.scrobble", "") }
	w.during["/:/scrobble"] = func(r *http.Request) { cross(r, "viewer-b", "media.scrobble", "") }
	w.during["/:/rate"] = func(r *http.Request) { // viewer-b's is the only rating the stage writes
		push(h, plexApp, plexPlay("media.rate", "viewer-b", "11", `,"userRating":`+r.URL.Query().Get("rating"))) // the echo
		cross(r, "viewer-b", "media.rate", `,"userRating":9`)
	}
	for range 4 {
		run(t, h)
		h.Now = h.Now.Add(5 * time.Minute)
	}
	got := strings.Join(w.since(0), "\n")
	for call, want := range map[string]int{"friend /:/scrobble?identifier=com.plexapp.plugins.library&key=11": 1,
		"friend /:/rate": 2, "key=11&rating=9": 1, "owner /:/scrobble": 0,
		"owner /:/progress?identifier=com.plexapp.plugins.library&key=11&time=0&": 1} {
		if n := strings.Count(got, call); n != want {
			t.Errorf("%q sent %d times in 4 runs, want %d", call, n, want)
		}
	}
}

// Catches: what Jellyfin holds kept by library item, so Heat re-created by Jellyfin under a new id without its user
// data (a renamed folder of an item it had not identified) is never written again and the apps diverge silently.
func TestJellyfinRecreatedItemIsWritten(t *testing.T) {
	h, w, _, _ := newHarness(t)
	run(t, h)
	h.Jellyfin.Handle("GET", "/Items", 200, `{"Items":[{"Id":"a2","Path":"`+heat+`"}]}`)
	h.Now = h.Now.Add(25 * time.Hour) // the run relists after its writes
	run(t, h)
	done := len(w.since(0))
	run(t, h)
	if got := strings.Join(w.since(done), "\n"); got != `/UserItems/a2/UserData?userId=u1 {"LastPlayedDate":"2024-08-09T00:00:00Z","PlayCount":2,"PlaybackPositionTicks":0,"Played":true}` {
		t.Fatalf("after Jellyfin re-created Heat:\n%s", got)
	}
}

// Catches: the Jellyfin listing left to collapse collections. Jellyfin then answers each movie in a collection with
// the collection, whose path is outside the library, so every watched film on a shelf stayed pending and was never
// written (2026-10-06: 394 films, 490 person-items, all in Plex's listing and in Jellyfin).
func TestJellyfinListsMoviesInCollections(t *testing.T) {
	h, w, _, _ := newHarness(t)
	h.Jellyfin.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		item := `{"Id":"d1","Path":"` + dune + `"}`
		if r.URL.Query().Get("collapseBoxSetItems") != "false" { // Jellyfin's default for a movie query
			item = `{"Id":"s1","Path":"/config/data/collections/Dune [boxset]"}`
		}
		rw.Write([]byte(`{"Items":[` + item + `]}`))
	})
	run(t, h)
	want := `/UserItems/d1/UserData?userId=u1 {"LastPlayedDate":"2024-08-09T00:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}`
	if got := strings.Join(w.since(0), "\n"); !strings.Contains(got, want) {
		t.Fatalf("Dune, in a Jellyfin collection, not written:\n%s", got)
	}
}

// Catches: a webhook entry of another shape read as no entry, so the POST adding ours, which replaces the whole list,
// would drop the owner's other hooks every day; a document of another kind read as an empty list; and the real empty
// list (plex.tv's answer on 2026-10-05) refused.
func TestWebhookList(t *testing.T) {
	for raw, want := range map[string]string{
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<webhooks>\n</webhooks>\n":       "",
		`<webhooks><webhook url="http://a/1"/><webhook url="http://b/2"/></webhooks>`: "http://a/1 http://b/2",
		`<webhooks><webhook><url>http://a/1</url></webhook></webhooks>`:               "error: unrecognised entry <webhook>",
		`<webhooks><hook url="http://a/1"/></webhooks>`:                               "error: unrecognised entry <hook>",
		`<html></html>`: "error: root <html>",
	} {
		urls, err := webhookURLs([]byte(raw))
		got := strings.Join(urls, " ")
		if err != nil {
			got = "error: " + strings.TrimPrefix(err.Error(), "plex.tv webhooks: ")
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}

// Catches: a person configured with no Plex account (plex = "") written as the server's unnamed account, which the
// NAS has (their watch state would land in another account), or alerted about on every run; a Plex name absent from the backup copy (an account
// added since it was taken) marked imported without an alert, so its Plex writes would replay plays Plex holds,
// adding views its API cannot remove; and an import of one account standing for another configured later.
func TestPlexAccountConfig(t *testing.T) {
	h, _, options, _ := newHarness(t)
	for _, c := range []struct {
		plex, alert      string
		imported, writes int
	}{
		{`""`, "", 5, 0}, // owner's five films import
		{`"Viewer-b R"`, `"Viewer-b R" (users entry "viewer-b") is not in the backup copy`, 0, 0},
		{`"viewer-b-tv"`, `plex has no account "viewer-b-tv"`, 0, 0}, // in the copy, with no plays
		{`"viewer-b"`, "", 1, 1}, // the Wire episode he finished in Plex; Ran, finished in Jellyfin, goes to Plex
	} {
		h.SetOptions(strings.Replace(options, `plex = "viewer-b"`, "plex = "+c.plex, 1))
		rep := run(t, h)
		alerts := strings.Join(rep.Alerts, "|")
		if rep.Counts["imported.plex"] != c.imported || rep.Counts["to_plex.viewer-b"] != c.writes ||
			!strings.Contains(alerts, c.alert) || (c.alert == "") != (alerts == "") {
			t.Errorf("plex = %s: counts %v alerts %q", c.plex, rep.Counts, rep.Alerts)
		}
	}
}

// Catches: a failing app failing the run, a failed item retried before its backoff, a down app being sent its
// whole backlog in one run, and a Plex write that failed part-way repeating the scrobble that went through.
func TestWriteFailuresBackOff(t *testing.T) {
	h, w, _, _ := newHarness(t)
	for _, id := range []string{"a1", "b1", "c1", "d1", "e1", "f1"} {
		w.status["/UserItems/"+id+"/UserData"] = 500
	}
	w.status["/:/rate"] = 500
	count := func(calls []string, s string) int { return strings.Count(strings.Join(calls, "\n"), s) }
	rep := run(t, h)
	if calls := w.since(0); count(calls, "/UserItems/") != maxFailRun || rep.Counts["deferred.jellyfin"] != 2 ||
		count(calls, "friend /:/") != 2 {
		t.Fatalf("first run: %v, counts %v", calls, rep.Counts)
	}
	seen := len(w.since(0))
	if run(t, h); count(w.since(seen), "/UserItems/") != 2 || count(w.since(seen), "friend") != 0 { // only the items never tried
		t.Fatalf("second run retried early: %v", w.since(seen))
	}
	seen = len(w.since(0))
	h.Now = h.Now.Add(6 * time.Minute)
	if run(t, h); count(w.since(seen), "/UserItems/") != maxFailRun || count(w.since(seen), "friend /:/rate") != 1 ||
		count(w.since(seen), "scrobble") != 0 {
		t.Fatalf("after the backoff: %v", w.since(seen))
	}
}

// Catches: a due listing run before the writes (a whole-library Plex listing takes minutes and would hold up every
// Jellyfin write); an app listed more than once a day (the sources rule allows one listing per app per day), for an
// item added since or to retry a failed listing; and a failed listing raising no alert.
func TestListingsFollowTheWrites(t *testing.T) {
	h, w, _, _ := newHarness(t)
	run(t, h)
	h.Plex.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, r *http.Request) {
		w.add(rw, r, "list plex")
		rw.WriteHeader(500)
	})
	for i, step := range []struct {
		after     time.Duration
		key, want string // key: a film owner finishes in Plex before the run
	}{
		{time.Hour, "17", ""}, // added since the listing: it waits for the next one
		{23 * time.Hour, "15", "/UserItems/d1/ list plex"},
		{30 * time.Minute, "", ""},
		{24 * time.Hour, "", "list plex"},
	} {
		done := len(w.since(0))
		h.Now = h.Now.Add(step.after)
		if step.key != "" {
			push(h, plexApp, plexPlay("media.scrobble", "owner-plex", step.key, ""))
		}
		rep := run(t, h)
		var got []string
		for _, c := range w.since(done) {
			got = append(got, c[:min(len(c), 14)])
		}
		if strings.Join(got, " ") != step.want || len(rep.Alerts) != strings.Count(step.want, "list plex") {
			t.Errorf("run %d: %v, alerts %q", i+1, w.since(done), rep.Alerts)
		}
	}
}

// Catches: an item removed from Jellyfin since the listing (404) stalling the import; one person's failing import
// holding up the next person's; a stopped or cancelled import restarting from the first item rather than resuming;
// an import walked again (after a new listing) recording its history twice; and a stuck import never alerting.
func TestJellyfinImportResumes(t *testing.T) {
	h, _, _, _ := newHarness(t)
	h.Jellyfin.Handle("GET", "/UserItems/a1/UserData", 404, "") // removed since the listing
	broken := true
	h.Jellyfin.HandleFunc("GET", "/UserItems/c1/UserData", func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("userId") != "u1":
			rw.Write([]byte(`{}`))
		case broken:
			rw.WriteHeader(500)
		default:
			rw.Write([]byte(`{"PlaybackPositionTicks":12000000000}`))
		}
	})
	gone := func() int { return len(h.Jellyfin.Calls("GET", "/UserItems/a1/UserData")) }
	if rep := run(t, h); rep.Counts["waiting.jellyfin.owner"] != 1 || rep.Counts["waiting.jellyfin.viewer-b"] != 0 {
		t.Fatalf("first run: counts %v", rep.Counts)
	}
	for i, after := range []time.Duration{5 * time.Minute, 25 * time.Hour} { // the second ends with a new listing
		h.Now = h.Now.Add(after)
		rep := run(t, h)
		if gone() != 2 || strings.Contains(strings.Join(rep.Alerts, "|"), "owner stops at item c1 (3 runs in a row)") != (i == 1) {
			t.Fatalf("run %d: %d asks for a1 (it resumes at c1), alerts %q", i+2, gone(), rep.Alerts)
		}
	}
	broken = false
	h.Now = h.Now.Add(5 * time.Minute) // the new listing: the walk starts again
	rep := run(t, h)
	var rows int
	h.Env.Store.DB().QueryRow(`SELECT count(*) FROM watch_events WHERE source = 'jellyfin-backfill'`).Scan(&rows)
	if gone() != 3 || rep.Counts["waiting.jellyfin.owner"] != 0 || rows != 3 { // owner's Ran and Clerks, viewer-b's Ran
		t.Fatalf("after the fix: %d asks for a1, %d imported rows, counts %v", gone(), rows, rep.Counts)
	}

	h, _, _, _ = newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.Jellyfin.HandleFunc("GET", "/UserItems/b1/UserData", func(http.ResponseWriter, *http.Request) { cancel() })
	if err := (Stage{}).Run(ctx, h.Env, &stage.Report{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled import returned %v", err)
	}
	if run(t, h); len(h.Jellyfin.Calls("GET", "/UserItems/a1/UserData")) != 2 { // owner's before the cancel, viewer-b's
		t.Errorf("the cancelled import restarted")
	}
}

// Catches: a listing or a history import run while someone other than the owner is watching (on the NAS a
// whole-library Jellyfin listing took 5m44s and the import about 19 min per person); a guest arriving mid-import
// not pausing it, or the paused walk restarting from its first item; and the writes held up with the heavy reads.
func TestHeavyReadsWaitForGuests(t *testing.T) {
	h, w, _, _ := newHarness(t)
	guest := func(on bool) {
		body := `<MediaContainer size="0"></MediaContainer>`
		if on {
			body = `<MediaContainer size="1"><Video type="movie" title="Ran"><User title="viewer-b"/></Video></MediaContainer>`
		}
		h.Plex.Handle("GET", "/status/sessions", 200, body)
	}
	listings := func() int { return len(h.Plex.Calls("GET", "/accounts")) + len(h.Jellyfin.Calls("GET", "/Items")) }
	asked := func(id string) int { // owner's history import asking for one item
		return strings.Count(fmt.Sprint(h.Jellyfin.Calls("GET", "/UserItems/"+id+"/UserData")), "userId=u1")
	}
	h.Plex.Handle("GET", "/status/sessions", 500, "") // who is watching is unknown: someone may be
	if rep := run(t, h); listings() != 0 || len(w.since(0)) != 0 || fmt.Sprint(rep.Counts) != "map[viewer_wait:1]" {
		t.Fatalf("viewers unknown: %d listing requests, writes %v, counts %v", listings(), w.since(0), rep.Counts)
	}
	guest(false)
	arrives := true
	h.Jellyfin.HandleFunc("GET", "/UserItems/c1/UserData", func(rw http.ResponseWriter, r *http.Request) {
		if arrives { // a guest starts watching while owner's import reads Clerks
			arrives = false
			guest(true)
			h.Now = h.Now.Add(h.Env.Viewers.Poll)
		}
		rw.Write([]byte(`{}`))
	})
	if run(t, h); listings() != 2 || asked("c1") != 1 || asked("d1") != 0 {
		t.Fatalf("guest arriving mid-import: %d listing requests, c1 asked %d, d1 asked %d", listings(), asked("c1"), asked("d1"))
	}
	guest(false)
	if run(t, h); asked("a1") != 1 || asked("d1") != 1 {
		t.Fatalf("after the guest left: a1 asked %d (resumes past it), d1 asked %d", asked("a1"), asked("d1"))
	}
	guest(true)
	h.Now = h.Now.Add(25 * time.Hour) // both listings due
	done := len(w.since(0))
	push(h, plexApp, plexPlay("media.stop", "viewer-b", "15", `,"viewOffset":600000,"duration":6000000`))
	if run(t, h); listings() != 2 || strings.Join(w.since(done), "\n") != `/UserItems/d1/UserData?userId=u2 {"LastPlayedDate":"2026-10-06T07:00:00Z","PlaybackPositionTicks":6000000000}` {
		t.Fatalf("guest watching, listings due: %d listing requests, writes %v", listings(), w.since(done))
	}
}

// Catches: a push resolved into the record but not folded into what its app holds (a crash or a failed write
// between the two): the next run would write Plex's own scrobble back to Plex, a second view nobody can remove.
func TestResolveCommitsWithTheSink(t *testing.T) {
	h, w, _, _ := newHarness(t)
	run(t, h)
	db := h.Env.Store.DB()
	if _, err := db.Exec(`CREATE TRIGGER gone BEFORE INSERT ON jfsync_sink BEGIN SELECT RAISE(ABORT, 'disk gone'); END`); err != nil {
		t.Fatal(err)
	}
	push(h, plexApp, plexPlay("media.scrobble", "viewer-b", "15", ""))
	if err := (Stage{}).Run(context.Background(), h.Env, &stage.Report{}); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("run with a failing sink write returned %v", err)
	}
	db.Exec(`DROP TRIGGER gone`)
	done := len(w.since(0))
	run(t, h)
	if got := strings.Join(w.since(done), "\n"); strings.Contains(got, "scrobble") || !strings.Contains(got, "/UserItems/d1/UserData?userId=u2") {
		t.Fatalf("after the failed resolve:\n%s", got)
	}
}

// Catches: an unreadable backup copy failing the run (the Jellyfin direction stops with it) or letting Plex writes
// go out before Plex's history seeds what Plex holds; a stale copy imported silently (plays since it are missing);
// and a Jellyfin user created after the first run never getting its history imported.
func TestImportsWaitForTheirSource(t *testing.T) {
	h, w, _, backups := newHarness(t)
	bad := filepath.Join(backups, "pre-z.db")
	os.WriteFile(bad, []byte("not a database"), 0o644)
	os.Chtimes(bad, h.Now, h.Now)
	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"owner","Id":"u1"}]`)
	push(h, plexApp, plexPlay("media.scrobble", "owner-plex", "15", ""))
	rep := run(t, h)
	if got := strings.Join(w.since(0), "\n"); got != `/UserItems/d1/UserData?userId=u1 {"LastPlayedDate":"2026-10-05T06:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}` ||
		len(rep.Alerts) != 2 || !strings.Contains(rep.Alerts[0], "Plex watch history not imported") || rep.Counts["waiting.plex.owner"] != 1 {
		t.Fatalf("bad copy: writes %q, alerts %q, counts %v", got, rep.Alerts, rep.Counts)
	}
	os.Remove(bad)
	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"owner","Id":"u1"},{"Name":"viewer-b","Id":"u2"}]`)
	h.Now = h.Now.Add(25 * time.Hour)
	if rep = run(t, h); !strings.Contains(strings.Join(rep.Alerts, "|"), "taken 25h10m0s ago") { // also relists Jellyfin
		t.Errorf("a stale copy imported without an alert: %q", rep.Alerts)
	}
	done := len(w.since(0))
	run(t, h)
	if got := strings.Join(w.since(0), "\n"); !strings.Contains(got, "owner /:/progress?identifier=com.plexapp.plugins.library&key=11") ||
		!strings.Contains(strings.Join(w.since(done), "\n"), "friend /:/scrobble?identifier=com.plexapp.plugins.library&key=11") {
		t.Fatalf("after the fixes:\n%s", got)
	}
}

// Catches: Plex's stop after a finish overwriting the finish with a resume point near the end; a removed rating
// (-1) recorded as a rating; a Jellyfin push taken as a rating (the plugin sends 0 for an unrated item, and a
// favourite toggle saves with the reason a like does, carrying the old rating); plays of unmapped accounts or of the Home Videos section recorded; a part-way
// Jellyfin stop recorded as a whole state (its played=false would unmark a play made in Plex) while an explicit
// "mark unplayed" is lost; Jellyfin's progress saves flooding the record; the echo of this stage's own write
// recorded (its played=true would mark every episode of a multi-episode file); a dashed item GUID never matching
// the API's id; and a drop that does not say why.
// pluginPayload is the Jellyfin Webhook plugin's UserDataSaved body ("Send All Properties": GetBaseDataObject,
// AddBaseItemData, AddUserItemData in its DataObjectHelpers.cs) for an unrated film stopped 10 min in; %s: SaveReason.
const pluginPayload = `{"ServerId":"f1e2d3c4b5a697881726354453627181","ServerName":"NAS","ServerVersion":"10.11.0",
"ServerUrl":"localhost:8096","NotificationType":"UserDataSaved","Timestamp":"2026-10-05T01:00:00.0000000-07:00",
"UtcTimestamp":"2026-10-05T08:00:00.0000000Z","Name":"Fargo","Overview":"","Tagline":"","ItemId":"0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d",
"ItemType":"Movie","RunTimeTicks":58800000000,"RunTime":"01:38:00","Year":1996,"PremiereDate":"1996-03-08","Genres":"Crime, Drama",
"Likes":false,"Rating":0,"PlaybackPositionTicks":6000000000,"PlaybackPosition":"00:10:00","PlayCount":0,"Favorite":true,
"Played":false,"AudioStreamIndex":-1,"SubtitleStreamIndex":-1,"SaveReason":"%s","NotificationUsername":"owner",
"UserId":"9f8e7d6c-5b4a-3928-1706-f5e4d3c2b1a0"}`

func TestParseHooks(t *testing.T) {
	opt := defaults()
	if err := fake.DecodeOver(name, `users = [{ name = "owner", plex = "owner-plex", jellyfin = "owner" }]`, &opt); err != nil {
		t.Fatal(err)
	}
	plex := func(event, account, section, extra string) string {
		return `{"event":"` + event + `","Account":{"id":9,"title":"` + account + `"},"rating":-1,"Metadata":{"ratingKey":"7","type":"movie","librarySectionID":` +
			section + `,"duration":1000000` + extra + `}}`
	}
	jf := func(reason, extra string) string {
		return `{"NotificationType":"UserDataSaved","NotificationUsername":"owner","ItemId":"AB-CD","ItemType":"Movie","SaveReason":"` + reason + `"` + extra + `}`
	}
	cases := []struct {
		name, hook, payload, want string // want: "kind pos rating ref", or "dropped: <reason>"
	}{
		{"scrobble", "plex", plex("media.scrobble", "owner-plex", "4", ""), "finished 0 <nil> plex:7"},
		{"stop midway", "plex", plex("media.stop", "owner-plex", "4", `,"viewOffset":500000`), "stopped 500000 <nil> plex:7"},
		{"stop at the end", "plex", plex("media.stop", "owner-plex", "4", `,"viewOffset":950000`), "dropped: stop at the start or in the last tenth"},
		{"rating removed", "plex", plex("media.rate", "owner-plex", "4", ""), "rated 0 <nil> plex:7"},
		{"unmapped account", "plex", plex("media.scrobble", "someone", "4", ""), `dropped: account "someone" (id 9) is in no users entry`},
		{"home videos", "plex", plex("media.scrobble", "owner-plex", `"5"`, ""), "dropped: section 5 is neither"},
		{"part-way stop", "jellyfin", jf("PlaybackFinished", `,"Played":true,"PlaybackPositionTicks":12340000`), "stopped 1234 <nil> jellyfin:abcd"},
		{"marked unplayed", "jellyfin", jf("TogglePlayed", `,"Played":false`), "state 0 <nil> jellyfin:abcd"},
		{"progress save", "jellyfin", jf("PlaybackProgress", ""), "dropped: save reason PlaybackProgress"},
		{"our own write echoed", "jellyfin", jf("UpdateUserData", `,"Played":true`), "dropped: save reason UpdateUserData"},
		{"plugin: part-way stop, unrated", "jellyfin", fmt.Sprintf(pluginPayload, "PlaybackFinished"), "stopped 600000 <nil> jellyfin:0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d"},
		{"plugin: favourite toggled", "jellyfin", fmt.Sprintf(pluginPayload, "UpdateUserRating"), "dropped: save reason UpdateUserRating"},
	}
	for _, c := range cases {
		parse := parseJellyfin
		if c.hook == plexApp {
			parse = func(r *http.Request, opt Options, now time.Time) (watch.Event, dropped, error) {
				return parsePlex(r, opt, now, nil)
			}
		}
		e, drop, err := parse(hookRequest(c.hook, c.payload), opt, time.Unix(0, 0))
		got, rating := "dropped: "+drop.reason, "<nil>"
		if e.Rating != nil {
			rating = fmt.Sprint(*e.Rating)
		}
		if drop.reason == "" {
			got = fmt.Sprintf("%s %d %s %s", e.Kind, e.PositionMs, rating, e.Ref)
		}
		if err != nil || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: got %q (err %v), want %q", c.name, got, err, c.want)
		}
	}
}

// Catches: watch state recorded for a person no users entry names (the letterboxd stage's person spelled otherwise)
// reaching neither app with nothing said, and a person who has an entry alerted about.
func TestRecordedPersonWithoutUsersEntryAlerts(t *testing.T) {
	h, _, _, _ := newHarness(t)
	ctx, db := context.Background(), h.Env.Store.DB()
	if err := watch.Open(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"Owner", "owner"} {
		if err := watch.Add(ctx, db, watch.Event{Person: person, Item: "movie/tmdb-11645", Ref: "letterboxd:1", Source: "letterboxd",
			At: h.Now, Kind: watch.Logged, Plays: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if rep := run(t, h); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], `state for "Owner" (1 items)`) {
		t.Errorf("alerts %q, want one naming Owner", rep.Alerts)
	}
}
