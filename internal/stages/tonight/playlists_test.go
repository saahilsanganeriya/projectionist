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

	"github.com/BurntSushi/toml"

	"projectionist/internal/fake"
	"projectionist/internal/watch"
)

// ---- a fake plex.tv, and each account's own playlists on fake Plex and fake Jellyfin ----

// plexTVFake is plex.tv: the owner's Home (list, switch, each user's servers) and the accounts this server is shared
// with, whose sharing settings it reads and writes as Plex Web does.
//
// The Home: the owner (admin: the server's own sign-in), Aurelia as the managed user "roger" and Aaron by username. A
// switched user's server token is fake Plex's PlexUserToken plus their name. The shares: Aurelia, Aaron, a friend,
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
		"owner":   map[string]any{"id": 1, "title": "Saahil", "username": "saahilsanganeriya"},
		"invited": map[string]any{"id": id, "title": title, "username": username, "sharingSettings": settings}, "sharingSettings": settings}
}

func newPlexTV(t *testing.T) *plexTVFake {
	tv := &plexTVFake{failWrite: map[string]bool{}, shares: []map[string]any{
		shareOf("machine1", 578, "roger", nil, ""),
		shareOf("machine1", 372, "Aaron Test", "aarontest941", ""),
		shareOf("machine1", 114, "adam7", "adam7", ""),
		shareOf("elsewhere", 999, "elsewhere-friend", "elsewhere-friend", ""),
	}}
	switched := map[string]string{"u-roger": "tv-roger", "u-aaron": "tv-aaron"}
	tv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Plex-Token")
		uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/home/users/"), "/switch")
		tv.mu.Lock()
		defer tv.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v2/home/users" && tok == fake.PlexToken:
			io.WriteString(w, `{"id":1,"users":[{"uuid":"u-owner","title":"Saahil","username":"saahilsanganeriya","admin":true},`+
				`{"uuid":"u-roger","title":"roger","username":null,"restricted":true},`+
				`{"uuid":"u-aaron","title":"Aaron Test","username":"aarontest941"}]}`)
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
	srv.Handle("GET", "/Users", 200, `[{"Name":"guest","Id":"u2"},{"Name":"saahil","Id":"u1"},{"Name":"Roger","Id":"u3"},{"Name":"aaron","Id":"u4"}]`)
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

// ownWorld is a world where each person also gets their own picks: plex.tv's Home and shares, and jfsync's users above.
type ownWorld struct {
	*world
	px *plexPlaylists
	jp *jfPlaylists
	tv *plexTVFake
}

const jfsyncUsers = `users = [
  { name = "saahil", plex = "saahilsanganeriya", jellyfin = "saahil" },
  { name = "roger",  plex = "roger",             jellyfin = "roger" },
  { name = "aaron", plex = "Aaron Test",      jellyfin = "aaron" },
]`

func newOwn(t *testing.T, people string) *ownWorld {
	w := newWorld(t, "", 0)
	o := &ownWorld{world: w, px: newPlexPlaylists(w.h.Plex), jp: newJFPlaylists(w.h.Jellyfin, w.jf), tv: newPlexTV(t)}
	w.base += fmt.Sprintf("plex_tv = %q\nplaylist = \"Tonight\"\n", o.tv.srv.URL)
	w.options(people)
	w.h.Env.PeerOptions = func(stage string, v any) error {
		if stage != "jfsync" {
			return nil
		}
		_, err := toml.Decode(jfsyncUsers, v)
		return err
	}
	return o
}

// pickedFor is a valid answer for a viewer who rated liked 4 stars or more.
func pickedFor(liked string, ids ...int) string {
	return strings.ReplaceAll(answer3(ids...), "Licorice Pizza (2021)", liked)
}

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
  { person = "saahil", user = "saahilsan", plex_user = "saahilsanganeriya", export_dir = "testdata/export" },
  { person = "roger", user = "auralb", plex_user = "roger", name = "Aurelia" },
  { person = "aaron", user = "aaron1009", plex_user = "aarontest941" },
]
`

// threeDays is a valid answer for the household, then for each of the three, in the order the stage asks.
var threeDays = []string{answer3(389, 346, 655), pickedFor("Licorice Pizza (2021)", 1398, 949, 18148),
	pickedFor("Columbus (2017)", 559907, 954, 13), pickedFor("The Matrix (1999)", 843, 419704, 10948)}

// shelfItems is the items of the live shelf titled title, "" when there is none.
func (w *ownWorld) shelfItems(title string) string {
	if s, _, _ := w.plex.shelf(title); s != nil {
		return strings.Join(s.items, ",")
	}
	return ""
}

// Catches a person's own picks reaching anyone else. In Plex each person's shelf "Tonight · <name>" must hold their
// picks alone, labelled for them: his on his own Home alone, Aurelia's and Aaron's on the shared Home alone, each below
// the household row, with every account the server is shared with excluding every shelf but its own in its Movies
// restrictions (a friend all three, another server's friend none), and no Plex playlist made in anyone's account. In
// Jellyfin each person's playlist is owned by their user and private (Jellyfin makes a playlist public unless told).
// Also: a person's candidates taken from the household's unseen films (Aurelia's must include films only the owner has
// seen), or his own offering a film his Plex or Jellyfin account played; the household's row taking a person's picks;
// and a run with nothing new writing a restriction, switching to a Home user or reading a playlist.
func TestOwnPicksReachOnlyTheirPerson(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	w.ai.script(threeDays...)
	if rep := w.run(t); len(rep.Alerts) != 0 || w.ai.calls() != 4 {
		t.Fatalf("alerts %q, %d AI calls (want the household's and one each)\n%s", rep.Alerts, w.ai.calls(), w.logs)
	}
	if w.listed(1, 680) || w.listed(1, 13) || !w.listed(2, 559907) || !w.listed(2, 13) {
		t.Errorf("his own candidates list a film he played in Plex or Jellyfin, or hers lack films only he has seen")
	}
	for i, c := range []struct {
		who, user, shelf, label string
		ids                     []int
		own                     bool
	}{
		{"saahil", "u1", "Tonight · Saahil", "Tonight-saahil", []int{1398, 949, 18148}, true},
		{"roger", "u3", "Tonight · Aurelia", "Tonight-roger", []int{559907, 954, 13}, false},
		{"aaron", "u4", "Tonight · Aaron", "Tonight-aaron", []int{843, 419704, 10948}, false},
	} {
		s, hub, at := w.plex.shelf(c.shelf)
		if s == nil || strings.Join(s.items, ",") != plexKeys(c.ids...) || !slices.Equal(s.labels, []string{"projectionist", c.label}) {
			t.Errorf("%s's shelf %q: %+v; want %s, labelled %s", c.who, c.shelf, s, plexKeys(c.ids...), c.label)
		} else if want := (plexHub{Identifier: hub.Identifier, PromotedToOwnHome: c.own, PromotedToSharedHome: !c.own}); hub != want || at != i+1 {
			t.Errorf("%s's shelf hub %+v at %d; want own Home %v, shared Home %v, at %d below the household row", c.who, hub, at, c.own, !c.own, i+1)
		}
		if got, want := w.jp.owned(c.user), []string{"Tonight: " + jfKeys(c.ids...)}; !slices.Equal(got, want) {
			t.Errorf("%s's Jellyfin playlists %q, want %q", c.who, got, want)
		}
	}
	for name, want := range map[string]string{"roger": "label!=Tonight-aaron,Tonight-saahil",
		"aarontest941": "label!=Tonight-roger,Tonight-saahil", "adam7": "label!=Tonight-aaron,Tonight-roger,Tonight-saahil",
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
	if got := w.plex.members(); !slices.Equal(got, []string{rk(346), rk(389), rk(655)}) || w.plex.hubs[0].Identifier != "custom.collection.4.900" {
		t.Errorf("household collection %v, hubs %+v", got, w.plex.hubs)
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
	w := newOwn(t, `people = [{ person = "roger", user = "auralb", plex_user = "roger" }]`+"\n")
	w.ai.script(pickedFor("Columbus (2017)", 389, 346, 655), pickedFor("Columbus (2017)", 559907, 954, 13))
	w.run(t)
	w.record(t, "roger", 954, watch.Finished, 0)
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if p, j := w.shelfItems("Tonight · Roger"), w.jp.owned("u3"); p != plexKeys(559907, 13) || !slices.Equal(j, []string{"Tonight: " + jfKeys(559907, 13)}) {
		t.Fatalf("after she played her pick Mission - Impossible: shelf %q, Jellyfin %q\n%s", p, j, w.logs)
	}
	w.ai.script(pickedFor("Columbus (2017)", 1398, 949, 18148), pickedFor("Columbus (2017)", 843, 419704, 10948))
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	if p, j := w.shelfItems("Tonight · Roger"), w.jp.owned("u3"); p != plexKeys(843, 419704, 10948) ||
		!slices.Equal(j, []string{"Tonight: " + jfKeys(843, 419704, 10948)}) || len(w.plex.shelves) != 1 {
		t.Fatalf("next day: shelf %q (of %d), Jellyfin %q; want only the day's picks, one shelf and one playlist", p, len(w.plex.shelves), j)
	}
}

// Catches the household's shelf made anew instead of renamed when its title changes ("Tonight" became "Tonight ·
// Household" on 2026-10-06): the collection and the BoxSet this stage made keep their ids and take the new title.
func TestHouseholdShelfIsRenamedInPlace(t *testing.T) {
	w := newWorld(t, `collection = "Tonight"`+"\n", 0)
	w.ai.script(answer3(389, 346, 655))
	w.run(t)
	w.options("") // the default title
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t); len(rep.Alerts) != 0 || rep.Counts["plex_synced"]+rep.Counts["jellyfin_synced"] != 2 {
		t.Fatalf("alerts %q, counts %v\n%s", rep.Alerts, rep.Counts, w.logs)
	}
	if creates, boxes := len(w.h.Plex.Calls("POST", "/library/collections")), len(w.h.Jellyfin.Calls("POST", "/Collections")); creates != 1 || boxes != 1 {
		t.Fatalf("plex creates %d, jellyfin creates %d: want the first run's alone", creates, boxes)
	}
	if w.plex.title != "Tonight · Household" || w.jf.name != "Tonight · Household" {
		t.Fatalf("titles: plex %q, jellyfin %q", w.plex.title, w.jf.name)
	}
	if st := w.state(t); st.Plex.ID != "900" || st.Jellyfin.ID != "box1" {
		t.Fatalf("state ids: plex %q, jellyfin %q", st.Plex.ID, st.Jellyfin.ID)
	}
}

// Catches a collection or playlist the stage did not make edited or deleted: Aurelia's own collection "Tonight ·
// Roger" (no label) and her own Jellyfin "Tonight" playlist must keep their films through two days of picks, beside
// the shelf and the playlist the stage makes her.
func TestForeignCollectionsAndPlaylistsAreUntouched(t *testing.T) {
	w := newOwn(t, `people = [{ person = "roger", user = "auralb", plex_user = "roger" }]`+"\n")
	w.plex.addForeign("778", "Tonight · Roger")
	w.jp.add("jplhers", "u3", "Tonight", jfID(13), jfID(389))
	w.ai.script(pickedFor("Columbus (2017)", 346, 655, 1398), pickedFor("Columbus (2017)", 389, 346, 655))
	w.run(t)
	w.ai.script(pickedFor("Columbus (2017)", 949, 18148, 843), pickedFor("Columbus (2017)", 559907, 954, 13))
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	if p := w.shelfItems("Tonight · Roger"); p != plexKeys(559907, 954, 13) {
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
	w := newOwn(t, `people = [{ person = "roger", user = "auralb", plex_user = "roger" }]`+"\n")
	w.jp.dropCreate = true
	w.ai.script(pickedFor("Columbus (2017)", 346, 655, 1398), pickedFor("Columbus (2017)", 389, 346, 655))
	if rep := w.run(t); rep.Counts["roger.jellyfin_retry"] != 1 {
		t.Fatalf("run 1 counts %v\n%s", rep.Counts, w.logs)
	}
	w.jp.mu.Lock()
	w.jp.dropCreate = false
	w.jp.mu.Unlock()
	w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
	if rep := w.run(t); rep.Counts["roger.jellyfin_synced"] != 1 {
		t.Fatalf("run 2 counts %v\n%s", rep.Counts, w.logs)
	}
	if j := w.jp.owned("u3"); len(j) != 1 {
		t.Fatalf("her Jellyfin playlists %q; want one", j)
	}
	if st := w.state(t).Persons["roger"]; st.Jellyfin.ID == "" || st.Jellyfin.Pending != nil {
		t.Fatalf("her state: jellyfin %+v", st.Jellyfin)
	}
}
