// Package jellyfin is a small Jellyfin client authenticated by an API key read from a file on every request.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	base    string
	keyFile string
	http    *http.Client
}

func New(baseURL, keyFile string, timeout time.Duration) *Client {
	return &Client{base: baseURL, keyFile: keyFile, http: &http.Client{Timeout: timeout}}
}

// WithTimeout is a copy of c whose requests may take up to d, e.g. a whole-library listing during a scan.
func (c *Client) WithTimeout(d time.Duration) *Client {
	cp := *c
	cp.http = &http.Client{Timeout: d}
	return &cp
}

func (c *Client) do(ctx context.Context, method, path string, in any, out any) error {
	key, err := os.ReadFile(c.keyFile)
	if err != nil {
		return fmt.Errorf("jellyfin key: %w", err)
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, strings.TrimSpace(string(key))))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("jellyfin %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("jellyfin %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("jellyfin %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// PostJSON sends body as JSON; out may be nil when the answer does not matter.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

type Session struct {
	ID              string           `json:"Id"`
	UserName        string           `json:"UserName"`
	NowPlayingItem  *Item            `json:"NowPlayingItem"`
	TranscodingInfo *TranscodingInfo `json:"TranscodingInfo"`
}

type Item struct {
	Name string `json:"Name"`
	Path string `json:"Path"`
}

type TranscodingInfo struct {
	IsVideoDirect            bool   `json:"IsVideoDirect"`
	HardwareAccelerationType string `json:"HardwareAccelerationType"` // "" when absent or null
}

// Sessions lists sessions active within the window (Jellyfin keeps idle clients listed for a while).
func (c *Client) Sessions(ctx context.Context, activeWithin time.Duration) ([]Session, error) {
	var out []Session
	err := c.GetJSON(ctx, fmt.Sprintf("/Sessions?activeWithinSeconds=%d", int(activeWithin.Seconds())), &out)
	return out, err
}

// MediaUpdated tells Jellyfin these folders gained files, so it scans just them.
func (c *Client) MediaUpdated(ctx context.Context, paths ...string) error {
	type update struct {
		Path       string `json:"Path"`
		UpdateType string `json:"UpdateType"`
	}
	body := struct {
		Updates []update `json:"Updates"`
	}{}
	for _, p := range paths {
		body.Updates = append(body.Updates, update{Path: p, UpdateType: "Created"})
	}
	return c.PostJSON(ctx, "/Library/Media/Updated", body, nil)
}
