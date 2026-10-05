// Package fanart is a direct client for fanart.tv's v3.2 API: the community artwork (posters, logos, clear art,
// backgrounds, season posters) for a film or a show, each image with its language and likes.
//
// Films are keyed by TMDB or IMDb id, shows by TVDB id (tmdb.Details.ExternalIDs.TVDB). The API key is read from
// its file on every request and sent in the api-key header, never in a URL. Each request times out after 10 s;
// a failure is returned for the stage to retry on its next run. Answers are cached in the store's http_cache for
// CacheTTL (24 h).
package fanart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/store"
)

const cachePrefix = "fanart:"

var ErrNoKey = errors.New("fanart: no API key")

type Options struct {
	APIKeyFile string
	BaseURL    string        // "" = https://webservice.fanart.tv/v3.2
	CacheTTL   time.Duration // 0 = 24h
	Timeout    time.Duration // per request; 0 = 10s
}

type Client struct {
	opt   Options
	http  *http.Client
	cache *store.Store // nil: no cache
	now   func() time.Time
}

func New(opt Options, cache *store.Store) *Client {
	if opt.BaseURL == "" {
		opt.BaseURL = "https://webservice.fanart.tv/v3.2"
	}
	if opt.CacheTTL <= 0 {
		opt.CacheTTL = 24 * time.Hour
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	return &Client{opt: opt, http: &http.Client{Timeout: opt.Timeout}, cache: cache, now: time.Now}
}

// Image is one artwork. Type is fanart.tv's name for the kind: movieposter, hdmovielogo, moviebackground, tvposter,
// hdtvlogo, seasonposter, ...
type Image struct {
	Type   string
	ID     string
	URL    string
	Lang   string // ISO 639-1; "" or "00" when the art has no text
	Likes  int
	Season string // season art only: "1", "0" for specials, "all"
	Width  int    // 0 when fanart.tv does not say
	Height int
}

// Movie lists a film's art by TMDB id ("949") or IMDb id ("tt0113277"); none is an empty list.
func (c *Client) Movie(ctx context.Context, id string) ([]Image, error) {
	return c.images(ctx, "/movies/"+id)
}

// TV lists a show's art by TVDB id; none is an empty list.
func (c *Client) TV(ctx context.Context, tvdbID int) ([]Image, error) {
	return c.images(ctx, "/tv/"+strconv.Itoa(tvdbID))
}

// images flattens the answer's per-type arrays; the other keys (name, ids, *_count) are skipped. Within a type,
// fanart.tv's order (most liked first) is kept, and types come in key order so the result is stable.
func (c *Client) images(ctx context.Context, path string) ([]Image, error) {
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var byType map[string]json.RawMessage
	if err := json.Unmarshal(body, &byType); err != nil {
		return nil, fmt.Errorf("fanart GET %s: %w", path, err)
	}
	types := make([]string, 0, len(byType))
	for t, raw := range byType {
		if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			types = append(types, t)
		}
	}
	slices.Sort(types)
	var out []Image
	for _, t := range types {
		var raw []struct{ ID, URL, Lang, Likes, Season, Width, Height string }
		if err := json.Unmarshal(byType[t], &raw); err != nil {
			return nil, fmt.Errorf("fanart GET %s: %s: %w", path, t, err)
		}
		for _, r := range raw {
			likes, _ := strconv.Atoi(r.Likes)
			w, _ := strconv.Atoi(r.Width)
			h, _ := strconv.Atoi(r.Height)
			out = append(out, Image{Type: t, ID: r.ID, URL: r.URL, Lang: r.Lang, Likes: likes, Season: r.Season, Width: w, Height: h})
		}
	}
	return out, nil
}

// get returns the answer's body; a 404 (no art for the title) reads as an empty object, which is also what
// fanart.tv sometimes answers with a 200.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	key := cachePrefix + path
	now := c.now()
	if c.cache != nil {
		if body, ok, err := c.cache.CacheGet(ctx, key, now.Add(-c.opt.CacheTTL)); err == nil && ok {
			return body, nil
		}
	}
	apiKey, err := os.ReadFile(c.opt.APIKeyFile)
	if errors.Is(err, os.ErrNotExist) || (err == nil && strings.TrimSpace(string(apiKey)) == "") {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, fmt.Errorf("fanart key: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opt.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("api-key", strings.TrimSpace(string(apiKey)))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fanart GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case err != nil:
		return nil, fmt.Errorf("fanart GET %s: %w", path, err)
	case resp.StatusCode == http.StatusNotFound:
		body = []byte("{}")
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("fanart GET %s: HTTP %d", path, resp.StatusCode)
	}
	if c.cache != nil {
		c.cache.CachePut(ctx, key, body, now) // a failed write only costs a request
	}
	return body, nil
}
