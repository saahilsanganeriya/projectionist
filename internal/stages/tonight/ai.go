package tonight

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"projectionist/internal/llm"
)

// ---- the pick call: the tonight task's model, the daily cap, one answer (+ one retry) ----

// errOverBudget refuses a call when it and its one retry, at their worst, would take the day's spend past the cap.
var errOverBudget = errors.New("tonight: the call would exceed the daily AI budget")

// retryExtra is what sizing allows for the violations a retry appends, in tokens. A longer list (many invalid picks
// with long titles) is caught by the cap check made on the retry before it is sent.
const retryExtra = 300

// pickRequest is one pick conversation, constrained to the picks schema, with an upper estimate of its input.
func pickRequest(opt Options, taste string, msgs []llm.Message) llm.Request {
	return estimated(llm.Request{MaxTokens: opt.MaxTokens, ResponseFormat: llm.JSONAnswer("picks", picksSchema),
		Messages: append([]llm.Message{llm.System(instructions + "\n\n" + taste)}, msgs...)})
}

// estimated sets req's input estimate from its messages, as they stand.
func estimated(req llm.Request) llm.Request {
	req.InputTokens = 0
	for _, m := range req.Messages {
		req.InputTokens += 4 // the role and the framing
		for _, p := range m.Content {
			req.InputTokens += llm.EstimateTokens(p.Text)
		}
	}
	return req
}

func userMessage(text string) llm.Message { return llm.User(llm.Text(text)) }

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
- For each pick, "mood" is its mood in two or three words, and "liked" is the title of the film he rated 4 stars
  or more that the pick draws on, written as in his ratings. Use these two fields for that bookkeeping.
- "reason" is one finished sentence of at most 160 characters, for him to read: it names the "liked" film or its
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

// cleanReason cuts what the model glued onto a reason's last sentence: a note, or a stray JSON bracket (every reason
// of one Sonnet 5.5 answer on 2026-10-05 ended ".}"). cut reports whether anything was cut.
func cleanReason(s string) (reason string, cut bool) {
	reason = strings.TrimSpace(s)
	if m := gluedNote.FindStringSubmatchIndex(reason); m != nil {
		reason = reason[:m[3]]
	}
	reason = strings.TrimRight(reason, "}] ")
	return reason, reason != strings.TrimSpace(s)
}

// lovedFilms holds the films he rated 4★ or more, keyed by title alone and by title and year (filmKey).
func lovedFilms(hist []rated) map[string]bool {
	loved := map[string]bool{}
	for _, h := range hist {
		if h.Rating >= 4 {
			loved[filmKey(h.Name, 0)], loved[filmKey(h.Name, h.Year)] = true, true
		}
	}
	return loved
}

var titleYear = regexp.MustCompile(`^(.+?) \((\d{4})\)$`)

// lovedKey is a "liked" value's key in lovedFilms: "Rope (1948)" by title and year, "Rope" by title alone.
func lovedKey(liked string) string {
	if m := titleYear.FindStringSubmatch(strings.TrimSpace(liked)); m != nil {
		year, _ := strconv.Atoi(m[2])
		return filmKey(m[1], year)
	}
	return filmKey(liked, 0)
}

// validate keeps the answer's picks that name an unseen film once and draw on a film he rated 4★ or more, and lists
// every violation for the retry. glued holds the reasons something was cut from (cleanReason), as the model wrote
// them, so the log shows whether the cause is gone.
func validate(text string, unseen map[int]candidate, loved map[string]bool, want int) (picks []pick, violations, glued []string) {
	var ans answer
	if err := json.Unmarshal([]byte(text), &ans); err != nil {
		return nil, []string{"the answer is not JSON in the schema: " + err.Error()}, nil
	}
	seen := map[int]bool{}
	for i, p := range ans.Picks {
		c, ok := unseen[p.ID]
		reason, cut := cleanReason(p.Reason)
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf("pick %d: id %d (%q) is not in the UNSEEN list", i+1, p.ID, p.Title))
		case seen[p.ID]:
			violations = append(violations, fmt.Sprintf("pick %d: id %d is picked twice", i+1, p.ID))
		case reason == "":
			violations = append(violations, fmt.Sprintf("pick %d: id %d has no reason", i+1, p.ID))
		case !loved[lovedKey(p.Liked)]:
			violations = append(violations, fmt.Sprintf("pick %d: liked %q is not a film he rated 4 stars or more", i+1, p.Liked))
		default:
			seen[p.ID] = true
			if cut {
				glued = append(glued, p.Reason)
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
