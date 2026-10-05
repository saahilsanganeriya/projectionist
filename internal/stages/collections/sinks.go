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
	"projectionist/internal/stage"
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
	ID, Sort string // Sort: the sort title the app was given (Jellyfin's ForcedSortName), not a form it derives
	Count    int    // members, as the app counts them
}

const (
	addressTTL   = 24 * time.Hour // the item list is read once a day...
	addressRetry = time.Hour      // ...and early, at most hourly, while it lacks a wanted film on disk...
	lookTwice    = 2              // ...until that many reads in a row have lacked it: then it waits for the daily read
	backoffBase  = 15 * time.Minute
	backoffMax   = 24 * time.Hour
	alertAfter   = 24 * time.Hour
)

// pageSize is how many films one item-list request asks for. The whole Movies section in one answer (2,200 films,
// 5 MB from Plex) took both apps about a minute on the loaded NAS, past their 20 s client timeout; a 200-film page
// took 3-13 s. A var so tests can page through a small library.
var pageSize = 200

// errBudget ends a sink's pass when its share of the run is spent. What it sent is kept; the next run carries on.
var errBudget = errors.New("time budget spent")

// sinks builds each app's sink on its own client, bounded by call_timeout: the shared clients keep their short bound
// for the viewer gate and the other stages, and nothing waits on these calls but this stage's own budget.
func (r *run) sinks() ([]sink, error) {
	var out []sink
	for _, n := range r.opt.Sinks {
		switch n {
		case "plex":
			out = append(out, &plexSink{c: r.env.Plex.WithTimeout(r.opt.CallTimeout), section: r.opt.PlexSection})
		case "jellyfin":
			out = append(out, &jellyfinSink{c: r.env.Jellyfin.WithTimeout(r.opt.CallTimeout)})
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

// fail records one more failure: the wait doubles from 15 minutes up to a day.
func (b *backoff) fail(now time.Time, err error) {
	if b.Failures == 0 {
		b.Since = now
	}
	b.Failures++
	wait := backoffBase
	for i := 1; i < b.Failures && wait < backoffMax; i++ {
		wait *= 2
	}
	b.RetryAfter, b.Err = now.Add(min(wait, backoffMax)), err.Error()
}

// pass is one sink's reconcile in one run. The passes run concurrently, so a pass changes only its own fields and its
// own report (merged into the run's afterwards); the run's desired state is read-only by then.
type pass struct {
	*run
	s        sink
	rep      *stage.Report // this pass's counts and alerts; shadows run.rep
	deadline time.Time     // no app call starts after it
	parked   map[string]backoff
	dirty    bool // parked changed

	skipped   int    // collection changes refused or not possible this pass
	firstSkip string // the first of them, for the alert
}

// apply reconciles one sink. Its failures are logged and backed off, never returned: a sink never fails the run.
func (p *pass) apply(ctx context.Context) {
	sn := p.s.Name()
	var b backoff
	if _, err := p.env.Store.GetJSON(ctx, name, "backoff/"+sn, &b); err != nil {
		p.env.Log.Error("backoff state unreadable", "event", "store_error", "sink", sn, "err", err)
	}
	now := p.env.Now()
	if now.Before(b.RetryAfter) {
		p.rep.Count(sn+"_waiting", 1)
		p.env.Log.Info("sink backing off", "event", "sink_waiting", "sink", sn, "retry_after", b.RetryAfter.UTC().Format(time.RFC3339))
	} else {
		p.deadline = now.Add(p.opt.SinkBudget)
		err := p.reconcile(ctx)
		if p.skipped > 0 {
			p.rep.Alert("%s: %d collection change(s) refused or not possible, retried next run; first: %s", sn, p.skipped, p.firstSkip)
		}
		switch {
		case err == nil:
			b = backoff{}
		case ctx.Err() != nil:
			return // shutting down; not the sink's fault
		case errors.Is(err, errBudget):
			// A slow app, not a broken one: no backoff, and the next run starts as soon as this one ends.
			p.rep.Count(sn+"_unfinished", 1)
			p.env.Log.Warn("sink pass out of time; the next run carries on", "event", "sink_unfinished", "sink", sn,
				"budget", p.opt.SinkBudget.String())
			p.env.Trigger(name)
		default:
			b.fail(now, err)
			p.rep.Count(sn+"_failed", 1)
			p.env.Log.Warn("sink failed; retrying later", "event", "sink_failed", "sink", sn, "attempt", b.Failures,
				"retry_after", b.RetryAfter.UTC().Format(time.RFC3339), "err", err)
		}
		if err := p.env.Store.PutJSON(ctx, name, "backoff/"+sn, b); err != nil {
			p.env.Log.Error("backoff state not saved", "event", "store_error", "sink", sn, "err", err)
		}
	}
	if b.Failures > 0 && now.Sub(b.Since) >= alertAfter {
		p.rep.Alert("%s has failed collection updates since %s: %s", sn, b.Since.UTC().Format(time.RFC3339), b.Err)
	}
}

// call starts one app request unless the pass's budget is spent; each request is bounded by call_timeout.
func (p *pass) call(send func() error) error {
	if !p.env.Now().Before(p.deadline) {
		return errBudget
	}
	return send()
}

type addressCache struct {
	At     time.Time      `json:"at"`
	Items  map[int]string `json:"items"`
	Misses map[int]int    `json:"misses"` // wanted films on disk the list lacked, and in how many reads in a row
}

// address maps TMDB ids to the sink's item ids from the app's item list, read once a day. A wanted film on disk that
// the list lacks (the app had not scanned it yet) earns an early re-read, at most hourly, until lookTwice reads in a
// row have lacked it; then it waits for the daily read like everything else.
func (p *pass) address(ctx context.Context) (map[int]string, error) {
	var cache addressCache
	found, err := p.env.Store.GetJSON(ctx, name, "address/"+p.s.Name(), &cache)
	if err != nil {
		return nil, err
	}
	age := p.env.Now().Sub(cache.At)
	early := slices.ContainsFunc(p.lacking(cache.Items), func(id int) bool { return cache.Misses[id] < lookTwice })
	if found && age < addressTTL && (age < addressRetry || !early) {
		return cache.Items, nil
	}
	var items map[int]string
	if err := p.call(func() (err error) { items, err = p.s.Address(ctx); return err }); err != nil {
		return nil, err
	}
	fresh := addressCache{At: p.env.Now(), Items: items, Misses: map[int]int{}}
	for _, id := range p.lacking(items) {
		fresh.Misses[id] = cache.Misses[id] + 1
	}
	if err := p.env.Store.PutJSON(ctx, name, "address/"+p.s.Name(), fresh); err != nil {
		return nil, err
	}
	return items, nil
}

// lacking lists the wanted films on disk that items does not address.
func (p *pass) lacking(items map[int]string) []int {
	var out []int
	for _, c := range p.targets {
		for _, id := range c.Members {
			if p.disk[id] && items[id] == "" {
				out = append(out, id)
			}
		}
	}
	return out
}

func (p *pass) collections(ctx context.Context) (have map[string]held, err error) {
	err = p.call(func() error { have, err = p.s.Collections(ctx); return err })
	return have, err
}

// reconcile sends a sink the missing creates, member adds, sort titles and posters, one collection at a time; see
// settled for what a failed change does.
func (p *pass) reconcile(ctx context.Context) error {
	items, err := p.address(ctx)
	if err != nil {
		return err
	}
	have, err := p.collections(ctx)
	if err != nil {
		return err
	}
	if _, err := p.env.Store.GetJSON(ctx, name, "parked/"+p.s.Name(), &p.parked); err != nil {
		return err
	}
	if p.parked == nil {
		p.parked = map[string]backoff{}
	}
	defer p.unpark(ctx)
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
	created := map[string][]string{} // the members each collection made this pass was made with
	for _, c := range p.targets {
		ids := addressed(c)
		if _, ok := have[key(c.Name)]; ok || len(ids) == 0 || p.waiting(c) {
			continue
		}
		err := p.write(c, "create", len(ids), func() error { return p.s.Create(ctx, c.Name, ids) })
		if err == nil {
			created[key(c.Name)] = ids
		}
		if err == nil && p.opt.DryRun { // what the real run does next to the collection it just made
			if c.Sort != "" {
				p.write(c, "sort", 0, nil) // a dry run never calls send, so it cannot fail
			}
			err = p.poster(ctx, c, "")
		}
		if err := p.settled(c, err); err != nil {
			return err
		}
	}
	if len(created) > 0 && !p.opt.DryRun {
		if have, err = p.collections(ctx); err != nil {
			return err
		}
	}
	for _, c := range p.targets {
		h, ok := have[key(c.Name)]
		if !ok || p.waiting(c) {
			continue
		}
		if err := p.settled(c, p.settle(ctx, c, h, addressed(c), created[key(c.Name)])); err != nil {
			return err
		}
	}
	return nil
}

// settle gives one app collection what it lacks: members, the sort title and the owner's poster.
func (p *pass) settle(ctx context.Context, c *collection, h held, want, created []string) error {
	if err := p.members(ctx, c, h, want, created); err != nil {
		return err
	}
	if c.Sort != "" && !strings.EqualFold(h.Sort, c.Sort) {
		if err := p.write(c, "sort", 0, func() error { return p.s.SetSort(ctx, h.ID, c.Sort) }); err != nil {
			return err
		}
	}
	return p.poster(ctx, c, h.ID)
}

// settled files the outcome of one collection's changes. Done clears its parking. A refusal (4xx) says that request
// is wrong, so it is skipped and alerted and asked again next run. A 5xx while the app answers its lists parks just
// this collection on its own backoff (alerted after a day), so one poison write cannot hold up the others. Anything
// else (no answer, a spent budget, the store) is returned and ends the pass.
func (p *pass) settled(c *collection, err error) error {
	k := key(c.Name)
	switch code := status(err); {
	case err == nil:
		if _, ok := p.parked[k]; ok {
			delete(p.parked, k)
			p.dirty = true
		}
		return nil
	case code/100 == 4:
		p.skip(c, err)
		return nil
	case code/100 == 5:
		b := p.parked[k]
		b.fail(p.env.Now(), err)
		p.parked[k], p.dirty = b, true
		p.rep.Count(p.s.Name()+"_collection_failed", 1)
		p.env.Log.Warn("collection change failed; it waits on its own backoff", "event", "collection_failed", "sink", p.s.Name(),
			"collection", c.Name, "attempt", b.Failures, "retry_after", b.RetryAfter.UTC().Format(time.RFC3339), "err", err)
		return nil
	default:
		return err
	}
}

// waiting reports whether a parked collection's backoff still runs.
func (p *pass) waiting(c *collection) bool {
	b, ok := p.parked[key(c.Name)]
	if ok && p.env.Now().Before(b.RetryAfter) {
		p.rep.Count(p.s.Name()+"_collection_waiting", 1)
		return true
	}
	return false
}

// unpark stores the parked collections when they changed, and alerts those failing for over a day.
func (p *pass) unpark(ctx context.Context) {
	if p.dirty && !p.opt.DryRun {
		if err := p.env.Store.PutJSON(ctx, name, "parked/"+p.s.Name(), p.parked); err != nil {
			p.env.Log.Error("parked collections not saved", "event", "store_error", "sink", p.s.Name(), "err", err)
		}
	}
	var stuck []string
	for k, b := range p.parked {
		if p.env.Now().Sub(b.Since) >= alertAfter {
			stuck = append(stuck, k)
		}
	}
	if len(stuck) > 0 {
		slices.Sort(stuck)
		p.rep.Alert("%s: %d collection(s) failing for over a day, retried with backoff; first: %s", p.s.Name(), len(stuck), p.parked[stuck[0]].Err)
	}
}

// seenMembers is what this stage last saw (and added) in one app collection.
type seenMembers struct {
	Count int      `json:"count"`
	Items []string `json:"items"`
}

// members adds what an app collection lacks. Its member list is read only when the app's count moved since the last
// run or a wanted item is new, so a quiet run asks each app for one collection list, not one call per collection; a
// collection this pass made is not read at all, since it was given exactly created.
func (p *pass) members(ctx context.Context, c *collection, h held, want, created []string) error {
	seenKey := "members/" + p.s.Name() + "/" + h.ID
	if created != nil {
		return p.env.Store.PutJSON(ctx, name, seenKey, seenMembers{Count: len(created), Items: created})
	}
	var seen seenMembers
	found, err := p.env.Store.GetJSON(ctx, name, seenKey, &seen)
	if err != nil {
		return err
	}
	if found && seen.Count == h.Count && !slices.ContainsFunc(want, func(id string) bool { return !slices.Contains(seen.Items, id) }) {
		return nil
	}
	var cur map[string]bool
	if err := p.call(func() (err error) { cur, err = p.s.Members(ctx, h.ID); return err }); err != nil {
		return fmt.Errorf("members %q: %w", c.Name, err)
	}
	var add []string
	for _, id := range want {
		if !cur[id] {
			add = append(add, id)
		}
	}
	if len(add) > 0 {
		if err := p.write(c, "add", len(add), func() error { return p.s.Add(ctx, h.ID, add) }); err != nil {
			return err // nothing recorded, so the next run asks again
		}
	}
	if p.opt.DryRun {
		return nil
	}
	seen = seenMembers{Count: len(cur) + len(add), Items: add}
	for id := range cur {
		seen.Items = append(seen.Items, id)
	}
	return p.env.Store.PutJSON(ctx, name, seenKey, seen)
}

// write sends one change, or only logs it in a dry run; n is the number of members it carries. An error names the
// change and the collection.
func (p *pass) write(c *collection, op string, n int, send func() error) error {
	event := op
	if p.opt.DryRun {
		event = "would_" + op
	} else if err := p.call(send); err != nil {
		return fmt.Errorf("%s %q: %w", op, c.Name, err)
	}
	p.rep.Count(p.s.Name()+"_"+op, 1)
	if n > 0 {
		p.rep.Count(p.s.Name()+"_"+op+"_members", n)
	}
	p.env.Log.Info("collection "+event, "event", event, "sink", p.s.Name(), "collection", c.Name, "members", n, "sort", c.Sort)
	return nil
}

func (p *pass) skip(c *collection, err error) {
	p.skipped++
	if p.firstSkip == "" {
		p.firstSkip = err.Error()
	}
	p.rep.Count(p.s.Name()+"_skipped", 1)
	p.env.Log.Warn("collection change skipped", "event", "change_skipped", "sink", p.s.Name(), "collection", c.Name, "err", err)
}

// status is the HTTP status an app answered with, or 0 when it did not answer (transport error, timeout) or the error
// is not the app's. Both clients end a non-2xx error with "HTTP <code>".
func status(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	i := strings.LastIndex(msg, "HTTP ")
	if i < 0 {
		return 0
	}
	code, _ := strconv.Atoi(msg[i+len("HTTP "):])
	return code
}

// poster uploads an owner collection's poster once per app collection: recorded, because an app may draw its own
// poster for a collection, so the app's image list cannot tell whether ours went up. id "" is a dry run's planned
// collection. A poster file missing from poster_dir (the deploy copy skipped) is skipped and alerted.
func (p *pass) poster(ctx context.Context, c *collection, id string) error {
	ps, ok := p.s.(posterSink)
	if !ok || c.Poster == "" {
		return nil
	}
	if id != "" {
		var done string
		if found, err := p.env.Store.GetJSON(ctx, name, "poster/"+p.s.Name()+"/"+id, &done); err != nil || found {
			return err
		}
	}
	jpeg, err := os.ReadFile(c.Poster)
	if errors.Is(err, fs.ErrNotExist) {
		p.skip(c, fmt.Errorf("poster %q: %w", c.Name, err))
		return nil
	}
	if err != nil {
		return err
	}
	if err := p.write(c, "poster", 0, func() error { return ps.Poster(ctx, id, jpeg) }); err != nil || p.opt.DryRun {
		return err
	}
	return p.env.Store.PutJSON(ctx, name, "poster/"+p.s.Name()+"/"+id, c.Poster)
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
		ID             string `json:"Id"`
		Name           string `json:"Name"`
		Path           string `json:"Path"`
		ForcedSortName string `json:"ForcedSortName"`
		ChildCount     int    `json:"ChildCount"`
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
// of its children, when no userId is given. The sort title is ForcedSortName (Fields=Settings), as it was set:
// SortName is Jellyfin's derived form ("zzBong Joon-ho" shows as "zzbong joonho", digit runs padded to ten).
func (j *jellyfinSink) Collections(ctx context.Context) (map[string]held, error) {
	user, err := j.admin(ctx)
	if err != nil {
		return nil, err
	}
	var res jfItems
	if err := j.c.GetJSON(ctx, "/Items?IncludeItemTypes=BoxSet&Recursive=true&Fields=Settings,ChildCount&userId="+url.QueryEscape(user), &res); err != nil {
		return nil, err
	}
	out := map[string]held{}
	for _, it := range res.Items {
		out[key(it.Name)] = held{ID: it.ID, Sort: it.ForcedSortName, Count: it.ChildCount}
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
