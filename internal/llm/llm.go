// Package llm calls models through the Vercel AI Gateway's OpenAI-compatible Chat Completions API, the one path for
// every stage that asks a model: one key, one process-wide bound on calls in flight, one failure policy, and the dollar
// cost of every call so a stage can hold a hard budget. Each task (posters, collections, tonight) names its model and
// an ordered list of fallbacks in [llm.tasks.<task>]; a Claude model is just another id ("anthropic/claude-sonnet-5.5").
//
// Failure policy (docs/STAGES.md repeats it):
//
//   - at most in_flight calls (8) are in the air across the process; each attempt has its own timeout (120 s);
//   - 429, 5xx, timeouts and dropped connections are retried with jittered backoff, or after the server's Retry-After
//     (one over 5 minutes is not waited for), up to attempts (4) in all; any other answer fails the call at once;
//   - an attempt whose request was sent in full but whose answer was lost (a timeout, a dropped connection) may have
//     been billed: the call counts it at its worst case (Result.CostUSD) even when a retry then answers;
//   - a Session parks after park_after (5) failed calls in a row, and at once when the gateway refuses for billing
//     (402, or a 403 for no card on file or a model outside the free tier): a parked session makes no more calls, and
//     the billing refusal is no model failure and bills nothing. The refusal is kept in the store until a call is
//     answered again; meanwhile no call is sent but one an hour (billingRecheck), across the process and its restarts,
//     and the stages raise one alert for it in all (TellBilling).
//
// Errors never carry the key or a request body.
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- configuration ----

// Config is the [llm] table.
type Config struct {
	API       string          `toml:"api"`
	KeyFile   string          `toml:"key_file"`
	InFlight  int             `toml:"in_flight"`  // calls in the air across the process
	Timeout   time.Duration   `toml:"timeout"`    // per attempt
	Attempts  int             `toml:"attempts"`   // per call, the first included
	ParkAfter int             `toml:"park_after"` // failed calls in a row before a session parks
	Tasks     map[string]Task `toml:"tasks"`
}

// Task is one [llm.tasks.<name>] table: the model, the fallbacks the gateway tries in order when it fails, and how
// much it may reason: "off" asks for the least the model allows (no reasoning, or its lowest effort), any other value
// is sent as the effort ("low", "medium", ...). Reasoning applies to the fallbacks too.
type Task struct {
	Model     string   `toml:"model"`
	Fallbacks []string `toml:"fallbacks"`
	Reasoning string   `toml:"reasoning"`
}

// The bake-off's picks (2026-10-06): the posters and collections judge, and the cheap fallback for every task.
const (
	VisionModel   = "alibaba/qwen3-vl-instruct"
	FallbackModel = "alibaba/qwen3.7-flash"
)

// Defaults is the configuration a missing [llm] key takes. The vision tasks send effort "none": "off" reads the
// first model's controls, and qwen3-vl-instruct has none, so the fallback would reason.
func Defaults() Config {
	return Config{
		API: "https://ai-gateway.vercel.sh/v1", KeyFile: "/run/secrets/ai_gateway_api_key",
		InFlight: 8, Timeout: 120 * time.Second, Attempts: 4, ParkAfter: 5,
		Tasks: map[string]Task{
			"posters":     {Model: VisionModel, Fallbacks: []string{FallbackModel}, Reasoning: "none"},
			"collections": {Model: VisionModel, Fallbacks: []string{FallbackModel}, Reasoning: "none"},
			"tonight":     {Model: "deepseek/deepseek-v4.1-flash", Fallbacks: []string{FallbackModel}, Reasoning: "off"},
		},
	}
}

// withDefaults fills every unset key from Defaults, a task's keys one by one (a table may set only its fallbacks).
func (c Config) withDefaults() Config {
	d := Defaults()
	if c.API == "" {
		c.API = d.API
	}
	if c.KeyFile == "" {
		c.KeyFile = d.KeyFile
	}
	if c.InFlight <= 0 {
		c.InFlight = d.InFlight
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.Attempts <= 0 {
		c.Attempts = d.Attempts
	}
	if c.ParkAfter <= 0 {
		c.ParkAfter = d.ParkAfter
	}
	tasks := map[string]Task{}
	for name, t := range d.Tasks {
		tasks[name] = t
	}
	for name, t := range c.Tasks {
		def := tasks[name]
		if t.Model == "" {
			t.Model = def.Model
		}
		if t.Fallbacks == nil { // absent; "fallbacks = []" is none
			t.Fallbacks = def.Fallbacks
		}
		if t.Reasoning == "" {
			t.Reasoning = def.Reasoning
		}
		tasks[name] = t
	}
	c.Tasks = tasks
	return c
}

// Store keeps the daily model catalogue and a billing refusal across runs (store.Store).
type Store interface {
	GetJSON(ctx context.Context, stage, key string, v any) (bool, error)
	PutJSON(ctx context.Context, stage, key string, v any) error
}

// Client is one per process: its bound on calls in flight holds across every stage.
type Client struct {
	cfg   Config
	Key   func() (string, error) // read on every request, so a rotated key needs no restart
	HTTP  *http.Client           // no timeout of its own: Config.Timeout bounds each attempt
	store Store
	now   func() time.Time
	sem   chan struct{}
	// Backoff is the pause before retry n (1, 2, ...) when the server named none; tests set it to zero.
	Backoff func(n int) time.Duration

	catMu sync.Mutex
	cat   catalogue

	blockMu sync.Mutex
	blocked *blocked      // nil until read from the store
	probe   chan struct{} // held by the one call that tries a refusing gateway again
}

func New(cfg Config, st Store, now func() time.Time) *Client {
	cfg = cfg.withDefaults()
	return &Client{
		cfg: cfg, store: st, now: now, HTTP: &http.Client{}, sem: make(chan struct{}, cfg.InFlight), Backoff: jittered,
		probe: make(chan struct{}, 1),
		Key: func() (string, error) {
			b, err := os.ReadFile(cfg.KeyFile)
			return strings.TrimSpace(string(b)), err
		},
	}
}

// InFlight is how many calls the client lets into the air at once: a stage fans out this wide.
func (c *Client) InFlight() int { return c.cfg.InFlight }

// Task is the named task's configuration.
func (c *Client) Task(name string) Task { return c.cfg.Tasks[name] }

func jittered(n int) time.Duration {
	d := time.Duration(1<<n) * time.Second // 2 s, 4 s, 8 s
	return d + rand.N(d)
}

// maxRetryAfter is the longest Retry-After the client waits for; a longer one fails the call at once.
const maxRetryAfter = 5 * time.Minute

// ---- requests and answers ----

type Request struct {
	Model          string          `json:"model"`
	Models         []string        `json:"models,omitempty"` // the fallbacks, tried in order by the gateway
	Messages       []Message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens"` // reasoning counts towards it
	Reasoning      *Reasoning      `json:"reasoning,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	InputTokens int    `json:"-"` // the caller's upper estimate of the input, for the worst case
	Effort      string `json:"-"` // this call's reasoning, "" = the task's
}

type Reasoning struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

type ResponseFormat struct {
	Type       string      `json:"type"` // "json_schema"
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

type JSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// JSONAnswer constrains the answer to schema, where the model supports it; every object in a strict schema lists all
// its properties as required and sets "additionalProperties": false.
func JSONAnswer(name string, schema json.RawMessage) *ResponseFormat {
	return &ResponseFormat{Type: "json_schema", JSONSchema: &JSONSchema{Name: name, Strict: true, Schema: schema}}
}

type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content []Part `json:"content"`
	// CacheControl marks the end of a prefix the provider caches (Anthropic's explicit caching, passed through by the
	// gateway): a later call that starts with the same bytes reads it at a tenth of the input price.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"` // ephemeral: five minutes from its last read
}

// Ephemeral is the one cache Anthropic offers through the gateway's Chat Completions API.
var Ephemeral = &CacheControl{Type: "ephemeral"}

// MarshalJSON sends a message of one text part as a plain string, the form every provider takes.
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Content) == 1 && m.Content[0].Type == "text" {
		return json.Marshal(struct {
			Role         string        `json:"role"`
			Content      string        `json:"content"`
			CacheControl *CacheControl `json:"cache_control,omitempty"`
		}{m.Role, m.Content[0].Text, m.CacheControl})
	}
	type plain Message
	return json.Marshal(plain(m))
}

// UnmarshalJSON reads both forms of a message's content.
func (m *Message) UnmarshalJSON(b []byte) error {
	var raw struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		CacheControl *CacheControl   `json:"cache_control"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Role, m.Content, m.CacheControl = raw.Role, nil, raw.CacheControl
	var text string
	if json.Unmarshal(raw.Content, &text) == nil {
		m.Content = []Part{Text(text)}
		return nil
	}
	return json.Unmarshal(raw.Content, &m.Content)
}

type Part struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

func Text(s string) Part { return Part{Type: "text", Text: s} }

func JPEG(b []byte) Part {
	return Part{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b)}}
}

func System(s string) Message { return Message{Role: "system", Content: []Part{Text(s)}} }
func User(parts ...Part) Message {
	return Message{Role: "user", Content: parts}
}
func Assistant(s string) Message { return Message{Role: "assistant", Content: []Part{Text(s)}} }

// EstimateTokens is an upper estimate of the tokens in s: no tokenizer the gateway's models use packs fewer than three
// bytes of English, digits or punctuation into a token, and a CJK character is three bytes.
func EstimateTokens(s string) int { return len(s)/3 + 1 }

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"` // reasoning included
}

// Result is one call's outcome; on an error it still carries what the failed attempts may have cost.
type Result struct {
	Text        string
	Model       string // the model that answered: the task's, or a fallback
	Finish      string // stop | length | ...
	Usage       Usage
	CostUSD     float64 // the answer's cost (the gateway's own figure, else usage at the catalogue's price) plus MaybeBilled at the worst case
	MaybeBilled int     // attempts sent in full whose answer was lost
	Attempts    int
	Took        time.Duration
}

// completion is the gateway's answer. Its documented shape has no cost; the figure it bills (usage.cost, which
// prices cached input at the cache's rate) is used when present, else one in provider_metadata.gateway.cost (the AI
// SDK's name for it).
type completion struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          *string         `json:"content"`
			ProviderMetadata json.RawMessage `json:"provider_metadata"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		Usage
		Cost any `json:"cost"`
	} `json:"usage"`
	ProviderMetadata json.RawMessage `json:"provider_metadata"`
}

// cost is the gateway's own figure for the answer: a number, or a decimal string with or without "$". Any other shape
// counts as no figure, never as an unreadable answer (which would be asked again and paid twice).
func (c completion) cost() (float64, bool) {
	figures := []any{c.Usage.Cost}
	metas := []json.RawMessage{c.ProviderMetadata}
	if len(c.Choices) > 0 {
		metas = append([]json.RawMessage{c.Choices[0].Message.ProviderMetadata}, metas...)
	}
	for _, raw := range metas {
		var m struct {
			Gateway struct {
				Cost any `json:"cost"`
			} `json:"gateway"`
		}
		if json.Unmarshal(raw, &m) == nil {
			figures = append(figures, m.Gateway.Cost)
		}
	}
	for _, figure := range figures {
		var f float64
		var err error
		switch v := figure.(type) {
		case float64:
			f = v
		case string:
			f, err = strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(v), "$"), 64)
		default:
			continue
		}
		if err == nil && f >= 0 && !math.IsInf(f, 0) { // NaN fails f >= 0
			return f, true
		}
	}
	return 0, false
}

// ---- errors ----

var (
	// ErrBilling marks the gateway refusing to run calls until its account is fixed: no credits, or no card on file.
	ErrBilling = errors.New("gateway: billing refuses calls")
	// ErrParked marks a session that makes no more calls this run.
	ErrParked = errors.New("llm: parked")
	// ErrUnknownModel marks a model id the gateway's catalogue does not list: a typo, or a model withdrawn.
	ErrUnknownModel = errors.New("model not in the gateway's catalogue")
)

// APIError is a non-2xx answer; Type is the gateway's error type ("invalid_request_error", "rate_limit_exceeded",
// "customer_verification_required", ...).
type APIError struct {
	Status        int
	Type, Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway: HTTP %d %s: %s", e.Status, e.Type, e.Message)
}

// Is makes errors.Is(err, ErrBilling) true for a 402, and for a 403 that names a missing card, the free tier or
// credits (2026-10-06, a model outside the free tier: "no_providers_available", "Free tier users do not have access
// to this model. Upgrade to paid credits ...").
func (e *APIError) Is(target error) bool {
	if target != ErrBilling {
		return false
	}
	msg := strings.ToLower(e.Message)
	return e.Status == http.StatusPaymentRequired || e.Status == http.StatusForbidden && (e.Type == "customer_verification_required" ||
		strings.Contains(msg, "credit card") || strings.Contains(msg, "free tier") || strings.Contains(msg, "credits"))
}

// ---- sessions ----

// Session is one stage run's use of one task. It parks after ParkAfter failed calls in a row, or at once on a billing
// refusal; Parked says why, for the run's one alert.
type Session struct {
	c    *Client
	name string
	task Task

	mu     sync.Mutex
	fails  int
	parked error
}

func (c *Client) Session(task string) *Session {
	return &Session{c: c, name: task, task: c.cfg.Tasks[task]}
}

// Model is the task's first model.
func (s *Session) Model() string { return s.task.Model }

// Name is the task's.
func (s *Session) Name() string { return s.name }

// Parked says why the session makes no more calls; nil while it may. A billing refusal another session met parks it
// too, until the gateway is due to be tried again.
func (s *Session) Parked() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parked == nil {
		if b := s.c.billing(context.Background()); b.held(s.c.now()) {
			s.parked = parkedForBilling(b.Since, b.refusal())
		}
	}
	return s.parked
}

func (s *Session) models() []string { return append([]string{s.task.Model}, s.task.Fallbacks...) }

// WorstCase is the most one attempt of req may cost: its InputTokens at the dearest input price among the task's
// models, and all of max_tokens at the dearest output price.
func (s *Session) WorstCase(ctx context.Context, req Request) (float64, error) {
	p, err := s.Price(ctx)
	return p.Cost(req.InputTokens, req.MaxTokens), err
}

// Price is the dearest input and the dearest output price among the task's models.
func (s *Session) Price(ctx context.Context) (Price, error) {
	p, _, err := s.price(ctx)
	return p, err
}

// price is the dearest price among the task's models, and the first model's catalogue entry.
func (s *Session) price(ctx context.Context) (Price, ModelInfo, error) {
	var dearest Price
	var first ModelInfo
	for i, id := range s.models() {
		m, err := s.c.Model(ctx, id)
		if err != nil {
			return Price{}, ModelInfo{}, fmt.Errorf("llm task %s: %w", s.name, err)
		}
		if i == 0 {
			first = m
		}
		dearest = Price{In: max(dearest.In, m.In), Out: max(dearest.Out, m.Out)}
	}
	return dearest, first, nil
}

// Chat sends req as the task's models with the task's reasoning. Result.CostUSD is what the call costs, its lost
// attempts at their worst case included, whether or not it fails.
func (s *Session) Chat(ctx context.Context, req Request) (Result, error) {
	if err := s.Parked(); err != nil {
		return Result{}, err
	}
	p, first, err := s.price(ctx)
	if err != nil {
		s.settle(ctx, err)
		return Result{}, err
	}
	release, err := s.c.admit(ctx)
	if err != nil {
		s.settle(ctx, err)
		return Result{}, err
	}
	defer release()
	models := s.models()
	req.Model, req.Models = models[0], models[1:]
	effort := req.Effort
	if effort == "" {
		effort = s.task.Reasoning
	}
	req.Reasoning = first.reasoning(effort)
	res, comp, err := s.c.do(ctx, req)
	res.CostUSD = float64(res.MaybeBilled) * p.Cost(req.InputTokens, req.MaxTokens)
	if err == nil {
		res.CostUSD += s.answerCost(ctx, comp, res, p)
	}
	s.settle(ctx, err)
	return res, err
}

// answerCost is the gateway's figure, else the usage at the answering model's catalogue price (the dearest of the
// task's when the catalogue does not list the id it answered under).
func (s *Session) answerCost(ctx context.Context, comp completion, res Result, dearest Price) float64 {
	if usd, ok := comp.cost(); ok {
		return usd
	}
	p := dearest
	if m, err := s.c.Model(ctx, res.Model); err == nil {
		p = m.Price
	}
	return float64(res.Usage.PromptTokens)*p.In + float64(res.Usage.CompletionTokens)*p.Out
}

// settle counts the call towards parking: a success resets the count, a billing refusal parks at once (and is kept in
// the store), a cancelled run counts nothing, and anything else is a failure.
func (s *Session) settle(ctx context.Context, err error) {
	var since time.Time
	switch {
	case err == nil:
		s.c.unblock(ctx)
	case errors.Is(err, ErrBilling):
		since = s.c.block(ctx, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		s.fails = 0
	case s.parked != nil || ctx.Err() != nil:
	case errors.Is(err, ErrBilling):
		s.parked = parkedForBilling(since, err)
	default:
		if s.fails++; s.fails >= s.c.cfg.ParkAfter {
			s.parked = fmt.Errorf("%w after %d failed calls in a row; the last: %w", ErrParked, s.fails, err)
		}
	}
}

func parkedForBilling(since time.Time, err error) error {
	return fmt.Errorf("%w: the gateway has refused calls since %s: %w", ErrParked, since.Format(time.RFC3339), err)
}

// billingRecheck is how often a gateway that refuses for billing is tried again: by one call, which the others wait
// for. A refused call costs nothing; the hold keeps every run of every stage from asking (tonight runs every 30 min).
const billingRecheck = time.Hour

// blocked is a billing refusal, kept in the store until a call is answered again: since when, when a call was last
// refused (the next try is due billingRecheck later), and whether a stage has raised its one alert.
type blocked struct {
	Since   time.Time `json:"since,omitzero"`
	Tried   time.Time `json:"tried,omitzero"`
	Told    bool      `json:"told,omitempty"`
	Message string    `json:"message,omitempty"`
}

// held says the gateway refuses for billing and is not yet due to be tried again.
func (b blocked) held(now time.Time) bool {
	return !b.Since.IsZero() && now.Before(b.Tried.Add(billingRecheck))
}

// refusal is the error of a call the hold kept from being sent.
func (b blocked) refusal() error {
	return fmt.Errorf("%w: not sent, the gateway is tried again from %s; its last answer: %s", ErrBilling,
		b.Tried.Add(billingRecheck).Format(time.RFC3339), b.Message)
}

const storeStage = "llm"

// loadBlocked needs blockMu held.
func (c *Client) loadBlocked(ctx context.Context) *blocked {
	if c.blocked == nil {
		c.blocked = &blocked{}
		if _, err := c.store.GetJSON(ctx, storeStage, "blocked", c.blocked); err != nil {
			c.blocked = &blocked{}
		}
	}
	return c.blocked
}

func (c *Client) billing(ctx context.Context) blocked {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	return *c.loadBlocked(ctx)
}

// admit lets a call go. While the gateway refuses for billing, none goes until the next try is due, and then one at
// a time: the first is the try, and the rest wait for its answer, going on if it was answered and held if it was
// refused again. release ends the call's turn, after its outcome is recorded.
func (c *Client) admit(ctx context.Context) (release func(), err error) {
	switch b := c.billing(ctx); {
	case b.Since.IsZero():
		return func() {}, nil
	case b.held(c.now()):
		return nil, b.refusal()
	}
	select {
	case c.probe <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch b := c.billing(ctx); {
	case b.Since.IsZero(): // the try it waited for was answered
		<-c.probe
		return func() {}, nil
	case b.held(c.now()): // and refused again
		<-c.probe
		return nil, b.refusal()
	}
	return func() { <-c.probe }, nil
}

// block records a billing refusal and returns since when the gateway has refused calls. Only a refusal the gateway
// sent moves the next try; a call the hold kept back changes nothing.
func (c *Client) block(ctx context.Context, err error) time.Time {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	b := c.loadBlocked(ctx)
	if api := (*APIError)(nil); errors.As(err, &api) {
		if b.Since.IsZero() {
			b.Since = c.now()
		}
		b.Tried, b.Message = c.now(), err.Error()
		c.store.PutJSON(context.WithoutCancel(ctx), storeStage, "blocked", b)
	}
	return b.Since
}

func (c *Client) unblock(ctx context.Context) {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	if b := c.loadBlocked(ctx); !b.Since.IsZero() {
		*b = blocked{}
		c.store.PutJSON(context.WithoutCancel(ctx), storeStage, "blocked", b)
	}
}

// TellBilling is true once per billing refusal, for the stage that raises the one alert about it, and false after,
// until a call is answered and the gateway refuses anew. The mark is kept in the store with the refusal.
func (c *Client) TellBilling(ctx context.Context) bool {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	b := c.loadBlocked(ctx)
	if b.Since.IsZero() || b.Told {
		return false
	}
	b.Told = true
	c.store.PutJSON(context.WithoutCancel(ctx), storeStage, "blocked", b)
	return true
}

// ---- transport ----

// do sends req, retrying what the failure policy retries, and returns the answer.
func (c *Client) do(ctx context.Context, req Request) (Result, completion, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return Result{}, completion{}, err
	}
	start := time.Now()
	var res Result
	for {
		res.Attempts++
		comp, a := c.attempt(ctx, payload)
		if a.maybeBilled {
			res.MaybeBilled++
		}
		if a.err == nil {
			res.Model, res.Usage, res.Took = comp.Model, comp.Usage.Usage, time.Since(start)
			if res.Model == "" {
				res.Model = req.Model
			}
			if len(comp.Choices) > 0 {
				ch := comp.Choices[0]
				res.Finish = ch.FinishReason
				if ch.Message.Content != nil {
					res.Text = *ch.Message.Content
				}
			}
			return res, comp, nil
		}
		wait := a.retryAfter
		if wait == 0 {
			wait = c.Backoff(res.Attempts)
		}
		if !a.retry || res.Attempts >= c.cfg.Attempts || wait > maxRetryAfter || ctx.Err() != nil {
			res.Took = time.Since(start)
			return res, completion{}, fmt.Errorf("after %d attempt(s): %w", res.Attempts, a.err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			res.Took = time.Since(start)
			return res, completion{}, ctx.Err()
		case <-t.C:
		}
	}
}

type attemptOutcome struct {
	err         error
	retry       bool
	maybeBilled bool
	retryAfter  time.Duration
}

// attempt is one POST /chat/completions inside the bound on calls in flight and the per-attempt timeout.
func (c *Client) attempt(ctx context.Context, payload []byte) (completion, attemptOutcome) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return completion{}, attemptOutcome{err: ctx.Err()}
	}
	defer func() { <-c.sem }()
	key, err := c.Key()
	if err != nil {
		return completion{}, attemptOutcome{err: fmt.Errorf("gateway key: %w", err)}
	}
	actx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.cfg.API+"/chat/completions", nil)
	if err != nil {
		return completion{}, attemptOutcome{err: err}
	}
	body := &sentBody{r: bytes.NewReader(payload)}
	req.Body, req.ContentLength = body, int64(len(payload))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return completion{}, attemptOutcome{err: fmt.Errorf("gateway: %w", stripURL(err)), retry: true, maybeBilled: body.sent.Load()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode/100 == 2:
		var comp completion
		if err == nil {
			err = json.Unmarshal(raw, &comp)
		}
		if err != nil { // the gateway answered, so it may have billed: as good as a lost answer
			return completion{}, attemptOutcome{err: fmt.Errorf("gateway answer unreadable: %w", err), retry: true, maybeBilled: true}
		}
		return comp, attemptOutcome{}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return completion{}, attemptOutcome{err: apiError(resp.StatusCode, raw), retry: true, retryAfter: retryAfter(resp.Header.Get("Retry-After"), c.now())}
	default:
		return completion{}, attemptOutcome{err: apiError(resp.StatusCode, raw)}
	}
}

// stripURL keeps the URL out of a transport error's message.
func stripURL(err error) error {
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// retryAfter reads a Retry-After of seconds or an HTTP date; 0 = none.
func retryAfter(v string, now time.Time) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func apiError(status int, raw []byte) error {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &body)
	e := &APIError{Status: status, Type: body.Error.Type, Message: body.Error.Message}
	if code, ok := body.Error.Code.(string); ok && e.Type == "" {
		e.Type = code
	}
	if e.Type == "" && e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}

// sentBody records whether the transport has read the whole payload: a request cut off before that never reached the
// gateway complete, so it cannot have been billed.
type sentBody struct {
	r    *bytes.Reader
	sent atomic.Bool // the transport reads the body on its own goroutine
}

func (b *sentBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if b.r.Len() == 0 {
		b.sent.Store(true)
	}
	return n, err
}

func (b *sentBody) Close() error { return nil }
