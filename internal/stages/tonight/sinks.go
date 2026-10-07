package tonight

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
)

// ---- Plex and Jellyfin are sinks: written to, read only to address a write or to see what he watched ----

// sinkState is one sink's progress toward the desired picks. A failed write is retried on a later run with
// backoff and never fails the run.
type sinkState struct {
	ID           string         `json:"id,omitempty"`        // Plex collection ratingKey / Jellyfin BoxSet or playlist id
	Pending      *pendingCreate `json:"pending,omitempty"`   // a create sent whose answer has not named the collection
	PosterOf     string         `json:"poster_of,omitempty"` // the day of the last uploaded poster
	HubStale     bool           `json:"hub_stale,omitempty"` // Plex: members edited since the Home hub was last refreshed
	Synced       []string       `json:"synced,omitempty"`    // own Jellyfin playlist: the items last written, so unchanged picks cost no call
	Skip         string         `json:"skip,omitempty"`      // own playlist or shelf: why it is not written for them, alerted once
	Fails        int            `json:"fails,omitempty"`
	RetryAt      time.Time      `json:"retry_at,omitzero"`
	FailingSince time.Time      `json:"failing_since,omitzero"`
}

// pendingCreate is recorded (and saved) before a create is sent and cleared once a collection is known. If the
// create's answer was lost, the collection it made is the one with our title that is new since Before and holds
// exactly Items: the next run adopts it instead of making a second, which nothing would ever clean up.
type pendingCreate struct {
	Before []string `json:"before"` // ids of the collections with our title before the create
	Items  []string `json:"items"`  // what the create asked for
}

// adopt returns the collection among titled that the pending create made, or "".
func (pc *pendingCreate) adopt(titled []string, members func(id string) ([]string, error)) (string, error) {
	if pc == nil {
		return "", nil
	}
	for _, id := range titled {
		if slices.Contains(pc.Before, id) {
			continue
		}
		have, err := members(id)
		if err != nil {
			return "", err
		}
		if add, remove := diff(pc.Items, have); len(add)+len(remove) == 0 {
			return id, nil
		}
	}
	return "", nil
}

// backoff is 15 min, 30 min, 1 h, ... up to 4 h after consecutive failures.
func backoff(fails int) time.Duration {
	return min(15*time.Minute<<min(fails-1, 5), 4*time.Hour)
}

// idMap is a sink's TMDB → item map plus his watched films there. Refreshed at most once per MapEvery.
type idMap struct {
	FetchedAt time.Time      `json:"fetched_at,omitzero"`
	TriedAt   time.Time      `json:"tried_at,omitzero"`
	UserID    string         `json:"user_id,omitempty"` // Jellyfin only
	Items     map[int]string `json:"items"`             // tmdb → ratingKey / item id
	Watched   map[int]bool   `json:"watched"`
}

func (m idMap) stale(now time.Time, every time.Duration) bool {
	return now.Sub(m.FetchedAt) >= every && now.Sub(m.TriedAt) >= time.Hour
}

// desired is the sink-side ids of the picks, in pick order; picks the sink does not have yet are left out.
func (m idMap) desired(picks []pick) []string {
	var out []string
	for _, p := range picks {
		if id := m.Items[p.TMDB]; id != "" {
			out = append(out, id)
		}
	}
	return out
}

// diff is what membership needs: ids to add, ids to remove.
func diff(want, have []string) (add, remove []string) {
	for _, id := range want {
		if !slices.Contains(have, id) {
			add = append(add, id)
		}
	}
	for _, id := range have {
		if !slices.Contains(want, id) {
			remove = append(remove, id)
		}
	}
	return add, remove
}

func summary(picks []pick) string {
	lines := make([]string, len(picks))
	for i, p := range picks {
		lines[i] = fmt.Sprintf("%s (%d): %s", p.Title, p.Year, p.Reason)
	}
	return strings.Join(lines, "\n")
}

// ---- Plex ----

// plexLean keeps a section listing to the fields this stage reads. Items' Media elements are what make Plex slow: a
// 5-film collection's /children took 108 s at load 20, the same members listed this way 0.18 s (2026-10-05).
const plexLean = "excludeFields=summary,tagline&excludeElements=Media,Genre,Country,Director,Writer,Role,Collection,Producer,Label,Image"

// plexMap lists the Movies section once, in pages that each fit the client's timeout (a full listing takes
// ~18 s on this NAS, longer during a scan), with only the fields it needs.
func plexMap(ctx context.Context, px *plex.Client, section int) (idMap, error) {
	m := idMap{Items: map[int]string{}, Watched: map[int]bool{}}
	const page = 500
	for start := 0; ; start += page {
		var mc struct {
			TotalSize int `json:"totalSize"`
			Metadata  []struct {
				RatingKey string `json:"ratingKey"`
				ViewCount int    `json:"viewCount"`
				PlexGUID  string `json:"guid"` // "plex://movie/…"; declared so the key cannot land in Guid below
				Guid      []struct {
					ID string `json:"id"`
				} `json:"Guid"`
			} `json:"Metadata"`
		}
		path := fmt.Sprintf("/library/sections/%d/all?type=1&includeGuids=1&%s&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d",
			section, plexLean, start, page)
		if err := px.GetJSON(ctx, path, &mc); err != nil {
			return idMap{}, err
		}
		for _, it := range mc.Metadata {
			for _, g := range it.Guid {
				if id, err := strconv.Atoi(strings.TrimPrefix(g.ID, "tmdb://")); err == nil && strings.HasPrefix(g.ID, "tmdb://") {
					if m.Items[id] == "" {
						m.Items[id] = it.RatingKey
					}
					if it.ViewCount > 0 {
						m.Watched[id] = true
					}
				}
			}
		}
		if len(mc.Metadata) < page || start+page >= mc.TotalSize {
			return m, nil
		}
	}
}

// plexWatched reports which picks his Plex account has played: one small read of the picks' items.
func plexWatched(ctx context.Context, px *plex.Client, section int, m idMap, picks []pick) (map[int]bool, error) {
	byRK := map[string]int{}
	for _, p := range picks {
		if rk := m.Items[p.TMDB]; rk != "" {
			byRK[rk] = p.TMDB
		}
	}
	if len(byRK) == 0 {
		return nil, nil
	}
	var mc struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			ViewCount int    `json:"viewCount"`
		} `json:"Metadata"`
	}
	path := fmt.Sprintf("/library/sections/%d/all?type=1&id=%s&%s", section, strings.Join(sortedKeys(byRK), ","), plexLean)
	if err := px.GetJSON(ctx, path, &mc); err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, it := range mc.Metadata {
		if it.ViewCount > 0 {
			out[byRK[it.RatingKey]] = true
		}
	}
	return out, nil
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// plexMembers lists a collection's films by its tag index, through the lean section listing: a collection's own
// /children took 108 s at load 20, this 0.18 s (2026-10-05).
func plexMembers(ctx context.Context, px *plex.Client, section, index int) ([]string, error) {
	var mc struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
		} `json:"Metadata"`
	}
	if err := px.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/all?type=1&collection=%d&%s", section, index, plexLean), &mc); err != nil {
		return nil, err
	}
	var out []string
	for _, it := range mc.Metadata {
		out = append(out, it.RatingKey)
	}
	return out, nil
}

// plexTarget is one collection this stage keeps: the household's, or a person's own shelf.
type plexTarget struct {
	Section     int
	Title       string
	Machine     string   // ProcessedMachineIdentifier, for server:// item URIs
	Labels      []string // beside marker
	Own, Shared bool     // its Home row on the owner's Home, on the Home of everyone the library is shared with
	After       string   // the hub its Home row follows in the section's order; "" = first
}

func (t plexTarget) uri(rks []string) string {
	return url.QueryEscape(fmt.Sprintf("server://%s/com.plexapp.plugins.library/library/metadata/%s", t.Machine,
		strings.Join(rks, ",")))
}

// syncPlex converges the collection on the picks: members, summary, labels, the day's poster, and its Home row at
// the top of the Movies section's hubs (or right after t.After). On his Home only Continue Watching, On Deck and the
// rows of libraries listed before Movies come above the household's (2026-10-05: Recently Added in Home Videos, while
// that library has new items).
// persist saves the state around the create, so a crash or a lost answer never leaves a second collection behind.
func syncPlex(ctx context.Context, log *slog.Logger, px *plex.Client, t plexTarget, m idMap, s *sinkState, day string,
	picks []pick, poster func() ([]byte, error), persist func() error) error {
	if len(m.Items) == 0 {
		return errors.New("plex: no ratingKey map yet")
	}
	want := m.desired(picks)
	c, ok := rememberedPlexCollection(ctx, px, s.ID)
	if !ok {
		titled, err := plexCollections(ctx, px, t)
		if err != nil {
			return err
		}
		keys := make([]string, len(titled))
		for i, tc := range titled {
			keys[i] = tc.RatingKey
		}
		i := slices.IndexFunc(titled, plexColl.owned)
		if i < 0 {
			lost, err := s.Pending.adopt(keys, func(id string) ([]string, error) {
				return plexMembers(ctx, px, t.Section, titled[slices.Index(keys, id)].Index)
			})
			if err != nil {
				return err
			}
			if i = slices.Index(keys, lost); i >= 0 {
				log.Info("adopting the collection a create with a lost answer made", "event", "plex_write", "action", "adopt", "id", lost)
			}
		}
		switch {
		case i >= 0:
			c = titled[i]
		case len(want) == 0:
			return nil
		default:
			if len(titled) > 0 {
				log.Info("a collection with our title is not ours; making our own", "event", "plex_collection_foreign", "title", t.Title)
			}
			s.Pending = &pendingCreate{Before: keys, Items: want}
			if err := persist(); err != nil {
				return err
			}
			var made struct {
				Metadata []plexColl `json:"Metadata"`
			}
			if err := px.DoJSON(ctx, "POST", fmt.Sprintf("/library/collections?type=1&smart=0&sectionId=%d&title=%s&uri=%s",
				t.Section, url.QueryEscape(t.Title), t.uri(want)), &made); err != nil {
				return err
			}
			if len(made.Metadata) != 1 || made.Metadata[0].RatingKey == "" {
				return errors.New("plex: the collection create answered no ratingKey")
			}
			s.ID, s.Pending, s.PosterOf = made.Metadata[0].RatingKey, nil, ""
			if err := persist(); err != nil {
				return err
			}
			log.Info("collection created", "event", "plex_write", "action", "create", "id", s.ID, "items", len(want))
			if c, ok = rememberedPlexCollection(ctx, px, s.ID); !ok {
				return fmt.Errorf("plex: collection %s not readable yet", s.ID)
			}
		}
	}
	id := c.RatingKey
	s.ID, s.Pending = id, nil
	if c.Title != t.Title { // renamed in place (2026-10-06: "Tonight" became "Tonight · Household"), never made anew
		if err := px.Do(ctx, "PUT", fmt.Sprintf("/library/sections/%d/all?type=18&id=%s&title.value=%s&title.locked=1",
			t.Section, id, url.QueryEscape(t.Title))); err != nil {
			return err
		}
		log.Info("collection renamed", "event", "plex_write", "action", "rename", "id", id, "from", c.Title, "to", t.Title)
	}
	// Labelled right after the create, so a run that lost its state still knows the collection, and a person's shelf
	// is filtered out of every other account's library from its first moments.
	have := c.labels()
	missing := slices.DeleteFunc(append([]string{marker}, t.Labels...), func(l string) bool {
		return slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, l) })
	})
	if len(missing) > 0 {
		q := fmt.Sprintf("/library/sections/%d/all?type=18&id=%s&label.locked=1", t.Section, id)
		for i, l := range append(have, missing...) {
			q += fmt.Sprintf("&label[%d].tag.tag=%s", i, url.QueryEscape(l))
		}
		if err := px.Do(ctx, "PUT", q); err != nil {
			return err
		}
		log.Info("collection labelled", "event", "plex_write", "action", "label", "id", id, "labels", strings.Join(missing, ","))
	}
	have, err := plexMembers(ctx, px, t.Section, c.Index)
	if err != nil {
		return err
	}
	add, remove := diff(want, have)
	if len(add)+len(remove) > 0 && !s.HubStale {
		// Saved before the edit, so the Home hub is still refreshed on a later run when a write after it fails, or
		// when the edit's own answer is lost after Plex made it.
		s.HubStale = true
		if err := persist(); err != nil {
			return err
		}
	}
	if len(add) > 0 {
		if err := px.Do(ctx, "PUT", "/library/collections/"+id+"/items?uri="+t.uri(add)); err != nil {
			return err
		}
		log.Info("collection items added", "event", "plex_write", "action", "add", "id", id, "items", strings.Join(add, ","))
	}
	for _, rk := range remove {
		if err := px.Do(ctx, "DELETE", "/library/collections/"+id+"/items/"+rk); err != nil {
			return err
		}
		log.Info("collection item removed", "event", "plex_write", "action", "remove", "id", id, "item", rk)
	}
	if text := summary(picks); text != c.Summary {
		if err := px.Do(ctx, "PUT", fmt.Sprintf("/library/sections/%d/all?type=18&id=%s&summary.value=%s&summary.locked=1",
			t.Section, id, url.QueryEscape(text))); err != nil {
			return err
		}
		log.Info("collection summary set", "event", "plex_write", "action", "summary", "id", id)
	}
	// One poster a day, showing the day's picks: Plex keeps every poster uploaded to an item, so redrawing it when a
	// watched pick leaves the row would add up to six a day to its poster picker, for good.
	if s.PosterOf != day && len(picks) > 0 {
		img, err := poster()
		if err != nil {
			return err
		}
		if err := px.Send(ctx, "POST", "/library/metadata/"+id+"/posters", "image/jpeg", img); err != nil {
			return err
		}
		s.PosterOf = day
		log.Info("collection poster uploaded", "event", "plex_write", "action", "poster", "id", id, "bytes", len(img))
	}
	return promotePlexHub(ctx, log, px, t, id, &s.HubStale)
}

// marker labels (Plex) or tags (Jellyfin) the collection this stage owns. A collection with the same title and no
// marker is his: it is never edited.
const marker = "projectionist"

type plexColl struct {
	RatingKey string `json:"ratingKey"`
	Index     int    `json:"index"` // the collection's tag id, which a section listing filters on
	Type      string `json:"type"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Label     []struct {
		Tag string `json:"tag"`
	} `json:"Label"`
}

func (c plexColl) labels() []string {
	var out []string
	for _, l := range c.Label {
		out = append(out, l.Tag)
	}
	return out
}

// owned matches the label as Plex does, ignoring case: Plex answered "Projectionist" for the "projectionist" it was sent.
func (c plexColl) owned() bool {
	return slices.ContainsFunc(c.labels(), func(l string) bool { return strings.EqualFold(l, marker) })
}

// rememberedPlexCollection is the collection this stage made, if it is still a collection, whatever its title. A
// deleted one answers 404; the title search after it surfaces a real outage.
func rememberedPlexCollection(ctx context.Context, px *plex.Client, id string) (plexColl, bool) {
	var mc struct {
		Metadata []plexColl `json:"Metadata"`
	}
	if id == "" || px.GetJSON(ctx, "/library/metadata/"+id, &mc) != nil || len(mc.Metadata) != 1 {
		return plexColl{}, false
	}
	c := mc.Metadata[0]
	return c, c.Type == "collection"
}

// plexCollections lists the section's collections with our title, labels included.
func plexCollections(ctx context.Context, px *plex.Client, t plexTarget) ([]plexColl, error) {
	var list struct {
		Metadata []plexColl `json:"Metadata"`
	}
	if err := px.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/collections", t.Section), &list); err != nil {
		return nil, err
	}
	var out []plexColl
	for _, c := range list.Metadata {
		if c.Title != t.Title {
			continue
		}
		var full struct {
			Metadata []plexColl `json:"Metadata"`
		}
		if err := px.GetJSON(ctx, "/library/metadata/"+c.RatingKey, &full); err != nil {
			return nil, err
		}
		out = append(out, full.Metadata...)
	}
	return out, nil
}

type plexHub struct {
	Identifier            string `json:"identifier"`
	PromotedToOwnHome     bool   `json:"promotedToOwnHome"`
	PromotedToSharedHome  bool   `json:"promotedToSharedHome"`
	PromotedToRecommended bool   `json:"promotedToRecommended"`
}

func hubIdent(section int, id string) string {
	return fmt.Sprintf("custom.collection.%d.%s", section, id)
}

// promotePlexHub shows the collection on the Homes t names (the household row on the owner's and on everyone's the
// library is shared with: its reasons name films, never a viewer, a rating or a review), never on the library's
// Recommended tab, and places it first in the Movies section's hub order, or right after t.After once that hub is up.
// Plex caches each Home hub answer and keeps it when a collection's members change (2026-10-05: one /hubs/promoted
// query listed the old picks 27 minutes after the edit); a manage PUT on the hub drops it, so a stale hub gets the
// flags again, and stale is cleared only once that PUT succeeds.
func promotePlexHub(ctx context.Context, log *slog.Logger, px *plex.Client, t plexTarget, id string, stale *bool) error {
	base := fmt.Sprintf("/hubs/sections/%d/manage", t.Section)
	ident := hubIdent(t.Section, id)
	bit := map[bool]int{false: 0, true: 1}
	vis := fmt.Sprintf("promotedToRecommended=0&promotedToOwnHome=%d&promotedToSharedHome=%d", bit[t.Own], bit[t.Shared])
	var mc struct {
		Hub []plexHub `json:"Hub"`
	}
	if err := px.GetJSON(ctx, base, &mc); err != nil {
		return err
	}
	at := slices.IndexFunc(mc.Hub, func(h plexHub) bool { return h.Identifier == ident })
	switch {
	case at < 0:
		if err := px.Do(ctx, "POST", base+"?metadataItemId="+id+"&"+vis); err != nil {
			return err
		}
		log.Info("home row promoted", "event", "plex_write", "action", "promote", "hub", ident)
	case mc.Hub[at] != plexHub{Identifier: ident, PromotedToOwnHome: t.Own, PromotedToSharedHome: t.Shared} || *stale:
		if err := px.Do(ctx, "PUT", base+"/"+ident+"?"+vis); err != nil {
			return err
		}
		log.Info("home row promoted", "event", "plex_write", "action", "promote", "hub", ident, "hub_stale", *stale)
		*stale = false
	}
	move, want := base+"/"+ident+"/move", 0 // no "after": first in the section's order
	if t.After != "" {
		j := slices.IndexFunc(mc.Hub, func(h plexHub) bool { return h.Identifier == t.After })
		if j < 0 { // what it follows is not up yet
			return nil
		}
		move, want = move+"?after="+url.QueryEscape(t.After), j+1
	}
	if at == want {
		return nil
	}
	if err := px.Do(ctx, "PUT", move); err != nil {
		return err
	}
	log.Info("home row moved", "event", "plex_write", "action", "move", "hub", ident, "from", at, "after", t.After)
	return nil
}

// ---- Jellyfin ----

type jfItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Type        string            `json:"Type"`
	ProviderIDs map[string]string `json:"ProviderIds"`
	Tags        []string          `json:"Tags"`
	UserData    struct {
		Played bool `json:"Played"`
	} `json:"UserData"`
}

type jfItems struct {
	Items []jfItem `json:"Items"`
}

func jellyfinMap(ctx context.Context, jf *jellyfin.Client, user string) (idMap, error) {
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := jf.GetJSON(ctx, "/Users", &users); err != nil {
		return idMap{}, err
	}
	m := idMap{Items: map[int]string{}, Watched: map[int]bool{}}
	for _, u := range users {
		if strings.EqualFold(u.Name, user) {
			m.UserID = u.ID
		}
	}
	if m.UserID == "" {
		return idMap{}, fmt.Errorf("jellyfin: no user %q", user)
	}
	var all, played jfItems
	// collapseBoxSetItems=false: by default Jellyfin answers a film in a collection with the collection, Tonight too
	const q = "/Items?IncludeItemTypes=Movie&Recursive=true&collapseBoxSetItems=false&Fields=ProviderIds&EnableImages=false"
	if err := jf.GetJSON(ctx, q+"&EnableUserData=false", &all); err != nil {
		return idMap{}, err
	}
	if err := jf.GetJSON(ctx, q+"&Filters=IsPlayed&userId="+m.UserID, &played); err != nil {
		return idMap{}, err
	}
	for _, it := range all.Items {
		if id, err := strconv.Atoi(it.ProviderIDs["Tmdb"]); err == nil && m.Items[id] == "" {
			m.Items[id] = it.ID
		}
	}
	for _, it := range played.Items {
		if id, err := strconv.Atoi(it.ProviderIDs["Tmdb"]); err == nil {
			m.Watched[id] = true
		}
	}
	return m, nil
}

func jellyfinWatched(ctx context.Context, jf *jellyfin.Client, m idMap, picks []pick) (map[int]bool, error) {
	ids := m.desired(picks)
	if len(ids) == 0 || m.UserID == "" {
		return nil, nil
	}
	var res jfItems
	if err := jf.GetJSON(ctx, "/Items?Fields=ProviderIds&EnableImages=false&userId="+m.UserID+"&Ids="+strings.Join(ids, ","), &res); err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, it := range res.Items {
		if id, err := strconv.Atoi(it.ProviderIDs["Tmdb"]); err == nil && it.UserData.Played {
			out[id] = true
		}
	}
	return out, nil
}

// syncJellyfin converges the "Tonight" BoxSet: members, overview (the reasons) and the day's poster. persist saves
// the state around the create, as for Plex.
func syncJellyfin(ctx context.Context, log *slog.Logger, jf *jellyfin.Client, title string, m idMap, s *sinkState,
	day string, picks []pick, poster func() ([]byte, error), persist func() error) error {
	if len(m.Items) == 0 || m.UserID == "" {
		return errors.New("jellyfin: no item map yet")
	}
	want := m.desired(picks)
	members := func(box string) ([]string, error) { return jellyfinMembers(ctx, jf, box, m.UserID) }
	id, titled, err := findBoxSet(ctx, jf, title, s.ID)
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = s.Pending.adopt(titled, members); err != nil {
			return err
		}
		if id != "" {
			log.Info("adopting the collection a create with a lost answer made", "event", "jellyfin_write", "action", "adopt", "id", id)
		}
	}
	if id == "" {
		if len(want) == 0 {
			return nil
		}
		s.Pending = &pendingCreate{Before: titled, Items: want}
		if err := persist(); err != nil {
			return err
		}
		var created struct {
			ID string `json:"Id"`
		}
		if err := jf.PostJSON(ctx, "/Collections?IsLocked=true&Name="+url.QueryEscape(title)+"&Ids="+strings.Join(want, ","), nil, &created); err != nil {
			return err
		}
		if created.ID == "" {
			return errors.New("jellyfin: collection create returned no id")
		}
		id, s.ID, s.Pending, s.PosterOf = created.ID, created.ID, nil, ""
		if err := persist(); err != nil {
			return err
		}
		log.Info("collection created", "event", "jellyfin_write", "action", "create", "id", id, "items", len(want))
	}
	s.ID, s.Pending = id, nil
	have, err := members(id)
	if err != nil {
		return err
	}
	add, remove := diff(want, have)
	if len(add) > 0 {
		if err := jf.PostJSON(ctx, "/Collections/"+id+"/Items?Ids="+strings.Join(add, ","), nil, nil); err != nil {
			return err
		}
		log.Info("collection items added", "event", "jellyfin_write", "action", "add", "id", id, "items", strings.Join(add, ","))
	}
	if len(remove) > 0 {
		if err := jf.Delete(ctx, "/Collections/"+id+"/Items?Ids="+strings.Join(remove, ",")); err != nil {
			return err
		}
		log.Info("collection items removed", "event", "jellyfin_write", "action", "remove", "id", id, "items", strings.Join(remove, ","))
	}
	var item map[string]any
	if err := jf.GetJSON(ctx, "/Items/"+id+"?userId="+m.UserID, &item); err != nil {
		return err
	}
	tags, _ := item["Tags"].([]any)
	tagged := slices.Contains(tags, any(marker))
	if text := summary(picks); item["Overview"] != text || !tagged || item["Name"] != title {
		renamed := item["Name"] != title
		item["Overview"], item["Name"] = text, title
		if !tagged {
			item["Tags"] = append(tags, marker)
		}
		if err := jf.PostJSON(ctx, "/Items/"+id, item, nil); err != nil {
			return err
		}
		log.Info("collection overview set", "event", "jellyfin_write", "action", "overview", "id", id, "tag_added", !tagged, "renamed", renamed)
	}
	if s.PosterOf != day && len(picks) > 0 { // the same poster as Plex's
		img, err := poster()
		if err != nil {
			return err
		}
		body := []byte(base64.StdEncoding.EncodeToString(img))
		if err := jf.PostBody(ctx, "/Items/"+id+"/Images/Primary", "image/jpeg", body); err != nil {
			return err
		}
		s.PosterOf = day
		log.Info("collection poster uploaded", "event", "jellyfin_write", "action", "poster", "id", id, "bytes", len(img))
	}
	return nil
}

// findBoxSet returns the BoxSet this stage made (the remembered one if it still is a BoxSet, whatever its title, else
// one with our title and our tag); when there is none, the ids of the untagged BoxSets with our title.
func findBoxSet(ctx context.Context, jf *jellyfin.Client, title, remembered string) (id string, titled []string, err error) {
	var res jfItems
	if remembered != "" {
		if err := jf.GetJSON(ctx, "/Items?EnableImages=false&Ids="+remembered, &res); err != nil {
			return "", nil, err
		}
		for _, it := range res.Items {
			if it.ID == remembered && it.Type == "BoxSet" {
				return it.ID, nil, nil
			}
		}
	}
	res.Items = nil
	if err := jf.GetJSON(ctx, "/Items?IncludeItemTypes=BoxSet&Recursive=true&EnableImages=false&Fields=Tags&SearchTerm="+url.QueryEscape(title), &res); err != nil {
		return "", nil, err
	}
	for _, it := range res.Items {
		if it.Name != title {
			continue
		}
		if slices.Contains(it.Tags, marker) {
			return it.ID, nil, nil
		}
		titled = append(titled, it.ID)
	}
	return "", titled, nil
}

func jellyfinMembers(ctx context.Context, jf *jellyfin.Client, box, userID string) ([]string, error) {
	var children jfItems
	if err := jf.GetJSON(ctx, "/Items?EnableImages=false&ParentId="+box+"&userId="+userID, &children); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range children.Items {
		out = append(out, c.ID)
	}
	return out, nil
}
