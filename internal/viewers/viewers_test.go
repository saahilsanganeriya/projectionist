package viewers_test

import (
	"context"
	"io"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/jellyfin"
	"projectionist/internal/obs"
	"projectionist/internal/plex"
	"projectionist/internal/viewers"
)

var owners = map[string]bool{"saahilsanganeriya": true, "saahil": true}

// Catches: the owner's own viewing pausing jobs, or someone else's viewing not pausing them (the Big Lebowski
// incident: a move under a live stream 404s it). Same rule as apply_ops.viewers().
func TestOthers(t *testing.T) {
	playing := &jellyfin.Item{Name: "Heat"}
	cases := []struct {
		name string
		px   []plex.Session
		jf   []jellyfin.Session
		want int
	}{
		{"nobody", nil, nil, 0},
		{"owner on both", []plex.Session{{User: "saahilsanganeriya"}}, []jellyfin.Session{{UserName: "saahil", NowPlayingItem: playing}}, 0},
		{"guest on plex", []plex.Session{{User: "roger"}, {User: "saahilsanganeriya"}}, nil, 1},
		{"guest on jellyfin", nil, []jellyfin.Session{{UserName: "roger", NowPlayingItem: playing}}, 1},
		{"idle jellyfin client", nil, []jellyfin.Session{{UserName: "roger"}}, 0},
		{"plex session with no user counts", []plex.Session{{User: ""}}, nil, 1},
	}
	for _, c := range cases {
		if got := viewers.Others(c.px, c.jf, owners); got != c.want {
			t.Errorf("%s: Others = %d, want %d", c.name, got, c.want)
		}
	}
}

// Catches: the gate not reading both servers through their real APIs (auth headers, XML sessions, the 120 s window).
func TestCountAgainstFakeServers(t *testing.T) {
	ps, pc, _ := fake.Plex(t, nil)
	js, jc := fake.Jellyfin(t)
	ps.Handle("GET", "/status/sessions", 200, `<MediaContainer size="2">
  <Video type="movie" title="Heat"><User title="saahilsanganeriya"/></Video>
  <Track type="track" title="Song"><User title="roger"/></Track>
</MediaContainer>`)
	js.Handle("GET", "/Sessions", 200, `[{"Id":"a","UserName":"roger","NowPlayingItem":{"Name":"Ran"}}]`)
	g := &viewers.Gate{Plex: pc, Jellyfin: jc, Owners: owners, Poll: time.Millisecond, Log: obs.NewLogger(io.Discard, "text", "error")}
	n, err := g.Count(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want 2", n, err)
	}
	if q := js.Calls("GET", "/Sessions")[0].RawQuery; q != "activeWithinSeconds=120" {
		t.Fatalf("jellyfin query = %q", q)
	}
}
