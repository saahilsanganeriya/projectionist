// Package jellyfin is a small Jellyfin client authenticated by an API key read from a file on every request.
package jellyfin

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
	"time"
)

// ErrNotFound matches Jellyfin's 404 (errors.Is): the item or user does not exist, e.g. removed since a listing.
var ErrNotFound = errors.New("jellyfin: not found")

type Client struct {
	base    string
	keyFile string
	http    *http.Client
}

func New(baseURL, keyFile string, timeout time.Duration) *Client {
	return &Client{base: baseURL, keyFile: keyFile, http: &http.Client{Timeout: timeout}}
}

// WithTimeout is a copy of c whose requests each get timeout instead (see plex.Client.WithTimeout).
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	return &Client{base: c.base, keyFile: c.keyFile, http: &http.Client{Timeout: timeout}}
}

func (c *Client) do(ctx context.Context, method, path string, in any, out any) error {
	if in == nil {
		return c.send(ctx, method, path, "", nil, out)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.send(ctx, method, path, "application/json", raw, out)
}

func (c *Client) send(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	key, err := os.ReadFile(c.keyFile)
	if err != nil {
		return fmt.Errorf("jellyfin key: %w", err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, strings.TrimSpace(string(key))))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("jellyfin %s %s: %w", method, path, err)
	}
	return nil
}

// StatusError is a non-2xx answer; Code tells an outage (5xx) from a refusal (401/403).
type StatusError struct {
	Method, Path string
	Code         int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("jellyfin %s %s: HTTP %d", e.Method, e.Path, e.Code)
}

func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Code == http.StatusNotFound
}

func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// PostJSON sends body as JSON; out may be nil when the answer does not matter.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// PostBody sends a raw body, e.g. a base64 image for POST /Items/<id>/Images/Primary.
func (c *Client) PostBody(ctx context.Context, path, contentType string, body []byte) error {
	return c.send(ctx, http.MethodPost, path, contentType, body, nil)
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
