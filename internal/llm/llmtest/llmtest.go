// Package llmtest is a fake Vercel AI Gateway, for the tests and for the bake-off harness's dry run (cmd/bakeoff
// -fake): GET /v1/models lists its catalogue in the gateway's shape, GET /v1/models/<id>/endpoints one provider at the
// model's price, and POST /v1/chat/completions hands each request to a script.
package llmtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/llm"
)

// Model is one catalogue entry; prices are USD per million tokens, as people quote them.
type Model struct {
	ID      string
	In, Out float64
	Vision  bool
	Efforts []string
	Toggle  bool
}

// Catalogue is the gateway's real list (2026-10-05) for the models the defaults and the bake-off name.
var Catalogue = []Model{
	{ID: "deepseek/deepseek-v4-flash-0731", In: 0.13, Out: 0.26, Toggle: true, Efforts: []string{"none", "low", "medium", "high"}},
	{ID: "deepseek/deepseek-v4.1-flash", In: 0.30, Out: 1.20, Vision: true, Toggle: true, Efforts: []string{"none", "low", "high", "max"}},
	{ID: "google/gemma-4-31b-it", In: 0.14, Out: 0.40, Vision: true},
	{ID: "zai/glm-5.3-flash", In: 0.15, Out: 0.50, Vision: true, Efforts: []string{"low", "high", "max"}},
	{ID: "alibaba/qwen3.8-27b", In: 0.50, Out: 3.00, Vision: true, Efforts: []string{"none", "low", "medium", "xhigh"}},
	{ID: "deepseek/deepseek-v4-flash-vision-exp", In: 0.2156, Out: 0.6468, Vision: true, Toggle: true, Efforts: []string{"none", "low", "medium", "high"}},
	{ID: "anthropic/claude-sonnet-5.5", In: 2.2, Out: 11, Vision: true, Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
}

// Answer is the fake's reply to one call.
type Answer struct {
	Text    string
	Finish  string // "" = "stop"
	In, Out int    // usage
	Meta    string // the message's provider_metadata, raw JSON; "" = none
	Model   string // the model that answered; "" = the request's

	Status        int // an error answer when set
	Type, Message string
	RetryAfter    string

	Hang bool // never answers: the client's timeout ends the attempt
	Drop bool // reads the request whole, then drops the connection: the answer is lost
}

// Gateway is the fake. Reply answers call n (from 1); unset, every call is "{}".
type Gateway struct {
	Models []Model
	Reply  func(n int, req llm.Request) Answer

	mu     sync.Mutex
	calls  []llm.Request
	keys   []string
	listed int
}

func New(reply func(n int, req llm.Request) Answer) *Gateway {
	return &Gateway{Models: Catalogue, Reply: reply}
}

// Calls are the chat requests received so far, failed ones included.
func (g *Gateway) Calls() []llm.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]llm.Request(nil), g.calls...)
}

// Keys are the bearer tokens the requests carried.
func (g *Gateway) Keys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.keys...)
}

// Listed is how many times the catalogue was asked for.
func (g *Gateway) Listed() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.listed
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case p == "/v1/models":
		g.mu.Lock()
		g.listed++
		g.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": catalogueJSON(g.Models)})
	case strings.HasPrefix(p, "/v1/models/") && strings.HasSuffix(p, "/endpoints"): // a model's providers
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/models/"), "/endpoints")
		for _, m := range g.Models {
			if m.ID == id {
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "endpoints": []any{map[string]any{
					"provider_name": "fake", "pricing": map[string]any{"prompt": perToken(m.In), "completion": perToken(m.Out)}}}}})
				return
			}
		}
		http.NotFound(w, r)
	case p == "/v1/chat/completions":
		var req llm.Request
		raw, err := io.ReadAll(r.Body)
		if err == nil {
			err = json.Unmarshal(raw, &req)
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.calls = append(g.calls, req)
		g.keys = append(g.keys, r.Header.Get("Authorization"))
		n := len(g.calls)
		g.mu.Unlock()
		a := Answer{Text: "{}"}
		if g.Reply != nil {
			a = g.Reply(n, req)
		}
		g.write(w, r, req, a)
	default:
		http.NotFound(w, r)
	}
}

func (g *Gateway) write(w http.ResponseWriter, r *http.Request, req llm.Request, a Answer) {
	switch {
	case a.Hang:
		<-r.Context().Done()
		return
	case a.Drop:
		panic(http.ErrAbortHandler) // the server closes the connection without an answer
	case a.Status != 0:
		if a.RetryAfter != "" {
			w.Header().Set("Retry-After", a.RetryAfter)
		}
		w.WriteHeader(a.Status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": a.Type, "message": a.Message}})
		return
	}
	finish, model := a.Finish, a.Model
	if finish == "" {
		finish = "stop"
	}
	if model == "" {
		model = req.Model
	}
	msg := map[string]any{"role": "assistant", "content": a.Text}
	if a.Meta != "" {
		msg["provider_metadata"] = json.RawMessage(a.Meta)
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id": "gen_fake", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": a.In, "completion_tokens": a.Out, "total_tokens": a.In + a.Out},
	})
}

func perToken(usdPerM float64) string { return strconv.FormatFloat(usdPerM/1e6, 'f', -1, 64) }

func catalogueJSON(models []Model) []any {
	var out []any
	for _, m := range models {
		input := []string{"text"}
		if m.Vision {
			input = append(input, "image")
		}
		var reasoning []any
		if m.Toggle {
			reasoning = append(reasoning, map[string]any{"type": "toggle"})
		}
		if len(m.Efforts) > 0 {
			reasoning = append(reasoning, map[string]any{"type": "effort", "values": m.Efforts})
		}
		out = append(out, map[string]any{
			"id": m.ID, "object": "model", "type": "language", "modalities": map[string]any{"input": input, "output": []string{"text"}},
			"reasoning_options": reasoning,
			"pricing":           map[string]any{"input": perToken(m.In), "output": perToken(m.Out), "varies_by_provider": true},
		})
	}
	return append(out, map[string]any{"id": "openai/image-model", "type": "image", "pricing": map[string]any{"image": "0.04"}})
}

// Server serves g on a real listener, closed when t ends.
func (g *Gateway) Server(t testing.TB) *httptest.Server {
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

// Transport serves g in memory, for tests on a fake clock (testing/synctest), where a real listener's I/O would
// keep the clock from moving. A dropped answer is a transport error after the body was read.
func (g *Gateway) Transport() http.RoundTripper {
	return roundTrip(func(r *http.Request) (resp *http.Response, err error) {
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("connection reset by peer")
				}
			}()
			g.ServeHTTP(rec, r)
		}()
		select {
		case <-done:
			if err != nil {
				return nil, err
			}
			return rec.Result(), nil
		case <-r.Context().Done():
			<-done // the hanging handler returns once the request's context ends
			return nil, r.Context().Err()
		}
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Client is an llm client on g's server with cfg over the defaults, a key file holding "test-SECRET-gateway-key" (a
// log that shows "SECRET" leaked it), and no backoff pauses.
func Client(t testing.TB, g *Gateway, st llm.Store, now func() time.Time, cfg llm.Config) *llm.Client {
	key := filepath.Join(t.TempDir(), "ai_gateway_api_key")
	if err := os.WriteFile(key, []byte("test-SECRET-gateway-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.API, cfg.KeyFile = g.Server(t).URL+"/v1", key
	c := llm.New(cfg, st, now)
	c.Backoff = func(int) time.Duration { return 0 }
	return c
}
