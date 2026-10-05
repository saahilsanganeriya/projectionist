package posters

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"projectionist/internal/library"
	"projectionist/internal/tmdb"
)

// ---- titles on disk ----

// inventory lists every movie, show and season folder in the naming scheme; an empty root is skipped.
func inventory(moviesRoot, tvRoot string) ([]Title, error) {
	var out []Title
	seen := map[string]bool{}
	add := func(t Title) {
		if !seen[t.Key] { // two folders with one id: the first (in name order) stands for both
			seen[t.Key] = true
			out = append(out, t)
		}
	}
	if moviesRoot != "" {
		entries, err := os.ReadDir(moviesRoot)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if f, ok := library.ParseMovieFolder(e.Name()); ok && e.IsDir() {
				add(Title{Key: key(tmdb.Movie, f.ID, 0), Kind: tmdb.Movie, Name: f.Title, Year: f.Year, ID: f.ID,
					Folder: filepath.Join(moviesRoot, e.Name())})
			}
		}
	}
	if tvRoot != "" {
		entries, err := os.ReadDir(tvRoot)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			f, ok := library.ParseShowFolder(e.Name())
			if !ok || !e.IsDir() {
				continue
			}
			show := filepath.Join(tvRoot, e.Name())
			add(Title{Key: key(tmdb.Show, f.ID, 0), Kind: tmdb.Show, Name: f.Title, Year: f.Year, ID: f.ID, Folder: show})
			seasons, err := os.ReadDir(show)
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
				add(Title{Key: key(tmdb.Season, f.ID, n), Kind: tmdb.Season, Name: f.Title + " - " + label, Year: f.Year,
					ID: f.ID, Season: n, Folder: filepath.Join(show, s.Name())})
			}
		}
	}
	return out, nil
}

// key is a title's identity: "movie:tmdb-422", "show:tvdb-79169", "season:tmdb-1400:2".
func key(kind tmdb.Kind, id library.ID, season int) string {
	if kind == tmdb.Season {
		return fmt.Sprintf("%s:%s:%d", kind, id, season)
	}
	return string(kind) + ":" + id.String()
}

// ---- the owner's posters, as backed up before the rename ----

type ownerPoster struct {
	URL   string // owner:<kind>/<ratingKey>.jpg
	State string // upload | pick | default
}

// loadOwners maps title keys to the poster Plex showed at backup time and how it got there. The backup export
// (items.json + one JPEG per item) names items by Plex guid; the owner's intent comes from the Plex DB .backup
// made at the same moment: an upload:// thumb is an upload, a locked poster field (9) a pick.
func loadOwners(ctx context.Context, itemsPath, stateDB string) (map[string]ownerPoster, error) {
	raw, err := os.ReadFile(itemsPath)
	if err != nil {
		return nil, err
	}
	var items []struct {
		Kind      string   `json:"kind"`
		RatingKey string   `json:"ratingKey"`
		GUID      string   `json:"guid"`
		Title     string   `json:"title"`
		Parent    *string  `json:"parent"` // a season's show title
		Index     *int     `json:"index"`
		IDs       []string `json:"ids"` // "tmdb://584"; a season lists its own ids, not the show's
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", itemsPath, err)
	}
	states, err := ownerStates(ctx, stateDB)
	if err != nil {
		return nil, err
	}
	rank := map[string]int{ownerDefault: 0, ownerPick: 1, ownerUpload: 2}
	out := map[string]ownerPoster{}
	put := func(k string, p ownerPoster) {
		if old, ok := out[k]; !ok || rank[p.State] > rank[old.State] { // duplicates: the owner's strongest intent wins
			out[k] = p
		}
	}
	showIDs := map[string][]library.ID{} // show title → its ids; nil marks a title two shows share
	for _, it := range items {
		if it.Kind != "show" {
			continue
		}
		if _, dup := showIDs[it.Title]; dup {
			showIDs[it.Title] = nil
			continue
		}
		showIDs[it.Title] = parseIDs(it.IDs)
	}
	for _, it := range items {
		state := states[it.GUID]
		if state == "" {
			state = ownerDefault
		}
		p := ownerPoster{URL: "owner:" + it.Kind + "/" + it.RatingKey + ".jpg", State: state}
		switch it.Kind {
		case "movie", "show":
			for _, id := range parseIDs(it.IDs) {
				put(key(tmdb.Kind(it.Kind), id, 0), p)
			}
		case "season":
			if it.Parent == nil || it.Index == nil {
				continue
			}
			for _, id := range showIDs[*it.Parent] {
				put(key(tmdb.Season, id, *it.Index), p)
			}
		}
	}
	return out, nil
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

// ownerStates reads upload/pick per guid from a Plex DB .backup (never the live DB).
func ownerStates(ctx context.Context, path string) (map[string]string, error) {
	db, err := openBackup(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT guid, coalesce(user_thumb_url, ''), coalesce(user_fields, '')
		FROM metadata_items WHERE metadata_type IN (1, 2, 3)`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var guid, thumb, fields string
		if err := rows.Scan(&guid, &thumb, &fields); err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(thumb, "upload://"):
			out[guid] = ownerUpload
		case posterLocked(fields):
			out[guid] = ownerPick
		}
	}
	return out, rows.Err()
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

// openBackup opens a Plex DB copy read-only and immutable: SQLite then never writes a -wal/-shm beside it.
func openBackup(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&immutable=1")
}

// ---- Plex ratingKeys, for the Plex sink only ----

type plexItem struct {
	RatingKey string
	Section   int
	Type      int // 1 movie, 2 show, 3 season (Plex's metadata_type)
}

// plexIndex maps every movie, show and season folder to its Plex item, from the newest Plex DB .backup matching
// glob. A folder Plex gained after that backup is missing until a newer backup exists; its upload waits.
func plexIndex(ctx context.Context, glob string) (map[string]plexItem, error) {
	matches, err := filepath.Glob(glob)
	if err != nil {
		return nil, err
	}
	newest, newestMod := "", int64(0)
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() && st.ModTime().UnixNano() > newestMod {
			newest, newestMod = m, st.ModTime().UnixNano()
		}
	}
	if newest == "" {
		return nil, fmt.Errorf("no Plex DB backup matches %s", glob)
	}
	db, err := openBackup(newest)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT mp.file, m.id, m.metadata_type, m.library_section_id,
			coalesce(p.id, 0), coalesce(p.parent_id, 0)
		FROM media_parts mp JOIN media_items mi ON mi.id = mp.media_item_id
		JOIN metadata_items m ON m.id = mi.metadata_item_id
		LEFT JOIN metadata_items p ON p.id = m.parent_id
		WHERE mp.deleted_at IS NULL AND m.deleted_at IS NULL AND m.metadata_type IN (1, 4)`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", newest, err)
	}
	defer rows.Close()
	out := map[string]plexItem{}
	for rows.Next() {
		var file string
		var id, typ, section, season, show int64
		if err := rows.Scan(&file, &id, &typ, &section, &season, &show); err != nil {
			return nil, err
		}
		dir := filepath.Dir(file)
		if typ == 1 {
			out[dir] = plexItem{RatingKey: strconv.FormatInt(id, 10), Section: int(section), Type: 1}
			continue
		}
		if season != 0 { // an episode: its folder is the season's, the one above is the show's
			out[dir] = plexItem{RatingKey: strconv.FormatInt(season, 10), Section: int(section), Type: 3}
		}
		if show != 0 {
			out[filepath.Dir(dir)] = plexItem{RatingKey: strconv.FormatInt(show, 10), Section: int(section), Type: 2}
		}
	}
	return out, rows.Err()
}
