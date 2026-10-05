package jfsync

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
		INSERT INTO accounts VALUES (1, 'saahilsanganeriya'), (578, 'roger')`)
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
	calls  []string          // "<owner|friend> <path?query>" for Plex, "<path?query> <json>" for Jellyfin
	status map[string]int    // path → the status a write to it answers (default 200)
	during map[string]func() // path → called while a write to it is being answered
}

func (w *writes) add(rw http.ResponseWriter, path, call string) {
	w.Lock()
	w.calls = append(w.calls, call)
	status, during := w.status[path], w.during[path]
	w.Unlock()
	if during != nil {
		during()
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

// newHarness: Plex holds Heat (re-created after the backup: ratingKey 13, was 10), Ran, Alien, Clerks, Dune, Fargo
// and both episodes of a two-episode Wire file; Jellyfin holds all but Alien, the Wire file as one item. History:
// saahil watched Heat (twice), Alien, Clerks, Dune, Fargo in Plex, is 30 min into Ran in Jellyfin and 20 min into a
// rewatch of Clerks there; roger watched Wire E02 in Plex and Ran in Jellyfin, rating it 7.
func newHarness(t *testing.T) (h *fake.Harness, w *writes, options, backups string) {
	h = fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	dir := t.TempDir()
	backups = filepath.Join(dir, "backups")
	os.MkdirAll(backups, 0o755)
	T1, T2 := time.Date(2024, 8, 9, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	writeBackup(t, filepath.Join(backups, "pre-x.db"), nil) // older copy: ignored
	writeBackup(t, filepath.Join(backups, "pre-y.db"), [][]any{
		{"saahilsanganeriya", 10, 0, heat, 2, 0, T1}, {"saahilsanganeriya", 12, 0, alien, 1, 0, T1},
		{"saahilsanganeriya", 14, 0, clerks, 1, 0, T1}, {"saahilsanganeriya", 15, 0, dune, 1, 0, T1},
		{"saahilsanganeriya", 16, 0, fargo, 1, 0, T1},
		{"roger", 21, 2, wire, 1, 0, T2},
	})
	os.Chtimes(filepath.Join(backups, "pre-x.db"), time.Unix(T1, 0), time.Unix(T1, 0))
	os.Chtimes(filepath.Join(backups, "pre-y.db"), h.Now.Add(-10*time.Minute), h.Now.Add(-10*time.Minute))
	options = `plex_backups = "` + backups + `"
plex_tv = "` + h.Plex.URL + `"
plan_file = "` + filepath.Join(dir, "plan.tsv") + `"
users = [{ name = "saahil", plex = "saahilsanganeriya", jellyfin = "saahil" }, { name = "roger", plex = "roger", jellyfin = "roger" }]
`
	h.SetOptions(options)

	part := func(rk string, index int, file string) string {
		return `{"ratingKey":"` + rk + `","index":` + string(rune('0'+index)) + `,"Media":[{"Part":[{"file":"` + file + `"}]}]}`
	}
	h.Plex.Handle("GET", "/accounts", 200, `{"MediaContainer":{"Account":[{"id":1,"name":"saahilsanganeriya"},{"id":578,"name":"roger"}]}}`)
	h.Plex.Handle("GET", "/library/sections/4/all", 200, `{"MediaContainer":{"Metadata":[`+part("13", 0, heat)+","+
		part("11", 0, ran)+","+part("12", 0, alien)+","+part("14", 0, clerks)+","+part("15", 0, dune)+","+part("16", 0, fargo)+`]}}`)
	h.Plex.Handle("GET", "/library/sections/2/all", 200, `{"MediaContainer":{"Metadata":[`+part("20", 1, wire)+","+part("21", 2, wire)+`]}}`)
	h.Plex.Handle("GET", "/api/v2/user/webhooks", 200, `<webhooks><webhook url="http://tautulli:8181/hook"/></webhooks>`)
	h.Plex.Handle("POST", "/api/v2/user/webhooks", 201, ``)
	h.Plex.Handle("GET", "/api/servers/mid/shared_servers", 200,
		`<MediaContainer><SharedServer userID="578" accessToken="`+fake.PlexFriendToken+`"/><SharedServer userID="9" accessToken="other"/></MediaContainer>`)
	w = &writes{status: map[string]int{}, during: map[string]func(){}}
	for _, p := range []string{"GET /:/scrobble", "GET /:/unscrobble", "GET /:/progress", "PUT /:/rate"} {
		method, path, _ := strings.Cut(p, " ")
		h.Plex.HandleFunc(method, path, func(rw http.ResponseWriter, r *http.Request) {
			who := "owner"
			if r.Header.Get("X-Plex-Token") == fake.PlexFriendToken {
				who = "friend"
			}
			w.add(rw, r.URL.Path, who+" "+r.URL.Path+"?"+r.URL.RawQuery)
		})
	}

	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"saahil","Id":"u1"},{"Name":"roger","Id":"u2"}]`)
	h.Jellyfin.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("userId") == "u1" && q.Get("filters") == "IsResumable":
			rw.Write([]byte(`{"Items":[{"Id":"b1","Path":"` + ran + `","UserData":{"PlaybackPositionTicks":18000000000,"LastPlayedDate":"2025-01-01T00:00:00.0000000Z"}},` +
				`{"Id":"c1","Path":"` + clerks + `","UserData":{"PlaybackPositionTicks":12000000000,"LastPlayedDate":"2025-03-01T00:00:00Z"}}]}`))
		case q.Get("userId") == "u2" && q.Get("isPlayed") == "true":
			rw.Write([]byte(`{"Items":[{"Id":"b1","Path":"` + ran + `","UserData":{"Played":true,"PlayCount":1,"Rating":7,"LastPlayedDate":"2025-02-01T00:00:00Z"}}]}`))
		case q.Get("userId") != "":
			rw.Write([]byte(`{"Items":[]}`))
		default:
			rw.Write([]byte(`{"Items":[{"Id":"a1","Path":"` + heat + `"},{"Id":"b1","Path":"` + ran + `"},{"Id":"c1","Path":"` +
				clerks + `"},{"Id":"d1","Path":"` + dune + `"},{"Id":"e1","Path":"` + wire + `"},{"Id":"f1","Path":"` + fargo +
				`"},{"Id":"x1","Path":"/lib/Home Videos/x.mkv"}]}`))
		}
	})
	for _, id := range []string{"a1", "b1", "c1", "d1", "e1", "f1"} {
		h.Jellyfin.HandleFunc("POST", "/UserItems/"+id+"/UserData", func(rw http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.add(rw, r.URL.Path, r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
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
// friend's write sent with the owner's token; an existing plex.tv webhook dropped when ours is added; a rerun that
// writes again; a pushed play not reaching the other app, or not counted as held by the app that pushed it; the
// owner's pushed play lost because its payload names him by his plex.tv title; and a state that knows only a resume
// point sent to Jellyfin as played=false, playcount=0 (it would unmark a play there).
func TestRunTwoWay(t *testing.T) {
	h, w, options, _ := newHarness(t)
	h.SetOptions(options + "apply = false\n")
	rep := run(t, h)
	if len(w.since(0)) != 0 || len(h.Plex.Calls("POST", "/api/v2/user/webhooks")) != 0 {
		t.Fatalf("dry run wrote: %v", w.since(0))
	}
	if rep.Counts["pending.jellyfin.saahil"] != 1 || rep.Counts["jellyfin_from_plex.saahil"] != 3 || rep.Counts["jellyfin_from_plex.roger"] != 1 ||
		rep.Counts["plex_from_jellyfin.saahil"] != 2 || rep.Counts["plex_from_jellyfin.roger"] != 1 || len(rep.Alerts) != 0 {
		t.Fatalf("plan: counts %v alerts %v", rep.Counts, rep.Alerts)
	}

	h.SetOptions(options)
	run(t, h)
	got := strings.Join(w.since(0), "\n")
	for _, want := range []string{
		`/UserItems/a1/UserData?userId=u1 {"LastPlayedDate":"2024-08-09T00:00:00Z","PlayCount":2,"PlaybackPositionTicks":0,"Played":true}`,
		`/UserItems/e1/UserData?userId=u2 {"LastPlayedDate":"2024-09-01T00:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}`,
		"owner /:/progress?identifier=com.plexapp.plugins.library&key=11&time=1800000&state=stopped",
		"owner /:/progress?identifier=com.plexapp.plugins.library&key=14&time=1200000&state=stopped", // Clerks stays watched
		"friend /:/scrobble?identifier=com.plexapp.plugins.library&key=11",
		"friend /:/rate?identifier=com.plexapp.plugins.library&key=11&rating=7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing write %s in\n%s", want, got)
		}
	}
	if n := len(w.since(0)); n != 8 || strings.Contains(got, "key=13") || strings.Contains(got, "scrobble?identifier=com.plexapp.plugins.library&key=14") {
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
	push(h, plexApp, strings.Replace(plexPlay("media.scrobble", "Saahil S", "11", ""), `"title"`, `"id":1,"title"`, 1))
	push(h, plexApp, plexPlay("media.stop", "roger", "15", `,"viewOffset":600000,"duration":6000000`))
	run(t, h)
	if got := strings.Join(w.since(done), "\n"); got != `/UserItems/b1/UserData?userId=u1 {"LastPlayedDate":"2026-10-05T07:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}`+"\n"+
		`/UserItems/d1/UserData?userId=u2 {"LastPlayedDate":"2026-10-05T07:00:00Z","PlaybackPositionTicks":6000000000}` {
		t.Fatalf("after the Plex play and roger's stop:\n%s", got)
	}

	// A rewatch stopped 10 min in on Jellyfin: Plex already holds the play (it pushed it), so only the position goes.
	h.Now = h.Now.Add(10 * time.Minute)
	code := push(h, jellyfinApp, `{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"b1","ItemType":"Movie","SaveReason":"PlaybackFinished","Played":true,"PlayCount":1,"PlaybackPositionTicks":6000000000}`)
	run(t, h)
	if got := w.since(done + 2); code != http.StatusNoContent || len(got) != 1 ||
		got[0] != "owner /:/progress?identifier=com.plexapp.plugins.library&key=11&time=600000&state=stopped" {
		t.Fatalf("after the Jellyfin rewatch (hook %d): %v", code, got)
	}
}

// Catches: a push that crosses this stage's write to the same item lost for good. Saahil marks Heat unplayed in
// Jellyfin while the run writes Heat's Plex play there; the next run would skip the item (its newest state came
// from Jellyfin) and both apps would keep the play. Also: Heat's re-created Plex item, which nothing is known to
// hold, not unscrobbled.
func TestPushCrossingAWrite(t *testing.T) {
	h, w, _, _ := newHarness(t)
	code := 0
	w.during["/UserItems/a1/UserData"] = func() {
		code = push(h, jellyfinApp, `{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"a1","ItemType":"Movie","SaveReason":"TogglePlayed","Played":false}`)
	}
	run(t, h)
	delete(w.during, "/UserItems/a1/UserData")
	done := len(w.since(0))
	h.Now = h.Now.Add(5 * time.Minute)
	run(t, h)
	got := strings.Join(w.since(done), "\n")
	if code != http.StatusNoContent || !strings.Contains(got, `/UserItems/a1/UserData?userId=u1 {"PlayCount":0,"PlaybackPositionTicks":0,"Played":false}`) ||
		!strings.Contains(got, "owner /:/unscrobble?identifier=com.plexapp.plugins.library&key=13") {
		t.Fatalf("after the crossing push (hook %d):\n%s", code, got)
	}
	done = len(w.since(0))
	if run(t, h); len(w.since(done)) != 0 {
		t.Fatalf("third run wrote %v", w.since(done))
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
	if calls := w.since(0); count(calls, "/UserItems/") != maxFailRun || rep.Counts["deferred.jellyfin"] != 1 ||
		count(calls, "friend /:/") != 2 {
		t.Fatalf("first run: %v, counts %v", calls, rep.Counts)
	}
	seen := len(w.since(0))
	if run(t, h); count(w.since(seen), "/UserItems/") != 1 || count(w.since(seen), "friend") != 0 { // only the item never tried
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
// Jellyfin write); a play of an item added since the listing waiting a day for the next one; and a failed listing
// retried every run instead of backing off.
func TestListingsFollowTheWrites(t *testing.T) {
	h, w, _, _ := newHarness(t)
	run(t, h)
	h.Plex.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, r *http.Request) {
		w.add(rw, "list", "list plex")
		rw.WriteHeader(500)
	})
	done := len(w.since(0))
	h.Now = h.Now.Add(time.Hour)
	push(h, plexApp, plexPlay("media.scrobble", "saahilsanganeriya", "15", ""))
	push(h, plexApp, plexPlay("media.scrobble", "saahilsanganeriya", "17", "")) // added after the listing
	run(t, h)
	if got := w.since(done); len(got) != 2 || !strings.HasPrefix(got[0], "/UserItems/d1/") || got[1] != "list plex" {
		t.Fatalf("new item: %v", got)
	}
	for i, step := range []time.Duration{24 * time.Hour, 5 * time.Minute} { // daily: due; 5 min later: backing off
		done = len(w.since(0))
		h.Now = h.Now.Add(step)
		if run(t, h); (len(w.since(done)) == 1) != (i == 0) {
			t.Errorf("run %d after the failure: %v", i+1, w.since(done))
		}
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
	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"saahil","Id":"u1"}]`)
	push(h, plexApp, plexPlay("media.scrobble", "saahilsanganeriya", "15", ""))
	rep := run(t, h)
	if got := strings.Join(w.since(0), "\n"); got != `/UserItems/d1/UserData?userId=u1 {"LastPlayedDate":"2026-10-05T06:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}` ||
		len(rep.Alerts) != 2 || !strings.Contains(rep.Alerts[0], "Plex watch history not imported") || rep.Counts["waiting.plex.saahil"] != 1 {
		t.Fatalf("bad copy: writes %q, alerts %q, counts %v", got, rep.Alerts, rep.Counts)
	}
	os.Remove(bad)
	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Name":"saahil","Id":"u1"},{"Name":"roger","Id":"u2"}]`)
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
// (-1) recorded as a rating; plays of unmapped accounts or of the Home Videos section recorded; a part-way
// Jellyfin stop recorded as a whole state (its played=false would unmark a play made in Plex) while an explicit
// "mark unplayed" is lost; Jellyfin's progress saves flooding the record; the echo of this stage's own write
// recorded (its played=true would mark every episode of a multi-episode file); a dashed item GUID never matching
// the API's id; and a drop that does not say why.
func TestParseHooks(t *testing.T) {
	opt := defaults()
	opt.Users = []User{{Name: "saahil", Plex: "saahilsanganeriya", Jellyfin: "saahil"}}
	plex := func(event, account, section, extra string) string {
		return `{"event":"` + event + `","Account":{"id":9,"title":"` + account + `"},"rating":-1,"Metadata":{"ratingKey":"7","type":"movie","librarySectionID":` +
			section + `,"duration":1000000` + extra + `}}`
	}
	jf := func(reason, extra string) string {
		return `{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"AB-CD","ItemType":"Movie","SaveReason":"` + reason + `"` + extra + `}`
	}
	cases := []struct {
		name, hook, payload, want string // want: "kind pos rating ref", or "dropped: <reason>"
	}{
		{"scrobble", "plex", plex("media.scrobble", "saahilsanganeriya", "4", ""), "finished 0 <nil> plex:7"},
		{"stop midway", "plex", plex("media.stop", "saahilsanganeriya", "4", `,"viewOffset":500000`), "stopped 500000 <nil> plex:7"},
		{"stop at the end", "plex", plex("media.stop", "saahilsanganeriya", "4", `,"viewOffset":950000`), "dropped: stop at the start or in the last tenth"},
		{"rating removed", "plex", plex("media.rate", "saahilsanganeriya", "4", ""), "rated 0 <nil> plex:7"},
		{"unmapped account", "plex", plex("media.scrobble", "someone", "4", ""), `dropped: account "someone" (id 9) is in no users entry`},
		{"home videos", "plex", plex("media.scrobble", "saahilsanganeriya", `"5"`, ""), "dropped: section 5 is neither"},
		{"part-way stop", "jellyfin", jf("PlaybackFinished", `,"Played":true,"PlaybackPositionTicks":12340000`), "stopped 1234 <nil> jellyfin:abcd"},
		{"marked unplayed", "jellyfin", jf("TogglePlayed", `,"Played":false`), "state 0 <nil> jellyfin:abcd"},
		{"progress save", "jellyfin", jf("PlaybackProgress", ""), "dropped: save reason PlaybackProgress"},
		{"our own write echoed", "jellyfin", jf("UpdateUserData", `,"Played":true`), "dropped: save reason UpdateUserData"},
	}
	for _, c := range cases {
		parse := parseJellyfin
		if c.hook == plexApp {
			parse = func(r *http.Request, opt Options, now time.Time) (watch.Event, dropped, error) {
				return parsePlex(r, opt, now, nil)
			}
		}
		e, drop, err := parse(hookRequest(c.hook, c.payload), opt, time.Unix(0, 0))
		got := "dropped: " + drop.reason
		if drop.reason == "" {
			got = fmt.Sprintf("%s %d %v %s", e.Kind, e.PositionMs, e.Rating, e.Ref)
		}
		if err != nil || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: got %q (err %v), want %q", c.name, got, err, c.want)
		}
	}
}
