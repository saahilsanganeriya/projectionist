// Package tmdb is a direct client for the TMDB v3 API (api.themoviedb.org): search, external-id lookup, show and
// season details. It talks to TMDB only — never through Plex or Jellyfin — and reads the owner's key file on every
// call, so a rotated key needs no restart.
//
// Failure policy: 10 s timeout per request; a 429, a 5xx or a dropped connection is retried twice (1 s and 2 s plus
// jitter, honouring ctx); a 404 is a definite "no such title" (found=false), never an error. Requests are reads, so a
// retry is always safe. The key never reaches a log or an error: a v3 key rides in the query string and transport
// errors quote the URL, so they are rewritten without it.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	KeyFile string       // a v3 API key, or a v4 read-access token (a JWT, sent as a bearer header)
	API     string       // "" means https://api.themoviedb.org/3
	HTTP    *http.Client // nil means a 10 s timeout
}

// RetryDelay is the pause before retry n (1, 2); tests set it to 0.
var RetryDelay = func(n int) time.Duration {
	return time.Duration(n)*time.Second + rand.N(500*time.Millisecond)
}

// Result is one search or find hit, a film or a show.
type Result struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`  // the title in English (TMDB's title / name)
	Original string `json:"original"`
	Year     int    `json:"year"` // 0 when TMDB has no date
}

type Episode struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Name    string `json:"name"`
}

// Show is a TV series with the season numbers it lists (0 = specials).
type Show struct {
	Result
	Seasons []int `json:"seasons"`
}

type hit struct {
	ID            int    `json:"id"`
	Title         string `json:"title"`
	Name          string `json:"name"`
	OriginalTitle string `json:"original_title"`
	OriginalName  string `json:"original_name"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
}

func (h hit) result() Result {
	r := Result{ID: h.ID, Name: h.Title, Original: h.OriginalTitle, Year: year(h.ReleaseDate)}
	if r.Name == "" { // a show
		r.Name, r.Original, r.Year = h.Name, h.OriginalName, year(h.FirstAirDate)
	}
	return r
}

func year(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

func results(hits []hit) []Result {
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.result())
	}
	return out
}

// SearchMovie lists TMDB's first page of films matching query (filtered to year when year > 0), best first.
func (c *Client) SearchMovie(ctx context.Context, query string, year int) ([]Result, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}}
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var resp struct {
		Results []hit `json:"results"`
	}
	_, err := c.get(ctx, "/search/movie", q, &resp)
	return results(resp.Results), err
}

// SearchTV lists TMDB's first page of shows matching query (first aired in year when year > 0), best first.
func (c *Client) SearchTV(ctx context.Context, query string, year int) ([]Result, error) {
	q := url.Values{"query": {query}, "include_adult": {"false"}}
	if year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(year))
	}
	var resp struct {
		Results []hit `json:"results"`
	}
	_, err := c.get(ctx, "/search/tv", q, &resp)
	return results(resp.Results), err
}

// Find maps an external id (source "imdb_id" or "tvdb_id") to TMDB's films and shows.
func (c *Client) Find(ctx context.Context, id, source string) (movies, shows []Result, err error) {
	var resp struct {
		Movies []hit `json:"movie_results"`
		Shows  []hit `json:"tv_results"`
	}
	_, err = c.get(ctx, "/find/"+url.PathEscape(id), url.Values{"external_source": {source}}, &resp)
	return results(resp.Movies), results(resp.Shows), err
}

// TV is a show's details; found is false when TMDB has no such show.
func (c *Client) TV(ctx context.Context, id int) (show Show, found bool, err error) {
	var resp struct {
		hit
		Seasons []struct {
			Number int `json:"season_number"`
		} `json:"seasons"`
	}
	found, err = c.get(ctx, fmt.Sprintf("/tv/%d", id), nil, &resp)
	show.Result = resp.hit.result()
	for _, s := range resp.Seasons {
		show.Seasons = append(show.Seasons, s.Number)
	}
	return show, found, err
}

// Season lists one season's episodes; found is false when TMDB has no such season.
func (c *Client) Season(ctx context.Context, id, season int) (eps []Episode, found bool, err error) {
	var resp struct {
		Episodes []struct {
			Season  int    `json:"season_number"`
			Episode int    `json:"episode_number"`
			Name    string `json:"name"`
		} `json:"episodes"`
	}
	found, err = c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", id, season), nil, &resp)
	for _, e := range resp.Episodes {
		eps = append(eps, Episode{e.Season, e.Episode, e.Name})
	}
	return eps, found, err
}

// get decodes path's JSON into out; found is false on 404.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) (found bool, err error) {
	raw, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return false, fmt.Errorf("tmdb key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return false, errors.New("tmdb key: file is empty")
	}
	api, hc := c.API, c.HTTP
	if api == "" {
		api = "https://api.themoviedb.org/3"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("language", "en-US")
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+path, nil)
		if err != nil {
			return false, err
		}
		if strings.Count(key, ".") == 2 {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			q.Set("api_key", key)
		}
		req.URL.RawQuery = q.Encode()
		req.Header.Set("Accept", "application/json")
		status, body, err := do(hc, req)
		if retry := err != nil || status == http.StatusTooManyRequests || status >= 500; retry && attempt < 2 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(RetryDelay(attempt + 1)):
			}
			continue
		}
		switch {
		case err != nil:
			return false, fmt.Errorf("tmdb GET %s: %w", path, err)
		case status == http.StatusNotFound:
			return false, nil
		case status/100 != 2:
			return false, fmt.Errorf("tmdb GET %s: HTTP %d", path, status)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return false, fmt.Errorf("tmdb GET %s: %w", path, err)
		}
		return true, nil
	}
}

// do returns transport errors without the request URL (which may carry the key).
func do(hc *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, err
}
