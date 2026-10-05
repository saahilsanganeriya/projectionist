package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func client(t *testing.T, h http.HandlerFunc) *Client {
	RetryDelay = func(int) time.Duration { return 0 }
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{API: srv.URL, HTTP: srv.Client(), Key: func() (string, error) { return "sk-test", nil }}
}

// Catches: "haiku:latest" resolving to the first listed or an older Haiku, or to another family, which would stop
// a new Haiku from being picked up the day it ships; and a model's effort support misread (Haiku 4.5 answers 400 to
// output_config.effort, and Sonnet 5.5 without it thinks at high effort into max_tokens).
func TestLatest(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"claude-sonnet-5-5","created_at":"2026-09-01T00:00:00Z","capabilities":{"effort":{"supported":true,"low":{"supported":true}}}},
 {"id":"claude-3-5-haiku-20241022","created_at":"2024-10-22T00:00:00Z"},
 {"id":"claude-haiku-5","created_at":"2026-11-01T00:00:00Z","capabilities":{"effort":{"supported":true,"low":{"supported":true}}}},
 {"id":"claude-haiku-4-5-20251001","created_at":"2025-10-01T00:00:00Z","capabilities":{"effort":{"supported":false,"low":{"supported":false}}}}]}`)
	})
	for family, want := range map[string]Model{"haiku": {ID: "claude-haiku-5", LowEffort: true}, "sonnet": {ID: "claude-sonnet-5-5", LowEffort: true},
		"haiku-4-5": {ID: "claude-haiku-4-5-20251001"}} {
		if got, err := c.Latest(context.Background(), family); err != nil || got != want {
			t.Errorf("Latest(%q) = %+v, %v; want %+v", family, got, err, want)
		}
	}
}

// Catches: the owner's switch from Sonnet to Haiku happening a moment early or late (the rule holds until its
// instant, exclusive), a later rule winning over an earlier one, and a policy whose rules have all expired picking
// a model anyway.
func TestPolicy(t *testing.T) {
	switchAt := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC) // 00:00 PST
	custom := Policy{{Model: "opus:latest", Until: switchAt.Add(-time.Hour)}, {Model: "claude-haiku-4-5", Until: switchAt}}
	cases := []struct {
		name   string
		policy Policy
		at     time.Time
		want   string
		err    error
	}{
		{"default, just before", nil, switchAt.Add(-time.Nanosecond), "sonnet:latest", nil},
		{"default, at the instant", nil, switchAt, "haiku:latest", nil},
		{"default, after", DefaultPolicy, switchAt.AddDate(1, 0, 0), "haiku:latest", nil},
		{"first rule still running", custom, switchAt.Add(-2 * time.Hour), "opus:latest", nil},
		{"second rule", custom, switchAt.Add(-time.Hour), "claude-haiku-4-5", nil},
		{"all expired", custom, switchAt, "", ErrPolicyExpired},
	}
	for _, c := range cases {
		got, err := c.policy.Model(c.at)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%s: %q, %v; want %q, %v", c.name, got, err, c.want, c.err)
		}
	}
}

// Catches: a model priced under its list price (a budget that stops late), the batch discount missed, or an
// unknown model budgeted at less than its family's dearest price, including one whose id extends a known one.
func TestPriceOf(t *testing.T) {
	u := Usage{InputTokens: 1_000_000, OutputTokens: 100_000}
	cases := []struct {
		model   string
		batch   bool
		usd     float64
		assumed bool
	}{
		{"claude-haiku-4-5-20251001", false, 1.5, false},
		{"anthropic/claude-haiku-4.5", true, 0.75, false},
		{"claude-sonnet-5-5", false, 3, false},
		{"claude-haiku-5", false, 3, true},    // twice Haiku 4.5's price until the table knows it
		{"claude-sonnet-5-6", false, 6, true}, // a later Sonnet is not priced as Sonnet 5 by its shared prefix
	}
	for _, c := range cases {
		p := PriceOf(c.model)
		if got := p.Cost(u, c.batch); got != c.usd || p.Assumed != c.assumed {
			t.Errorf("%s batch=%v: $%v assumed=%v; want $%v assumed=%v", c.model, c.batch, got, p.Assumed, c.usd, c.assumed)
		}
	}
}

// Catches: an overloaded answer (529) failing the call instead of being retried, and a bad request retried (paid
// for again) or reported without the API's error type.
func TestRetries(t *testing.T) {
	calls := 0
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case strings.Contains(r.Header.Get("x-api-key"), "sk-test") && calls == 1:
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
		case calls == 2:
			io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
		default:
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad image"}}`)
		}
	})
	resp, err := c.Messages(context.Background(), Request{Model: "m", MaxTokens: 1})
	if err != nil || resp.Text() != "ok" || calls != 2 {
		t.Fatalf("after a 529: %q, %v, %d calls; want ok after 2", resp.Text(), err, calls)
	}
	_, err = c.Messages(context.Background(), Request{Model: "m", MaxTokens: 1})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Type != "invalid_request_error" || calls != 3 {
		t.Fatalf("a 400 must fail at once with its type: %v after %d calls", err, calls)
	}
}

// Catches: a POST that reached the server but lost its answer being retried (a second billed call, or a second batch
// nobody records) or reported like an unbilled failure; and a POST whose body never fully left (a refused connection,
// a batch upload cut off by the client timeout) being counted as billed instead of retried.
func TestUnknownOutcome(t *testing.T) {
	var calls atomic.Int32
	dropped := client(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	_, err := dropped.CreateBatch(context.Background(), nil)
	if !errors.Is(err, ErrUnknownOutcome) || calls.Load() != 1 {
		t.Fatalf("connection dropped after sending: %v after %d calls; want ErrUnknownOutcome after 1", err, calls.Load())
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ln.Close()
	refused := &Client{API: "http://" + ln.Addr().String(), HTTP: &http.Client{}, Key: dropped.Key}
	if _, err := refused.Messages(context.Background(), Request{}); err == nil || errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("connection refused: %v; nothing was sent, so it must not count as billed", err)
	}
	var attempts int
	cutOff := &Client{API: "http://api.invalid", Key: dropped.Key, HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		attempts++
		io.CopyN(io.Discard, r.Body, 10) // the upload stalls after 10 bytes and the client gives up
		return nil, errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")
	})}}
	reqs := []BatchRequest{{CustomID: "c1", Params: Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: "user", Content: []Block{Text("hi")}}}}}}
	if _, err := cutOff.CreateBatch(context.Background(), reqs); err == nil || errors.Is(err, ErrUnknownOutcome) || attempts != 3 {
		t.Fatalf("upload cut off part-way: %v after %d attempts; it never reached the API, so retry it (3 attempts) and never count it", err, attempts)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
