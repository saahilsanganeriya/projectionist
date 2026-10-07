package tonight

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"projectionist/internal/plex"
	"projectionist/internal/stage"
	"projectionist/internal/stages/letterboxd"
)

// ---- each person's own shelf: a Plex collection on their own Home alone ----
//
// The owner's shelf is promoted to his Home alone. Anyone else's is promoted to the shared Home, which every account
// the server is shared with sees, so each such account's Movies restrictions exclude the shelf label of every person
// but their own: plex.tv's sharing settings, read, merged and read back, never replaced. A shelf other than the
// owner's is shown only while every other account excludes it and its person's own restrictions exclude the others'.
// While plex.tv's shares cannot be read, no such shelf is created, edited or taken down; one that some account would
// see is taken down until that account's restrictions take the labels. Once a person's shelf is up, the private Plex
// playlist an earlier build made them is deleted.

// shelfLabel is the label on p's shelf that every other account's restrictions exclude. It starts upper case because
// Plex stores a new label so ("projectionist" came back "Projectionist"): the label and the restriction spell it alike.
func shelfLabel(p letterboxd.Person) string { return "Tonight-" + p.Person }

func displayName(p letterboxd.Person) string {
	return cmp.Or(p.Name, strings.ToUpper(p.Person[:1])+p.Person[1:])
}

// shelfMode is how one person's shelf is kept this run.
type shelfMode int

const (
	shelfAsIs   shelfMode = iota // plex.tv's shares were not read: nothing is created, edited or taken down
	shelfOwn                     // the owner's: on his Home alone
	shelfShared                  // anyone else's: on the shared Home, where every other account's restrictions filter it out
	shelfDown                    // not shown: taken down if it is up
)

// planShelves brings the sharing restrictions in line with the people's shelves, alerting once per failure, and
// decides how each person's shelf is kept this run. The owner is the server's own sign-in (PlexOnlineUsername).
func planShelves(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, st *state, people []letterboxd.Person) map[string]shelfMode {
	labels := map[string]string{} // plex_user, lower-cased → their shelf label
	for _, p := range people {
		if p.PlexUser != "" {
			labels[strings.ToLower(p.PlexUser)] = shelfLabel(p)
		}
	}
	var excl map[string][]string
	var failed map[string]error
	prefs, err := env.Plex.Prefs()
	if err == nil {
		rctx, cancel := context.WithTimeout(ctx, opt.SinkTimeout)
		excl, failed, err = restrict(rctx, env.Log, opt.PlexTVURL, prefs, labels)
		cancel()
	}
	if st.Restrict == nil {
		st.Restrict = map[string]bool{}
	}
	alert := func(key, format string, args ...any) {
		if !st.Restrict[key] {
			rep.Alert(format, args...)
			st.Restrict[key] = true
		}
	}
	if err != nil {
		env.Log.Warn("sharing restrictions not read; no shelf but the owner's changes", "event", "plex_tv_restrictions_error", "err", err)
		alert("", "tonight: plex.tv's sharing restrictions could not be read, so no one's own Tonight shelf but the owner's is made or "+
			"changed until they can: %v", err)
	} else {
		for key := range st.Restrict {
			if failed[key] == nil {
				delete(st.Restrict, key)
			}
		}
		for _, user := range slices.Sorted(maps.Keys(failed)) {
			env.Log.Warn("sharing restrictions not brought in line", "event", "plex_tv_restrictions_error", "user", user, "err", failed[user])
			alert(user, "tonight: the Plex sharing restrictions of %s could not be made to exclude the others' Tonight shelves, so no "+
				"shelf they could see is shown: %v", user, failed[user])
		}
	}
	plan := map[string]shelfMode{}
	for _, p := range people {
		me, why := strings.ToLower(p.PlexUser), ""
		_, shared := excl[me]
		switch {
		case p.PlexUser == "":
			plan[p.Person], why = shelfDown, "no plex_user names their Plex account"
		case strings.EqualFold(p.PlexUser, prefs["PlexOnlineUsername"]):
			plan[p.Person] = shelfOwn
		case err != nil:
			plan[p.Person] = shelfAsIs
		case !shared:
			plan[p.Person], why = shelfDown, fmt.Sprintf("this server is not shared with a Plex account %q", p.PlexUser)
		default:
			plan[p.Person] = shelfShared
			if held := heldBack(me, excl, labels); held != "" { // the account at fault has its own alert
				plan[p.Person] = shelfDown
				env.Log.Info("own shelf held back", "event", "tonight_shelf_held", "person", p.Person, "reason", held)
			}
		}
		if s := &st.person(p.Person).Plex; why != "" {
			skip(env.Log, rep, s, "Plex shelf", p.Person, why)
		} else if plan[p.Person] != shelfAsIs {
			s.Skip = ""
		}
	}
	return plan
}

// heldBack is why the shelf of account me (lower-cased) may not be shown, "" when it may: its own restrictions must
// exclude every other shelf, and every other account's must exclude it.
func heldBack(me string, excl map[string][]string, labels map[string]string) string {
	for _, l := range others(labels, me) {
		if !containsFold(excl[me], l) {
			return "their own sharing restrictions do not exclude " + l
		}
	}
	for _, u := range slices.Sorted(maps.Keys(excl)) {
		if u != me && !containsFold(excl[u], labels[me]) {
			return fmt.Sprintf("the sharing restrictions of %q do not exclude it", u)
		}
	}
	return ""
}

// others is every shelf label but user's, sorted.
func others(labels map[string]string, user string) []string {
	var out []string
	for u, l := range labels {
		if u != strings.ToLower(user) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// ---- plex.tv's sharing restrictions, as Plex Web reads and writes them ----

// share is one account this server is shared with, as plex.tv's shared servers list it.
type share struct {
	Machine string `json:"machineIdentifier"`
	ID      int64  `json:"invitedId"`
	Invited struct {
		Title    string `json:"title"`
		Username string `json:"username"` // "" for a managed user
	} `json:"invited"`
	Settings map[string]json.RawMessage `json:"sharingSettings"` // written back whole, filterMovies alone changed
}

func (s share) name() string { return cmp.Or(s.Invited.Username, s.Invited.Title) }

// movies is the share's Movies filter as plex.tv answers it, and plain, as Plex Web writes it (plex.tv may answer it
// URL-encoded).
func (s share) movies() (raw, plain string, err error) {
	if len(s.Settings) == 0 {
		return "", "", errors.New("plex.tv lists no sharing settings for it")
	}
	if f := s.Settings["filterMovies"]; f != nil {
		if err := json.Unmarshal(f, &raw); err != nil {
			return "", "", err
		}
	}
	plain, err = url.PathUnescape(raw)
	return raw, plain, err
}

// restrict makes the Movies restrictions of every account this server is shared with exclude the shelf label of each
// person but their own (labels: plex_user, lower-cased → label), keeping every filter they had and every other
// setting. It answers the labels each account's restrictions exclude once done (by name, lower-cased; read back after
// any write) and the accounts it could not bring in line.
func restrict(ctx context.Context, log *slog.Logger, base string, prefs map[string]string, labels map[string]string) (
	excl map[string][]string, failed map[string]error, err error) {
	tok, machine := prefs["PlexOnlineToken"], prefs["ProcessedMachineIdentifier"]
	if tok == "" {
		return nil, nil, errors.New("plex: Preferences.xml has no PlexOnlineToken")
	}
	read := func() ([]share, error) {
		var all []share
		if err := plexTV(ctx, tok, "projectionist", http.MethodGet, base+"/api/v2/shared_servers/owned/accepted", nil, &all); err != nil {
			return nil, err
		}
		return slices.DeleteFunc(all, func(s share) bool { return s.Machine != machine }), nil
	}
	shares, err := read()
	if err != nil {
		return nil, nil, err
	}
	failed = map[string]error{}
	wrote := false
	for _, s := range shares {
		raw, before, err := s.movies()
		if err != nil {
			failed[s.name()] = err
			continue
		}
		after := excludeLabels(before, others(labels, s.name()))
		if after == before {
			continue
		}
		settings := maps.Clone(s.Settings)
		settings["filterMovies"], _ = json.Marshal(after)
		body := map[string]any{"settings": settings}
		if s.Invited.Username != "" {
			body["invitedEmail"] = s.Invited.Username // as Plex Web names an account
		} else {
			body["invitedId"] = s.ID // a managed user has no username
		}
		log.Info("sharing restrictions: excluding the others' shelves", "event", "plex_tv_write", "action", "restrict", "user", s.name(),
			"id", s.ID, "before", raw, "after", after)
		if err := plexTV(ctx, tok, "projectionist", http.MethodPost, base+"/api/v2/sharing_settings", body, nil); err != nil {
			failed[s.name()] = err
			continue
		}
		wrote = true
	}
	if wrote {
		if shares, err = read(); err != nil {
			return nil, nil, fmt.Errorf("reading the restrictions back: %w", err)
		}
	}
	excl = map[string][]string{}
	for _, s := range shares {
		_, f, err := s.movies()
		excl[strings.ToLower(s.name())] = excluded(f)
		if err == nil && failed[s.name()] == nil && excludeLabels(f, others(labels, s.name())) != f {
			failed[s.name()] = errors.New("plex.tv took the write, but its restrictions still lack the labels")
		}
	}
	return excl, failed, nil
}

// excludeLabels adds labels to filter's excluded labels (its first label!= group, else a group of its own at the
// end), leaving every other group, and every label already there, as it was. Filters are as Plex Web writes them:
// groups joined by "&", a group's alternatives by "|", values by ",". A group of alternatives is never edited: a label
// added there would be one more alternative, not an exclusion.
func excludeLabels(filter string, labels []string) string {
	var groups []string
	if filter != "" {
		groups = strings.Split(filter, "&")
	}
	i := slices.IndexFunc(groups, labelGroup)
	var have []string
	if i >= 0 {
		have = labelValues(groups[i])
	}
	added := false
	for _, l := range labels {
		if !containsFold(have, l) {
			have, added = append(have, l), true
		}
	}
	if !added {
		return filter
	}
	group := "label!=" + strings.Join(have, ",")
	if i >= 0 {
		groups[i] = group
	} else {
		groups = append(groups, group)
	}
	return strings.Join(groups, "&")
}

// excluded is every label filter's label!= groups exclude.
func excluded(filter string) []string {
	var out []string
	for _, g := range strings.Split(filter, "&") {
		if labelGroup(g) {
			out = append(out, labelValues(g)...)
		}
	}
	return out
}

func labelGroup(g string) bool { return strings.HasPrefix(g, "label!=") && !strings.Contains(g, "|") }

func labelValues(g string) []string {
	return slices.DeleteFunc(strings.Split(strings.TrimPrefix(g, "label!="), ","), func(v string) bool { return v == "" })
}

// ---- taking a shelf down, and the old playlists ----

// takeDown removes person's shelf, so no account that does not exclude it can see it; it is made again once every
// account does.
func takeDown(ctx context.Context, log *slog.Logger, px *plex.Client, person string, s *sinkState) error {
	if err := px.Do(ctx, http.MethodDelete, "/library/collections/"+s.ID); err != nil && !errors.Is(err, plex.ErrNotFound) {
		return err
	}
	log.Info("own shelf taken down", "event", "plex_write", "action", "delete", "person", person, "id", s.ID)
	*s = sinkState{Skip: s.Skip}
	return nil
}

// removeOwnPlaylist deletes, as who, the private Plex playlist an earlier build made them: the one it recorded, by id.
func removeOwnPlaylist(ctx context.Context, log *slog.Logger, home *plexHome, who letterboxd.Person, s *sinkState) error {
	prefs, err := home.env.Plex.Prefs()
	if err != nil {
		return err
	}
	px, skipped, err := home.client(ctx, who, prefs["ProcessedMachineIdentifier"])
	if err != nil {
		return err
	}
	if skipped != "" {
		return errors.New("cannot act as them to remove their old playlist: " + skipped)
	}
	if err := px.Do(ctx, http.MethodDelete, "/playlists/"+s.ID); err != nil && !errors.Is(err, plex.ErrNotFound) {
		return err
	}
	log.Info("own playlist removed: their shelf replaces it", "event", "plex_write", "action", "playlist_delete", "person", who.Person, "id", s.ID)
	*s = sinkState{}
	return nil
}
