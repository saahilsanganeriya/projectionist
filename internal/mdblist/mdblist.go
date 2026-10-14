// Package mdblist reads films' ratings from MDBList (api.mdblist.com) by TMDB id, up to Batch films a request: among
// them Letterboxd's average and Rotten Tomatoes' critics score. A free key allows 1,000 requests a day (reset at 00:00
// UTC). Cloudflare in front of it refuses a client that does not name itself, so every request carries a User-Agent.
package mdblist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Batch is the most TMDB ids one request asks for (200 cost one request on 2026-10-14).
const Batch = 200

// Ratings are a film's scores on MDBList's 0-100 scale; -1 where a source has none. Letterboxd's is its average
// stars × 20 (the score agrees between MDBList's endpoints; the raw value is out of 5 in one and out of 10 in another).
type Ratings struct {
	Letterboxd int
	Tomatoes   int // the critics' score, not the audience's ("popcorn")
}

var (
	ErrNoKey = errors.New("mdblist: no API key")
	ErrQuota = errors.New("mdblist: the day's requests are spent")
)

type Client struct {
	BaseURL string // https://api.mdblist.com
	KeyFile string
	HC      *http.Client
}

// Movies are the ratings of the films MDBList knows among ids (at most Batch); a film it does not know is absent.
func (c *Client) Movies(ctx context.Context, ids []int) (map[int]Ratings, error) {
	if len(ids) > Batch {
		return nil, fmt.Errorf("mdblist: %d ids, at most %d a request", len(ids), Batch)
	}
	key, err := os.ReadFile(c.KeyFile)
	if err != nil || strings.TrimSpace(string(key)) == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoKey, c.KeyFile)
	}
	body, _ := json.Marshal(map[string][]int{"ids": ids})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/tmdb/movie?apikey="+strings.TrimSpace(string(key)), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "projectionist (+https://github.com/saahilsanganeriya/projectionist)")
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mdblist: %w", errors.Unwrap(err)) // the URL carries the key: never in an error
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	switch {
	case err != nil:
		return nil, fmt.Errorf("mdblist: %w", err)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrQuota
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("mdblist: HTTP %d: %.200s", resp.StatusCode, raw)
	}
	var films []struct {
		IDs struct {
			TMDB int `json:"tmdb"`
		} `json:"ids"`
		Ratings []struct {
			Source string `json:"source"`
			Score  *int   `json:"score"`
		} `json:"ratings"`
	}
	if err := json.Unmarshal(raw, &films); err != nil {
		return nil, fmt.Errorf("mdblist: %w", err)
	}
	out := make(map[int]Ratings, len(films))
	for _, f := range films {
		r := Ratings{Letterboxd: -1, Tomatoes: -1}
		for _, s := range f.Ratings {
			switch {
			case s.Score == nil || *s.Score <= 0:
			case s.Source == "letterboxd":
				r.Letterboxd = *s.Score
			case s.Source == "tomatoes":
				r.Tomatoes = *s.Score
			}
		}
		if f.IDs.TMDB > 0 {
			out[f.IDs.TMDB] = r
		}
	}
	return out, nil
}
