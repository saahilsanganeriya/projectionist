// Package tmdb is a direct client for TMDB's v3 API (api.themoviedb.org): search, film and show details, a season's
// episodes, and artwork. It is projectionist's source of identity, episode lists, specials, collections, external
// ids and images; no request goes through Plex or Jellyfin, and there is no fallback through either.
//
// Auth: the v4 read-access token as a bearer, read from its file on every request; the v3 key (sent as api_key)
// only when the token file is missing or empty. Neither ever reaches a log, an error or a cache key.
//
// One Client per process, shared by every stage: its token bucket (40 req/s, one-second burst) keeps the whole
// service under TMDB's ~50 req/s per IP, which Jellyfin's own TMDB provider also spends from. Each request times
// out after 10 s; a 429, a 5xx or a transport error is retried twice (Retry-After, else 1 s then 2 s), then fails
// so the stage retries on its next run. Answers are cached in the store's http_cache for CacheTTL (24 h): stages
// asking about one title on one day cost one request.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectionist/internal/store"
)

var (
	ErrNotFound      = errors.New("tmdb: not found")
	ErrNoCredentials = errors.New("tmdb: no read token and no API key")
)

type Kind string

const (
	Movie Kind = "movie"
	TV    Kind = "tv"
)

const (
	retries     = 2
	cachePrefix = "tmdb:"
)

type Options struct {
	ReadTokenFile string        // v4 read-access token, preferred
	APIKeyFile    string        // v3 API key, used only when the token file is missing or empty
	BaseURL       string        // "" = https://api.themoviedb.org/3
	Rate          float64       // requests per second, also the burst; 0 = 40
	CacheTTL      time.Duration // 0 = 24h
	Timeout       time.Duration // per request; 0 = 10s
}

type Client struct {
	opt   Options
	http  *http.Client
	cache *store.Store // nil: no cache
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu     sync.Mutex // guards the token bucket
	tokens float64
	last   time.Time

	pruned sync.Once
}

// New builds the process's client; cache may be nil.
func New(opt Options, cache *store.Store) *Client {
	if opt.BaseURL == "" {
		opt.BaseURL = "https://api.themoviedb.org/3"
	}
	if opt.Rate <= 0 {
		opt.Rate = 40
	}
	if opt.CacheTTL <= 0 {
		opt.CacheTTL = 24 * time.Hour
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	return &Client{opt: opt, http: &http.Client{Timeout: opt.Timeout}, cache: cache, now: time.Now, sleep: sleepCtx, tokens: opt.Rate}
}

// ImageURL is the download address of an image's FilePath at a size ("original", "w500", ...).
func ImageURL(filePath, size string) string { return "https://image.tmdb.org/t/p/" + size + filePath }

// Date is a TMDB calendar date at UTC midnight; zero when TMDB has none.
type Date struct{ time.Time }

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" { // null and "" both mean unknown
		return err
	}
	t, err := time.Parse(time.DateOnly, s)
	d.Time = t
	return err
}

// MarshalJSON keeps TMDB's spelling, so a stored Date reads back (time.Time's own would write RFC 3339).
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(d.Format(time.DateOnly))
}

// Result is one search hit. Title and Date are the film's title and release date, or the show's name and first air
// date.
type Result struct {
	ID               int     `json:"id"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"original_title"`
	Date             Date    `json:"release_date"`
	OriginalLanguage string  `json:"original_language"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	Popularity       float64 `json:"popularity"`
	VoteCount        int     `json:"vote_count"`
}

type Details struct {
	ID            int         `json:"id"`
	Kind          Kind        `json:"kind"`
	Title         string      `json:"title"`
	OriginalTitle string      `json:"original_title"`
	Date          Date        `json:"release_date"`
	Overview      string      `json:"overview"`
	Language      string      `json:"original_language"` // ISO 639-1
	Genres        []Genre     `json:"genres"`
	PosterPath    string      `json:"poster_path"`
	BackdropPath  string      `json:"backdrop_path"`
	Runtime       int         `json:"runtime"`               // minutes; films only
	Status        string      `json:"status"`                // "Released", "Returning Series", "Ended", ...
	Collection    *Collection `json:"belongs_to_collection"` // films only; nil when the film is in none
	ExternalIDs   ExternalIDs `json:"external_ids"`
	Seasons       []Season    `json:"seasons"` // shows only; season 0 holds the specials
}

type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Collection struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
}

type ExternalIDs struct {
	IMDb string `json:"imdb_id"` // "tt0113277"
	TVDB int    `json:"tvdb_id"` // shows only; fanart.tv keys show art by it
}

type Season struct {
	Number       int    `json:"season_number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
	AirDate      Date   `json:"air_date"`
	PosterPath   string `json:"poster_path"`
}

type Episode struct {
	ID       int    `json:"id"`
	Season   int    `json:"season_number"`
	Number   int    `json:"episode_number"`
	Name     string `json:"name"`
	AirDate  Date   `json:"air_date"`
	Runtime  int    `json:"runtime"` // minutes
	Overview string `json:"overview"`
}

type Image struct {
	FilePath    string  `json:"file_path"`
	Language    string  `json:"iso_639_1"` // "" when untagged, which on TMDB usually means textless
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
}

type Images struct {
	Posters   []Image `json:"posters"`
	Backdrops []Image `json:"backdrops"`
	Logos     []Image `json:"logos"`
}

// showSpelling holds the fields TMDB names differently for shows; fromShow copies them over the film spellings.
type showSpelling struct {
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
	FirstAirDate Date   `json:"first_air_date"`
}

func (s showSpelling) fromShow(title, original *string, date *Date) {
	*title, *original, *date = s.Name, s.OriginalName, s.FirstAirDate
}

// Search asks TMDB for films or shows matching query; year (0 = any) narrows by release or first-air year.
// The first page (TMDB's 20 best matches) is returned in TMDB's order.
func (c *Client) Search(ctx context.Context, kind Kind, query string, year int) ([]Result, error) {
	q := url.Values{"query": {query}}
	if year > 0 {
		param := "year"
		if kind == TV {
			param = "first_air_date_year"
		}
		q.Set(param, strconv.Itoa(year))
	}
	var resp struct {
		Results []struct {
			Result
			showSpelling
		} `json:"results"`
	}
	if err := c.get(ctx, "/search/"+string(kind), q, &resp); err != nil {
		return nil, err
	}
	out := make([]Result, len(resp.Results))
	for i, r := range resp.Results {
		out[i] = r.Result
		if kind == TV {
			r.fromShow(&out[i].Title, &out[i].OriginalTitle, &out[i].Date)
		}
	}
	return out, nil
}

// Details is a film's or show's record, with its external ids, collection (films) and seasons (shows).
func (c *Client) Details(ctx context.Context, kind Kind, id int) (Details, error) {
	var raw struct {
		Details
		showSpelling
	}
	if err := c.get(ctx, fmt.Sprintf("/%s/%d", kind, id), url.Values{"append_to_response": {"external_ids"}}, &raw); err != nil {
		return Details{}, err
	}
	d := raw.Details
	d.Kind = kind
	if kind == TV {
		raw.fromShow(&d.Title, &d.OriginalTitle, &d.Date)
	}
	return d, nil
}

// Episodes lists one season of a show; season 0 is the specials.
func (c *Client) Episodes(ctx context.Context, showID, season int) ([]Episode, error) {
	var resp struct {
		Episodes []Episode `json:"episodes"`
	}
	err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", showID, season), nil, &resp)
	return resp.Episodes, err
}

// Images lists a film's or show's posters, backdrops and logos. langs (ISO 639-1; "null" = untagged) narrows the
// list; none means every language.
func (c *Client) Images(ctx context.Context, kind Kind, id int, langs ...string) (Images, error) {
	return c.images(ctx, fmt.Sprintf("/%s/%d/images", kind, id), langs)
}

// SeasonImages lists a season's posters.
func (c *Client) SeasonImages(ctx context.Context, showID, season int, langs ...string) (Images, error) {
	return c.images(ctx, fmt.Sprintf("/tv/%d/season/%d/images", showID, season), langs)
}

func (c *Client) images(ctx context.Context, path string, langs []string) (Images, error) {
	q := url.Values{}
	if len(langs) > 0 {
		q.Set("include_image_language", strings.Join(langs, ","))
	}
	var out Images
	err := c.get(ctx, path, q, &out)
	return out, err
}

// get decodes GET path?query into out, answering from the cache while the stored copy is younger than CacheTTL.
// A cache that cannot be read or written only costs a request.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	key := cachePrefix + path + "?" + query.Encode()
	now := c.now()
	fresh := now.Add(-c.opt.CacheTTL)
	if c.cache != nil {
		c.pruned.Do(func() { c.cache.CachePrune(ctx, cachePrefix, fresh) })
		if body, ok, err := c.cache.CacheGet(ctx, key, fresh); err == nil && ok && json.Unmarshal(body, out) == nil {
			return nil
		}
	}
	body, err := c.fetch(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("tmdb GET %s: %w", path, err)
	}
	if c.cache != nil {
		c.cache.CachePut(ctx, key, body, now)
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, path string, query url.Values) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, retryIn, err := c.do(ctx, path, query, attempt)
		if err == nil || retryIn == 0 || attempt == retries {
			return body, err
		}
		if err := c.sleep(ctx, retryIn); err != nil {
			return nil, err
		}
	}
}

// do sends one request. retryIn > 0 means the failure is transient and names the wait before the next attempt.
func (c *Client) do(ctx context.Context, path string, query url.Values, attempt int) (body []byte, retryIn time.Duration, err error) {
	backoff := time.Second << attempt
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opt.BaseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if err := c.authorize(req, q); err != nil {
		return nil, 0, err
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "application/json")
	if err := c.wait(ctx); err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // ue's message quotes the URL, which may hold the v3 key
		}
		return nil, backoff, fmt.Errorf("tmdb GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case err != nil:
		return nil, backoff, fmt.Errorf("tmdb GET %s: %w", path, err)
	case resp.StatusCode == http.StatusNotFound:
		return nil, 0, fmt.Errorf("tmdb GET %s: %w", path, ErrNotFound)
	case resp.StatusCode == http.StatusTooManyRequests:
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			backoff = time.Duration(s) * time.Second
		}
		return nil, backoff, fmt.Errorf("tmdb GET %s: HTTP 429", path)
	case resp.StatusCode >= 500:
		return nil, backoff, fmt.Errorf("tmdb GET %s: HTTP %d", path, resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return nil, 0, fmt.Errorf("tmdb GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, 0, nil
}

// authorize adds the bearer token, or the v3 key to q when there is no token.
func (c *Client) authorize(req *http.Request, q url.Values) error {
	token, err := readSecret(c.opt.ReadTokenFile)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	key, err := readSecret(c.opt.APIKeyFile)
	if err != nil {
		return err
	}
	if key == "" {
		return ErrNoCredentials
	}
	q.Set("api_key", key)
	return nil
}

// readSecret returns the file's trimmed content; a missing file (or none configured) is "".
func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("tmdb secret: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// wait takes one token from the bucket, sleeping off any deficit. Tokens are reserved under the lock, so
// concurrent callers queue up at the rate instead of all waking at once.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := c.now()
	if !c.last.IsZero() {
		c.tokens = math.Min(c.opt.Rate, c.tokens+now.Sub(c.last).Seconds()*c.opt.Rate)
	}
	c.last = now
	c.tokens--
	deficit := -c.tokens
	c.mu.Unlock()
	if deficit <= 0 {
		return nil
	}
	return c.sleep(ctx, time.Duration(deficit/c.opt.Rate*float64(time.Second)))
}

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
