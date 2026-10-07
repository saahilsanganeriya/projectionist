package tonight

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
	"projectionist/internal/stage"
	"projectionist/internal/stages/jfsync"
	"projectionist/internal/stages/letterboxd"
)

// ---- each person's own Jellyfin playlist, private to them; and plex.tv, as the owner or as one of his Home ----
//
// The Jellyfin sink writes only when the person's picks differ from what it last wrote (sinkState.Synced), so an
// unchanged day costs no call. It edits only the playlist it made (sinkState.ID): one of theirs with the same title is
// never read for a write. A person it cannot write for (no Jellyfin user) is skipped with one alert per reason, and
// tried again each run.

// skip records why a person's own what (a Jellyfin playlist, a Plex shelf) is not written, alerting once per reason.
func skip(log *slog.Logger, rep *stage.Report, s *sinkState, what, person, reason string) {
	log.Warn("own "+what+" skipped", "event", "tonight_own_skipped", "what", what, "person", person, "reason", reason)
	if s.Skip != reason {
		rep.Alert("tonight: no own %s for %s: %s", what, person, reason)
		s.Skip = reason
	}
}

// ---- plex.tv: the owner's Home, whose users the old playlists' removal acts as ----

// homeUser is one member of the owner's Plex Home, as plex.tv lists them.
type homeUser struct {
	UUID      string `json:"uuid"`
	Title     string `json:"title"`
	Username  string `json:"username"` // "" for a managed user
	Admin     bool   `json:"admin"`    // the owner: the server's own sign-in
	Protected bool   `json:"protected"`
}

// plexHome finds, for each person, the Plex client that acts as them on this server: the server's own sign-in for
// the Home admin, else a Home switch to their Home user (as the letterboxd stage makes for their watchlist) and their
// token for this server from plex.tv's resources, both per run and never stored. The Home is listed at most once a run.
type plexHome struct {
	env   *stage.Env
	base  string
	users []homeUser
}

// resource is a server the account may use, with its token for that server.
type resource struct {
	ClientIdentifier string `json:"clientIdentifier"`
	AccessToken      string `json:"accessToken"`
}

var plexTVClient = &http.Client{Timeout: 20 * time.Second}

// plexTV sends one request to plex.tv with tok, as the device clientID, with body as JSON (nil = none), and decodes
// the JSON answer into out (nil = not read). The token goes in a header, never in the URL, so no error or log names it.
func plexTV(ctx context.Context, tok, clientID, method, rawURL string, body, out any) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	path := req.URL.Path
	req.Header.Set("X-Plex-Token", tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", clientID)
	req.Header.Set("X-Plex-Product", "projectionist")
	resp, err := plexTVClient.Do(req)
	if err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("plex.tv %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	return nil
}

// client is the Plex client that acts as who on this server, or why there is none.
func (h *plexHome) client(ctx context.Context, who letterboxd.Person, machine string) (px *plex.Client, skipped string, err error) {
	if who.PlexUser == "" {
		return nil, "no plex_user names their Plex Home user", nil
	}
	prefs, err := h.env.Plex.Prefs()
	if err != nil {
		return nil, "", err
	}
	owner := prefs["PlexOnlineToken"]
	if owner == "" {
		return nil, "", errors.New("plex: Preferences.xml has no PlexOnlineToken")
	}
	if h.users == nil {
		var home struct {
			Users []homeUser `json:"users"`
		}
		if err := plexTV(ctx, owner, "projectionist", http.MethodGet, h.base+"/api/v2/home/users", nil, &home); err != nil {
			return nil, "", err
		}
		h.users = home.Users
	}
	i := slices.IndexFunc(h.users, func(u homeUser) bool { return cmp.Or(u.Username, u.Title) == who.PlexUser })
	switch {
	case i < 0:
		return nil, fmt.Sprintf("no Plex Home user %q (its username, or a managed user's name)", who.PlexUser), nil
	case h.users[i].Admin:
		return h.env.Plex, "", nil
	case h.users[i].Protected:
		return nil, fmt.Sprintf("Plex Home user %q has a PIN, which projectionist cannot enter", who.PlexUser), nil
	}
	// The person's own device id, so the device the switch signs in is theirs and never one of the owner's.
	device := "projectionist-tonight-" + who.Person
	var sw struct {
		AuthToken string `json:"authToken"`
	}
	if err := plexTV(ctx, owner, device, http.MethodPost, h.base+"/api/v2/home/users/"+url.PathEscape(h.users[i].UUID)+"/switch", nil, &sw); err != nil {
		return nil, "", err
	}
	if sw.AuthToken == "" {
		return nil, "", errors.New("plex.tv: the Home switch answered no token")
	}
	var servers []resource
	if err := plexTV(ctx, sw.AuthToken, device, http.MethodGet, h.base+"/api/v2/resources?includeHttps=1", nil, &servers); err != nil {
		return nil, "", err
	}
	j := slices.IndexFunc(servers, func(r resource) bool { return r.ClientIdentifier == machine && r.AccessToken != "" })
	if j < 0 {
		return nil, fmt.Sprintf("Plex Home user %q has no access to this server", who.PlexUser), nil
	}
	return h.env.Plex.As(servers[j].AccessToken), "", nil
}

// ---- Jellyfin: a playlist owned by the person's Jellyfin user ----

// jellyfinUsers resolves each person to their Jellyfin user id through jfsync's users, listing Jellyfin's users once
// a run.
type jellyfinUsers struct {
	jf     *jellyfin.Client
	mapped []jfsync.User
	ids    map[string]string // Jellyfin user name → id; nil until listed
}

func (u *jellyfinUsers) id(ctx context.Context, person string) (id, skipped string, err error) {
	i := slices.IndexFunc(u.mapped, func(m jfsync.User) bool { return m.Name == person })
	if i < 0 || u.mapped[i].Jellyfin == "" {
		return "", "jfsync's users name no Jellyfin user for them", nil
	}
	if u.ids == nil {
		var users []struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		}
		if err := u.jf.GetJSON(ctx, "/Users", &users); err != nil {
			return "", "", err
		}
		u.ids = map[string]string{}
		for _, j := range users {
			u.ids[strings.ToLower(j.Name)] = j.ID
		}
	}
	if id = u.ids[strings.ToLower(u.mapped[i].Jellyfin)]; id == "" {
		return "", fmt.Sprintf("no Jellyfin user %q", u.mapped[i].Jellyfin), nil
	}
	return id, "", nil
}

// syncOwnJellyfin converges person's own Jellyfin playlist on their picks. Jellyfin lets an API key create a playlist
// for a user and delete an item, but edits a playlist only for its owner, so a playlist whose films no longer match is
// replaced by a new one: it never grows. It is made private (IsPublic false, and shared with no one): Jellyfin makes a
// new playlist public unless told.
func syncOwnJellyfin(ctx context.Context, log *slog.Logger, rep *stage.Report, users *jellyfinUsers, title, person string,
	m idMap, s *sinkState, picks []pick, persist func() error) error {
	if len(m.Items) == 0 {
		return errors.New("jellyfin: no item map yet")
	}
	want := m.desired(picks)
	if slices.Equal(want, s.Synced) && s.Skip == "" {
		return nil
	}
	userID, skipped, err := users.id(ctx, person)
	if err != nil {
		return err
	}
	if skipped != "" {
		skip(log, rep, s, "Jellyfin playlist", person, skipped)
		return nil
	}
	s.Skip = ""
	members := func(id string) ([]string, error) { return jellyfinMembers(ctx, users.jf, id, userID) }
	if s.ID != "" {
		var res jfItems
		if err := users.jf.GetJSON(ctx, "/Items?EnableImages=false&userId="+userID+"&Ids="+s.ID, &res); err != nil {
			return err
		}
		if slices.ContainsFunc(res.Items, func(it jfItem) bool { return it.ID == s.ID && it.Type == "Playlist" }) {
			have, err := members(s.ID)
			if err != nil {
				return err
			}
			if add, remove := diff(want, have); len(add)+len(remove) == 0 {
				s.Synced = want
				return nil
			}
			if err := users.jf.Delete(ctx, "/Items/"+s.ID); err != nil {
				return err
			}
			log.Info("own playlist removed, to be replaced", "event", "jellyfin_write", "action", "playlist_delete", "person", person, "id", s.ID)
		}
		s.ID = ""
		if err := persist(); err != nil {
			return err
		}
	}
	if len(want) == 0 {
		s.Synced = want
		return nil
	}
	var res jfItems
	if err := users.jf.GetJSON(ctx, "/Items?IncludeItemTypes=Playlist&Recursive=true&EnableImages=false&userId="+userID, &res); err != nil {
		return err
	}
	var titled []string
	for _, it := range res.Items {
		if it.Name == title {
			titled = append(titled, it.ID)
		}
	}
	id, err := s.Pending.adopt(titled, members)
	if err != nil {
		return err
	}
	if id == "" {
		s.Pending = &pendingCreate{Before: titled, Items: want}
		if err := persist(); err != nil {
			return err
		}
		var created struct {
			ID string `json:"Id"`
		}
		body := map[string]any{"Name": title, "Ids": want, "UserId": userID, "MediaType": "Video", "IsPublic": false}
		if err := users.jf.PostJSON(ctx, "/Playlists", body, &created); err != nil {
			return err
		}
		if created.ID == "" {
			return errors.New("jellyfin: playlist create returned no id")
		}
		id = created.ID
		log.Info("own playlist created", "event", "jellyfin_write", "action", "playlist_create", "person", person, "id", id, "items", len(want))
	}
	s.ID, s.Pending, s.Synced = id, nil, want
	return persist()
}
