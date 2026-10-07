package tonight

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
)

// Catches restrictions replaced instead of merged: each account must keep every filter it had (a content rating group,
// a group of alternatives, labels it already excludes, a filter plex.tv answers URL-encoded) with the shelf labels
// added to its excluded labels; every other setting sent back as plex.tv listed it; a managed user named by id and
// anyone else by username, as Plex Web does; each change logged before and after; and an account that already
// excludes every shelf but its own (in another case) not written.
func TestRestrictionsAreMergedNotReplaced(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	w.tv.mu.Lock()
	for i, f := range []string{"contentRating=G,PG&label!=Tonight-saahil", "contentRating=R|label!=Gore&label!=Kids%2CHorror",
		"label!=Tonight-aaron,tonight-roger,Tonight-saahil"} {
		w.tv.shares[i]["sharingSettings"].(map[string]any)["filterMovies"] = f
	}
	w.tv.mu.Unlock()
	w.ai.script(threeDays...)
	if rep := w.run(t); len(rep.Alerts) != 0 {
		t.Fatalf("alerts %q\n%s", rep.Alerts, w.logs)
	}
	for name, want := range map[string]string{"roger": "contentRating=G,PG&label!=Tonight-saahil,Tonight-aaron",
		"aarontest941": "contentRating=R|label!=Gore&label!=Kids,Horror,Tonight-roger,Tonight-saahil",
		"adam7":    "label!=Tonight-aaron,tonight-roger,Tonight-saahil"} {
		if got := w.tv.filter(name); got != want {
			t.Errorf("%s's Movies restrictions %q, want %q", name, got, want)
		}
	}
	if len(w.tv.writes) != 2 || w.tv.writes[0].InvitedID != 578 || w.tv.writes[0].InvitedEmail != "" ||
		w.tv.writes[1].InvitedEmail != "aarontest941" || w.tv.writes[1].InvitedID != 0 {
		t.Fatalf("writes %+v: want Aurelia's by id and Aaron's by username, and none for the friend", w.tv.writes)
	}
	kept := shareOf("", 0, "", nil, "")["sharingSettings"].(map[string]any)
	delete(kept, "filterMovies")
	for _, wr := range w.tv.writes {
		got := maps.Clone(wr.Settings)
		delete(got, "filterMovies")
		if n, ok := got["allowTuners"].(float64); ok {
			got["allowTuners"] = int(n)
		}
		if !maps.Equal(got, kept) {
			t.Errorf("settings sent back %v, want the rest as listed: %v", wr.Settings, kept)
		}
	}
	if !strings.Contains(w.logs.String(), `user=aarontest941 id=372 before="contentRating=R|label!=Gore&label!=Kids%2CHorror" `+
		`after="contentRating=R|label!=Gore&label!=Kids,Horror,Tonight-roger,Tonight-saahil"`) {
		t.Errorf("no before/after log of Aaron's restrictions:\n%s", w.logs)
	}
}

// Catches an unreadable plex.tv showing someone's shelf to everyone: while the shares cannot be read no shelf but the
// owner's is made, with one alert and none 30 min later; once they read, the others' shelves follow; and a later
// failure to read takes no shelf down.
func TestUnreadableRestrictionsHoldShelvesAndAlertOnce(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	w.tv.failRead = true
	w.ai.script(threeDays...)
	rep := w.run(t)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "sharing restrictions could not be read") {
		t.Fatalf("alerts %q, want one\n%s", rep.Alerts, w.logs)
	}
	if w.shelfItems("Tonight · Saahil") == "" || w.shelfItems("Tonight · Aurelia") != "" || w.shelfItems("Tonight · Aaron") != "" {
		t.Fatalf("shelves: his %q, Aurelia's %q, Aaron's %q; want his alone", w.shelfItems("Tonight · Saahil"),
			w.shelfItems("Tonight · Aurelia"), w.shelfItems("Tonight · Aaron"))
	}
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t); len(rep.Alerts) != 0 || w.shelfItems("Tonight · Aurelia") != "" {
		t.Fatalf("30 min later, still unreadable: alerts %q, Aurelia's shelf %q", rep.Alerts, w.shelfItems("Tonight · Aurelia"))
	}
	w.tv.mu.Lock()
	w.tv.failRead = false
	w.tv.mu.Unlock()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t); len(rep.Alerts) != 0 || w.shelfItems("Tonight · Aurelia") != plexKeys(559907, 954, 13) ||
		w.shelfItems("Tonight · Aaron") != plexKeys(843, 419704, 10948) {
		t.Fatalf("readable again: alerts %q, Aurelia's shelf %q, Aaron's %q\n%s", rep.Alerts, w.shelfItems("Tonight · Aurelia"),
			w.shelfItems("Tonight · Aaron"), w.logs)
	}
	w.tv.mu.Lock()
	w.tv.failRead = true
	w.tv.mu.Unlock()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if w.shelfItems("Tonight · Aurelia") == "" || w.shelfItems("Tonight · Aaron") == "" {
		t.Fatal("a failed read took a shelf down")
	}
}

// Catches a shelf left up where an account that does not exclude it would see it: once Aurelia's restrictions lose
// the labels (someone cleared them) and cannot take them again, her own shelf and Aaron's (which she could see) must
// come down, not the owner's, with one alert naming her, none 30 min later and no shelf made again meanwhile; once her
// restrictions take the labels, both shelves come back.
func TestUnwritableRestrictionsTakeDownWhatTheyWouldShow(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	w.ai.script(threeDays...)
	w.run(t)
	w.tv.mu.Lock()
	w.tv.shares[0]["sharingSettings"] = shareOf("machine1", 578, "roger", nil, "")["sharingSettings"]
	w.tv.failWrite["roger"] = true
	w.tv.mu.Unlock()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "restrictions of roger") {
		t.Fatalf("alerts %q, want one naming roger\n%s", rep.Alerts, w.logs)
	}
	if w.shelfItems("Tonight · Aurelia") != "" || w.shelfItems("Tonight · Aaron") != "" || w.shelfItems("Tonight · Saahil") == "" {
		t.Fatalf("shelves: his %q, Aurelia's %q, Aaron's %q; want his alone", w.shelfItems("Tonight · Saahil"),
			w.shelfItems("Tonight · Aurelia"), w.shelfItems("Tonight · Aaron"))
	}
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t); len(rep.Alerts) != 0 || w.shelfItems("Tonight · Aurelia") != "" || len(w.plex.shelves) != 3 {
		t.Fatalf("30 min later, still unwritable: alerts %q, Aurelia's shelf %q, %d shelves made", rep.Alerts,
			w.shelfItems("Tonight · Aurelia"), len(w.plex.shelves))
	}
	w.tv.mu.Lock()
	w.tv.failWrite["roger"] = false
	w.tv.mu.Unlock()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if w.shelfItems("Tonight · Aurelia") != plexKeys(559907, 954, 13) || w.shelfItems("Tonight · Aaron") != plexKeys(843, 419704, 10948) {
		t.Fatalf("writable again: Aurelia's shelf %q, Aaron's %q\n%s", w.shelfItems("Tonight · Aurelia"), w.shelfItems("Tonight · Aaron"), w.logs)
	}
}

// Catches the old private Plex playlists left behind, or the wrong ones removed: once a person's shelf is up, the
// playlist an earlier build made in their account (tracked by id, in the state 7e548a9 wrote) must be deleted, as them;
// a person whose shelf is held back (Aaron: the server is not shared with him here) keeps theirs; a playlist of hers
// with our title stays.
func TestOwnPlaylistsAreRemovedOnceTheShelfIsUp(t *testing.T) {
	w := newOwn(t, threeWithPlex)
	w.px.add(fake.PlexToken, "5003", "Tonight", rk(680))
	w.px.add(userToken("roger"), "5001", "Tonight", rk(13))
	w.px.add(userToken("roger"), "4000", "Tonight", rk(389)) // hers
	w.px.add(userToken("aaron"), "5002", "Tonight", rk(954))
	w.tv.shares = slices.DeleteFunc(w.tv.shares, func(s map[string]any) bool { return s["invitedId"] == 372 })
	deployed := map[string]any{"recent": map[string]string{}, "spend": map[string]float64{}, "persons": map[string]any{}} // as 7e548a9 left it
	for person, id := range map[string]string{"saahil": "5003", "roger": "5001", "aaron": "5002"} {
		deployed["persons"].(map[string]any)[person] = map[string]any{"recent": map[string]string{}, "plex": map[string]any{"id": id, "synced": []string{rk(13)}}}
	}
	if err := w.h.Env.Store.PutJSON(context.Background(), name, "state", deployed); err != nil {
		t.Fatal(err)
	}
	w.ai.script(threeDays...)
	rep := w.run(t)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "aaron") {
		t.Fatalf("alerts %q, want one, for Aaron's shelf\n%s", rep.Alerts, w.logs)
	}
	if mine, hers, his := w.px.lists(fake.PlexToken), w.px.lists(userToken("roger")), w.px.lists(userToken("aaron")); len(mine) != 0 ||
		!slices.Equal(hers, []string{"Tonight: " + rk(389)}) || !slices.Equal(his, []string{"Tonight: " + rk(954)}) {
		t.Fatalf("Plex playlists: the owner's %q, Aurelia's %q, Aaron's %q; want only hers and Aaron's (no shelf yet) left", mine, hers, his)
	}
	if got := w.state(t).Persons; got["saahil"].Playlist.ID != "" || got["roger"].Playlist.ID != "" || got["aaron"].Playlist.ID != "5002" {
		t.Fatalf("tracked playlists: saahil %q, roger %q, aaron %q", got["saahil"].Playlist.ID, got["roger"].Playlist.ID, got["aaron"].Playlist.ID)
	}
}
