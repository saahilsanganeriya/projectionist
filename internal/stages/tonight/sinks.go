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
	ID           string    `json:"id,omitempty"`         // Plex collection ratingKey / Jellyfin BoxSet id
	PosterDay    string    `json:"poster_day,omitempty"` // the day whose poster was last uploaded
	Fails        int       `json:"fails,omitempty"`
	RetryAt      time.Time `json:"retry_at,omitzero"`
	FailingSince time.Time `json:"failing_since,omitzero"`
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
		path := fmt.Sprintf("/library/sections/%d/all?type=1&includeGuids=1&excludeFields=summary,tagline"+
			"&excludeElements=Media,Genre,Country,Director,Writer,Role,Collection,Producer,Label,Image"+
			"&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", section, start, page)
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
func plexWatched(ctx context.Context, px *plex.Client, m idMap, picks []pick) (map[int]bool, error) {
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
	if err := px.GetJSON(ctx, "/library/metadata/"+strings.Join(sortedKeys(byRK), ","), &mc); err != nil {
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

type plexTarget struct {
	Section int
	Title   string
	Machine string // ProcessedMachineIdentifier, for server:// item URIs
}

func (t plexTarget) uri(rks []string) string {
	return url.QueryEscape(fmt.Sprintf("server://%s/com.plexapp.plugins.library/library/metadata/%s", t.Machine,
		strings.Join(rks, ",")))
}

// syncPlex converges the collection on the picks: members, summary, the day's poster, and its Home row at the
// top of the Movies section's hubs (directly below Continue Watching and On Deck on his Home).
func syncPlex(ctx context.Context, log *slog.Logger, px *plex.Client, t plexTarget, m idMap, s *sinkState, day string,
	picks []pick, poster func() ([]byte, error)) error {
	if len(m.Items) == 0 {
		return errors.New("plex: no ratingKey map yet")
	}
	want := m.desired(picks)
	id, current, err := findPlexCollection(ctx, px, t, s.ID)
	if err != nil {
		return err
	}
	if id == "" {
		if len(want) == 0 {
			return nil
		}
		if err := px.Do(ctx, "POST", fmt.Sprintf("/library/collections?type=1&smart=0&sectionId=%d&title=%s&uri=%s",
			t.Section, url.QueryEscape(t.Title), t.uri(want))); err != nil {
			return err
		}
		if id, current, err = findPlexCollection(ctx, px, t, ""); err != nil || id == "" {
			return errors.Join(errors.New("plex: created collection not found"), err)
		}
		s.PosterDay = ""
		log.Info("collection created", "event", "plex_write", "action", "create", "id", id, "items", len(want))
	}
	s.ID = id
	var children struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
		} `json:"Metadata"`
	}
	if err := px.GetJSON(ctx, "/library/collections/"+id+"/children", &children); err != nil {
		return err
	}
	var have []string
	for _, c := range children.Metadata {
		have = append(have, c.RatingKey)
	}
	add, remove := diff(want, have)
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
	if text := summary(picks); text != current {
		if err := px.Do(ctx, "PUT", fmt.Sprintf("/library/sections/%d/all?type=18&id=%s&summary.value=%s&summary.locked=1",
			t.Section, id, url.QueryEscape(text))); err != nil {
			return err
		}
		log.Info("collection summary set", "event", "plex_write", "action", "summary", "id", id)
	}
	if s.PosterDay != day && len(picks) > 0 {
		img, err := poster()
		if err != nil {
			return err
		}
		if err := px.Send(ctx, "POST", "/library/metadata/"+id+"/posters", "image/jpeg", img); err != nil {
			return err
		}
		s.PosterDay = day
		log.Info("collection poster uploaded", "event", "plex_write", "action", "poster", "id", id, "bytes", len(img))
	}
	return promotePlexHub(ctx, log, px, t.Section, id)
}

// findPlexCollection returns the collection's ratingKey and summary: the remembered key if it still names our
// collection, else one found by title, else "" (a crash after the create finds it here instead of making two).
func findPlexCollection(ctx context.Context, px *plex.Client, t plexTarget, remembered string) (id, summary string, err error) {
	type coll struct {
		RatingKey string `json:"ratingKey"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Summary   string `json:"summary"`
	}
	var mc struct {
		Metadata []coll `json:"Metadata"`
	}
	if remembered != "" { // a deleted collection answers 404: fall through to the search, which surfaces an outage
		err := px.GetJSON(ctx, "/library/metadata/"+remembered, &mc)
		if err == nil && len(mc.Metadata) == 1 && mc.Metadata[0].Type == "collection" && mc.Metadata[0].Title == t.Title {
			return remembered, mc.Metadata[0].Summary, nil
		}
	}
	mc.Metadata = nil
	if err := px.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/collections", t.Section), &mc); err != nil {
		return "", "", err
	}
	for _, c := range mc.Metadata {
		if c.Title == t.Title {
			return c.RatingKey, c.Summary, nil
		}
	}
	return "", "", nil
}

type plexHub struct {
	Identifier        string `json:"identifier"`
	PromotedToOwnHome bool   `json:"promotedToOwnHome"`
}

// promotePlexHub shows the collection on his own Home (not his friends') and moves it to the top of the Movies
// section's hub order.
func promotePlexHub(ctx context.Context, log *slog.Logger, px *plex.Client, section int, id string) error {
	base := fmt.Sprintf("/hubs/sections/%d/manage", section)
	ident := fmt.Sprintf("custom.collection.%d.%s", section, id)
	const vis = "promotedToRecommended=1&promotedToOwnHome=1&promotedToSharedHome=0"
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
	case !mc.Hub[at].PromotedToOwnHome:
		if err := px.Do(ctx, "PUT", base+"/"+ident+"?"+vis); err != nil {
			return err
		}
		log.Info("home row promoted", "event", "plex_write", "action", "promote", "hub", ident)
	}
	if at == 0 {
		return nil
	}
	if err := px.Do(ctx, "PUT", base+"/"+ident+"/move"); err != nil { // no "after": first in the section's order
		return err
	}
	log.Info("home row moved to the top", "event", "plex_write", "action", "move", "hub", ident, "from", at)
	return nil
}

// ---- Jellyfin ----

type jfItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Type        string            `json:"Type"`
	ProviderIDs map[string]string `json:"ProviderIds"`
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
	const q = "/Items?IncludeItemTypes=Movie&Recursive=true&Fields=ProviderIds&EnableImages=false"
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

// syncJellyfin converges the "Tonight" BoxSet: members, overview (the reasons) and the day's poster.
func syncJellyfin(ctx context.Context, log *slog.Logger, jf *jellyfin.Client, title string, m idMap, s *sinkState,
	day string, picks []pick, poster func() ([]byte, error)) error {
	if len(m.Items) == 0 || m.UserID == "" {
		return errors.New("jellyfin: no item map yet")
	}
	want := m.desired(picks)
	id, err := findBoxSet(ctx, jf, title, s.ID)
	if err != nil {
		return err
	}
	if id == "" {
		if len(want) == 0 {
			return nil
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
		id, s.PosterDay = created.ID, ""
		log.Info("collection created", "event", "jellyfin_write", "action", "create", "id", id, "items", len(want))
	}
	s.ID = id
	var children jfItems
	if err := jf.GetJSON(ctx, "/Items?EnableImages=false&ParentId="+id+"&userId="+m.UserID, &children); err != nil {
		return err
	}
	var have []string
	for _, c := range children.Items {
		have = append(have, c.ID)
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
	if text := summary(picks); item["Overview"] != text {
		item["Overview"] = text
		if err := jf.PostJSON(ctx, "/Items/"+id, item, nil); err != nil {
			return err
		}
		log.Info("collection overview set", "event", "jellyfin_write", "action", "overview", "id", id)
	}
	if s.PosterDay != day && len(picks) > 0 {
		img, err := poster()
		if err != nil {
			return err
		}
		body := []byte(base64.StdEncoding.EncodeToString(img))
		if err := jf.PostBody(ctx, "/Items/"+id+"/Images/Primary", "image/jpeg", body); err != nil {
			return err
		}
		s.PosterDay = day
		log.Info("collection poster uploaded", "event", "jellyfin_write", "action", "poster", "id", id, "bytes", len(img))
	}
	return nil
}

func findBoxSet(ctx context.Context, jf *jellyfin.Client, title, remembered string) (string, error) {
	var res jfItems
	if remembered != "" {
		if err := jf.GetJSON(ctx, "/Items?EnableImages=false&Ids="+remembered, &res); err != nil {
			return "", err
		}
		for _, it := range res.Items {
			if it.ID == remembered && it.Type == "BoxSet" && it.Name == title {
				return it.ID, nil
			}
		}
	}
	res.Items = nil
	if err := jf.GetJSON(ctx, "/Items?IncludeItemTypes=BoxSet&Recursive=true&EnableImages=false&SearchTerm="+url.QueryEscape(title), &res); err != nil {
		return "", err
	}
	for _, it := range res.Items {
		if it.Name == title {
			return it.ID, nil
		}
	}
	return "", nil
}
