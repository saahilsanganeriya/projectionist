package lists

// Letterboxd side: polite fetching (one request per 2 s for the whole process, via lbpace; a real User-Agent; no
// login), page parsing, and the SQLite cache that makes every answer reusable: film page → TMDB id forever, list
// pages until they are stale.
//
// Cloudflare challenges anonymous requests for paginated *user* pages (/<user>/lists/page/2/, /<user>/films/page/2/,
// measured 2026-10-05), so only page 1 of those is ever requested; list pages (/<user>/list/<slug>/page/N/) are fine.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/lbpace"
	"projectionist/internal/store"
)

var (
	errBlocked  = errors.New("letterboxd refused the request (challenge or rate limit)")
	errNotFound = errors.New("not on letterboxd (404)")
)

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type fetcher struct {
	http     *http.Client
	ua       string
	pace     *lbpace.Limiter
	log      *slog.Logger
	requests int
	blocked  bool // set by the first 403/429; no further request this run
}

func newFetcher(ua string, pace *lbpace.Limiter, log *slog.Logger) *fetcher {
	return &fetcher{
		http: &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		ua: ua, pace: pace, log: log,
	}
}

// get fetches one URL when the shared limiter allows: the body on 200, the Location on a redirect (never followed,
// so every hop is paced), errNotFound on 404/410, errBlocked on 403/429. Other failures are retried twice, 5 s then
// 10 s later.
func (f *fetcher) get(ctx context.Context, rawURL string) (body []byte, location string, err error) {
	for attempt := 1; ; attempt++ {
		if f.blocked {
			return nil, "", errBlocked
		}
		if err := f.pace.Wait(ctx); err != nil {
			return nil, "", err
		}
		body, location, err = f.once(ctx, rawURL)
		if err == nil || errors.Is(err, errNotFound) || errors.Is(err, errBlocked) || ctx.Err() != nil || attempt == 3 {
			return body, location, err
		}
		f.log.Warn("letterboxd request failed", "event", "letterboxd_retry", "url", rawURL, "attempt", attempt, "err", err)
		if err := sleep(ctx, time.Duration(attempt)*5*time.Second); err != nil {
			return nil, "", err
		}
	}
}

func (f *fetcher) once(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	f.requests++
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return b, "", err
	case resp.StatusCode/100 == 3 && resp.Header.Get("Location") != "":
		return nil, resp.Header.Get("Location"), nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, "", errNotFound
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		f.blocked = true
		return nil, "", fmt.Errorf("%w: HTTP %d for %s", errBlocked, resp.StatusCode, rawURL)
	default:
		return nil, "", fmt.Errorf("letterboxd GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
}

// ---- parsing ----

// entry is one film on a list or grid page, as Letterboxd names it.
type entry struct {
	Slug  string `json:"s"`
	LID   string `json:"l,omitempty"` // boxd.it code; the export's film URIs use the same codes
	Title string `json:"t"`           // "Name (Year)"
}

type page struct {
	Title   string   `json:"title,omitempty"` // og:title: the list's name
	About   string   `json:"about,omitempty"` // the list's description, when it has one
	LID     string   `json:"lid,omitempty"`   // the list's own boxd.it code: stable across renames
	Entries []entry  `json:"entries"`
	Next    string   `json:"next,omitempty"`  // path of the next page
	Lists   []string `json:"lists,omitempty"` // on the owner's lists page: the paths of his lists
	Gone    bool     `json:"gone,omitempty"`  // answered 404 (a private or deleted list); asked again after maxAge
}

var (
	posterTag  = regexp.MustCompile(`<div class="react-component" data-component-class="LazyPoster"[^>]*>`)
	itemSlug   = regexp.MustCompile(`data-item-slug="([^"]+)"`)
	itemName   = regexp.MustCompile(`data-item-name="([^"]*)"`)
	itemLID    = regexp.MustCompile(`(?:&quot;|&#034;|")lid(?:&quot;|&#034;|"):(?:&quot;|&#034;|")([0-9A-Za-z]+)`)
	ogTitle    = regexp.MustCompile(`<meta property="og:title" content="([^"]*)"`)
	metaDesc   = regexp.MustCompile(`<meta name="description" content="([^"]*)"`)
	nextLink   = regexp.MustCompile(`<a class="next" href="([^"]+)"`)
	listShort  = regexp.MustCompile(`data-list-boxdit-url="https://boxd\.it/([0-9A-Za-z]+)"`)
	indexEntry = regexp.MustCompile(`<h2 class="name prettify">\s*<a href="(/[^"/]+/list/[^"/]+/)">([^<]*)</a>`)
	tmdbButton = regexp.MustCompile(`href="https://www\.themoviedb\.org/(movie|tv)/(\d+)/?"[^>]*data-track-action="TMDB"`)
	bodyTMDB   = regexp.MustCompile(`<body[^>]*data-tmdb-type="(\w*)"[^>]*data-tmdb-id="(\d*)"`)
	shortCode  = regexp.MustCompile(`^https?://boxd\.it/([0-9A-Za-z]+)/?$`)
	filmInLink = regexp.MustCompile(`/film/([^/]+)/`)
)

// parsePage reads a list page, a film grid page (the owner's films) or the owner's lists page.
func parsePage(body []byte) page {
	var p page
	if m := ogTitle.FindSubmatch(body); m != nil {
		p.Title = html.UnescapeString(string(m[1]))
	}
	if m := metaDesc.FindSubmatch(body); m != nil {
		if _, about, ok := strings.Cut(html.UnescapeString(string(m[1])), "About this list: "); ok {
			p.About = strings.TrimSpace(about)
		}
	}
	if m := listShort.FindSubmatch(body); m != nil {
		p.LID = string(m[1])
	}
	if m := nextLink.FindSubmatch(body); m != nil {
		p.Next = html.UnescapeString(string(m[1]))
	}
	for _, m := range indexEntry.FindAllSubmatch(body, -1) {
		p.Lists = append(p.Lists, string(m[1]))
	}
	p.Entries = []entry{}
	for _, tag := range posterTag.FindAll(body, -1) {
		slug := itemSlug.FindSubmatch(tag)
		if slug == nil {
			continue
		}
		e := entry{Slug: string(slug[1])}
		if m := itemName.FindSubmatch(tag); m != nil {
			e.Title = html.UnescapeString(string(m[1]))
		}
		if m := itemLID.FindSubmatch(tag); m != nil {
			e.LID = string(m[1])
		}
		p.Entries = append(p.Entries, e)
	}
	return p
}

// parseFilm reads a film page's TMDB link. The TMDB button wins over the body attributes: a miniseries page says
// data-tmdb-type="movie" with an empty id while its button links /tv/<id>. ok is false for a page that is not a film
// page at all (a challenge answered with HTTP 200, a changed layout): no answer, rather than "no TMDB link".
func parseFilm(body []byte) (kind string, tmdb int, ok bool) {
	if m := tmdbButton.FindSubmatch(body); m != nil {
		n, _ := strconv.Atoi(string(m[2]))
		return string(m[1]), n, true
	}
	m := bodyTMDB.FindSubmatch(body)
	if m == nil {
		return "", 0, false
	}
	if len(m[2]) == 0 {
		return "", 0, true // a film page without a TMDB link
	}
	n, _ := strconv.Atoi(string(m[2]))
	return string(m[1]), n, true
}

type rssItem struct {
	Link string `xml:"link"`
	TMDB int    `xml:"https://themoviedb.org movieId"`
}

// parseRSS reads the diary entries of the owner's feed: film slug and TMDB movie id.
func parseRSS(body []byte) ([]rssItem, error) {
	var feed struct {
		Items []rssItem `xml:"channel>item"`
	}
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("letterboxd rss: %w", err)
	}
	return feed.Items, nil
}

// ---- the owner's export (lists/*.csv, watched.csv), read only for list codes and watched films ----

// readExport returns the boxd.it codes of the owner's exported lists and of every film in watched.csv. The export
// may sit directly in dir or in one subfolder of it (the unzipped folder).
func readExport(dir string) (listLIDs, watchedLIDs []string, err error) {
	if dir == "" {
		return nil, nil, nil
	}
	files := func(pattern string) []string {
		a, _ := filepath.Glob(filepath.Join(dir, pattern))
		b, _ := filepath.Glob(filepath.Join(dir, "*", pattern))
		return append(a, b...)
	}
	for _, path := range files(filepath.Join("lists", "*.csv")) {
		rows, err := readCSV(path)
		if err != nil {
			return nil, nil, err
		}
		// line 1 "Letterboxd list export v7", line 2 the list header, line 3 Date,Name,Tags,URL,Description
		if len(rows) >= 3 && len(rows[2]) >= 4 {
			if m := shortCode.FindStringSubmatch(rows[2][3]); m != nil {
				listLIDs = append(listLIDs, m[1])
			}
		}
	}
	for _, path := range files("watched.csv") {
		rows, err := readCSV(path)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			if len(r) >= 4 {
				if m := shortCode.FindStringSubmatch(r[3]); m != nil {
					watchedLIDs = append(watchedLIDs, m[1])
				}
			}
		}
	}
	return listLIDs, watchedLIDs, nil
}

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("letterboxd export %s: %w", path, err)
	}
	return rows, nil
}

// ---- cache ----

const cacheSchema = `
CREATE TABLE IF NOT EXISTS lists_films (
  slug       TEXT PRIMARY KEY,
  lid        TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT '',  -- movie or tv, from the film page's TMDB link
  tmdb       INTEGER NOT NULL DEFAULT 0,
  checked_at TEXT NOT NULL DEFAULT ''   -- '' until the film page was read
);
CREATE TABLE IF NOT EXISTS lists_pages (
  path       TEXT PRIMARY KEY,
  fetched_at TEXT NOT NULL,
  page       TEXT NOT NULL              -- parsed page, JSON
);
CREATE TABLE IF NOT EXISTS lists_links (
  lid        TEXT PRIMARY KEY,          -- boxd.it code of one of the owner's lists
  path       TEXT NOT NULL,             -- where it redirects; '' when it leads nowhere
  checked_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS lists_seen (
  key TEXT PRIMARY KEY                  -- slug:<s> or tmdb:<n>, seen on the owner's Letterboxd; only ever grows
);
`

// film is what the cache knows about one Letterboxd film.
type film struct {
	Kind    string
	TMDB    int
	Checked bool
}

// recheckAfter is how long a film page that had no TMDB link waits before it is read again.
const recheckAfter = 30 * 24 * time.Hour

type letterboxd struct {
	base, short string // https://letterboxd.com, https://boxd.it
	user        string
	f           *fetcher
	db          *sql.DB
	now         func() time.Time
	log         *slog.Logger
}

// cachedPage returns the page at path, fetched again only when its copy is older than maxAge. When the fetch fails
// for any reason but a 404, or the page shows nothing, the stale copy is used; ok is false when there is neither.
func (lb *letterboxd) cachedPage(ctx context.Context, path string, maxAge time.Duration) (p page, ok bool, err error) {
	var fetched, raw string
	err = lb.db.QueryRowContext(ctx, `SELECT fetched_at, page FROM lists_pages WHERE path=?`, path).Scan(&fetched, &raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return page{}, false, err
	default:
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return page{}, false, err
		}
		at, _ := time.Parse(store.TimeFormat, fetched)
		switch {
		case lb.now().Sub(at) < maxAge && p.Gone:
			return page{}, false, errNotFound
		case lb.now().Sub(at) < maxAge:
			return p, true, nil
		}
		ok = !p.Gone
	}
	fresh, err := lb.fetchPage(ctx, path)
	if err == nil && len(fresh.Entries) == 0 && len(fresh.Lists) == 0 {
		// every page read here lists films or lists: an empty one is a challenge answered with HTTP 200 or a changed
		// layout, and taking it would empty the shelves
		err = errors.New("no films or lists on the page")
	}
	switch {
	case err == nil:
		return fresh, true, lb.storePage(ctx, path, fresh)
	case ctx.Err() != nil:
		return page{}, false, ctx.Err()
	case errors.Is(err, errNotFound):
		return page{}, false, errors.Join(errNotFound, lb.storePage(ctx, path, page{Gone: true}))
	default:
		lb.log.Warn("letterboxd page unavailable", "event", "page_unavailable", "path", path, "stale_copy", ok, "err", err)
		return p, ok, nil
	}
}

// fetchPage follows at most two redirects within Letterboxd (a renamed list or film keeps answering at its old path).
func (lb *letterboxd) fetchPage(ctx context.Context, path string) (page, error) {
	for hop := 0; hop < 3; hop++ {
		body, loc, err := lb.f.get(ctx, lb.base+path)
		if err != nil {
			return page{}, err
		}
		if loc == "" {
			return parsePage(body), nil
		}
		if path, err = lb.localPath(loc); err != nil {
			return page{}, err
		}
	}
	return page{}, fmt.Errorf("letterboxd %s: too many redirects", path)
}

// localPath turns a Location into a path on Letterboxd, refusing other hosts.
func (lb *letterboxd) localPath(loc string) (string, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	base, _ := url.Parse(lb.base)
	if u.Host != "" && u.Host != base.Host {
		return "", fmt.Errorf("letterboxd redirected off-site to %s", u.Host)
	}
	return u.Path, nil
}

// storePage keeps the page and records each film's code and name, so a list's films can be matched by code later.
func (lb *letterboxd) storePage(ctx context.Context, path string, p page) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := lb.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO lists_pages(path, fetched_at, page) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET fetched_at=excluded.fetched_at, page=excluded.page`,
		path, lb.now().UTC().Format(store.TimeFormat), string(raw)); err != nil {
		return err
	}
	for _, e := range p.Entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO lists_films(slug, lid, title) VALUES (?, ?, ?)
			ON CONFLICT(slug) DO UPDATE SET lid=excluded.lid, title=excluded.title`, e.Slug, e.LID, e.Title); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// list returns the entries of the list whose first page is at path, stopping once it holds stopAfter entries
// (0 = all). complete is false when a page is neither cached nor fetchable; the error is errNotFound for a list
// that does not exist publicly (private or deleted).
func (lb *letterboxd) list(ctx context.Context, path string, maxAge time.Duration, stopAfter int) (first page, entries []entry, complete bool, err error) {
	for n := 0; path != ""; n++ {
		p, ok, err := lb.cachedPage(ctx, path, maxAge)
		if err != nil {
			return first, nil, false, err
		}
		if !ok {
			return first, entries, false, nil
		}
		if n == 0 {
			first = p
		}
		entries = append(entries, p.Entries...)
		if stopAfter > 0 && len(entries) >= stopAfter {
			return first, entries[:stopAfter], true, nil
		}
		path = p.Next
	}
	return first, entries, true, nil
}

// listPath resolves one of the owner's list codes to its current path via boxd.it, cached for 30 days. "" means the
// code leads to no list of the owner's.
func (lb *letterboxd) listPath(ctx context.Context, lid string) (string, error) {
	var path, checked string
	err := lb.db.QueryRowContext(ctx, `SELECT path, checked_at FROM lists_links WHERE lid=?`, lid).Scan(&path, &checked)
	if err == nil {
		if at, _ := time.Parse(store.TimeFormat, checked); lb.now().Sub(at) < recheckAfter {
			return path, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	_, loc, err := lb.f.get(ctx, lb.short+"/"+lid)
	switch {
	case err == nil && loc != "":
		if path, err = lb.localPath(loc); err != nil {
			return "", err
		}
		if !strings.HasPrefix(path, "/"+lb.user+"/list/") {
			path = ""
		}
	case errors.Is(err, errNotFound), err == nil:
		path = ""
	default:
		return "", err
	}
	_, err = lb.db.ExecContext(ctx, `INSERT INTO lists_links(lid, path, checked_at) VALUES (?, ?, ?)
		ON CONFLICT(lid) DO UPDATE SET path=excluded.path, checked_at=excluded.checked_at`,
		lid, path, lb.now().UTC().Format(store.TimeFormat))
	return path, err
}

// films loads everything the cache knows, keyed by slug.
func (lb *letterboxd) films(ctx context.Context) (map[string]film, error) {
	rows, err := lb.db.QueryContext(ctx, `SELECT slug, kind, tmdb, checked_at FROM lists_films`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]film{}
	for rows.Next() {
		var slug, kind, checked string
		var tmdb int
		if err := rows.Scan(&slug, &kind, &tmdb, &checked); err != nil {
			return nil, err
		}
		f := film{Kind: kind, TMDB: tmdb, Checked: checked != ""}
		if f.Checked && tmdb == 0 {
			if at, _ := time.Parse(store.TimeFormat, checked); lb.now().Sub(at) >= recheckAfter {
				f.Checked = false // a page without a TMDB link gets another look now and then
			}
		}
		out[slug] = f
	}
	return out, rows.Err()
}

// resolve reads the film page of every slug in order that the cache has not checked, storing each answer as it
// arrives so an interrupted run resumes where it stopped. It ends early, without error, when Letterboxd blocks us.
func (lb *letterboxd) resolve(ctx context.Context, slugs []string, known map[string]film) (resolved int, err error) {
	for _, slug := range slugs {
		if known[slug].Checked {
			continue
		}
		body, loc, err := lb.f.get(ctx, lb.base+"/film/"+slug+"/")
		if loc != "" && err == nil { // a renamed film: one more paced hop
			var path string
			if path, err = lb.localPath(loc); err == nil {
				body, _, err = lb.f.get(ctx, lb.base+path)
			}
		}
		var f film
		switch {
		case ctx.Err() != nil:
			return resolved, ctx.Err()
		case errors.Is(err, errBlocked):
			return resolved, nil
		case errors.Is(err, errNotFound):
			f.Checked = true
		case err == nil:
			var ok bool
			if f.Kind, f.TMDB, ok = parseFilm(body); !ok {
				lb.log.Warn("film page unreadable", "event", "film_unavailable", "slug", slug, "bytes", len(body))
				continue
			}
			f.Checked = true
		default:
			lb.log.Warn("film page unavailable", "event", "film_unavailable", "slug", slug, "err", err)
			continue
		}
		if _, err := lb.db.ExecContext(ctx, `INSERT INTO lists_films(slug, kind, tmdb, checked_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(slug) DO UPDATE SET kind=excluded.kind, tmdb=excluded.tmdb, checked_at=excluded.checked_at`,
			slug, f.Kind, f.TMDB, lb.now().UTC().Format(store.TimeFormat)); err != nil {
			return resolved, err
		}
		known[slug] = f
		resolved++
	}
	return resolved, nil
}

// refreshSeen adds what the owner logged recently (films page 1 and the RSS diary, each at most once per maxAge)
// to the seen table, and returns the whole table.
func (lb *letterboxd) refreshSeen(ctx context.Context, maxAge time.Duration) (map[string]bool, error) {
	var keys []string
	grid, ok, err := lb.cachedPage(ctx, "/"+lb.user+"/films/", maxAge)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	if ok {
		for _, e := range grid.Entries {
			keys = append(keys, "slug:"+e.Slug)
		}
	}
	rss, err := lb.feed(ctx, maxAge)
	if err != nil {
		return nil, err
	}
	for _, it := range rss {
		if m := filmInLink.FindStringSubmatch(it.Link); m != nil {
			keys = append(keys, "slug:"+m[1])
		}
		if it.TMDB > 0 {
			keys = append(keys, "tmdb:"+strconv.Itoa(it.TMDB))
		}
	}
	for _, k := range keys {
		if _, err := lb.db.ExecContext(ctx, `INSERT OR IGNORE INTO lists_seen(key) VALUES (?)`, k); err != nil {
			return nil, err
		}
	}
	rows, err := lb.db.QueryContext(ctx, `SELECT key FROM lists_seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		seen[k] = true
	}
	return seen, rows.Err()
}

// feed fetches the RSS diary at most once per maxAge (it is stored with the page cache's timestamps).
func (lb *letterboxd) feed(ctx context.Context, maxAge time.Duration) ([]rssItem, error) {
	path := "/" + lb.user + "/rss/"
	var fetched string
	err := lb.db.QueryRowContext(ctx, `SELECT fetched_at FROM lists_pages WHERE path=?`, path).Scan(&fetched)
	if err == nil {
		if at, _ := time.Parse(store.TimeFormat, fetched); lb.now().Sub(at) < maxAge {
			return nil, nil // its films are already in lists_seen
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	body, _, err := lb.f.get(ctx, lb.base+path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lb.log.Warn("letterboxd rss unavailable", "event", "page_unavailable", "path", path, "err", err)
		return nil, nil
	}
	items, err := parseRSS(body)
	if err != nil {
		lb.log.Warn("letterboxd rss unreadable", "event", "page_unavailable", "path", path, "err", err)
		return nil, nil
	}
	return items, lb.storePage(ctx, path, page{Entries: []entry{}})
}
