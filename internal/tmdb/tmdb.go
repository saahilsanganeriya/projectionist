// Package tmdb is a direct client for TMDB's v3 API. It authenticates with the v4 read token, read from a file on
// every request and sent as a header (never in a URL), so a rotated token needs no restart and no log can leak it.
// It never goes through Plex or Jellyfin.
//
// Failure policy: one attempt per call with the client's timeout and no retry; callers keep the work pending and
// redo it on their next run. ErrNotFound marks an id TMDB does not know, which no retry can fix.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const DefaultURL = "https://api.themoviedb.org/3"

var ErrNotFound = errors.New("tmdb: not found")

type Client struct {
	base      string
	tokenFile string
	http      *http.Client
}

func New(baseURL, tokenFile string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimSuffix(baseURL, "/"), tokenFile: tokenFile, http: &http.Client{Timeout: timeout}}
}

// GetJSON decodes the answer to GET path (relative to the API root, query included) into out.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	tok, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("tmdb token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("tmdb GET %s: %w", path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("tmdb GET %s: %w", path, ErrNotFound)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("tmdb GET %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("tmdb GET %s: %w", path, err)
	}
	return nil
}

type Person struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Movie is the part of /movie/{id} the pipeline uses.
type Movie struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Collection *struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"belongs_to_collection"` // nil when the film is in no TMDB collection
	Directors []Person `json:"-"`
}

// Movie fetches a film with its directors (credits appended, so one request).
func (c *Client) Movie(ctx context.Context, id int) (Movie, error) {
	var raw struct {
		Movie
		Credits struct {
			Crew []struct {
				Person
				Job string `json:"job"`
			} `json:"crew"`
		} `json:"credits"`
	}
	if err := c.GetJSON(ctx, fmt.Sprintf("/movie/%d?append_to_response=credits", id), &raw); err != nil {
		return Movie{}, err
	}
	m := raw.Movie
	for _, p := range raw.Credits.Crew {
		if p.Job == "Director" {
			m.Directors = append(m.Directors, p.Person)
		}
	}
	return m, nil
}

// Directed lists the TMDB ids of every film the person is credited as Director on.
func (c *Client) Directed(ctx context.Context, personID int) ([]int, error) {
	var raw struct {
		Crew []struct {
			ID  int    `json:"id"`
			Job string `json:"job"`
		} `json:"crew"`
	}
	if err := c.GetJSON(ctx, fmt.Sprintf("/person/%d/movie_credits", personID), &raw); err != nil {
		return nil, err
	}
	var ids []int
	for _, f := range raw.Crew {
		if f.Job == "Director" {
			ids = append(ids, f.ID)
		}
	}
	return ids, nil
}
