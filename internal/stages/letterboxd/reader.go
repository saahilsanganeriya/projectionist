package letterboxd

// ---- for other stages (tonight's household): members' public pages and exports, read as this stage reads them ----

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"projectionist/internal/lbpace"
)

type (
	FeedEntry = rssEntry  // one diary entry of a feed: viewing id, TMDB id, title, year, watched date, half stars, review
	FeedCache = feedCache // a feed's conditional-GET validators and when it last answered
	GridFilm  = gridFilm  // one film of a films grid, with the member's rating in half stars
	Export    = export    // a member's newest account export
	ExportRow = exportRow
)

// Reader reads members' public Letterboxd pages through this stage's fetcher: its User-Agent, redirects returned
// rather than followed, a Cloudflare refusal named as one, every request waiting on pace, at most budget requests.
type Reader struct {
	base string
	f    *fetcher
}

func NewReader(base string, pace *lbpace.Limiter, budget int) *Reader {
	return &Reader{base: base, f: &fetcher{
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		ua: defaults().UserAgent, pace: pace, budget: budget,
	}}
}

// Feed reads user's feed as this stage does (see fetcher.feed); changed is false when it has not changed since c.
func (r *Reader) Feed(ctx context.Context, user string, c FeedCache, now time.Time) ([]FeedEntry, FeedCache, bool, error) {
	return r.f.feed(ctx, r.base, user, c, now)
}

// Films reads page 1 of user's watched films, their newest with ratings: Cloudflare refuses anonymous deeper pages
// (see crawl), and every sorted view. A page listing no films is a refusal, as in the films crawl.
func (r *Reader) Films(ctx context.Context, user string) ([]GridFilm, error) {
	url := fmt.Sprintf("%s/%s/films/", r.base, user)
	p, err := r.f.get(ctx, url, map[string]string{"Accept": "text/html"})
	if err != nil {
		return nil, err
	}
	films, _ := parseGrid(p.body)
	if len(films) == 0 {
		return nil, fmt.Errorf("%w: GET %s: HTTP %d lists no films", errChallenged, url, p.status)
	}
	return films, nil
}

// ReadExport loads user's newest export folder under dir; ok is false when there is none.
func ReadExport(dir, user string) (Export, bool, error) { return readExport(dir, user) }
