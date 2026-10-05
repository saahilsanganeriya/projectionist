// Package llm calls Claude through the Anthropic API directly (no gateway): one Messages call, Message Batches for
// bulk work at half price, the newest model of a family from the Models API (for aliases like "haiku:latest"), and
// the dollar cost of every call so a stage can hold a hard budget.
//
// Failure policy: every request has the HTTP client's timeout; 429, 5xx (529 = overloaded) and dropped connections
// are retried twice with jittered backoff (or the server's retry-after); anything else fails at once. Errors never
// carry the key or a request body.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiVersion = "2023-06-01"

type Client struct {
	API  string                 // "https://api.anthropic.com"
	Key  func() (string, error) // read on every request, so a rotated key needs no restart
	HTTP *http.Client
}

// ---- Messages ----

type Request struct {
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	System       string        `json:"system,omitempty"`
	Messages     []Message     `json:"messages"`
	OutputConfig *OutputConfig `json:"output_config,omitempty"`
}

type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

type Block struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// OutputConfig.Format constrains the answer to a JSON schema (structured outputs; every object needs
// "additionalProperties": false).
type OutputConfig struct {
	Format *Format `json:"format,omitempty"`
}

type Format struct {
	Type   string          `json:"type"` // "json_schema"
	Schema json.RawMessage `json:"schema"`
}

func Text(s string) Block { return Block{Type: "text", Text: s} }

func JPEG(b []byte) Block {
	return Block{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(b)}}
}

type Response struct {
	ID         string  `json:"id"`
	Model      string  `json:"model"`
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// Text joins the answer's text blocks.
func (r Response) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// APIError is a non-2xx answer; Type is the API's error type ("invalid_request_error", "overloaded_error", ...).
type APIError struct {
	Status  int
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: HTTP %d %s: %s", e.Status, e.Type, e.Message)
}

func (c *Client) Messages(ctx context.Context, req Request) (Response, error) {
	var resp Response
	err := c.call(ctx, http.MethodPost, "/v1/messages", req, &resp)
	return resp, err
}

// ---- Models ----

// Latest is the newest model (by release date) whose id contains family, e.g. "haiku". Callers cache the answer
// for a day, so a new model is picked up the day it ships.
func (c *Client) Latest(ctx context.Context, family string) (string, error) {
	var list struct {
		Data []struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"data"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/models?limit=1000", nil, &list); err != nil {
		return "", err
	}
	best, bestAt := "", time.Time{}
	for _, m := range list.Data { // newest first, so a tie keeps the first
		if strings.Contains(m.ID, family) && (best == "" || m.CreatedAt.After(bestAt)) {
			best, bestAt = m.ID, m.CreatedAt
		}
	}
	if best == "" {
		return "", fmt.Errorf("anthropic: no model id contains %q", family)
	}
	return best, nil
}

// ---- Message Batches ----

type BatchRequest struct {
	CustomID string  `json:"custom_id"` // ^[a-zA-Z0-9_-]{1,64}$
	Params   Request `json:"params"`
}

type Batch struct {
	ID               string    `json:"id"`
	ProcessingStatus string    `json:"processing_status"` // in_progress | canceling | ended
	ExpiresAt        time.Time `json:"expires_at"`
	ResultsURL       string    `json:"results_url"`
}

// BatchResult is one line of a batch's results; Type is succeeded | errored | canceled | expired, and only a
// succeeded request is billed.
type BatchResult struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string          `json:"type"`
		Message Response        `json:"message"`
		Error   json.RawMessage `json:"error"`
	} `json:"result"`
}

func (c *Client) CreateBatch(ctx context.Context, reqs []BatchRequest) (Batch, error) {
	var b Batch
	err := c.call(ctx, http.MethodPost, "/v1/messages/batches", map[string]any{"requests": reqs}, &b)
	return b, err
}

func (c *Client) Batch(ctx context.Context, id string) (Batch, error) {
	var b Batch
	err := c.call(ctx, http.MethodGet, "/v1/messages/batches/"+url.PathEscape(id), nil, &b)
	return b, err
}

// BatchResults streams an ended batch's results (in any order: match them by CustomID) into fn.
func (c *Client) BatchResults(ctx context.Context, id string, fn func(BatchResult) error) error {
	body, err := c.open(ctx, http.MethodGet, "/v1/messages/batches/"+url.PathEscape(id)+"/results", nil)
	if err != nil {
		return err
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r BatchResult
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return fmt.Errorf("anthropic batch %s results: %w", id, err)
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ---- prices ----

// Price is USD per million tokens. Assumed marks a model missing from the table: its price is twice the highest
// known price of its family, so a budget computed with it errs towards stopping early.
type Price struct {
	Input, Output float64
	Assumed       bool
}

// prices is the first-party list price; longest prefix first, so "claude-sonnet-5-5" wins over "claude-sonnet-5".
var prices = []struct {
	prefix string
	Price
}{
	{"claude-sonnet-5-5", Price{Input: 2, Output: 10}},
	{"claude-sonnet-5", Price{Input: 2, Output: 10}},
	{"claude-haiku-4-5", Price{Input: 1, Output: 5}},
	{"claude-opus-5-5", Price{Input: 4, Output: 20}},
}

// PriceOf accepts any spelling of a model id: "claude-haiku-4-5-20251001", or "anthropic/claude-haiku-4.5" as an
// AI gateway names it.
func PriceOf(model string) Price {
	id := strings.ReplaceAll(strings.TrimPrefix(model, "anthropic/"), ".", "-")
	for _, p := range prices {
		if strings.HasPrefix(id, p.prefix) {
			return p.Price
		}
	}
	var base Price // the family's dearest listed price; every family's when the id names none
	for _, p := range prices {
		if f := family(id); f == "" || f == family(p.prefix) {
			base.Input, base.Output = max(base.Input, p.Input), max(base.Output, p.Output)
		}
	}
	return Price{Input: 2 * base.Input, Output: 2 * base.Output, Assumed: true}
}

func family(id string) string {
	for _, f := range []string{"haiku", "sonnet", "opus"} {
		if strings.Contains(id, f) {
			return f
		}
	}
	return ""
}

// Cost is what u is billed in USD; batch requests are billed at half price. Cache writes cost 1.25x input, reads 0.1x.
func (p Price) Cost(u Usage, batch bool) float64 {
	in := float64(u.InputTokens) + 1.25*float64(u.CacheCreationInputTokens) + 0.1*float64(u.CacheReadInputTokens)
	usd := (in*p.Input + float64(u.OutputTokens)*p.Output) / 1e6
	if batch {
		usd /= 2
	}
	return usd
}

// ---- transport ----

// RetryDelay is the pause before retry n (1, 2) when the server named none; tests set it to zero.
var RetryDelay = func(n int) time.Duration {
	return time.Duration(1<<n)*time.Second + rand.N(time.Second)
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	body, err := c.open(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return fmt.Errorf("anthropic %s %s: %w", method, path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("anthropic %s %s: %w", method, path, err)
	}
	return nil
}

// open sends the request (retrying transient failures) and returns the 2xx body for the caller to close.
func (c *Client) open(ctx context.Context, method, path string, in any) (io.ReadCloser, error) {
	key, err := c.Key()
	if err != nil {
		return nil, fmt.Errorf("anthropic key: %w", err)
	}
	var payload []byte
	if in != nil {
		if payload, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.API+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", strings.TrimSpace(key))
		req.Header.Set("anthropic-version", apiVersion)
		if in != nil {
			req.Header.Set("content-type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			if ue := (*url.Error)(nil); errors.As(err, &ue) {
				err = ue.Err // keeps the URL out of the message
			}
			err = fmt.Errorf("anthropic %s %s: %w", method, path, err)
		case resp.StatusCode/100 == 2:
			return resp.Body, nil
		default:
			err = apiError(resp)
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return nil, err
			}
			if s, convErr := strconv.Atoi(resp.Header.Get("retry-after")); convErr == nil {
				wait = time.Duration(s) * time.Second
			}
		}
		if attempt == 2 || ctx.Err() != nil {
			return nil, err
		}
		if wait == 0 {
			wait = RetryDelay(attempt + 1)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func apiError(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &body)
	return &APIError{Status: resp.StatusCode, Type: body.Error.Type, Message: body.Error.Message}
}
