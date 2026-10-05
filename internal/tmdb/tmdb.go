// Package tmdb is a direct TMDB v3 client (api.themoviedb.org): no media app in between and no fallback through one.
//
// Failure policy: the HTTP client's timeout bounds each attempt; 429, 5xx and dropped connections are retried twice
// with jittered backoff; a 404 is ErrNotFound. Errors never carry a request URL, which would hold a v3 key.
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
	"strings"
	"time"
)

type Client struct {
	API  string                 // "https://api.themoviedb.org/3"
	Key  func() (string, error) // a v4 read-access token (a JWT, sent as a bearer header) or a v3 key; read per call
	HTTP *http.Client
}

var ErrNotFound = errors.New("tmdb: not found")

// RetryDelay is the pause before retry n (1, 2); tests set it to zero.
var RetryDelay = func(n int) time.Duration {
	return time.Duration(n)*time.Second + rand.N(500*time.Millisecond)
}

// Get decodes the JSON at path (e.g. "/movie/603", query may be nil) into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	key, err := c.Key()
	if err != nil {
		return fmt.Errorf("tmdb key: %w", err)
	}
	key = strings.TrimSpace(key)
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	bearer := strings.Count(key, ".") == 2
	if !bearer {
		q.Set("api_key", key)
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.API+path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		if bearer {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Accept", "application/json")
		status, body, err := c.do(req)
		retry := err != nil || status == http.StatusTooManyRequests || status >= 500
		if retry && attempt < 2 && ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(RetryDelay(attempt + 1)):
			}
			continue
		}
		switch {
		case err != nil:
			return fmt.Errorf("tmdb GET %s: %w", path, err)
		case status == http.StatusNotFound:
			return fmt.Errorf("tmdb GET %s: %w", path, ErrNotFound)
		case status/100 != 2:
			return fmt.Errorf("tmdb GET %s: HTTP %d", path, status)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("tmdb GET %s: %w", path, err)
		}
		return nil
	}
}

func (c *Client) do(req *http.Request) (int, []byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, body, err
}
