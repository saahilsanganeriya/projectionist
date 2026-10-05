package tonight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ---- Anthropic Messages API: model aliases, the cost cap, one pick call (+ one retry) ----

// errOverBudget refuses a call whose worst-case cost would take the day's spend past the cap.
var errOverBudget = errors.New("tonight: the call would exceed the daily AI budget")

// price is USD per million tokens. A 5-minute cache write costs 1.25 × In.
type price struct {
	In        float64 `toml:"input"`
	Out       float64 `toml:"output"`
	CacheRead float64 `toml:"cache_read"`
}

// familyPrices are Anthropic's first-party rates for the current generation of each family (2026-09). An alias
// resolving to a model whose price differs needs the price override in the stage's config.
var familyPrices = map[string]price{
	"opus":   {In: 4, Out: 20, CacheRead: 0.20},
	"sonnet": {In: 2, Out: 10, CacheRead: 0.20},
	"haiku":  {In: 1, Out: 5, CacheRead: 0.10},
}

func priceFor(model string, override price) (price, error) {
	if override.In > 0 {
		return override, nil
	}
	for fam, p := range familyPrices {
		if strings.Contains(model, fam) {
			return p, nil
		}
	}
	return price{}, fmt.Errorf("tonight: no price for model %q; set [stages.tonight.price]", model)
}

type usage struct{ Input, CacheWrite, CacheRead, Output int64 }

func (u usage) add(v usage) usage {
	return usage{u.Input + v.Input, u.CacheWrite + v.CacheWrite, u.CacheRead + v.CacheRead, u.Output + v.Output}
}

func (p price) cost(u usage) float64 {
	return (float64(u.Input)*p.In + float64(u.CacheWrite)*1.25*p.In + float64(u.CacheRead)*p.CacheRead +
		float64(u.Output)*p.Out) / 1e6
}

// worstCase bounds a request before it is sent: cached tokens at the read price, every other input token at
// the cache-write price, and all of max_tokens as output.
func (p price) worstCase(inputTokens, cachedTokens, maxTokens int64) float64 {
	return p.cost(usage{CacheWrite: inputTokens - cachedTokens, CacheRead: cachedTokens, Output: maxTokens})
}

// newestModel resolves a family alias ("sonnet") to the most recently released model id containing it.
func newestModel(models []anthropic.ModelInfo, family string) string {
	var best anthropic.ModelInfo
	for _, m := range models {
		if strings.Contains(m.ID, family) && m.CreatedAt.After(best.CreatedAt) {
			best = m
		}
	}
	return best.ID
}

type ai struct {
	client anthropic.Client
}

// newAI reads the key once per run from its file; it never reaches a log or an error.
func newAI(base, keyFile string, timeout time.Duration) (*ai, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("tonight: AI key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, errors.New("tonight: AI key file is empty")
	}
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(), // no ANTHROPIC_* env var may redirect or re-key the call
		option.WithHTTPClient(&http.Client{Timeout: timeout}),
		option.WithAPIKey(key),
		option.WithMaxRetries(2), // 408/409/429/5xx and connection errors, with the SDK's jittered backoff
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	return &ai{client: anthropic.NewClient(opts...)}, nil
}

// resolve maps "<family>:latest" to the newest model of that family via GET /v1/models; other names are ids.
func (a *ai) resolve(ctx context.Context, name string) (string, error) {
	family, ok := strings.CutSuffix(name, ":latest")
	if !ok {
		return name, nil
	}
	var models []anthropic.ModelInfo
	pager := a.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	for pager.Next() {
		models = append(models, pager.Current())
	}
	if err := pager.Err(); err != nil {
		return "", fmt.Errorf("tonight: list models: %w", err)
	}
	id := newestModel(models, family)
	if id == "" {
		return "", fmt.Errorf("tonight: no model matches %q", name)
	}
	return id, nil
}

// request is one pick conversation. The taste block is the stable prefix and carries the cache breakpoint.
type request struct {
	Model     string
	Effort    string
	MaxTokens int64
	Taste     string
	Messages  []anthropic.MessageParam
}

func (r request) system() []anthropic.TextBlockParam {
	return []anthropic.TextBlockParam{
		{Text: instructions},
		{Text: r.Taste, CacheControl: anthropic.NewCacheControlEphemeralParam()},
	}
}

func (r request) outputConfig() anthropic.OutputConfigParam {
	return anthropic.OutputConfigParam{
		Effort: anthropic.OutputConfigEffort(r.Effort),
		Format: anthropic.JSONOutputFormatParam{Schema: picksSchema},
	}
}

func (a *ai) countTokens(ctx context.Context, r request) (int64, error) {
	n, err := a.client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model: anthropic.Model(r.Model), Messages: r.Messages, OutputConfig: r.outputConfig(),
		System: anthropic.MessageCountTokensParamsSystemUnion{OfTextBlockArray: r.system()},
	})
	if err != nil {
		return 0, fmt.Errorf("tonight: count tokens: %w", err)
	}
	return n.InputTokens, nil
}

// reply is the model's answer: its text, the message to replay on a retry, and what it cost in tokens.
type reply struct {
	Text  string
	Param anthropic.MessageParam
	Usage usage
	Stop  string
}

func (a *ai) send(ctx context.Context, r request) (reply, error) {
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model(r.Model), MaxTokens: r.MaxTokens, System: r.system(), Messages: r.Messages,
		OutputConfig: r.outputConfig(),
	})
	if err != nil {
		return reply{}, fmt.Errorf("tonight: messages: %w", err)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	u := msg.Usage
	return reply{Text: text.String(), Param: msg.ToParam(), Stop: string(msg.StopReason),
		Usage: usage{Input: u.InputTokens, CacheWrite: u.CacheCreationInputTokens, CacheRead: u.CacheReadInputTokens,
			Output: u.OutputTokens}}, nil
}

// ---- prompt ----

const instructions = `You choose what one person watches tonight, from films he owns and has not seen.

He is a cinephile who logs and rates every film on Letterboxd right after watching it. His ratings and reviews
follow; they are the only evidence of his taste.

Rules:
- Pick only films from the UNSEEN list in the user message, and give each pick's id exactly as listed.
- Make the picks differ in mood (for example light, tense, moving, strange, slow) and in length: include a
  short one and a long one when the list has them. Avoid several picks from one genre, decade or country.
- Each reason is one line of at most 160 characters. It names a specific film he rated 4 stars or more, or a
  director whose films he rated highly, and says what tonight's film shares with it. No spoilers.
- Answer with JSON only, in the given schema.`

var picksSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"picks": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     map[string]any{"type": "integer"},
					"title":  map[string]any{"type": "string"},
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"id", "title", "reason"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"picks"},
	"additionalProperties": false,
}

type answer struct {
	Picks []struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Reason string `json:"reason"`
	} `json:"picks"`
}

// validate keeps the answer's picks that name an unseen film once, and lists every violation for the retry.
func validate(text string, unseen map[int]candidate, want int) (picks []pick, violations []string) {
	var ans answer
	if err := json.Unmarshal([]byte(text), &ans); err != nil {
		return nil, []string{"the answer is not JSON in the schema: " + err.Error()}
	}
	seen := map[int]bool{}
	for i, p := range ans.Picks {
		c, ok := unseen[p.ID]
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf("pick %d: id %d (%q) is not in the UNSEEN list", i+1, p.ID, p.Title))
		case seen[p.ID]:
			violations = append(violations, fmt.Sprintf("pick %d: id %d is picked twice", i+1, p.ID))
		case strings.TrimSpace(p.Reason) == "":
			violations = append(violations, fmt.Sprintf("pick %d: id %d has no reason", i+1, p.ID))
		default:
			seen[p.ID] = true
			picks = append(picks, pick{TMDB: p.ID, Title: c.Title, Year: c.Year, Reason: oneLine(p.Reason, 200),
				PosterPath: c.PosterPath})
		}
	}
	if len(picks) > want {
		picks = picks[:want]
	}
	if len(picks) < want {
		violations = append(violations, fmt.Sprintf("%d valid picks; %d are needed", len(picks), want))
	}
	return picks, violations
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
