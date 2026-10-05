package tonight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"projectionist/internal/llm"
)

// ---- the pick call: model aliases, the daily cap, one answer (+ one retry) ----

// errOverBudget refuses a call when it and its one retry, at their worst, would take the day's spend past the cap.
var errOverBudget = errors.New("tonight: the call would exceed the daily AI budget")

// retryExtra bounds the violations message a retry appends, in tokens.
const retryExtra = 300

// bound is a request's cost before it is sent. Its one cache breakpoint ends the system prompt, so the prefix is
// billed as a cache write (or a read, when read is set) and every later token as plain input; all of max_tokens
// is counted as output.
func bound(p llm.Price, total, prefix, maxTokens int, read bool) float64 {
	u := llm.Usage{InputTokens: total - prefix, OutputTokens: maxTokens}
	if read {
		u.CacheReadInputTokens = prefix
	} else {
		u.CacheCreationInputTokens = prefix
	}
	return p.Cost(u, false)
}

// newAI reads the key file on every request; the key never reaches a log or an error.
func newAI(base, keyFile string, timeout time.Duration) *llm.Client {
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &llm.Client{API: base, HTTP: &http.Client{Timeout: timeout}, Key: func() (string, error) {
		raw, err := os.ReadFile(keyFile)
		return string(raw), err
	}}
}

// resolve maps "<family>:latest" to the newest model of that family; other names are model ids.
func resolve(ctx context.Context, a *llm.Client, name string) (string, error) {
	family, ok := strings.CutSuffix(name, ":latest")
	if !ok {
		return name, nil
	}
	return a.Latest(ctx, family)
}

// pickRequest is one pick conversation. The taste block is the stable prefix and carries the cache breakpoint.
func pickRequest(model string, opt Options, taste string, msgs []llm.Message) llm.CachedRequest {
	return llm.CachedRequest{
		Model: model, MaxTokens: opt.MaxTokens, Messages: msgs,
		System: []llm.SystemBlock{
			{Type: "text", Text: instructions},
			{Type: "text", Text: taste, CacheControl: &llm.CacheControl{Type: "ephemeral"}},
		},
		OutputConfig: &llm.OutputConfig{Effort: opt.Effort, Format: &llm.Format{Type: "json_schema", Schema: picksSchema}},
	}
}

func userMessage(text string) llm.Message {
	return llm.Message{Role: "user", Content: []llm.Block{llm.Text(text)}}
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
- The reason field holds only that finished sentence: no notes, checks or corrections.
- Answer with JSON only, in the given schema.`

var picksSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["picks"],
"properties":{"picks":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["id","title","reason"],
"properties":{"id":{"type":"integer"},"title":{"type":"string"},"reason":{"type":"string"}}}}}}`)

type answer struct {
	Picks []struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Reason string `json:"reason"`
	} `json:"picks"`
}

// gluedNote is a note the model sometimes appends to a reason's last sentence with no space ("...of Moonlight.Quiet
// and slow.Hmm.", "...package.12"). Two lowercase letters before the stop keep initials ("L.A.", "E.T.") intact.
var gluedNote = regexp.MustCompile(`(\p{Ll}{2}[.!?])(?:\p{Lu}|\d)`)

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
			reason := p.Reason
			if m := gluedNote.FindStringSubmatchIndex(reason); m != nil {
				reason = reason[:m[3]]
			}
			picks = append(picks, pick{TMDB: p.ID, Title: c.Title, Year: c.Year, Reason: oneLine(reason, 200),
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
