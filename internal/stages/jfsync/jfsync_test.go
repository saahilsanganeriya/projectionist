package jfsync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

type writes struct {
	sync.Mutex
	calls []string // "<token kind> <path?query>" for Plex, "<path> <json>" for Jellyfin
}

func (w *writes) add(s string) { w.Lock(); w.calls = append(w.calls, s); w.Unlock() }
func (w *writes) since(n int) []string {
	w.Lock()
	defer w.Unlock()
	return append([]string(nil), w.calls[n:]...)
}

// newHarness: Plex holds Heat (re-created after the backup: ratingKey 13, was 10), Ran, Alien, Clerks, Dune, Fargo
// and both episodes of a two-episode Wire file; Jellyfin holds all but Alien, the Wire file as one item. History:
// saahil watched Heat (twice), Alien, Clerks, Dune, Fargo in Plex, is 30 min into Ran in Jellyfin and 20 min into a
// rewatch of Clerks there; roger watched Wire E02 in Plex and Ran in Jellyfin.
func newHarness(t *testing.T, jellyfinStatus int) (h *fake.Harness, w *writes, options, plan string) {
	h = fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "backups"), 0o755)
	T1, T2 := time.Date(2024, 8, 9, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	writeBackup(t, filepath.Join(dir, "backups", "pre-x.db"), nil) // older copy: ignored
	os.Chtimes(filepath.Join(dir, "backups", "pre-x.db"), time.Unix(T1, 0), time.Unix(T1, 0))
	writeBackup(t, filepath.Join(dir, "backups", "pre-y.db"), [][]any{
		{"saahilsanganeriya", 10, 0, heat, 2, 0, T1}, {"saahilsanganeriya", 12, 0, alien, 1, 0, T1},
		{"saahilsanganeriya", 14, 0, clerks, 1, 0, T1}, {"saahilsanganeriya", 15, 0, dune, 1, 0, T1},
		{"saahilsanganeriya", 16, 0, fargo, 1, 0, T1},
		{"roger", 21, 2, wire, 1, 0, T2},
	})
	plan = filepath.Join(dir, "plan.tsv")
	options = `plex_backups = "` + filepath.Join(dir, "backups") + `"
plex_tv = "` + h.Plex.URL + `"
plan_file = "` + plan + `"
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
	w = &writes{}
	for _, p := range []string{"GET /:/scrobble", "GET /:/unscrobble", "GET /:/progress", "PUT /:/rate"} {
		method, path, _ := strings.Cut(p, " ")
		h.Plex.HandleFunc(method, path, func(rw http.ResponseWriter, r *http.Request) {
			who := "owner"
			if r.Header.Get("X-Plex-Token") == fake.PlexFriendToken {
				who = "friend"
			}
			w.add(who + " " + r.URL.Path + "?" + r.URL.RawQuery)
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
			rw.Write([]byte(`{"Items":[{"Id":"b1","Path":"` + ran + `","UserData":{"Played":true,"PlayCount":1,"LastPlayedDate":"2025-02-01T00:00:00Z"}}]}`))
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
			body, _ := json.Marshal(json.RawMessage(mustRead(r)))
			w.add(r.URL.Path + "?" + r.URL.RawQuery + " " + string(body))
			rw.WriteHeader(jellyfinStatus)
		})
	}
	return h, w, options, plan
}

func mustRead(r *http.Request) []byte {
	var buf bytes.Buffer
	buf.ReadFrom(r.Body)
	return buf.Bytes()
}

func run(t *testing.T, h *fake.Harness) *stage.Report {
	t.Helper()
	rep := &stage.Report{}
	if err := (Stage{}).Run(context.Background(), h.Env, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func plexHook(t *testing.T, h *fake.Harness, payload string) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("payload", payload)
	mw.Close()
	req := httptest.NewRequest("POST", "/hooks/plex", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	(Stage{}).Hooks(h.Env)["POST /hooks/plex"].ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("plex hook: %d %s", rec.Code, rec.Body)
	}
}

// Catches: a dry run that writes; a state written back to the app it came from (Heat's re-created Plex item would
// be scrobbled again, adding a play); Jellyfin's "not played" on a part-watched item unmarking a Plex play, or the
// Plex history not counted as held (Clerks would be unscrobbled or scrobbled again); a friend's write sent with the
// owner's token; an existing plex.tv webhook
// dropped when ours is added; a rerun that writes again; a pushed play not reaching the other app; and a pushed
// play not counted as held by the app that pushed it (Plex would be scrobbled again, adding a play).
func TestRunTwoWay(t *testing.T) {
	h, w, options, plan := newHarness(t, 200)
	h.SetOptions(options + "apply = false\n")
	rep := run(t, h)
	if len(w.since(0)) != 0 || len(h.Plex.Calls("POST", "/api/v2/user/webhooks")) != 0 {
		t.Fatalf("dry run wrote: %v", w.since(0))
	}
	if raw, _ := os.ReadFile(plan); strings.Count(string(raw), "\n") != 8 { // header + 7 planned writes
		t.Fatalf("plan:\n%s", raw)
	}
	if rep.Counts["pending.jellyfin.saahil"] != 1 || rep.Counts["jellyfin_from_plex.saahil"] != 3 ||
		rep.Counts["plex_from_jellyfin.saahil"] != 2 || rep.Counts["plex_from_jellyfin.roger"] != 1 {
		t.Fatalf("counts %v", rep.Counts)
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
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing write %s in\n%s", want, got)
		}
	}
	if n := len(w.since(0)); n != 7 || strings.Contains(got, "key=13") || strings.Contains(got, "scrobble?identifier=com.plexapp.plugins.library&key=14") {
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
	plexHook(t, h, `{"event":"media.scrobble","Account":{"title":"saahilsanganeriya"},"Metadata":{"ratingKey":"11","type":"movie","librarySectionID":4}}`)
	run(t, h)
	if got := w.since(done); len(got) != 1 || got[0] != `/UserItems/b1/UserData?userId=u1 {"LastPlayedDate":"2026-10-05T07:00:00Z","PlayCount":1,"PlaybackPositionTicks":0,"Played":true}` {
		t.Fatalf("after the Plex play: %v", got)
	}

	// A rewatch stopped 10 min in on Jellyfin: Plex already holds the play (it pushed it), so only the position goes.
	rec := httptest.NewRecorder()
	(Stage{}).Hooks(h.Env)["POST /hooks/jellyfin"].ServeHTTP(rec, httptest.NewRequest("POST", "/hooks/jellyfin", strings.NewReader(
		`{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"b1","ItemType":"Movie","SaveReason":"PlaybackFinished","Played":true,"PlayCount":1,"PlaybackPositionTicks":6000000000}`)))
	h.Now = h.Now.Add(time.Minute)
	run(t, h)
	if got := w.since(done + 1); rec.Code != http.StatusNoContent || len(got) != 1 ||
		got[0] != "owner /:/progress?identifier=com.plexapp.plugins.library&key=11&time=600000&state=stopped" {
		t.Fatalf("after the Jellyfin rewatch (hook %d): %v", rec.Code, got)
	}
}

// Catches: a failing app failing the run, a failed item retried before its backoff, and a down app being sent its
// whole backlog in one run.
func TestWriteFailuresBackOff(t *testing.T) {
	h, w, _, _ := newHarness(t, 500)
	jf := func(calls []string) (n int) {
		for _, c := range calls {
			n += strings.Count(c, "/UserItems/")
		}
		return n
	}
	rep := run(t, h)
	if n := jf(w.since(0)); n != maxFailRun || rep.Counts["deferred.jellyfin"] != 1 {
		t.Fatalf("first run: %d Jellyfin attempts, counts %v", n, rep.Counts)
	}
	seen := len(w.since(0))
	if run(t, h); jf(w.since(seen)) != 1 { // only the item never tried
		t.Fatalf("second run retried early: %v", w.since(seen))
	}
	seen = len(w.since(0))
	h.Now = h.Now.Add(6 * time.Minute)
	if run(t, h); jf(w.since(seen)) != maxFailRun {
		t.Fatalf("after the backoff: %v", w.since(seen))
	}
}

// Catches: Plex's stop after a finish overwriting the finish with a resume point near the end; a removed rating
// (-1) recorded as a rating; plays of unmapped accounts or of the Home Videos section recorded; a part-way
// Jellyfin stop recorded as a whole state (its played=false would unmark a play made in Plex) while an explicit
// "mark unplayed" is lost; Jellyfin's every-few-seconds progress saves flooding the record; and a dashed item GUID
// never matching the API's id.
func TestParseHooks(t *testing.T) {
	opt := defaults()
	opt.Users = []User{{Name: "saahil", Plex: "saahilsanganeriya", Jellyfin: "saahil"}}
	plex := func(event, account, section, extra string) string {
		return `{"event":"` + event + `","Account":{"title":"` + account + `"},"rating":-1,"Metadata":{"ratingKey":"7","type":"movie","librarySectionID":` +
			section + `,"duration":1000000` + extra + `}}`
	}
	cases := []struct {
		name, hook, payload, want string // want: "kind pos rating" or "" for ignored
	}{
		{"scrobble", "plex", plex("media.scrobble", "saahilsanganeriya", "4", ""), "finished 0 <nil> plex:7"},
		{"stop midway", "plex", plex("media.stop", "saahilsanganeriya", "4", `,"viewOffset":500000`), "stopped 500000 <nil> plex:7"},
		{"stop at the end", "plex", plex("media.stop", "saahilsanganeriya", "4", `,"viewOffset":950000`), ""},
		{"rating removed", "plex", plex("media.rate", "saahilsanganeriya", "4", ""), "rated 0 <nil> plex:7"},
		{"unmapped account", "plex", plex("media.scrobble", "someone", "4", ""), ""},
		{"home videos", "plex", plex("media.scrobble", "saahilsanganeriya", `"5"`, ""), ""},
		{"user data saved", "jellyfin", `{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"AB-CD","ItemType":"Movie",` +
			`"SaveReason":"PlaybackFinished","Played":true,"PlaybackPositionTicks":12340000}`, "stopped 1234 <nil> jellyfin:abcd"},
		{"marked unplayed", "jellyfin", `{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"ab","ItemType":"Movie",` +
			`"SaveReason":"TogglePlayed","Played":false}`, "state 0 <nil> jellyfin:ab"},
		{"progress save", "jellyfin", `{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"ab","ItemType":"Movie",` +
			`"SaveReason":"PlaybackProgress"}`, ""},
	}
	for _, c := range cases {
		var req *http.Request
		parse := parseJellyfin
		if c.hook == "plex" {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			mw.WriteField("payload", c.payload)
			mw.Close()
			req = httptest.NewRequest("POST", "/", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			parse = parsePlex
		} else {
			req = httptest.NewRequest("POST", "/", strings.NewReader(c.payload))
		}
		e, ok, err := parse(req, opt, time.Unix(0, 0))
		got := ""
		if ok {
			got = fmt.Sprintf("%s %d %v %s", e.Kind, e.PositionMs, e.Rating, e.Ref)
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (err %v), want %q", c.name, got, err, c.want)
		}
	}
}
