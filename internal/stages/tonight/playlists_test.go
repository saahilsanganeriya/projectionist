package tonight

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/watch"
)

// ---- a fake plex.tv, and each account's own playlists on fake Plex and fake Jellyfin ----

// plexTVFake is plex.tv: the owner's Home (list, switch, each user's servers) and the accounts this server is shared
// with, whose sharing settings it reads and writes as Plex Web does.
//
// The Home: the owner (admin: the server's own sign-in), Viewer B as the managed user "viewer-b" and Viewer-a by username. A
// switched user's server token is fake Plex's PlexUserToken plus their name. The shares: Viewer B, Viewer-a, a friend,
// and a friend of another server.
type plexTVFake struct {
	srv       *httptest.Server
	switches  atomic.Int32
	mu        sync.Mutex
	shares    []map[string]any // as /api/v2/shared_servers/owned/accepted answers them
	writes    []sharingWrite
	failRead  bool
	failWrite map[string]bool // an account's name (username, or title): its sharing_settings writes answer 503
}

// sharingWrite is the body of one POST /api/v2/sharing_settings.
type sharingWrite struct {
	Settings     map[string]any `json:"settings"`
	InvitedEmail string         `json:"invitedEmail"`
	InvitedID    int            `json:"invitedId"`
}

// shareOf is one account the server machine is shared with, its Movies restrictions filter, its other settings as
// plex.tv lists a fresh share; username nil for a managed user.
func shareOf(machine string, id int, title string, username any, filter string) map[string]any {
	settings := map[string]any{"allowChannels": false, "filterMovies": filter, "filterMusic": "", "filterPhotos": "",
		"filterTelevision": "label!=Kids", "filterAll": nil, "allowSync": true, "allowCameraUpload": false, "allowSubtitleAdmin": false,
		"allowTuners": 0}
	return map[string]any{"id": 30000 + id, "machineIdentifier": machine, "invitedId": id, "invitedEmail": "user@example.com",
		"owner":   map[string]any{"id": 1, "title": "Owner", "username": "owner-plex"},
		"invited": map[string]any{"id": id, "title": title, "username": username, "sharingSettings": settings}, "sharingSettings": settings}
}

func newPlexTV(t *testing.T) *plexTVFake {
	tv := &plexTVFake{failWrite: map[string]bool{}, shares: []map[string]any{
		shareOf("machine1", 578, "viewer-b", nil, ""),
		shareOf("machine1", 372, "Viewer A", "viewer-a-plex", ""),
		shareOf("machine1", 114, "viewer-c-plex", "viewer-c-plex", ""),
		shareOf("elsewhere", 999, "elsewhere-friend", "elsewhere-friend", ""),
	}}
	switched := map[string]string{"u-viewer-b": "tv-viewer-b", "u-viewer-a": "tv-viewer-a"}
	tv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Plex-Token")
		uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/home/users/"), "/switch")
		tv.mu.Lock()
		defer tv.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v2/home/users" && tok == fake.PlexToken:
			io.WriteString(w, `{"id":1,"users":[{"uuid":"u-owner","title":"Owner","username":"owner-plex","admin":true},`+
				`{"uuid":"u-viewer-b","title":"viewer-b","username":null,"restricted":true},`+
				`{"uuid":"u-viewer-a","title":"Viewer A","username":"viewer-a-plex"}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/switch") && tok == fake.PlexToken && switched[uuid] != "":
			tv.switches.Add(1)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"uuid": uuid, "authToken": switched[uuid]})
		case r.URL.Path == "/api/v2/resources" && strings.HasPrefix(tok, "tv-"):
			json.NewEncoder(w).Encode([]map[string]string{{"clientIdentifier": "elsewhere", "accessToken": "test-other"},
				{"clientIdentifier": "machine1", "accessToken": fake.PlexUserToken + strings.TrimPrefix(tok, "tv-")}})
		case r.Method == "GET" && r.URL.Path == "/api/v2/shared_servers/owned/accepted" && tok == fake.PlexToken && !tv.failRead:
			json.NewEncoder(w).Encode(tv.shares)
		case r.Method == "POST" && r.URL.Path == "/api/v2/sharing_settings" && tok == fake.PlexToken:
			var body sharingWrite
			json.NewDecoder(r.Body).Decode(&body)
			tv.writes = append(tv.writes, body)
			for _, sh := range tv.shares {
				inv := sh["invited"].(map[string]any)
				name, _ := inv["username"].(string)
				if (body.InvitedEmail != "" && body.InvitedEmail == name) || (body.InvitedID != 0 && body.InvitedID == sh["invitedId"]) {
					if tv.failWrite[cmp.Or(name, inv["title"].(string))] {
						http.Error(w, "busy", http.StatusServiceUnavailable)
						return
					}
					sh["sharingSettings"], inv["sharingSettings"] = body.Settings, body.Settings
					io.WriteString(w, `{}`)
					return
				}
			}
			http.Error(w, "no such share", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(tv.srv.Close)
	return tv
}

// filter is the Movies restrictions filter of the account named name (username, or title), as plex.tv lists it.
func (tv *plexTVFake) filter(name string) string {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	for _, sh := range tv.shares {
		inv := sh["invited"].(map[string]any)
		if u, _ := inv["username"].(string); cmp.Or(u, inv["title"].(string)) == name {
			return sh["sharingSettings"].(map[string]any)["filterMovies"].(string)
		}
	}
	return "NO SUCH ACCOUNT"
}

type plexPlaylist struct {
	id, title string
	items     []string // ratingKeys
	entries   []int    // playlistItemID of each item
}

// plexPlaylists is every account's playlists on fake Plex, by the token the account calls with.
type plexPlaylists struct {
	srv        *fake.Server
	mu         sync.Mutex
	by         map[string][]*plexPlaylist
	next       int
	dropCreate bool // POST /playlists makes the playlist, then hangs up: its answer is lost
}

func newPlexPlaylists(srv *fake.Server) *plexPlaylists {
	p := &plexPlaylists{srv: srv, by: map[string][]*plexPlaylist{}}
	mc := func(rw http.ResponseWriter, v any) { json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": v}) }
	srv.HandleFunc("GET", "/playlists", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var md []map[string]string
		for _, pl := range p.by[r.Header.Get("X-Plex-Token")] {
			md = append(md, map[string]string{"ratingKey": pl.id, "title": pl.title, "playlistType": "video"})
		}
		mc(rw, map[string]any{"Metadata": md})
	})
	srv.HandleFunc("POST", "/playlists", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		q := r.URL.Query()
		u := q.Get("uri")
		pl := p.add(r.Header.Get("X-Plex-Token"), "", q.Get("title"), strings.Split(u[strings.LastIndex(u, "/")+1:], ",")...)
		if p.dropCreate {
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		mc(rw, map[string]any{"Metadata": []map[string]string{{"ratingKey": pl.id}}})
	})
	return p
}

// add makes a playlist for the account calling with tok (id "" = the next free one) and its routes; the caller holds
// p.mu, or the server is not yet answering.
func (p *plexPlaylists) add(tok, id, title string, items ...string) *plexPlaylist {
	if id == "" {
		p.next++
		id = fmt.Sprintf("%d", 5000+p.next)
	}
	pl := &plexPlaylist{id: id, title: title}
	p.by[tok] = append(p.by[tok], pl)
	p.put(pl, items)
	mine := func(rw http.ResponseWriter, r *http.Request) bool { // another account's playlist is not found
		if !slices.Contains(p.by[r.Header.Get("X-Plex-Token")], pl) {
			http.NotFound(rw, r)
			return false
		}
		return true
	}
	p.srv.HandleFunc("GET", "/playlists/"+id+"/items", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !mine(rw, r) {
			return
		}
		var md []map[string]any
		for i, k := range pl.items {
			md = append(md, map[string]any{"ratingKey": k, "playlistItemID": pl.entries[i]})
		}
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": md}})
	})
	p.srv.HandleFunc("DELETE", "/playlists/"+id, func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if mine(rw, r) {
			tok := r.Header.Get("X-Plex-Token")
			p.by[tok] = slices.DeleteFunc(p.by[tok], func(x *plexPlaylist) bool { return x == pl })
		}
	})
	p.srv.HandleFunc("PUT", "/playlists/"+id+"/items", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if mine(rw, r) {
			u := r.URL.Query().Get("uri")
			p.put(pl, strings.Split(u[strings.LastIndex(u, "/")+1:], ","))
		}
	})
	return pl
}

// put appends items to pl, each with an entry id and a route that removes it.
func (p *plexPlaylists) put(pl *plexPlaylist, items []string) {
	for _, k := range items {
		p.next++
		entry := 90000 + p.next
		pl.items, pl.entries = append(pl.items, k), append(pl.entries, entry)
		p.srv.HandleFunc("DELETE", fmt.Sprintf("/playlists/%s/items/%d", pl.id, entry), func(rw http.ResponseWriter, r *http.Request) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if i := slices.Index(pl.entries, entry); i >= 0 && slices.Contains(p.by[r.Header.Get("X-Plex-Token")], pl) {
				pl.items, pl.entries = slices.Delete(pl.items, i, i+1), slices.Delete(pl.entries, i, i+1)
			}
		})
	}
}

// lists describes an account's playlists, "title: sorted items" each.
func (p *plexPlaylists) lists(tok string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, pl := range p.by[tok] {
		out = append(out, pl.title+": "+strings.Join(slices.Sorted(slices.Values(pl.items)), ","))
	}
	return out
}

func userToken(who string) string { return fake.PlexUserToken + who }

type jfPlaylist struct {
	id, owner, name string
	items           []string
	public          *bool // as the create sent it
	shares          []any
}

// jfPlaylists are the playlists on fake Jellyfin, served beside jfFake's routes through its playlist hook.
type jfPlaylists struct {
	srv        *fake.Server
	mu         sync.Mutex
	all        []*jfPlaylist
	next       int
	dropCreate bool // POST /Playlists makes the playlist, then hangs up
}

func newJFPlaylists(srv *fake.Server, j *jfFake) *jfPlaylists {
	p := &jfPlaylists{srv: srv}
	srv.Handle("GET", "/Users", 200, `[{"Name":"guest","Id":"u2"},{"Name":"owner","Id":"u1"},{"Name":"Viewer-b","Id":"u3"},{"Name":"viewer-a","Id":"u4"}]`)
	srv.HandleFunc("POST", "/Playlists", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var body struct {
			Name, UserId, MediaType string
			Ids                     []string
			IsPublic                *bool
			Users                   []any
		}
		json.NewDecoder(r.Body).Decode(&body)
		pl := p.add("", body.UserId, body.Name, body.Ids...)
		pl.public, pl.shares = body.IsPublic, body.Users
		if p.dropCreate {
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		json.NewEncoder(rw).Encode(map[string]string{"Id": pl.id})
	})
	j.playlists = p.answer
	return p
}

// add makes a playlist owned by user, with a route that deletes it; the caller holds p.mu, or the server is not yet
// answering.
func (p *jfPlaylists) add(id, user, name string, items ...string) *jfPlaylist {
	if id == "" {
		p.next++
		id = fmt.Sprintf("jpl%d", p.next)
	}
	pl := &jfPlaylist{id: id, owner: user, name: name, items: items}
	p.all = append(p.all, pl)
	p.srv.HandleFunc("DELETE", "/Items/"+id, func(http.ResponseWriter, *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.all = slices.DeleteFunc(p.all, func(x *jfPlaylist) bool { return x == pl })
	})
	return pl
}

// answer serves a GET /Items about playlists (a user's, one by id, or one's items), as Jellyfin shows them to that
// user; ok is false for any other query.
func (p *jfPlaylists) answer(q map[string][]string) (out []map[string]any, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	visible := func(pl *jfPlaylist) bool { return pl.owner == get("userId") }
	for _, pl := range p.all {
		switch {
		case get("IncludeItemTypes") == "Playlist":
			ok = true
			if visible(pl) {
				out = append(out, map[string]any{"Id": pl.id, "Name": pl.name, "Type": "Playlist"})
			}
		case get("Ids") == pl.id:
			ok = true
			if visible(pl) {
				out = append(out, map[string]any{"Id": pl.id, "Name": pl.name, "Type": "Playlist"})
			}
		case get("ParentId") == pl.id:
			ok = true
			for _, id := range pl.items {
				out = append(out, map[string]any{"Id": id})
			}
		}
	}
	return out, ok || get("IncludeItemTypes") == "Playlist" || strings.HasPrefix(get("Ids"), "jpl") || strings.HasPrefix(get("ParentId"), "jpl")
}

// owned describes user's playlists, "name: sorted items" each.
func (p *jfPlaylists) owned(user string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, pl := range p.all {
		if pl.owner == user {
			out = append(out, pl.name+": "+strings.Join(slices.Sorted(slices.Values(pl.items)), ","))
		}
	}
	return out
}

// ownWorld is the world every run now has: plex.tv's Home and shares, and jfsync's users below.
type ownWorld = world

const jfsyncUsers = `users = [
  { name = "owner", plex = "owner-plex", jellyfin = "owner" },
  { name = "viewer-b",  plex = "viewer-b",             jellyfin = "viewer-b" },
  { name = "viewer-a", plex = "Viewer A",      jellyfin = "viewer-a" },
]`

func newOwn(t *testing.T, people string) *ownWorld { return newWorld(t, people, 0) }

func plexKeys(ids ...int) string {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = rk(id)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

func jfKeys(ids ...int) string {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = jfID(id)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

const threeWithPlex = `people = [
  { person = "owner", user = "owner-lb", plex_user = "owner-plex", export_dir = "testdata/export" },
  { person = "viewer-b", user = "viewer-b-lb", plex_user = "viewer-b", name = "Viewer B" },
  { person = "viewer-a", user = "viewer-a-lb", plex_user = "viewer-a-plex" },
]
`

// shelfItems is the items of the live shelf titled title, "" when there is none.
func (w *ownWorld) shelfItems(title string) string {
	if s, _, _ := w.plex.shelf(title); s != nil {
		return strings.Join(s.items, ",")
	}
	return ""
}

// Catches a person's own picks reaching anyone else. In Plex each person's shelf "Tonight · <name>" must hold their
// picks alone, labelled for them: his on his own Home alone, Viewer B's and Viewer-a's on the shared Home alone, in that
// order at the top of the Movies hubs, with every account the server is shared with excluding every shelf but its own in
// its Movies restrictions (a friend all three, another server's friend none), and no Plex playlist made in anyone's
// account. In Jellyfin each person's playlist is owned by their user and private (Jellyfin makes a playlist public
// unless told). Also a run with nothing new writing a restriction, switching to a Home user or reading a playlist.
func TestOwnPicksReachOnlyTheirPerson(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	if rep := w.run(t); len(rep.Alerts) != 0 {
		t.Fatalf("alerts %q\n%s", rep.Alerts, w.logs)
	}
	for i, c := range []struct {
		who, user, shelf, label string
		own                     bool
	}{
		{"owner", "u1", "Tonight · Owner", "Tonight-owner", true},
		{"viewer-b", "u3", "Tonight · Viewer B", "Tonight-viewer-b", false},
		{"viewer-a", "u4", "Tonight · Viewer-a", "Tonight-viewer-a", false},
	} {
		ids := w.picksOf(t, c.who)
		s, hub, at := w.plex.shelf(c.shelf)
		if len(ids) != 3 || s == nil || strings.Join(s.items, ",") != plexKeys(ids...) || !slices.Equal(s.labels, []string{"projectionist", c.label}) {
			t.Errorf("%s's shelf %q: %+v; want their picks %s, labelled %s", c.who, c.shelf, s, plexKeys(ids...), c.label)
		} else if want := (plexHub{Identifier: hub.Identifier, PromotedToOwnHome: c.own, PromotedToSharedHome: !c.own}); hub != want || at != i {
			t.Errorf("%s's shelf hub %+v at %d; want own Home %v, shared Home %v, at %d", c.who, hub, at, c.own, !c.own, i)
		}
		if got, want := w.jp.owned(c.user), []string{"Tonight: " + jfKeys(ids...)}; !slices.Equal(got, want) {
			t.Errorf("%s's Jellyfin playlists %q, want %q", c.who, got, want)
		}
	}
	for name, want := range map[string]string{"viewer-b": "label!=Tonight-owner,Tonight-viewer-a",
		"viewer-a-plex": "label!=Tonight-owner,Tonight-viewer-b", "viewer-c-plex": "label!=Tonight-owner,Tonight-viewer-a,Tonight-viewer-b",
		"elsewhere-friend": ""} {
		if got := w.tv.filter(name); got != want {
			t.Errorf("%s's Movies restrictions %q, want %q", name, got, want)
		}
	}
	if len(w.px.by) != 0 {
		t.Errorf("Plex playlists made in %d accounts, want none", len(w.px.by))
	}
	for _, pl := range w.jp.all {
		if pl.public == nil || *pl.public || len(pl.shares) != 0 {
			t.Errorf("Jellyfin playlist %s of %s: IsPublic %v, shared with %v; want private", pl.id, pl.owner, pl.public, pl.shares)
		}
	}

	calls := func() [4]int {
		return [4]int{int(w.tv.switches.Load()), len(w.h.Plex.Calls("GET", "/playlists")), len(w.h.Jellyfin.Calls("GET", "/Users")), len(w.tv.writes)}
	}
	first := calls()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if again := calls(); again != first {
		t.Errorf("30 min later, nothing new: Home switches, Plex playlist listings, Jellyfin user reads, restriction writes went from %v to %v",
			first, again)
	}
}

// Catches own picks that pile up: a pick she has since played (the watch record, from jfsync) must leave her shelf and
// her Jellyfin playlist, and the next day's picks replace the last day's, in Plex (the same shelf) and in Jellyfin
// (one playlist, made anew).
func TestOwnPicksAreReplacedNotAppended(t *testing.T) {
	w := newOwn(t, `people = [{ person = "viewer-b", user = "viewer-b-lb", plex_user = "viewer-b" }]`+"\n")
	w.run(t)
	played := w.picksOf(t, "viewer-b")[1]
	w.record(t, "viewer-b", played, watch.Finished, 0)
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	left := w.picksOf(t, "viewer-b")
	if p, j := w.shelfItems("Tonight · Viewer-b"), w.jp.owned("u3"); len(left) != 2 || slices.Contains(left, played) || p != plexKeys(left...) ||
		!slices.Equal(j, []string{"Tonight: " + jfKeys(left...)}) {
		t.Fatalf("after she played her pick %d: picks %v, shelf %q, Jellyfin %q\n%s", played, left, p, j, w.logs)
	}
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	next := w.picksOf(t, "viewer-b")
	if p, j := w.shelfItems("Tonight · Viewer-b"), w.jp.owned("u3"); len(next) != 3 || p != plexKeys(next...) ||
		!slices.Equal(j, []string{"Tonight: " + jfKeys(next...)}) || len(w.plex.shelves) != 1 {
		t.Fatalf("next day: picks %v, shelf %q (of %d), Jellyfin %q; want only the day's picks, one shelf and one playlist", next, p, len(w.plex.shelves), j)
	}
}

// Catches a collection or playlist the stage did not make edited or deleted: Viewer B's own collection "Tonight ·
// Viewer-b" (no label) and her own Jellyfin "Tonight" playlist must keep their films through two days of picks, beside
// the shelf and the playlist the stage makes her.
func TestForeignCollectionsAndPlaylistsAreUntouched(t *testing.T) {
	w := newOwn(t, `people = [{ person = "viewer-b", user = "viewer-b-lb", plex_user = "viewer-b" }]`+"\n")
	w.plex.addForeign("778", "Tonight · Viewer-b")
	w.jp.add("jplhers", "u3", "Tonight", jfID(13), jfID(389))
	w.run(t)
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	if p := w.shelfItems("Tonight · Viewer-b"); p != plexKeys(w.picksOf(t, "viewer-b")...) {
		t.Fatalf("our shelf beside hers holds %q\n%s", p, w.logs)
	}
	if j := w.jp.owned("u3"); !slices.Contains(j, "Tonight: "+jfKeys(13, 389)) || len(j) != 2 {
		t.Fatalf("her Jellyfin playlists %q: hers changed or deleted, or ours missing", j)
	}
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		if strings.Contains(c.RawQuery, "id=778") {
			t.Errorf("her collection was edited: %s", c.RawQuery)
		}
	}
	for _, c := range append(w.h.Plex.Calls("DELETE", "/library/collections/778"), w.h.Jellyfin.Calls("DELETE", "/Items/jplhers")...) {
		t.Errorf("a delete of hers: %s %s", c.Method, c.Path)
	}
}

// Catches a second Jellyfin playlist in someone's account after a create whose answer was lost (Jellyfin made it, the
// stage never heard its id): the next run must adopt it, not make another that nothing would ever take away.
func TestOwnPlaylistCreateNeverDuplicates(t *testing.T) {
	w := newOwn(t, `people = [{ person = "viewer-b", user = "viewer-b-lb", plex_user = "viewer-b" }]`+"\n")
	w.jp.dropCreate = true
	if rep := w.run(t); rep.Counts["viewer-b.jellyfin_retry"] != 1 {
		t.Fatalf("run 1 counts %v\n%s", rep.Counts, w.logs)
	}
	w.jp.mu.Lock()
	w.jp.dropCreate = false
	w.jp.mu.Unlock()
	w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
	if rep := w.run(t); rep.Counts["viewer-b.jellyfin_synced"] != 1 {
		t.Fatalf("run 2 counts %v\n%s", rep.Counts, w.logs)
	}
	if j := w.jp.owned("u3"); len(j) != 1 {
		t.Fatalf("her Jellyfin playlists %q; want one", j)
	}
	if st := w.state(t).Persons["viewer-b"]; st.Jellyfin.ID == "" || st.Jellyfin.Pending != nil {
		t.Fatalf("her state: jellyfin %+v", st.Jellyfin)
	}
}
