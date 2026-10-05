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
// a new Haiku from being picked up the day it ships.
func TestLatest(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"claude-sonnet-5-5","created_at":"2026-09-01T00:00:00Z"},
 {"id":"claude-3-5-haiku-20241022","created_at":"2024-10-22T00:00:00Z"},
 {"id":"claude-haiku-5","created_at":"2026-11-01T00:00:00Z"},
 {"id":"claude-haiku-4-5-20251001","created_at":"2025-10-01T00:00:00Z"}]}`)
	})
	if got, err := c.Latest(context.Background(), "haiku"); err != nil || got != "claude-haiku-5" {
		t.Fatalf("Latest = %q, %v; want claude-haiku-5", got, err)
	}
}

// Catches: a model priced under its list price (a budget that stops late), the batch discount missed, or an
// unknown model budgeted at less than its family's dearest price.
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
		{"claude-haiku-5", false, 3, true}, // twice Haiku 4.5's price until the table knows it
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
// nobody records) or reported like an unbilled failure; and a connection that was never made being counted as billed.
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
}
