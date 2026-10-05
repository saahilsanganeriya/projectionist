package posters

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"projectionist/internal/library"
)

// ---- titles on disk ----

type kind string

const (
	movie  kind = "movie"
	show   kind = "show"
	season kind = "season"
)

// Title is one movie, show or season folder in the naming scheme.
type Title struct {
	Key    string     `json:"key"` // "movie:tmdb-422", "show:tvdb-79169", "season:tmdb-1400:2"
	Kind   kind       `json:"kind"`
	Name   string     `json:"name"` // as the judge reads it: "Seinfeld - Season 2"
	Year   int        `json:"year"`
	ID     library.ID `json:"id"` // the movie's or the show's
	Season int        `json:"season,omitempty"`
}

func key(k kind, id library.ID, seasonNo int) string {
	if k == season {
		return fmt.Sprintf("%s:%s:%d", k, id, seasonNo)
	}
	return string(k) + ":" + id.String()
}

// inventory lists every movie, show and season folder in the scheme; two folders with one id count once.
func inventory(moviesRoot, tvRoot string) ([]Title, error) {
	var out []Title
	seen := map[string]bool{}
	add := func(t Title) {
		if !seen[t.Key] {
			seen[t.Key] = true
			out = append(out, t)
		}
	}
	movies, err := os.ReadDir(moviesRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range movies {
		if f, ok := library.ParseMovieFolder(e.Name()); ok && e.IsDir() {
			add(Title{Key: key(movie, f.ID, 0), Kind: movie, Name: f.Title, Year: f.Year, ID: f.ID})
		}
	}
	shows, err := os.ReadDir(tvRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range shows {
		f, ok := library.ParseShowFolder(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		add(Title{Key: key(show, f.ID, 0), Kind: show, Name: f.Title, Year: f.Year, ID: f.ID})
		seasons, err := os.ReadDir(filepath.Join(tvRoot, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, s := range seasons {
			n, ok := library.ParseSeasonFolder(s.Name())
			if !ok || !s.IsDir() {
				continue
			}
			label := fmt.Sprintf("Season %d", n)
			if n == 0 {
				label = "Specials"
			}
			add(Title{Key: key(season, f.ID, n), Kind: season, Name: f.Title + " - " + label, Year: f.Year, ID: f.ID, Season: n})
		}
	}
	return out, nil
}

// ---- the owner's posters: a migration from Plex DB backups (Plex itself is never asked) ----

const (
	ownerUpload  = "upload"  // the owner uploaded it: never replaced
	ownerPick    = "pick"    // the owner chose it among Plex's: replaced only by a clearly better one
	ownerDefault = "default" // Plex's own choice
)

// importOwners merges one export into posters_owner: which poster Plex showed for every movie, show and season at
// backup time, and how it got there. The export (items.json + one JPEG per item, named by ratingKey) says which file
// and which external ids; the Plex DB .backup taken with it says, per guid, whether the poster was uploaded (an
// upload:// thumb) or picked (poster field 9 locked), and which show a season belongs to.
//
// A later export's upload or pick replaces the earlier entry (the owner's newer intent); its default never does: a
// title Plex re-created during the rename shows its default poster again, but the owner's upload still stands.
// Only an export taken before this stage's first upload says who chose a poster: after it, Plex lists this stage's
// uploads as uploads too.
func importOwners(ctx context.Context, db *sql.DB, itemsPath, postersDir, backupDB string) (int, error) {
	raw, err := os.ReadFile(itemsPath)
	if err != nil {
		return 0, err
	}
	var items []struct {
		Kind      string   `json:"kind"`
		RatingKey string   `json:"ratingKey"`
		GUID      string   `json:"guid"`
		Index     *int     `json:"index"`
		IDs       []string `json:"ids"` // "tmdb://584"
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0, fmt.Errorf("%s: %w", itemsPath, err)
	}
	plexDB, err := openBackup(backupDB)
	if err != nil {
		return 0, err
	}
	defer plexDB.Close()
	type plexRow struct {
		id, parent  int64
		state, guid string
	}
	byGUID, byID := map[string]plexRow{}, map[int64]plexRow{}
	rows, err := plexDB.QueryContext(ctx, `SELECT id, guid, coalesce(parent_id, 0), coalesce(user_thumb_url, ''),
		coalesce(user_fields, '') FROM metadata_items WHERE metadata_type IN (1, 2, 3)`)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", backupDB, err)
	}
	for rows.Next() {
		var r plexRow
		var thumb, fields string
		if err := rows.Scan(&r.id, &r.guid, &r.parent, &thumb, &fields); err != nil {
			rows.Close()
			return 0, err
		}
		switch {
		case strings.HasPrefix(thumb, "upload://"):
			r.state = ownerUpload
		case posterLocked(fields):
			r.state = ownerPick
		default:
			r.state = ownerDefault
		}
		byGUID[r.guid], byID[r.id] = r, r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	idsByGUID := map[string][]string{}
	for _, it := range items {
		idsByGUID[it.GUID] = it.IDs
	}

	rank := map[string]int{ownerDefault: 0, ownerPick: 1, ownerUpload: 2}
	type owned struct{ file, state, guid string }
	out := map[string]owned{}
	for _, it := range items {
		pr, ok := byGUID[it.GUID]
		file := filepath.Join(postersDir, it.Kind, it.RatingKey+".jpg")
		if _, err := os.Stat(file); !ok || err != nil {
			continue
		}
		var keys []string
		switch it.Kind {
		case "movie", "show":
			for _, id := range parseIDs(it.IDs) {
				keys = append(keys, key(kind(it.Kind), id, 0))
			}
		case "season":
			if it.Index == nil {
				continue
			}
			for _, id := range parseIDs(idsByGUID[byID[pr.parent].guid]) {
				keys = append(keys, key(season, id, *it.Index))
			}
		}
		for _, k := range keys { // one title listed twice: the owner's strongest intent wins
			if old, dup := out[k]; !dup || rank[pr.state] > rank[old.state] {
				out[k] = owned{file, pr.state, it.GUID}
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for k, o := range out {
		if _, err := tx.ExecContext(ctx, `INSERT INTO posters_owner(key, file, state, guid) VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET file=excluded.file, state=excluded.state, guid=excluded.guid
			WHERE excluded.state != 'default' OR posters_owner.state = 'default'`, k, o.file, o.state, o.guid); err != nil {
			return 0, err
		}
	}
	return len(out), tx.Commit()
}

func parseIDs(ids []string) []library.ID {
	var out []library.ID
	for _, s := range ids {
		if src, val, ok := strings.Cut(s, "://"); ok {
			out = append(out, library.ID{Source: src, Value: val})
		}
	}
	return out
}

// posterLocked reads Plex's user_fields ("lockedFields=2|9&...") for field 9, the poster.
func posterLocked(fields string) bool {
	_, locked, ok := strings.Cut(fields, "lockedFields=")
	if !ok {
		return false
	}
	locked, _, _ = strings.Cut(locked, "&")
	for _, f := range strings.Split(locked, "|") {
		if f == "9" {
			return true
		}
	}
	return false
}

// openBackup opens a Plex DB copy read-only and immutable, so SQLite never writes a -wal or -shm beside it.
func openBackup(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&immutable=1")
}

type ownerPoster struct{ File, State string }

func loadOwners(ctx context.Context, db *sql.DB) (map[string]ownerPoster, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, file, state FROM posters_owner`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ownerPoster{}
	for rows.Next() {
		var k string
		var p ownerPoster
		if err := rows.Scan(&k, &p.File, &p.State); err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, rows.Err()
}
