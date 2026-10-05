package letterboxd

// ---- Letterboxd: the polite fetcher and the parsers for the public pages, the RSS feed and the export ----

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// errChallenged is Cloudflare refusing a page (403/429/503, "Just a moment..."). Deep /films/ pages and /json/
// endpoints draw it for any client; the fetcher backs that page kind off instead of retrying.
var errChallenged = errors.New("letterboxd: challenged")

// errBudget means this run spent its request budget; the work resumes next run.
var errBudget = errors.New("letterboxd: request budget spent")

// fetcher is the only way the stage talks to Letterboxd: one request per interval across letterboxd.com and
// boxd.it, a browser User-Agent with a contact note, redirects returned rather than followed, a per-run budget.
type fetcher struct {
	http     *http.Client
	ua       string
	interval time.Duration
	budget   int
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	last     time.Time
	fetches  int
}

type page struct {
	status int
	header http.Header
	body   []byte
}

func (f *fetcher) get(ctx context.Context, url string, hdr map[string]string) (page, error) {
	if f.fetches >= f.budget {
		return page{}, errBudget
	}
	if wait := f.interval - f.now().Sub(f.last); !f.last.IsZero() && wait > 0 {
		if err := f.sleep(ctx, wait); err != nil {
			return page{}, err
		}
	}
	f.last, f.fetches = f.now(), f.fetches+1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return page{}, err
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return page{}, fmt.Errorf("letterboxd GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return page{}, fmt.Errorf("letterboxd GET %s: %w", url, err)
	}
	p := page{status: resp.StatusCode, header: resp.Header, body: body}
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return p, fmt.Errorf("%w: GET %s: HTTP %d", errChallenged, url, resp.StatusCode)
	}
	if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 {
		return p, nil
	}
	return p, fmt.Errorf("letterboxd GET %s: HTTP %d", url, resp.StatusCode)
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// filmPath finds the film slug in any Letterboxd path: /film/<slug>/ or /<user>/film/<slug>/[n/].
var filmPath = regexp.MustCompile(`/film/([a-z0-9-]+)/`)

func slugOf(path string) string {
	if m := filmPath.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// ---- RSS ----

// rssEntry is one diary entry from the feed (list items are skipped).
type rssEntry struct {
	ID       string // the feed guid, e.g. letterboxd-watch-1522858910
	Slug     string
	TMDB     int
	Title    string
	Year     int
	Watched  string // YYYY-MM-DD
	Rating   int    // half stars, 1..10; 0 = unrated
	LoggedAt time.Time
}

func parseRSS(body []byte) ([]rssEntry, error) {
	var doc struct {
		Items []struct {
			Link    string `xml:"link"`
			GUID    string `xml:"guid"`
			PubDate string `xml:"pubDate"`
			Watched string `xml:"https://letterboxd.com watchedDate"`
			Title   string `xml:"https://letterboxd.com filmTitle"`
			Year    int    `xml:"https://letterboxd.com filmYear"`
			Rating  string `xml:"https://letterboxd.com memberRating"`
			MovieID int    `xml:"https://themoviedb.org movieId"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("letterboxd rss: %w", err)
	}
	var out []rssEntry
	for _, it := range doc.Items {
		if it.Watched == "" || !(strings.HasPrefix(it.GUID, "letterboxd-watch-") || strings.HasPrefix(it.GUID, "letterboxd-review-")) {
			continue
		}
		logged, err := time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", it.PubDate) // the day has no leading zero
		if err != nil {
			return nil, fmt.Errorf("letterboxd rss %s: pubDate %q: %w", it.GUID, it.PubDate, err)
		}
		out = append(out, rssEntry{ID: it.GUID, Slug: slugOf(it.Link), TMDB: it.MovieID, Title: it.Title, Year: it.Year,
			Watched: it.Watched, Rating: halfStars(it.Rating), LoggedAt: logged.UTC()})
	}
	return out, nil
}

// halfStars turns "3.5" (stars) into 7; anything unparsable or out of range is "no rating".
func halfStars(stars string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(stars), 64)
	if err != nil || f <= 0 || f > 5 {
		return 0
	}
	return int(f*2 + 0.5)
}

// ---- grid pages (/<user>/watchlist/page/N/, /<user>/films/page/N/) ----

type gridFilm struct {
	LID    string // boxd.it code; the export's URIs use the same code
	Slug   string
	Title  string
	Year   int
	Rating int // half stars from the films grid; 0 when unrated or on the watchlist
}

var (
	gridItem   = regexp.MustCompile(`(?s)<li class="griditem">(.*?)</li>`)
	gridSlug   = regexp.MustCompile(`data-item-slug="([^"]+)"`)
	gridName   = regexp.MustCompile(`data-item-name="([^"]*)"`)
	gridLID    = regexp.MustCompile(`&quot;lid&quot;:&quot;([A-Za-z0-9]+)&quot;`)
	gridRating = regexp.MustCompile(`class="rating[^"]*\brated-(\d+)"`)
	nameYear   = regexp.MustCompile(`^(.*) \((\d{4})\)$`)
	nextPage   = regexp.MustCompile(`<a class="next" href="[^"]*/page/(\d+)/"`)
)

// parseGrid returns the films on one grid page and whether an older page follows.
func parseGrid(body []byte) (films []gridFilm, hasNext bool) {
	for _, m := range gridItem.FindAllSubmatch(body, -1) {
		item := m[1]
		slug := gridSlug.FindSubmatch(item)
		if slug == nil {
			continue
		}
		g := gridFilm{Slug: string(slug[1])}
		if lid := gridLID.FindSubmatch(item); lid != nil {
			g.LID = string(lid[1])
		}
		if n := gridName.FindSubmatch(item); n != nil {
			g.Title, g.Year = splitNameYear(html.UnescapeString(string(n[1])))
		}
		if r := gridRating.FindSubmatch(item); r != nil {
			g.Rating, _ = strconv.Atoi(string(r[1]))
		}
		films = append(films, g)
	}
	return films, nextPage.Match(body)
}

func splitNameYear(s string) (string, int) {
	if m := nameYear.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[2])
		return m[1], y
	}
	return s, 0
}

// ---- film page ----

var (
	bodyTMDB = regexp.MustCompile(`<body[^>]*\bdata-tmdb-id="(\d+)"`)
	bodyKind = regexp.MustCompile(`<body[^>]*\bdata-tmdb-type="([a-z]+)"`)
)

// parseFilmPage reads the TMDB id and type ("movie" or "tv") from the page's <body>; 0 when the page names none.
func parseFilmPage(body []byte) (tmdb int, kind string) {
	if m := bodyTMDB.FindSubmatch(body); m != nil {
		tmdb, _ = strconv.Atoi(string(m[1]))
	}
	if m := bodyKind.FindSubmatch(body); m != nil {
		kind = string(m[1])
	}
	return tmdb, kind
}

// ---- export (the owner's account export, unzipped on the NAS) ----

type exportRow struct {
	LID     string // from the boxd.it URI
	Title   string
	Year    int
	Date    string // the row's "Date": when it was logged / rated / added
	Watched string // diary only: "Watched Date"
	Rating  int    // half stars; 0 = none
}

type export struct {
	Dir       string
	At        time.Time // when Letterboxd made it (from the folder name)
	Watched   []exportRow
	Diary     []exportRow
	Ratings   []exportRow
	Watchlist []exportRow
}

var exportName = regexp.MustCompile(`^letterboxd-.+-(\d{4}-\d{2}-\d{2}-\d{2}-\d{2})-utc$`)

// readExport loads the newest export folder under dir (its name carries the date); ok is false when there is none.
func readExport(dir string) (ex export, ok bool, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return export{}, false, nil
	}
	if err != nil {
		return export{}, false, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && exportName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return export{}, false, nil
	}
	sort.Strings(names) // ISO dates in the name: the last is the newest
	name := names[len(names)-1]
	ex.Dir = filepath.Join(dir, name)
	if ex.At, err = time.Parse("2006-01-02-15-04", exportName.FindStringSubmatch(name)[1]); err != nil {
		return export{}, false, err
	}
	for file, dst := range map[string]*[]exportRow{"watched.csv": &ex.Watched, "diary.csv": &ex.Diary, "ratings.csv": &ex.Ratings, "watchlist.csv": &ex.Watchlist} {
		if *dst, err = readCSV(filepath.Join(ex.Dir, file)); err != nil {
			return export{}, false, err
		}
	}
	return ex, true, nil
}

func readCSV(path string) ([]exportRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[strings.TrimPrefix(h, "\uFEFF")] = i
	}
	if _, ok := col["Letterboxd URI"]; !ok {
		return nil, fmt.Errorf("%s: no Letterboxd URI column", path)
	}
	field := func(r []string, name string) string {
		if i, ok := col[name]; ok && i < len(r) {
			return r[i]
		}
		return ""
	}
	out := make([]exportRow, 0, len(recs)-1)
	for _, r := range recs[1:] {
		year, _ := strconv.Atoi(field(r, "Year"))
		uri := field(r, "Letterboxd URI")
		out = append(out, exportRow{LID: uri[strings.LastIndex(uri, "/")+1:], Title: field(r, "Name"), Year: year,
			Date: field(r, "Date"), Watched: field(r, "Watched Date"), Rating: halfStars(field(r, "Rating"))})
	}
	return out, nil
}
