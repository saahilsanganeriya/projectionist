package lists

// Sinks: each shelf becomes a Plex collection and a Jellyfin BoxSet. Every run compares what the app has with the
// shelf and sends only the difference, so the work left by a failed or slow run is simply recomputed by the next
// one: the reconciliation is the queue. Calls are retried with backoff inside a per-app budget; a call that keeps
// failing defers the app's remaining work to the next run and never fails the run.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectionist/internal/store"
)

const sinkSchema = `
CREATE TABLE IF NOT EXISTS lists_sinks (
  app       TEXT NOT NULL,              -- plex, jellyfin
  list      TEXT NOT NULL,              -- list key
  id        TEXT NOT NULL DEFAULT '',   -- the collection's ratingKey or BoxSet id; '' until a create is confirmed
  intent_at TEXT NOT NULL DEFAULT '',   -- when a create was sent: adopt the result if its answer was lost
  poster    TEXT NOT NULL DEFAULT '',   -- sha256 of the last poster uploaded
  promoted  INTEGER NOT NULL DEFAULT 0, -- Plex: this stage promoted it to the owner's Home
  PRIMARY KEY (app, list)
);
`

// sleep is how sinks and the Letterboxd pacer wait; tests replace it with a fake clock.
var sleep = sleepCtx

type sinkRow struct {
	ID, Poster string
	IntentAt   time.Time
	Promoted   bool
}

func getRow(ctx context.Context, db *sql.DB, app, key string) (sinkRow, error) {
	var r sinkRow
	var intent string
	err := db.QueryRowContext(ctx, `SELECT id, intent_at, poster, promoted FROM lists_sinks WHERE app=? AND list=?`, app, key).
		Scan(&r.ID, &intent, &r.Poster, &r.Promoted)
	if errors.Is(err, sql.ErrNoRows) {
		return sinkRow{}, nil
	}
	if intent != "" {
		r.IntentAt, _ = time.Parse(store.TimeFormat, intent)
	}
	return r, err
}

// shelvedKeys returns the keys of every list the stage has a record of in either app.
func shelvedKeys(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT list FROM lists_sinks ORDER BY list`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// made returns the ids of the collections the stage made in app, and the times of its creates still unconfirmed.
func made(ctx context.Context, db *sql.DB, app string) (ids map[string]bool, intents []time.Time, err error) {
	rows, err := db.QueryContext(ctx, `SELECT id, intent_at FROM lists_sinks WHERE app=?`, app)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids = map[string]bool{}
	for rows.Next() {
		var id, intent string
		if err := rows.Scan(&id, &intent); err != nil {
			return nil, nil, err
		}
		if id != "" {
			ids[id] = true
		}
		if at, err := time.Parse(store.TimeFormat, intent); err == nil {
			intents = append(intents, at)
		}
	}
	return ids, intents, rows.Err()
}

// adoptWindow bounds adoption by intent: a collection with a shelf's title is taken for the answer to a create the
// stage sent (its answer lost) only if the app made it between a minute before the create (rounding) and this long
// after. One made later is someone else's — the owner's, the collections stage's — and is never adopted.
const adoptWindow = 10 * time.Minute

// answersCreate tells whether a collection made at created can be the result of a create sent at one of intents.
func answersCreate(created time.Time, intents ...time.Time) bool {
	for _, at := range intents {
		if !at.IsZero() && !created.Before(at.Add(-time.Minute)) && !created.After(at.Add(adoptWindow)) {
			return true
		}
	}
	return false
}

func deleteRow(ctx context.Context, db *sql.DB, app, key string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM lists_sinks WHERE app=? AND list=?`, app, key)
	return err
}

func putRow(ctx context.Context, db *sql.DB, app, key string, r sinkRow) error {
	intent := ""
	if !r.IntentAt.IsZero() {
		intent = r.IntentAt.UTC().Format(store.TimeFormat)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO lists_sinks(app, list, id, intent_at, poster, promoted) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(app, list) DO UPDATE SET id=excluded.id, intent_at=excluded.intent_at, poster=excluded.poster,
		promoted=excluded.promoted`, app, key, r.ID, intent, r.Poster, r.Promoted)
	return err
}

// writer runs one app's calls: idempotent ones up to three times (2 s, then 4 s apart); the first call that still
// fails, or the end of the budget, stops the app for this run.
type writer struct {
	app     string
	log     *slog.Logger
	writes  int
	stopped string // why the rest was deferred; "" while healthy
}

func (w *writer) read(ctx context.Context, what string, fn func(context.Context) error) bool {
	return w.try(ctx, what, 3, fn)
}

func (w *writer) write(ctx context.Context, what string, fn func(context.Context) error) bool {
	ok := w.try(ctx, what, 3, fn)
	if ok {
		w.writes++
	}
	return ok
}

// writeOnce is for a call that is not idempotent (creating a collection): no retry; the next run adopts the
// result if the call went through after all.
func (w *writer) writeOnce(ctx context.Context, what string, fn func(context.Context) error) bool {
	ok := w.try(ctx, what, 1, fn)
	if ok {
		w.writes++
	}
	return ok
}

func (w *writer) try(ctx context.Context, what string, attempts int, fn func(context.Context) error) bool {
	if w.stopped != "" {
		return false
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(ctx); err == nil {
			return true
		}
		if ctx.Err() != nil || attempt == attempts {
			break
		}
		w.log.Warn(w.app+" call failed", "event", w.app+"_retry", "call", what, "attempt", attempt, "err", err)
		if sleep(ctx, time.Duration(attempt)*2*time.Second) != nil {
			break
		}
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("budget spent (%w)", ctx.Err())
	}
	w.stopped = what + ": " + err.Error()
	w.log.Warn(w.app+" writes deferred to the next run", "event", w.app+"_deferred", "call", what, "err", err)
	return false
}

// poster returns the shelf's poster: the configured file, else one rendered from the name.
func poster(s shelf) (img []byte, contentType string, err error) {
	if s.List.Poster != "" {
		img, err = os.ReadFile(s.List.Poster)
		if strings.EqualFold(filepath.Ext(s.List.Poster), ".png") {
			return img, "image/png", err
		}
		return img, "image/jpeg", err
	}
	kicker := "LETTERBOXD · MY LIST"
	if s.List.Canon {
		kicker = "THE CANON"
	}
	img, err = renderPoster(s.List.Name, kicker)
	return img, "image/png", err
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// planMoves turns the order have into want (the same members) with "move item after X" steps ("" = to the front).
// The longest run of items already in list order stays put and only the others move, each right after its
// predecessor in want: an unchanged list costs nothing and one displaced film costs one move.
type move struct{ Item, After string }

func planMoves(have, want []string) []move {
	pos := make(map[string]int, len(want))
	for i, k := range want {
		pos[k] = i
	}
	var seq []int // want positions of have's items, in have's order
	for _, k := range have {
		if i, ok := pos[k]; ok {
			seq = append(seq, i)
		}
	}
	keep := map[int]bool{}
	for _, i := range longestIncreasing(seq) {
		keep[i] = true
	}
	var out []move
	for i, k := range want {
		if keep[i] {
			continue
		}
		after := ""
		if i > 0 {
			after = want[i-1]
		}
		out = append(out, move{k, after})
	}
	return out
}

// longestIncreasing returns the values of one longest strictly increasing subsequence of seq.
func longestIncreasing(seq []int) []int {
	tails := []int{} // tails[l] = index in seq of the smallest tail of an increasing run of length l+1
	prev := make([]int, len(seq))
	for i, v := range seq {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if seq[tails[mid]] < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		prev[i] = -1
		if lo > 0 {
			prev[i] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	out := make([]int, len(tails))
	if len(tails) == 0 {
		return out
	}
	for i, k := len(tails)-1, tails[len(tails)-1]; i >= 0; i, k = i-1, prev[k] {
		out[i] = seq[k]
	}
	return out
}

// diffSets returns the members to drop (in have, not in want) and to add (in want, not in have), each in its
// list's order.
func diffSets(have, want []string) (drop, add []string) {
	in := func(xs []string) map[string]bool {
		m := make(map[string]bool, len(xs))
		for _, x := range xs {
			m[x] = true
		}
		return m
	}
	h, w := in(have), in(want)
	for _, x := range have {
		if !w[x] {
			drop = append(drop, x)
		}
	}
	for _, x := range want {
		if !h[x] {
			add = append(add, x)
		}
	}
	return drop, add
}

func batches(xs []string, n int) [][]string {
	var out [][]string
	for len(xs) > n {
		out = append(out, xs[:n])
		xs = xs[n:]
	}
	if len(xs) > 0 {
		out = append(out, xs)
	}
	return out
}

// sameTitle compares collection titles as Plex matches collection tags: ignoring case, and ignoring the invisible
// U+200E two hand-made Plex collections carry.
func sameTitle(a, b string) bool { return fold(a) == fold(b) }
