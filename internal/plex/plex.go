// Package plex is a small Plex Media Server client. Plex's API shares one SQLite database with its scanner and
// stalls during scans, so stages use it only for what has no file-based source: live sessions, preference writes,
// and scan requests. Settings are read from Preferences.xml directly (ReadPrefs).
package plex

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	base      string
	prefsPath string
	http      *http.Client
}

// New reads the token from prefsPath on every request, so a re-sign-in never needs a restart.
func New(baseURL, prefsPath string, timeout time.Duration) *Client {
	return &Client{base: baseURL, prefsPath: prefsPath, http: &http.Client{Timeout: timeout}}
}

// WithTimeout is a copy of c whose requests may take up to d, e.g. a whole-section listing on a busy server.
func (c *Client) WithTimeout(d time.Duration) *Client {
	cp := *c
	cp.http = &http.Client{Timeout: d}
	return &cp
}

// ReadPrefs returns the attributes of Preferences.xml's root element. It never touches the API.
func ReadPrefs(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plex prefs %s: %w", path, err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			prefs := make(map[string]string, len(start.Attr))
			for _, a := range start.Attr {
				prefs[a.Name.Local] = a.Value
			}
			return prefs, nil
		}
	}
}

// Prefs reads the server's Preferences.xml (file, not API).
func (c *Client) Prefs() (map[string]string, error) { return ReadPrefs(c.prefsPath) }

func (c *Client) token() (string, error) {
	prefs, err := ReadPrefs(c.prefsPath)
	if err != nil {
		return "", err
	}
	if prefs["PlexOnlineToken"] == "" {
		return "", errors.New("plex: Preferences.xml has no PlexOnlineToken")
	}
	return prefs["PlexOnlineToken"], nil
}

func (c *Client) do(ctx context.Context, method, path, accept string) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	return c.send(ctx, tok, method, c.base+path, accept, nil)
}

// send puts the token in a header, never in the URL, so request logs cannot leak it.
func (c *Client) send(ctx context.Context, tok, method, rawURL, accept string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	path := req.URL.Path
	req.Header.Set("X-Plex-Token", tok)
	req.Header.Set("X-Plex-Client-Identifier", "projectionist") // plex.tv v2 refuses requests without one
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("plex %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("plex %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return raw, nil
}

// Do sends a request whose answer does not matter (PUT /:/prefs, scan requests).
func (c *Client) Do(ctx context.Context, method, path string) error {
	_, err := c.do(ctx, method, path, "")
	return err
}

// DoAs is Do with another account's token, e.g. a friend's, to write that friend's watch state.
func (c *Client) DoAs(ctx context.Context, token, method, path string) error {
	_, err := c.send(ctx, token, method, c.base+path, "", nil)
	return err
}

// Account calls plex.tv (an absolute URL) as the server owner, e.g. to manage webhooks or read shared users'
// server tokens; form, when not nil, is the urlencoded body. The answer is plex.tv's XML.
func (c *Client) Account(ctx context.Context, method, rawURL string, form url.Values) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	return c.send(ctx, tok, method, rawURL, "", form)
}

// GetJSON decodes the response's MediaContainer object into v; an empty body leaves v untouched.
func (c *Client) GetJSON(ctx context.Context, path string, v any) error {
	body, err := c.do(ctx, http.MethodGet, path, "application/json")
	if err != nil || len(body) == 0 {
		return err
	}
	var wrap struct {
		MediaContainer json.RawMessage `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return fmt.Errorf("plex GET %s: %w", path, err)
	}
	return json.Unmarshal(wrap.MediaContainer, v)
}

// Session is one item playing now. Attribute values are kept as Plex's strings ("" when absent).
type Session struct {
	Type      string // movie, episode, track, ...
	Title     string
	User      string            // Plex account title
	Transcode *TranscodeSession // nil when direct playing
}

type TranscodeSession struct {
	Key              string `xml:"key,attr"`
	VideoDecision    string `xml:"videoDecision,attr"` // transcode, copy, directplay
	HWDecoding       string `xml:"transcodeHwDecoding,attr"`
	HWEncoding       string `xml:"transcodeHwEncoding,attr"`
	SourceVideoCodec string `xml:"sourceVideoCodec,attr"`
	VideoCodec       string `xml:"videoCodec,attr"`
}

// Sessions lists what is playing. XML, because every attribute is then a plain string with one meaning.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	body, err := c.do(ctx, http.MethodGet, "/status/sessions", "application/xml")
	if err != nil {
		return nil, err
	}
	return ParseSessions(body)
}

func ParseSessions(body []byte) ([]Session, error) {
	var mc struct {
		Items []struct {
			Type  string `xml:"type,attr"`
			Title string `xml:"title,attr"`
			User  struct {
				Title string `xml:"title,attr"`
			} `xml:"User"`
			Transcode *TranscodeSession `xml:"TranscodeSession"`
		} `xml:",any"`
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return nil, fmt.Errorf("plex sessions: %w", err)
	}
	out := make([]Session, 0, len(mc.Items))
	for _, it := range mc.Items {
		out = append(out, Session{Type: it.Type, Title: it.Title, User: it.User.Title, Transcode: it.Transcode})
	}
	return out, nil
}
