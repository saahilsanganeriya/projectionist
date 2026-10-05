package tonight

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

// ---- Letterboxd: the export CSVs plus the public RSS, which carries TMDB ids ----

// entry is one film in his Letterboxd history. TMDB is 0 for export rows (the export has no TMDB ids).
type entry struct {
	Name    string  `json:"name"`
	Year    int     `json:"year"`
	TMDB    int     `json:"tmdb,omitempty"`
	Rating  float64 `json:"rating,omitempty"` // 0.5..5; 0 = watched without a rating
	Review  string  `json:"review,omitempty"`
	Watched string  `json:"watched,omitempty"` // YYYY-MM-DD; feed entries only
}

// readExport reads ratings, reviews, diary and watched from the export in dir, or from the newest
// "letterboxd-*" export folder inside it. A missing export is not an error: the RSS alone still works.
func readExport(dir string) ([]entry, error) {
	if _, err := os.Stat(filepath.Join(dir, "ratings.csv")); err != nil {
		subs, _ := filepath.Glob(filepath.Join(dir, "letterboxd-*", "ratings.csv"))
		if len(subs) == 0 {
			return nil, nil
		}
		sort.Strings(subs) // export folders are named by date, so the last is the newest
		dir = filepath.Dir(subs[len(subs)-1])
	}
	var out []entry
	// ratings.csv first: it holds his current rating, and history keeps the first rating it sees per film
	for _, f := range []string{"ratings.csv", "reviews.csv", "diary.csv", "watched.csv"} {
		rows, err := readCSV(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			year, _ := strconv.Atoi(r["Year"])
			rating, _ := strconv.ParseFloat(r["Rating"], 64)
			out = append(out, entry{Name: r["Name"], Year: year, Rating: rating, Review: r["Review"]})
		}
	}
	return out, nil
}

func readCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	var rows []map[string]string
	for _, rec := range recs[1:] {
		row := map[string]string{}
		for i, h := range recs[0] {
			if i < len(rec) {
				row[h] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type rssFeed struct {
	Items []struct {
		GUID        string  `xml:"guid"`
		Watched     string  `xml:"https://letterboxd.com watchedDate"`
		Title       string  `xml:"https://letterboxd.com filmTitle"`
		Year        int     `xml:"https://letterboxd.com filmYear"`
		Rating      float64 `xml:"https://letterboxd.com memberRating"`
		TMDB        int     `xml:"https://themoviedb.org movieId"`
		Description string  `xml:"description"`
	} `xml:"channel>item"`
}

var paragraph = regexp.MustCompile(`(?s)<p>(.*?)</p>`)
var tag = regexp.MustCompile(`<[^>]+>`)

// parseRSS returns the feed's diary entries keyed by guid; list entries carry no film and are skipped.
func parseRSS(body []byte) (map[string]entry, error) {
	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("letterboxd rss: %w", err)
	}
	out := map[string]entry{}
	for _, it := range feed.Items {
		if it.Title == "" || it.TMDB == 0 {
			continue
		}
		var review []string
		for _, m := range paragraph.FindAllStringSubmatch(it.Description, -1) {
			text := strings.TrimSpace(html.UnescapeString(tag.ReplaceAllString(m[1], "")))
			if text != "" && !strings.HasPrefix(text, "Watched on ") && !strings.HasPrefix(text, "This review may contain spoilers") {
				review = append(review, text)
			}
		}
		out[it.GUID] = entry{Name: it.Title, Year: it.Year, TMDB: it.TMDB, Rating: it.Rating, Watched: it.Watched,
			Review: strings.Join(review, " ")}
	}
	return out, nil
}

// rssCache keeps every diary entry ever seen (the feed shows only the latest 50) and the validators for a
// conditional GET, so each run costs Letterboxd at most one small request.
type rssCache struct {
	ETag         string           `json:"etag"`
	LastModified string           `json:"last_modified"`
	Entries      map[string]entry `json:"entries"`
}

func refreshRSS(ctx context.Context, env *stage.Env, url string) (rssCache, error) {
	var c rssCache
	if _, err := env.Store.GetJSON(ctx, name, "rss", &c); err != nil {
		return c, err
	}
	if c.Entries == nil {
		c.Entries = map[string]entry{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return c, err
	}
	req.Header.Set("User-Agent", userAgent)
	if c.ETag != "" {
		req.Header.Set("If-None-Match", c.ETag)
	}
	if c.LastModified != "" {
		req.Header.Set("If-Modified-Since", c.LastModified)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return c, fmt.Errorf("letterboxd rss: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return c, nil
	}
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("letterboxd rss: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return c, fmt.Errorf("letterboxd rss: %w", err)
	}
	fresh, err := parseRSS(body)
	if err != nil {
		return c, err
	}
	for k, v := range fresh {
		c.Entries[k] = v
	}
	c.ETag, c.LastModified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return c, env.Store.PutJSON(ctx, name, "rss", c)
}

const userAgent = "projectionist/1.0 (personal home-media server; one RSS read per 30 min)"

// ---- the taste profile: the stable, cached prefix of the prompt ----

type rated struct {
	Name   string
	Year   int
	Rating float64
	Review string
}

// history merges the export and the feed into one latest rating (and longest review) per film. The feed is
// newer than the export, so its ratings win.
func history(export []entry, rss map[string]entry) []rated {
	byFilm := map[string]*rated{}
	merge := func(e entry, wins bool) {
		k := filmKey(e.Name, e.Year)
		r := byFilm[k]
		if r == nil {
			r = &rated{Name: e.Name, Year: e.Year}
			byFilm[k] = r
		}
		if e.Rating > 0 && (wins || r.Rating == 0) {
			r.Rating = e.Rating
		}
		if len(e.Review) > len(r.Review) {
			r.Review = e.Review
		}
	}
	for _, e := range export {
		merge(e, false)
	}
	guids := make([]string, 0, len(rss))
	for g := range rss {
		guids = append(guids, g)
	}
	sort.Slice(guids, func(i, j int) bool { return rss[guids[i]].Watched < rss[guids[j]].Watched })
	for _, g := range guids {
		merge(rss[g], true)
	}
	out := make([]rated, 0, len(byFilm))
	for _, r := range byFilm {
		out = append(out, *r)
	}
	return out
}

// tasteText renders his ratings grouped by stars, then his reviews. Sorted, so the bytes (and the prompt
// cache) change only when his diary does.
func tasteText(films []rated, maxReview int) string {
	sort.Slice(films, func(i, j int) bool {
		if films[i].Rating != films[j].Rating {
			return films[i].Rating > films[j].Rating
		}
		return filmKey(films[i].Name, films[i].Year) < filmKey(films[j].Name, films[j].Year)
	})
	var b strings.Builder
	b.WriteString("HIS LETTERBOXD RATINGS (stars out of 5):\n")
	last := -1.0
	for _, f := range films {
		if f.Rating == 0 {
			continue
		}
		if f.Rating != last {
			fmt.Fprintf(&b, "\n%s★: ", strconv.FormatFloat(f.Rating, 'f', -1, 64))
			last = f.Rating
		} else {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s (%d)", f.Name, f.Year)
	}
	b.WriteString("\n\nHIS REVIEWS:\n")
	for _, f := range films {
		if f.Review != "" {
			fmt.Fprintf(&b, "- %s (%d), %s★: %s\n", f.Name, f.Year, strconv.FormatFloat(f.Rating, 'f', -1, 64),
				oneLine(f.Review, maxReview))
		}
	}
	return b.String()
}

// filmKey is a title+year key that survives the punctuation and accent differences between Letterboxd's titles
// and the library's folder names ("Mission: Impossible" vs "Mission - Impossible", "Amélie" vs "Amelie").
func filmKey(title string, year int) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(strings.ReplaceAll(title, "&", "and"))) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) { // NFD splits "é" into "e" + a mark, and the mark is dropped
			b.WriteRune(r)
		}
	}
	return fmt.Sprintf("%s|%d", b.String(), year)
}

// ---- the library (files) and TMDB facts per film ----

// film is what the stage knows about one library film: identity from its folder, facts from TMDB.
type film struct {
	TMDB          int      `json:"tmdb"`
	Title         string   `json:"title"`
	TMDBTitle     string   `json:"tmdb_title,omitempty"` // the title Letterboxd shows ("Se7en" where the folder says "Seven")
	OriginalTitle string   `json:"original_title,omitempty"`
	Year          int      `json:"year"`
	Directors     []string `json:"directors,omitempty"`
	Genres        []string `json:"genres,omitempty"`
	Countries     []string `json:"countries,omitempty"`
	Runtime       int      `json:"runtime,omitempty"` // minutes
	Language      string   `json:"language,omitempty"`
	Overview      string   `json:"overview,omitempty"`
	PosterPath    string   `json:"poster_path,omitempty"`
	Vote          float64  `json:"vote,omitempty"`
	Votes         int      `json:"votes,omitempty"`
	Missing       bool     `json:"missing,omitempty"` // TMDB answered 404: never asked again
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true}

// scanLibrary lists the films that have a video file in a "Title (Year) {tmdb-N}" folder, keyed by TMDB id.
func scanLibrary(moviesDir string) (map[int]film, error) {
	paths, err := filepath.Glob(filepath.Join(moviesDir, "*", "*"))
	if err != nil {
		return nil, err
	}
	out := map[int]film{}
	for _, p := range paths {
		m, ok := library.ParseMovie(p)
		if !ok || m.ID.Source != "tmdb" || !videoExt[strings.ToLower(m.Ext)] {
			continue
		}
		id, err := strconv.Atoi(m.ID.Value)
		if err == nil && id > 0 {
			out[id] = film{TMDB: id, Title: m.Title, Year: m.Year}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tonight: no films found under %s", moviesDir)
	}
	return out, nil
}

const tmdbSchema = `CREATE TABLE IF NOT EXISTS tonight_tmdb (
  tmdb       INTEGER PRIMARY KEY,
  facts      TEXT NOT NULL,
  fetched_at TEXT NOT NULL
)`

// fillFacts adds cached TMDB facts to every library film and fetches the missing ones through the process's shared
// TMDB client (its token bucket keeps every stage together under TMDB's rate limit), 4 at a time. A film TMDB
// cannot answer now keeps its folder identity and is retried next run; facts are kept for good once fetched.
func fillFacts(ctx context.Context, env *stage.Env, lib map[int]film) (fetched, failed int, err error) {
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, tmdbSchema); err != nil {
		return 0, 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT tmdb, facts FROM tonight_tmdb`)
	if err != nil {
		return 0, 0, err
	}
	cached := map[int]bool{}
	for rows.Next() {
		var id int
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if f, ok := lib[id]; ok {
			var facts film
			if json.Unmarshal([]byte(raw), &facts) == nil {
				facts.Title, facts.Year = f.Title, f.Year // the folder is the identity the owner sees
				lib[id] = facts
				cached[id] = true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	var missing []int
	for id := range lib {
		if !cached[id] {
			missing = append(missing, id)
		}
	}
	sort.Ints(missing)
	jobs := make(chan int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				facts, ferr := movieFacts(ctx, env.TMDB, id)
				mu.Lock()
				if ferr != nil {
					failed++
					if failed <= 3 {
						env.Log.Warn("tmdb lookup failed", "event", "tmdb_error", "tmdb", id, "err", ferr)
					}
					mu.Unlock()
					continue
				}
				f := lib[id]
				facts.Title, facts.Year = f.Title, f.Year
				lib[id] = facts
				raw, _ := json.Marshal(facts)
				_, ferr = db.ExecContext(ctx, `INSERT OR REPLACE INTO tonight_tmdb(tmdb, facts, fetched_at) VALUES (?, ?, ?)`,
					id, string(raw), env.Now().UTC().Format(store.TimeFormat))
				if ferr == nil {
					fetched++
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, id := range missing {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return fetched, failed, ctx.Err()
}

// movieFacts is one film's facts: its details and its directors. A film TMDB no longer knows is Missing.
func movieFacts(ctx context.Context, tc *tmdb.Client, id int) (film, error) {
	d, err := tc.Details(ctx, tmdb.Movie, id)
	if errors.Is(err, tmdb.ErrNotFound) {
		return film{TMDB: id, Missing: true}, nil
	}
	if err != nil {
		return film{}, err
	}
	dirs, err := tc.Directors(ctx, id)
	if err != nil {
		return film{}, err
	}
	f := film{TMDB: id, TMDBTitle: d.Title, OriginalTitle: d.OriginalTitle, Runtime: d.Runtime,
		Language: d.Language, Overview: firstSentence(d.Overview, 140), PosterPath: d.PosterPath, Vote: d.VoteAverage,
		Votes: d.VoteCount}
	if d.OriginalTitle == d.Title {
		f.OriginalTitle = ""
	}
	for _, g := range d.Genres {
		f.Genres = append(f.Genres, g.Name)
	}
	for _, c := range d.Countries {
		f.Countries = append(f.Countries, c.ISO)
	}
	for _, p := range dirs {
		f.Directors = append(f.Directors, p.Name)
	}
	return f, nil
}

func firstSentence(s string, max int) string {
	s = oneLine(s, 1<<20)
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	return oneLine(s, max)
}

// ---- the unseen candidates ----

type candidate struct {
	film
	Score float64
}

// watchedSets is every way the stage learns he has seen a film: Letterboxd ids (feed), Letterboxd title+year
// (export rows have no ids), and the play state Plex and Jellyfin report for his accounts.
type watchedSets struct {
	IDs  map[int]string // tmdb → source
	Keys map[string]bool
}

func newWatched(export []entry, rss map[string]entry) watchedSets {
	w := watchedSets{IDs: map[int]string{}, Keys: map[string]bool{}}
	for _, e := range export {
		w.Keys[filmKey(e.Name, e.Year)] = true
	}
	for _, e := range rss {
		w.IDs[e.TMDB] = "letterboxd"
		w.Keys[filmKey(e.Name, e.Year)] = true
	}
	return w
}

// seen reports whether a library film appears in his history. Title+year matching tries the folder title, TMDB's
// title and TMDB's original title, one year either side (festival vs release years differ).
func (w watchedSets) seen(f film) bool {
	if w.IDs[f.TMDB] != "" {
		return true
	}
	for _, t := range []string{f.Title, f.TMDBTitle, f.OriginalTitle} {
		if t == "" {
			continue
		}
		for dy := -1; dy <= 1; dy++ {
			if w.Keys[filmKey(t, f.Year+dy)] {
				return true
			}
		}
	}
	return false
}

// rankCandidates returns the unseen films, best first: TMDB's vote shrunk toward a prior (so a 9.0 from 12
// votes does not beat a 8.1 from 20 000), plus his average for the film's director, plus a per-day jitter so
// the list the model sees rotates from day to day.
func rankCandidates(lib map[int]film, w watchedSets, history []rated, recent map[int]string, day string) []candidate {
	dirRatings := map[string][]float64{}
	ratingOf := map[string]float64{}
	for _, h := range history {
		if h.Rating > 0 {
			ratingOf[filmKey(h.Name, h.Year)] = h.Rating
		}
	}
	for _, f := range lib {
		r, ok := ratingOf[filmKey(f.Title, f.Year)]
		if !ok {
			continue
		}
		for _, d := range f.Directors {
			dirRatings[d] = append(dirRatings[d], r)
		}
	}
	var out []candidate
	for _, f := range lib {
		if f.Missing || w.seen(f) || recent[f.TMDB] != "" {
			continue
		}
		const priorVote, priorWeight = 6.5, 300.0
		score := (f.Vote*float64(f.Votes) + priorVote*priorWeight) / (float64(f.Votes) + priorWeight)
		for _, d := range f.Directors {
			if rs := dirRatings[d]; len(rs) > 0 {
				score += 0.6 * (mean(rs) - 3)
			}
		}
		h := fnv.New32a()
		fmt.Fprintf(h, "%s/%d", day, f.TMDB)
		score += float64(h.Sum32()%1000) / 1000
		out = append(out, candidate{film: f, Score: score})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].TMDB < out[j].TMDB
	})
	return out
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// line is one compact candidate row: id | title | year | director | genres | countries | minutes | language | overview.
func (c candidate) line() string {
	return strings.Join([]string{strconv.Itoa(c.TMDB), c.Title, strconv.Itoa(c.Year), strings.Join(c.Directors, ", "),
		strings.Join(c.Genres, "/"), strings.Join(c.Countries, "/"), strconv.Itoa(c.Runtime), c.Language, c.Overview}, " | ")
}
