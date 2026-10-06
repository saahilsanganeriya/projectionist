package posters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---- Criterion: the cover of a film the Criterion Collection released ----
//
// www.criterion.com answers every client that is not a browser with a Cloudflare challenge (HTTP 403 to curl and to
// Go from the home connection, 2026-10-06), and its terms (5.8 vii) forbid collecting data from it by automated means,
// so the stage sends it no request. Which films Criterion released comes from Wikidata (its "Criterion Collection film
// ID", P9584, beside the TMDB id, P4947: one query a week), and each film page's address from Criterion's sitemap,
// which its robots.txt publishes for crawlers. A film's cover is the og:image of its page as the Internet Archive last
// saved it with HTTP 200 (two requests a film, WaybackPace apart, once), downloaded from Criterion's image host. A
// cover once found is kept for good; a page the archive has no cover for is asked again after criterionRecheck.

const (
	srcCriterion     = "criterion"
	criterionCaption = "the Criterion Collection's cover" // the judge's label for it (systemPrompt)
	criterionRefresh = 7 * 24 * time.Hour                 // the catalogue: Criterion adds a few films a month
	criterionRecheck = 30 * 24 * time.Hour                // a page the archive had no cover for
)

// WaybackPace is the pause before each request to the Internet Archive; tests set it to zero.
var WaybackPace = 4 * time.Second

var (
	sitemapFilm = regexp.MustCompile(`<loc>(https://www\.criterion\.com/films/(\d+)-[^<]+)</loc>`)
	ogImage     = regexp.MustCompile(`<meta[^>]+property="og:image"[^>]+content="([^"]+)"`)
)

// criterionCovers finds the Criterion cover of every movie in titles that Criterion released, and sets r.covers. A
// title showing the owner's upload or pick is left out: it is never replaced, so its pool is his poster alone. The
// archive is asked one film at a time; an error other than a miss (it is down, it throttles) ends the lookups for this
// run, and the covers already found still count.
func (r *run) criterionCovers(ctx context.Context, titles []Title) error {
	r.covers = map[string]string{}
	if !r.opt.Criterion {
		return nil
	}
	pages, err := r.criterionCatalog(ctx)
	if err != nil {
		r.env.Log.Warn("no Criterion catalogue: no Criterion covers this run", "event", "criterion_catalog_failed", "err", err)
		return nil
	}
	lookups := true
	for _, t := range titles {
		id, _ := strconv.Atoi(t.ID.Value)
		page, owner := pages[id], r.owners[t.Key].State
		if t.Kind != movie || t.ID.Source != "tmdb" || page == "" || owner == ownerUpload || owner == ownerPick {
			continue
		}
		var c struct {
			Page  string    `json:"page"`
			Cover string    `json:"cover"` // "" : the archive had none
			At    time.Time `json:"at"`
		}
		found, err := r.env.Store.GetJSON(ctx, name, "criterion/"+t.Key, &c)
		if err != nil {
			return err
		}
		if lookups && (!found || c.Page != page || c.Cover == "" && r.env.Now().Sub(c.At) >= criterionRecheck) {
			cover, err := r.criterionCover(ctx, page)
			if err != nil {
				r.env.Log.Warn("Criterion lookups wait for a later run", "event", "criterion_lookup_failed", "item", t.Key, "err", err)
				lookups = false
				continue
			}
			c.Page, c.Cover, c.At = page, cover, r.env.Now()
			if err := r.env.Store.PutJSON(context.WithoutCancel(ctx), name, "criterion/"+t.Key, c); err != nil {
				return err
			}
			r.count("criterion_looked_up", 1)
		}
		if c.Page == page && c.Cover != "" {
			r.covers[t.Key] = c.Cover
		}
	}
	r.count("criterion_covers", len(r.covers))
	return nil
}

// criterionCatalog is the criterion.com page of every film Criterion released, by TMDB movie id, read again weekly; a
// refresh that fails keeps the last catalogue.
func (r *run) criterionCatalog(ctx context.Context) (map[int]string, error) {
	var cat struct {
		At    time.Time      `json:"at"`
		Pages map[int]string `json:"pages"`
	}
	found, err := r.env.Store.GetJSON(ctx, name, "criterion_catalog", &cat)
	if err != nil || found && r.env.Now().Sub(cat.At) < criterionRefresh {
		return cat.Pages, err
	}
	pages, err := r.readCriterionCatalog(ctx)
	if err != nil {
		if found {
			r.env.Log.Warn("Criterion catalogue not refreshed; the last one stands", "event", "criterion_catalog_stale", "err", err)
			return cat.Pages, nil
		}
		return nil, err
	}
	cat.At, cat.Pages = r.env.Now(), pages
	return pages, r.env.Store.PutJSON(ctx, name, "criterion_catalog", cat)
}

func (r *run) readCriterionCatalog(ctx context.Context) (map[int]string, error) {
	q := url.Values{"format": {"json"}, "query": {"SELECT ?tmdb ?criterion WHERE { ?film wdt:P9584 ?criterion; wdt:P4947 ?tmdb }"}}
	body, err := r.fetch(ctx, r.opt.WikidataSPARQL+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("wikidata: %w", err)
	}
	var res struct {
		Results struct {
			Bindings []struct{ TMDB, Criterion struct{ Value string } } `json:"bindings"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("wikidata: %w", err)
	}
	sitemap, err := r.fetch(ctx, r.opt.CriterionSitemap)
	if err != nil {
		return nil, fmt.Errorf("criterion sitemap: %w", err)
	}
	page := map[string]string{} // Criterion film id → page
	for _, m := range sitemapFilm.FindAllStringSubmatch(string(sitemap), -1) {
		page[m[2]] = m[1]
	}
	out := map[int]string{}
	for _, b := range res.Results.Bindings {
		if id, err := strconv.Atoi(b.TMDB.Value); err == nil && page[b.Criterion.Value] != "" {
			out[id] = page[b.Criterion.Value]
		}
	}
	if len(out) == 0 {
		return nil, errors.New("criterion catalogue: no film is both in Wikidata and in the sitemap")
	}
	return out, nil
}

// criterionCover is the og:image of page in the archive's latest copy saved with HTTP 200 (its other copies are
// Cloudflare's challenge), or "" when it has none.
func (r *run) criterionCover(ctx context.Context, page string) (string, error) {
	archived := func(u string) ([]byte, error) {
		if err := wikiSleep(ctx, WaybackPace); err != nil {
			return nil, err
		}
		b, err := r.fetch(ctx, u)
		if status := httpStatus(0); errors.As(err, &status) && status != http.StatusTooManyRequests && status < 500 {
			return nil, nil // the archive holds nothing there: a miss
		}
		return b, err
	}
	q := url.Values{"url": {page}, "filter": {"statuscode:200"}, "fl": {"timestamp"}, "limit": {"-1"}}
	stamps, err := archived(r.opt.Wayback + "/cdx/search/cdx?" + q.Encode())
	f := strings.Fields(string(stamps))
	if err != nil || len(f) == 0 {
		return "", err
	}
	saved, err := archived(r.opt.Wayback + "/web/" + f[len(f)-1] + "id_/" + page)
	if err != nil {
		return "", err
	}
	if m := ogImage.FindSubmatch(saved); m != nil && strings.HasPrefix(string(m[1]), "http") {
		return html.UnescapeString(string(m[1])), nil
	}
	return "", nil
}
