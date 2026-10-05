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
	"projectionist/internal/stage"
)

// ---- the pick call: the dated model policy, the daily cap, one answer (+ one retry) ----

// errOverBudget refuses a call when it and its one retry, at their worst, would take the day's spend past the cap.
var errOverBudget = errors.New("tonight: the call would exceed the daily AI budget")

// retryExtra bounds the violations message a retry appends, in tokens.
const retryExtra = 300

// bound is the most a request can cost once sent. Its one cache breakpoint ends the system prompt, so up to prefix
// tokens may be billed as a cache write: a retry meant to read them pays the write again when the cache misses (it
// lives 5 minutes, and a retry at another effort never matches it). Later tokens are plain input; all of max_tokens
// is output. Counts get 2% slack, because count_tokens is an estimate that "might differ by a small amount".
func bound(p llm.Price, total, prefix, maxTokens int) float64 {
	slack := func(n int) int { return n + n/50 }
	return p.Cost(llm.Usage{CacheCreationInputTokens: slack(prefix), InputTokens: slack(total - prefix), OutputTokens: maxTokens}, false)
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

// resolved is the model a policy alias named on the last lookup.
type resolved struct {
	Alias string `json:"alias,omitempty"`
	llm.Model
	At time.Time `json:"at,omitzero"`
}

// resolveModel is the dated policy's choice now (env.LLM). A "family:latest" alias resolves once a day, and at once
// when the policy moves to another alias; the last answer stands while the Models API is down.
func resolveModel(ctx context.Context, env *stage.Env, a *llm.Client, r *resolved) (llm.Model, error) {
	alias, err := env.LLM.Model(env.Now())
	if err != nil {
		return llm.Model{}, err
	}
	family, ok := strings.CutSuffix(alias, ":latest")
	if !ok {
		return llm.Model{ID: alias}, nil
	}
	if r.Alias == alias && env.Now().Sub(r.At) < 24*time.Hour {
		return r.Model, nil
	}
	m, err := a.Latest(ctx, family)
	if err != nil {
		if r.Alias != alias {
			return llm.Model{}, err
		}
		env.Log.Warn("model list failed; keeping the last answer", "event", "tonight_model_error", "model", r.ID, "err", err)
		return r.Model, nil
	}
	*r = resolved{Alias: alias, Model: m, At: env.Now()}
	env.Log.Info("model resolved", "event", "tonight_model", "alias", alias, "model", m.ID, "low_effort", m.LowEffort)
	return m, nil
}

// pickRequest is one pick conversation. The taste block is the stable prefix and carries the cache breakpoint.
// Effort goes only to a model that accepts it (Haiku 4.5 answers 400 to any effort).
func pickRequest(m llm.Model, opt Options, taste string, msgs []llm.Message) llm.CachedRequest {
	oc := &llm.OutputConfig{Format: &llm.Format{Type: "json_schema", Schema: picksSchema}}
	if m.LowEffort {
		oc.Effort = opt.Effort
	}
	return llm.CachedRequest{
		Model: m.ID, MaxTokens: opt.MaxTokens, Messages: msgs, OutputConfig: oc,
		System: []llm.SystemBlock{
			{Type: "text", Text: instructions},
			{Type: "text", Text: taste, CacheControl: &llm.CacheControl{Type: "ephemeral"}},
		},
	}
}

func userMessage(text string) llm.Message {
	return llm.Message{Role: "user", Content: []llm.Block{llm.Text(text)}}
}

// ---- prompt ----

// The mood and liked fields hold the bookkeeping the model otherwise wrote into the reason (2026-10-05: "...of
// Moonlight.Quiet and slow.Hmm."); the reason is then only the sentence he reads.
const instructions = `You choose what one person watches tonight, from films he owns and has not seen.

He is a cinephile who logs and rates every film on Letterboxd right after watching it. His ratings and reviews
follow; they are the only evidence of his taste.

Rules:
- Pick only films from the UNSEEN list in the user message, and give each pick's id exactly as listed.
- Make the picks differ in mood (for example light, tense, moving, strange, slow) and in length: include a
  short one and a long one when the list has them. Avoid several picks from one genre, decade or country.
- For each pick, "mood" is its mood in two or three words, and "liked" is the film he rated 4 stars or more, or
  the director whose films he rated highly, that the pick draws on. Use these two fields for that bookkeeping.
- "reason" is one finished sentence of at most 160 characters, for him to read: it names the "liked" film or
  director and says what tonight's film shares with it. No spoilers, no notes, no checks.
- Answer with JSON only, in the given schema.`

var picksSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["picks"],
"properties":{"picks":{"type":"array","items":{"type":"object","additionalProperties":false,
"required":["id","title","mood","liked","reason"],
"properties":{"id":{"type":"integer"},"title":{"type":"string"},"mood":{"type":"string"},"liked":{"type":"string"},
"reason":{"type":"string"}}}}}}`)

type answer struct {
	Picks []struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Mood   string `json:"mood"`
		Liked  string `json:"liked"`
		Reason string `json:"reason"`
	} `json:"picks"`
}

// gluedNote is a note glued onto a reason's last sentence with no space ("...of Moonlight.Quiet and slow.Hmm.",
// "...package.12"). Two lowercase letters before the stop keep initials ("L.A.", "E.T.") intact.
var gluedNote = regexp.MustCompile(`(\p{Ll}{2}[.!?])(?:\p{Lu}|\d)`)

// validate keeps the answer's picks that name an unseen film once, and lists every violation for the retry. glued
// holds the reasons that carried a glued note, as the model wrote them, so the log shows whether the cause is gone.
func validate(text string, unseen map[int]candidate, want int) (picks []pick, violations, glued []string) {
	var ans answer
	if err := json.Unmarshal([]byte(text), &ans); err != nil {
		return nil, []string{"the answer is not JSON in the schema: " + err.Error()}, nil
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
				glued = append(glued, reason)
				reason = reason[:m[3]]
			}
			picks = append(picks, pick{TMDB: p.ID, Title: c.Title, Year: c.Year, Reason: oneLine(reason, 200),
				Mood: p.Mood, Liked: p.Liked, PosterPath: c.PosterPath})
		}
	}
	if len(picks) > want {
		picks = picks[:want]
	}
	if len(picks) < want {
		violations = append(violations, fmt.Sprintf("%d valid picks; %d are needed", len(picks), want))
	}
	return picks, violations, glued
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
