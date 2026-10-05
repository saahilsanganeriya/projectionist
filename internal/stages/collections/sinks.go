package collections

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/library"
	"projectionist/internal/plex"
)

// A sink is an app that shows the collections. Item and collection ids are the app's own.
type sink interface {
	Name() string
	Address(ctx context.Context) (map[int]string, error) // TMDB id (from the file path) → item id
	Collections(ctx context.Context) (map[string]held, error)
	Members(ctx context.Context, id string) (map[string]bool, error)
	Create(ctx context.Context, name string, items []string) error
	Add(ctx context.Context, id string, items []string) error
	SetSort(ctx context.Context, id, sort string) error
}

// posterSink is a sink that takes the backed-up collection posters.
type posterSink interface {
	Poster(ctx context.Context, id string, jpeg []byte) error
}

// held is a collection as an app holds it.
type held struct {
	ID, Sort string
	Count    int // members, as the app counts them
}

const (
	addressTTL   = 24 * time.Hour // the item list is re-read once a day...
	addressRetry = time.Hour      // ...or hourly while a film on disk is missing from it (the app has not scanned it yet)
	backoffBase  = 15 * time.Minute
	backoffMax   = 24 * time.Hour
	alertAfter   = 24 * time.Hour
)

// pageSize is how many films one item-list request asks for. The whole Movies section in one answer (2,200 films,
// 5 MB from Plex) took both apps about a minute on the loaded NAS, past their 20 s client timeout; a 200-film page
// took 3-13 s. A var so tests can page through a small library.
var pageSize = 200

func (r *run) sinks() ([]sink, error) {
	var out []sink
	for _, n := range r.opt.Sinks {
		switch n {
		case "plex":
			out = append(out, &plexSink{c: r.env.Plex, section: r.opt.PlexSection})
		case "jellyfin":
			out = append(out, &jellyfinSink{c: r.env.Jellyfin})
		default:
			return nil, fmt.Errorf("collections: unknown sink %q (plex, jellyfin)", n)
		}
	}
	return out, nil
}

type backoff struct {
	Failures   int       `json:"failures"`
	Since      time.Time `json:"since"`
	RetryAfter time.Time `json:"retry_after"`
	Err        string    `json:"err"`
}

// apply reconciles one sink. Its failures are logged and backed off, never returned: a sink never fails the run.
func (r *run) apply(ctx context.Context, s sink) {
	var b backoff
	if _, err := r.env.Store.GetJSON(ctx, name, "backoff/"+s.Name(), &b); err != nil {
		r.env.Log.Error("backoff state unreadable", "event", "store_error", "sink", s.Name(), "err", err)
	}
	now := r.env.Now()
	switch {
	case now.Before(b.RetryAfter):
		r.rep.Count(s.Name()+"_waiting", 1)
		r.env.Log.Info("sink backing off", "event", "sink_waiting", "sink", s.Name(), "retry_after", b.RetryAfter.UTC().Format(time.RFC3339))
	default:
		r.skipped, r.firstSkip = 0, ""
		err := r.reconcile(ctx, s)
		if r.skipped > 0 {
			r.rep.Alert("%s: %d collection change(s) refused or not possible, retried next run; first: %s", s.Name(), r.skipped, r.firstSkip)
		}
		switch {
		case err == nil:
			b = backoff{}
		case ctx.Err() != nil:
			return // shutting down; not the sink's fault
		default:
			if b.Failures == 0 {
				b.Since = now
			}
			b.Failures++
			wait := backoffBase
			for i := 1; i < b.Failures && wait < backoffMax; i++ {
				wait *= 2
			}
			b.RetryAfter, b.Err = now.Add(min(wait, backoffMax)), err.Error()
			r.rep.Count(s.Name()+"_failed", 1)
			r.env.Log.Warn("sink failed; retrying later", "event", "sink_failed", "sink", s.Name(), "attempt", b.Failures,
				"retry_after", b.RetryAfter.UTC().Format(time.RFC3339), "err", err)
		}
		if err := r.env.Store.PutJSON(ctx, name, "backoff/"+s.Name(), b); err != nil {
			r.env.Log.Error("backoff state not saved", "event", "store_error", "sink", s.Name(), "err", err)
		}
	}
	if b.Failures > 0 && now.Sub(b.Since) >= alertAfter {
		r.rep.Alert("%s has failed collection updates since %s: %s", s.Name(), b.Since.UTC().Format(time.RFC3339), b.Err)
	}
}

type addressCache struct {
	At    time.Time      `json:"at"`
	Items map[int]string `json:"items"`
}

// address maps TMDB ids to the sink's item ids: one list call a day, cached, plus an hourly re-read while a wanted
// film on disk is missing from it.
func (r *run) address(ctx context.Context, s sink) (map[int]string, error) {
	var cache addressCache
	found, err := r.env.Store.GetJSON(ctx, name, "address/"+s.Name(), &cache)
	if err != nil {
		return nil, err
	}
	age := r.env.Now().Sub(cache.At)
	if found && age < addressTTL && (age < addressRetry || !r.missing(cache.Items)) {
		return cache.Items, nil
	}
	items, err := s.Address(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.env.Store.PutJSON(ctx, name, "address/"+s.Name(), addressCache{At: r.env.Now(), Items: items}); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *run) missing(items map[int]string) bool {
	for _, c := range r.targets {
		for _, id := range c.Members {
			if r.disk[id] && items[id] == "" {
				return true
			}
		}
	}
	return false
}

// reconcile sends a sink the missing creates, member adds, sort titles and posters. A change the app refuses is
// skipped for that collection only; any other error stops the sink for this run.
func (r *run) reconcile(ctx context.Context, s sink) error {
	items, err := r.address(ctx, s)
	if err != nil {
		return err
	}
	have, err := s.Collections(ctx)
	if err != nil {
		return err
	}
	addressed := func(c *collection) []string {
		var ids []string
		seen := map[string]bool{}
		for _, id := range c.Members {
			if it := items[id]; it != "" && !seen[it] {
				seen[it] = true
				ids = append(ids, it)
			}
		}
		return ids
	}
	created := 0
	for _, c := range r.targets {
		ids := addressed(c)
		if _, ok := have[key(c.Name)]; ok || len(ids) == 0 {
			continue
		}
		ok, err := r.write(s, "create", c, len(ids), func() error { return s.Create(ctx, c.Name, ids) })
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		created++
		if r.opt.DryRun { // what the real run does next to the collection it just made
			if c.Sort != "" {
				r.write(s, "sort", c, 0, nil) // a dry run never calls send, so it cannot fail
			}
			if err := r.poster(ctx, s, c, ""); err != nil {
				return err
			}
		}
	}
	if created > 0 && !r.opt.DryRun {
		if have, err = s.Collections(ctx); err != nil {
			return err
		}
	}
	for _, c := range r.targets {
		h, ok := have[key(c.Name)]
		if !ok {
			continue
		}
		if err := r.members(ctx, s, c, h, addressed(c)); err != nil {
			return err
		}
		if c.Sort != "" && !strings.EqualFold(h.Sort, c.Sort) {
			if _, err := r.write(s, "sort", c, 0, func() error { return s.SetSort(ctx, h.ID, c.Sort) }); err != nil {
				return err
			}
		}
		if err := r.poster(ctx, s, c, h.ID); err != nil {
			return err
		}
	}
	return nil
}

// seenMembers is what this stage last saw (and added) in one app collection.
type seenMembers struct {
	Count int      `json:"count"`
	Items []string `json:"items"`
}

// members adds what an app collection lacks. Its member list is read only when the app's count moved since the last
// run or a wanted item is new, so a quiet run asks each app for one collection list, not one call per collection.
func (r *run) members(ctx context.Context, s sink, c *collection, h held, want []string) error {
	var seen seenMembers
	found, err := r.env.Store.GetJSON(ctx, name, "members/"+s.Name()+"/"+h.ID, &seen)
	if err != nil {
		return err
	}
	if found && seen.Count == h.Count && !slices.ContainsFunc(want, func(id string) bool { return !slices.Contains(seen.Items, id) }) {
		return nil
	}
	cur, err := s.Members(ctx, h.ID)
	if err != nil {
		return err
	}
	var add []string
	for _, id := range want {
		if !cur[id] {
			add = append(add, id)
		}
	}
	if len(add) > 0 {
		if ok, err := r.write(s, "add", c, len(add), func() error { return s.Add(ctx, h.ID, add) }); err != nil || !ok {
			return err // refused: nothing recorded, so the next run asks again
		}
	}
	if r.opt.DryRun {
		return nil
	}
	seen = seenMembers{Count: len(cur) + len(add), Items: add}
	for id := range cur {
		seen.Items = append(seen.Items, id)
	}
	return r.env.Store.PutJSON(ctx, name, "members/"+s.Name()+"/"+h.ID, seen)
}

// write sends one change, or only logs it in a dry run. n is the number of members it carries. ok is false when the
// app refused the change (recorded by isolate); err is any other failure.
func (r *run) write(s sink, op string, c *collection, n int, send func() error) (ok bool, err error) {
	event := op
	if r.opt.DryRun {
		event = "would_" + op
	} else if err := send(); err != nil {
		return false, r.isolate(s, op, c, err)
	}
	r.rep.Count(s.Name()+"_"+op, 1)
	if n > 0 {
		r.rep.Count(s.Name()+"_"+op+"_members", n)
	}
	r.env.Log.Info("collection "+event, "event", event, "sink", s.Name(), "collection", c.Name, "members", n, "sort", c.Sort)
	return true, nil
}

// isolate turns the app's refusal of one collection's request into a skip, so the run goes on to the next
// collection; any other error is returned and backs the whole sink off.
func (r *run) isolate(s sink, op string, c *collection, err error) error {
	if !refusal(err) {
		return err
	}
	r.skip(s, op, c, err)
	return nil
}

func (r *run) skip(s sink, op string, c *collection, err error) {
	r.skipped++
	if r.firstSkip == "" {
		r.firstSkip = fmt.Sprintf("%s %q: %v", op, c.Name, err)
	}
	r.rep.Count(s.Name()+"_"+op+"_skipped", 1)
	r.env.Log.Warn("collection change skipped", "event", op+"_skipped", "sink", s.Name(), "collection", c.Name, "err", err)
}

// refusal reports whether the app answered with a 4xx: it rejected that one request, which retrying cannot fix and
// which says nothing about the next collection. Transport errors, timeouts and 5xx mean the app is unwell. Both
// clients end a non-2xx error with "HTTP <code>".
func refusal(err error) bool {
	msg := err.Error()
	i := strings.LastIndex(msg, "HTTP ")
	if i < 0 {
		return false
	}
	code, convErr := strconv.Atoi(msg[i+len("HTTP "):])
	return convErr == nil && code/100 == 4
}

// poster uploads an owner collection's poster once per app collection: recorded, because an app may draw its own
// poster for a collection, so the app's image list cannot tell whether ours went up. id "" is a dry run's planned
// collection. A poster file missing from poster_dir (the deploy copy skipped) is skipped and alerted.
func (r *run) poster(ctx context.Context, s sink, c *collection, id string) error {
	ps, ok := s.(posterSink)
	if !ok || c.Poster == "" {
		return nil
	}
	if id != "" {
		var done string
		if found, err := r.env.Store.GetJSON(ctx, name, "poster/"+s.Name()+"/"+id, &done); err != nil || found {
			return err
		}
	}
	jpeg, err := os.ReadFile(c.Poster)
	if errors.Is(err, fs.ErrNotExist) {
		r.skip(s, "poster", c, err)
		return nil
	}
	if err != nil {
		return err
	}
	sent, err := r.write(s, "poster", c, 0, func() error { return ps.Poster(ctx, id, jpeg) })
	if err != nil || !sent || r.opt.DryRun {
		return err
	}
	return r.env.Store.PutJSON(ctx, name, "poster/"+s.Name()+"/"+id, c.Poster)
}

// ---- Plex ----

type plexSink struct {
	c       *plex.Client
	section int
}

func (*plexSink) Name() string { return "plex" }

func (p *plexSink) Address(ctx context.Context) (map[int]string, error) {
	out := map[int]string{}
	for start := 0; ; start += pageSize {
		var mc struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
				Media     []struct {
					Part []struct {
						File string `json:"file"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		}
		path := fmt.Sprintf("/library/sections/%d/all?type=1&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", p.section, start, pageSize)
		if err := p.c.GetJSON(ctx, path, &mc); err != nil {
			return nil, err
		}
		for _, it := range mc.Metadata {
			for _, m := range it.Media {
				for _, part := range m.Part {
					addressFile(out, part.File, it.RatingKey)
				}
			}
		}
		if len(mc.Metadata) < pageSize {
			return out, nil
		}
	}
}

// addressFile maps the TMDB id in a file's folder name to an app item; the first item for an id wins.
func addressFile(out map[int]string, file, item string) {
	m, ok := library.ParseMovie(file)
	if !ok || m.ID.Source != "tmdb" {
		return
	}
	if id, err := strconv.Atoi(m.ID.Value); err == nil && out[id] == "" {
		out[id] = item
	}
}

func (p *plexSink) Collections(ctx context.Context) (map[string]held, error) {
	var mc struct {
		Metadata []struct {
			RatingKey  string `json:"ratingKey"`
			Title      string `json:"title"`
			TitleSort  string `json:"titleSort"`
			ChildCount int    `json:"childCount"`
		} `json:"Metadata"`
	}
	if err := p.c.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/collections", p.section), &mc); err != nil {
		return nil, err
	}
	out := map[string]held{}
	for _, c := range mc.Metadata {
		out[key(c.Title)] = held{ID: c.RatingKey, Sort: c.TitleSort, Count: c.ChildCount}
	}
	return out, nil
}

func (p *plexSink) Members(ctx context.Context, id string) (map[string]bool, error) {
	var mc struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
		} `json:"Metadata"`
	}
	if err := p.c.GetJSON(ctx, "/library/collections/"+url.PathEscape(id)+"/children", &mc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range mc.Metadata {
		out[m.RatingKey] = true
	}
	return out, nil
}

// uri names library items the way Plex's collection endpoints take them; the machine id comes from Preferences.xml.
func (p *plexSink) uri(items []string) (string, error) {
	prefs, err := p.c.Prefs()
	if err != nil {
		return "", err
	}
	machine := prefs["ProcessedMachineIdentifier"]
	if machine == "" {
		return "", errors.New("plex: Preferences.xml has no ProcessedMachineIdentifier")
	}
	return "server://" + machine + "/com.plexapp.plugins.library/library/metadata/" + strings.Join(items, ","), nil
}

func (p *plexSink) Create(ctx context.Context, name string, items []string) error {
	u, err := p.uri(items)
	if err != nil {
		return err
	}
	q := url.Values{"type": {"1"}, "title": {name}, "smart": {"0"}, "sectionId": {strconv.Itoa(p.section)}, "uri": {u}}
	return p.c.Do(ctx, http.MethodPost, "/library/collections?"+q.Encode())
}

func (p *plexSink) Add(ctx context.Context, id string, items []string) error {
	u, err := p.uri(items)
	if err != nil {
		return err
	}
	return p.c.Do(ctx, http.MethodPut, "/library/collections/"+url.PathEscape(id)+"/items?"+url.Values{"uri": {u}}.Encode())
}

func (p *plexSink) SetSort(ctx context.Context, id, sort string) error {
	q := url.Values{"type": {"18"}, "id": {id}, "titleSort.value": {sort}, "titleSort.locked": {"1"}}
	return p.c.Do(ctx, http.MethodPut, fmt.Sprintf("/library/sections/%d/all?%s", p.section, q.Encode()))
}

// Plex takes no posters from here: the owner's collections already show them there, where the export came from.

// ---- Jellyfin ----

type jellyfinSink struct {
	c    *jellyfin.Client
	user string // an administrator's id, read once per run
}

func (*jellyfinSink) Name() string { return "jellyfin" }

type jfItems struct {
	Items []struct {
		ID         string `json:"Id"`
		Name       string `json:"Name"`
		Path       string `json:"Path"`
		SortName   string `json:"SortName"`
		ChildCount int    `json:"ChildCount"`
	} `json:"Items"`
}

func (j *jellyfinSink) Address(ctx context.Context) (map[int]string, error) {
	out := map[int]string{}
	for start := 0; ; start += pageSize {
		var res jfItems
		if err := j.c.GetJSON(ctx, fmt.Sprintf("/Items?IncludeItemTypes=Movie&Recursive=true&Fields=Path&StartIndex=%d&Limit=%d", start, pageSize), &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			addressFile(out, it.Path, it.ID)
		}
		if len(res.Items) < pageSize {
			return out, nil
		}
	}
}

// Collections and Members ask as an administrator: Jellyfin 12.1 leaves ChildCount out of a BoxSet, and lists none
// of its children, when no userId is given.
func (j *jellyfinSink) Collections(ctx context.Context) (map[string]held, error) {
	user, err := j.admin(ctx)
	if err != nil {
		return nil, err
	}
	var res jfItems
	if err := j.c.GetJSON(ctx, "/Items?IncludeItemTypes=BoxSet&Recursive=true&Fields=SortName,ChildCount&userId="+url.QueryEscape(user), &res); err != nil {
		return nil, err
	}
	out := map[string]held{}
	for _, it := range res.Items {
		out[key(it.Name)] = held{ID: it.ID, Sort: it.SortName, Count: it.ChildCount}
	}
	return out, nil
}

func (j *jellyfinSink) Members(ctx context.Context, id string) (map[string]bool, error) {
	user, err := j.admin(ctx)
	if err != nil {
		return nil, err
	}
	var res jfItems
	if err := j.c.GetJSON(ctx, "/Items?ParentId="+url.QueryEscape(id)+"&userId="+url.QueryEscape(user), &res); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, it := range res.Items {
		out[it.ID] = true
	}
	return out, nil
}

func (j *jellyfinSink) Create(ctx context.Context, name string, items []string) error {
	q := url.Values{"name": {name}, "ids": {strings.Join(items, ",")}}
	return j.c.PostJSON(ctx, "/Collections?"+q.Encode(), nil, nil)
}

func (j *jellyfinSink) Add(ctx context.Context, id string, items []string) error {
	return j.c.PostJSON(ctx, "/Collections/"+url.PathEscape(id)+"/Items?"+url.Values{"ids": {strings.Join(items, ",")}}.Encode(), nil, nil)
}

// admin is the administrator the stage asks as (see Collections and SetSort), looked up once per run.
func (j *jellyfinSink) admin(ctx context.Context) (string, error) {
	if j.user != "" {
		return j.user, nil
	}
	var users []struct {
		ID     string `json:"Id"`
		Policy struct {
			IsAdministrator bool `json:"IsAdministrator"`
		} `json:"Policy"`
	}
	if err := j.c.GetJSON(ctx, "/Users", &users); err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Policy.IsAdministrator {
			j.user = u.ID
			return j.user, nil
		}
	}
	return "", errors.New("jellyfin: no administrator account")
}

// SetSort posts the item back whole: Jellyfin's update replaces every field, so a partial body would blank the rest.
func (j *jellyfinSink) SetSort(ctx context.Context, id, sort string) error {
	user, err := j.admin(ctx)
	if err != nil {
		return err
	}
	var item map[string]json.RawMessage
	if err := j.c.GetJSON(ctx, "/Items/"+url.PathEscape(id)+"?userId="+url.QueryEscape(user), &item); err != nil {
		return err
	}
	raw, err := json.Marshal(sort)
	if err != nil {
		return err
	}
	item["ForcedSortName"] = raw
	return j.c.PostJSON(ctx, "/Items/"+url.PathEscape(id), item, nil)
}

// Poster uploads the image base64-encoded: Jellyfin's SetItemImage decodes its body through FromBase64Transform,
// whatever the Content-Type says, and the Content-Type names the image format.
func (j *jellyfinSink) Poster(ctx context.Context, id string, jpeg []byte) error {
	body := base64.StdEncoding.AppendEncode(nil, jpeg)
	return j.c.PostBody(ctx, "/Items/"+url.PathEscape(id)+"/Images/Primary", "image/jpeg", body)
}
