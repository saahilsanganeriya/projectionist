package posters

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"projectionist/internal/llm"
	"projectionist/internal/stage"
)

// Bakeoff lends the judge to cmd/bakeoff, so a candidate model sees exactly what the stage would send: the same
// prompt, thumbnails and schema, read back the same way. It reads the store and the thumbnail cache, and writes a
// thumbnail only when the cache lacks it.
type Bakeoff struct{ r *run }

// Stored is one title as the stage left it: its pool and verdict.
type Stored struct {
	Title   Title
	Cands   []Candidate
	Verdict *Verdict
}

func NewBakeoff(env *stage.Env) (*Bakeoff, error) {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return nil, err
	}
	return &Bakeoff{r: &run{env: env, opt: opt, http: &http.Client{Timeout: 2 * time.Minute}, rep: &stage.Report{},
		budgetAlerted: map[bool]bool{}}}, nil
}

// Load is the stored title key.
func (b *Bakeoff) Load(ctx context.Context, key string) (Stored, error) {
	var title, cands, verdict sql.NullString
	if err := b.r.env.Store.DB().QueryRowContext(ctx, `SELECT title, cands, verdict FROM posters_items WHERE key=?`, key).
		Scan(&title, &cands, &verdict); err != nil {
		return Stored{}, fmt.Errorf("%s: %w", key, err)
	}
	var s Stored
	err := errors.Join(json.Unmarshal([]byte(title.String), &s.Title), json.Unmarshal([]byte(cands.String), &s.Cands))
	if verdict.Valid {
		s.Verdict = new(Verdict)
		err = errors.Join(err, json.Unmarshal([]byte(verdict.String), s.Verdict))
	}
	return s, err
}

// Request is the stage's judge request for s, before a model is set.
func (b *Bakeoff) Request(ctx context.Context, s Stored) (llm.Request, error) {
	return b.r.request(ctx, &row{Title: s.Title, Cands: s.Cands})
}

// Scores reads an answer for s as the stage does, into scores by candidate URL.
func (b *Bakeoff) Scores(s Stored, text string) (map[string]float64, error) {
	a, err := parseAnswer(text)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for li, ci := range order(s.Title.Key, len(s.Cands)) {
		if score, ok := a.Scores[letters[li:li+1]]; ok {
			out[s.Cands[ci].URL] = score
		}
	}
	return out, nil
}

// Thumb is the thumbnail the judge sees for c.
func (b *Bakeoff) Thumb(ctx context.Context, c Candidate) ([]byte, error) { return b.r.thumb(ctx, c) }
