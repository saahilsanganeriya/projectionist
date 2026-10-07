package tonight

import (
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

// ---- a fake plex.tv Home, and each account's own playlists on fake Plex and fake Jellyfin ----

// The Home: the owner (admin: the server's own sign-in), Aurelia as the managed user "roger", Aaron by username, a
// child behind a PIN, and a user this server is not shared with. A switched user's server token is fake Plex's
// PlexUserToken plus their name. switches counts the Home switches made.
func newPlexTV(t *testing.T) (srv *httptest.Server, switches *atomic.Int32) {
	switched := map[string]string{"u-roger": "tv-roger", "u-aaron": "tv-aaron", "u-nosrv": "tv-nosrv"}
	switches = &atomic.Int32{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Plex-Token")
		uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/home/users/"), "/switch")
		switch {
		case r.URL.Path == "/api/v2/home/users" && tok == fake.PlexToken:
			io.WriteString(w, `{"id":1,"users":[{"uuid":"u-owner","title":"Saahil","username":"saahilsanganeriya","admin":true},`+
				`{"uuid":"u-kid","title":"kid","username":null,"restricted":true,"protected":true},`+
				`{"uuid":"u-roger","title":"roger","username":null,"restricted":true},`+
				`{"uuid":"u-aaron","title":"Aaron Test","username":"aarontest941"},`+
				`{"uuid":"u-nosrv","title":"nosrv","username":"nosrv"}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/switch") && tok == fake.PlexToken && switched[uuid] != "":
			switches.Add(1)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"uuid": uuid, "authToken": switched[uuid]})
		case r.URL.Path == "/api/v2/resources" && strings.HasPrefix(tok, "tv-"):
			servers := []map[string]string{{"clientIdentifier": "elsewhere", "accessToken": "test-other"}}
			if who := strings.TrimPrefix(tok, "tv-"); who != "nosrv" {
				servers = append(servers, map[string]string{"clientIdentifier": "machine1", "accessToken": fake.PlexUserToken + who})
			}
			json.NewEncoder(w).Encode(servers)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, switches
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

// ownWorld is a world where each person also gets their own playlist: the Plex Home and jfsync's users above.
type ownWorld struct {
	*world
	px       *plexPlaylists
	jp       *jfPlaylists
	switches *atomic.Int32
}

const jfsyncUsers = `users = [
  { name = "saahil", plex = "saahilsanganeriya", jellyfin = "saahil" },
  { name = "roger",  plex = "roger",             jellyfin = "roger" },
  { name = "aaron", plex = "Aaron Test",      jellyfin = "aaron" },
]`

func newOwn(t *testing.T, people string) *ownWorld {
	w := newWorld(t, "", 0)
	tv, switches := newPlexTV(t)
	o := &ownWorld{world: w, px: newPlexPlaylists(w.h.Plex), jp: newJFPlaylists(w.h.Jellyfin, w.jf), switches: switches}
	w.base += fmt.Sprintf("plex_tv = %q\nplaylist = \"Tonight\"\n", tv.URL)
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
  { person = "roger", user = "auralb", plex_user = "roger" },
  { person = "aaron", user = "aaron1009", plex_user = "aarontest941" },
]
`

// Catches a person's own picks reaching anyone else: each person's playlist must hold their picks alone, in their own
// Plex account (his with the server's sign-in, Aurelia's and Aaron's through a Home switch to their server token) and
// owned by their own Jellyfin user, made private (Jellyfin makes a playlist public unless told), with no playlist in
// any other account; a person's candidates taken from the household's unseen films (Aurelia's must include films only
// the owner has seen), or his own offering a film his Plex or Jellyfin account played; the household's shelf taking a
// person's picks; and a run with nothing new switching to a Home user or reading a playlist (each run would sign in
// a device for each of them).
func TestOwnPlaylistReachesOnlyItsPerson(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	w.ai.script(answer3(389, 346, 655), // the household's
		pickedFor("Licorice Pizza (2021)", 1398, 949, 18148), // his
		pickedFor("Columbus (2017)", 559907, 954, 13),        // hers: films he has seen, she has not
		pickedFor("The Matrix (1999)", 843, 419704, 10948))   // Aaron's
	if rep := w.run(t); len(rep.Alerts) != 0 || w.ai.calls() != 4 {
		t.Fatalf("alerts %q, %d AI calls (want the household's and one each)\n%s", rep.Alerts, w.ai.calls(), w.logs)
	}
	if w.listed(1, 680) || w.listed(1, 13) || !w.listed(2, 559907) || !w.listed(2, 13) {
		t.Errorf("his own candidates list a film he played in Plex or Jellyfin, or hers lack films only he has seen")
	}
	for _, c := range []struct {
		who, tok, user string
		ids            []int
	}{
		{"saahil", fake.PlexToken, "u1", []int{1398, 949, 18148}},
		{"roger", userToken("roger"), "u3", []int{559907, 954, 13}},
		{"aaron", userToken("aaron"), "u4", []int{843, 419704, 10948}},
	} {
		if got, want := w.px.lists(c.tok), []string{"Tonight: " + plexKeys(c.ids...)}; !slices.Equal(got, want) {
			t.Errorf("%s's Plex playlists %q, want %q", c.who, got, want)
		}
		if got, want := w.jp.owned(c.user), []string{"Tonight: " + jfKeys(c.ids...)}; !slices.Equal(got, want) {
			t.Errorf("%s's Jellyfin playlists %q, want %q", c.who, got, want)
		}
	}
	for _, pl := range w.jp.all {
		if pl.public == nil || *pl.public || len(pl.shares) != 0 {
			t.Errorf("Jellyfin playlist %s of %s: IsPublic %v, shared with %v; want private", pl.id, pl.owner, pl.public, pl.shares)
		}
	}
	if n := len(w.px.by) + len(w.jp.all); n != 6 {
		t.Errorf("%d Plex accounts and Jellyfin playlists, want 3 + 3", n)
	}
	if got := w.plex.members(); !slices.Equal(got, []string{rk(346), rk(389), rk(655)}) {
		t.Errorf("household collection %v", got)
	}

	calls := func() [3]int {
		return [3]int{int(w.switches.Load()), len(w.h.Plex.Calls("GET", "/playlists")), len(w.h.Jellyfin.Calls("GET", "/Users"))}
	}
	first := calls()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if again := calls(); again != first {
		t.Errorf("30 min later, nothing new: Home switches, Plex playlist listings, Jellyfin user reads went from %v to %v", first, again)
	}
}

// Catches a playlist that grows: a pick she has since played (the watch record, from jfsync) must leave her
// playlists, and the next day's picks replace the last day's, in Plex (the same playlist) and in Jellyfin (one
// playlist, made anew).
func TestOwnPlaylistIsReplacedNotAppended(t *testing.T) {
	w := newOwn(t, `people = [{ person = "roger", user = "auralb", plex_user = "roger" }]`+"\n")
	w.ai.script(pickedFor("Columbus (2017)", 389, 346, 655), pickedFor("Columbus (2017)", 559907, 954, 13))
	w.run(t)
	w.record(t, "roger", 954, watch.Finished, 0)
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if p, j := w.px.lists(userToken("roger")), w.jp.owned("u3"); !slices.Equal(p, []string{"Tonight: " + plexKeys(559907, 13)}) ||
		!slices.Equal(j, []string{"Tonight: " + jfKeys(559907, 13)}) {
		t.Fatalf("after she played her pick Mission - Impossible: Plex %q, Jellyfin %q", p, j)
	}
	w.ai.script(pickedFor("Columbus (2017)", 1398, 949, 18148), pickedFor("Columbus (2017)", 843, 419704, 10948))
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	if p, j := w.px.lists(userToken("roger")), w.jp.owned("u3"); !slices.Equal(p, []string{"Tonight: " + plexKeys(843, 419704, 10948)}) ||
		!slices.Equal(j, []string{"Tonight: " + jfKeys(843, 419704, 10948)}) {
		t.Fatalf("next day: Plex %q, Jellyfin %q; want only the day's picks, one playlist each", p, j)
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

// Catches a person projectionist cannot write a Plex playlist for (a Home user behind a PIN, which it cannot enter;
// none by that name; one this server is not shared with) failing the others, switched to anyway, or alerting on every
// run instead of once.
func TestUnswitchablePlexUserAlertsOnceAndIsSkipped(t *testing.T) {
	for _, c := range []struct{ plexUser, alert string }{
		{"kid", `"kid" has a PIN`},
		{"nobody", `no Plex Home user "nobody"`},
		{"nosrv", `"nosrv" has no access to this server`},
	} {
		t.Run(c.plexUser, func(t *testing.T) {
			w := newOwn(t, fmt.Sprintf(`people = [
  { person = "saahil", user = "saahilsan", plex_user = "saahilsanganeriya", export_dir = "testdata/export" },
  { person = "aaron", user = "aaron1009", plex_user = %q },
]
`, c.plexUser))
			w.ai.script(answer3(389, 346, 655), pickedFor("Licorice Pizza (2021)", 1398, 949, 18148),
				pickedFor("The Matrix (1999)", 843, 419704, 10948))
			rep := w.run(t)
			if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "no own Plex playlist for aaron") ||
				!strings.Contains(rep.Alerts[0], c.alert) {
				t.Fatalf("alerts %q, want one naming aaron and %s\n%s", rep.Alerts, c.alert, w.logs)
			}
			if len(w.px.by) != 1 || len(w.px.lists(fake.PlexToken)) != 1 || len(w.jp.owned("u4")) != 1 {
				t.Fatalf("Plex accounts with playlists %d (his %q), Aaron's Jellyfin %q: want his alone in Plex, and Aaron's in Jellyfin",
					len(w.px.by), w.px.lists(fake.PlexToken), w.jp.owned("u4"))
			}
			w.h.Now = w.h.Now.Add(30 * time.Minute)
			if rep := w.run(t); len(rep.Alerts) != 0 {
				t.Fatalf("30 min later: alerts %q; want the one alert only", rep.Alerts)
			}
		})
	}
}

// Catches a playlist the stage did not make edited or deleted: Aurelia's own "Tonight" playlists, in Plex and in
// Jellyfin, with our title, must keep their films through two days of picks, beside the one the stage makes her.
func TestForeignPlaylistsAreUntouched(t *testing.T) {
	w := newOwn(t, `people = [{ person = "roger", user = "auralb", plex_user = "roger" }]`+"\n")
	w.px.add(userToken("roger"), "4000", "Tonight", rk(13), rk(389))
	w.jp.add("jplhers", "u3", "Tonight", jfID(13), jfID(389))
	w.ai.script(pickedFor("Columbus (2017)", 346, 655, 1398), pickedFor("Columbus (2017)", 389, 346, 655))
	w.run(t)
	w.ai.script(pickedFor("Columbus (2017)", 949, 18148, 843), pickedFor("Columbus (2017)", 559907, 954, 13))
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.run(t)
	if p := w.px.lists(userToken("roger")); !slices.Equal(p, []string{"Tonight: " + plexKeys(13, 389), "Tonight: " + plexKeys(559907, 954, 13)}) {
		t.Fatalf("her Plex playlists %q: hers changed, or ours missing", p)
	}
	if j := w.jp.owned("u3"); !slices.Contains(j, "Tonight: "+jfKeys(13, 389)) || len(j) != 2 {
		t.Fatalf("her Jellyfin playlists %q: hers changed or deleted, or ours missing", j)
	}
	for _, c := range append(w.h.Plex.Calls("PUT", "/playlists/4000/items"), w.h.Jellyfin.Calls("DELETE", "/Items/jplhers")...) {
		t.Errorf("a write to her own playlist: %s %s", c.Method, c.Path)
	}
}

// Catches a second playlist in someone's account after a create whose answer was lost (the app made it, the stage
// never heard its id): the next run must adopt it, not make another that nothing would ever take away.
func TestOwnPlaylistCreateNeverDuplicates(t *testing.T) {
	w := newOwn(t, `people = [{ person = "roger", user = "auralb", plex_user = "roger" }]`+"\n")
	w.px.dropCreate, w.jp.dropCreate = true, true
	w.ai.script(pickedFor("Columbus (2017)", 346, 655, 1398), pickedFor("Columbus (2017)", 389, 346, 655))
	if rep := w.run(t); rep.Counts["roger.plex_retry"]+rep.Counts["roger.jellyfin_retry"] != 2 {
		t.Fatalf("run 1 counts %v\n%s", rep.Counts, w.logs)
	}
	w.px.mu.Lock()
	w.px.dropCreate = false
	w.px.mu.Unlock()
	w.jp.mu.Lock()
	w.jp.dropCreate = false
	w.jp.mu.Unlock()
	w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
	if rep := w.run(t); rep.Counts["roger.plex_synced"]+rep.Counts["roger.jellyfin_synced"] != 2 {
		t.Fatalf("run 2 counts %v\n%s", rep.Counts, w.logs)
	}
	if p, j := w.px.lists(userToken("roger")), w.jp.owned("u3"); len(p) != 1 || len(j) != 1 {
		t.Fatalf("her playlists: Plex %q, Jellyfin %q; want one each", p, j)
	}
	if st := w.state(t).Persons["roger"]; st.Plex.ID == "" || st.Jellyfin.ID == "" || st.Plex.Pending != nil || st.Jellyfin.Pending != nil {
		t.Fatalf("her state: plex %+v, jellyfin %+v", st.Plex, st.Jellyfin)
	}
}
