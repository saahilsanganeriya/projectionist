// Package mediux reads the poster sets MediUX (mediux.pro) lists for a TMDB collection: who made each set and which
// image files it holds. MediUX has no anonymous set API, so the client reads the collection page the site renders for
// a browser, from the React Server Components payload Next.js embeds in it. Pages are cached a week in the store and
// fetched at most one every PageGap with a User-Agent that says who asks; 429 and 5xx answers are retried once after a
// backoff (Retry-After when the server names one). A page MediUX cannot serve (it answers 500 for a collection it does
// not know) is asked again only a day later. Images are not fetched here: AssetURL names them on api.mediux.pro, which
// serves them anonymously and resizes them on request.
package mediux

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"projectionist/internal/store"
)

const (
	PageTTL     = 7 * 24 * time.Hour
	PageGap     = 2 * time.Second
	failTTL     = 24 * time.Hour
	backoff     = 10 * time.Second // before the one retry of a 429 or 5xx, unless the server names its own wait
	cachePrefix = "mediux:"
	UserAgent   = "projectionist-posters/1 (a home media server; one page every 2 s, each kept a week)"
)

var (
	// ErrParse is a page whose sets the client cannot find: MediUX changed its pages.
	ErrParse = errors.New("mediux: collection page not understood")
	// ErrUnavailable is a page MediUX did not serve (the collection unknown to it, or the site failing).
	ErrUnavailable = errors.New("mediux: page unavailable")
)

type Client struct {
	Site   string // https://mediux.pro
	Assets string // https://api.mediux.pro/assets
	Cache  *store.Store
	HTTP   *http.Client
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error

	mu     sync.Mutex // one page request at a time, PageGap apart
	last   time.Time
	pruned sync.Once
}

func New(site, assets string, cache *store.Store, now func() time.Time, sleep func(context.Context, time.Duration) error) *Client {
	return &Client{Site: strings.TrimSuffix(site, "/"), Assets: strings.TrimSuffix(assets, "/"), Cache: cache,
		HTTP: &http.Client{Timeout: time.Minute}, Now: now, Sleep: sleep}
}

// Collection is a TMDB collection as MediUX lists it.
type Collection struct {
	ID   string
	Name string
	Sets []Set
}

type Set struct {
	ID      string
	Name    string
	Creator string // the MediUX username
	Files   []File
}

type File struct {
	ID    string // the asset's UUID
	Title string // "Back to the Future Part II (1989)"; the collection poster's carries no year
	Type  string // poster | backdrop | ...
}

// Film is one film a set should have a poster for.
type Film struct {
	TMDB  int
	Title string
	Year  int
}

// AssetURL is an image on api.mediux.pro; width 0 is the full size (about 1000×1500 for a poster), else a JPEG that
// wide.
func (c *Client) AssetURL(id string, width int) string {
	if width == 0 {
		return c.Assets + "/" + id
	}
	return fmt.Sprintf("%s/%s?width=%d&format=jpg", c.Assets, id, width)
}

// Credit is how the set's artist is credited wherever the stage uses one of its posters.
func (s Set) Credit() string {
	return "Poster by " + s.Creator + " on MediUX. https://mediux.pro/sets/" + s.ID
}

var yearIn = regexp.MustCompile(`\((\d{4})\)`)

// Posters is the set's collection poster ("" when it has none) and its poster for each film, by TMDB id, as asset ids.
// A file names its film "Title (Year)": it is the poster of the film of exactly that title (of that year, when the set
// has two files of that title), else of the one film of that year whose title no other film has; never by prefix,
// which would give "Scary Movie" the poster of "Scary Movie 2".
func (s Set) Posters(films []Film) (collection string, byFilm map[int]string) {
	type titled struct {
		id, title string
		year      int
	}
	var files []titled
	for _, f := range s.Files {
		if f.Type != "poster" {
			continue
		}
		m := yearIn.FindStringSubmatchIndex(f.Title)
		if m == nil {
			if collection == "" {
				collection = f.ID
			}
			continue
		}
		year, _ := strconv.Atoi(f.Title[m[2]:m[3]])
		files = append(files, titled{f.ID, fold(f.Title[:m[0]]), year})
	}
	known := map[string]bool{}
	for _, f := range films {
		known[fold(f.Title)] = true
	}
	byFilm = map[int]string{}
	for _, f := range films {
		title := fold(f.Title)
		var exact, sameYear []titled
		for _, p := range files {
			switch {
			case p.title == title:
				exact = append(exact, p)
			case p.year == f.Year && !known[p.title]:
				sameYear = append(sameYear, p)
			}
		}
		if len(exact) > 1 {
			var ofYear []titled
			for _, p := range exact {
				if p.year == f.Year {
					ofYear = append(ofYear, p)
				}
			}
			exact = ofYear
		}
		switch {
		case len(exact) == 1:
			byFilm[f.TMDB] = exact[0].id
		case len(exact) == 0 && len(sameYear) == 1:
			byFilm[f.TMDB] = sameYear[0].id
		}
	}
	return collection, byFilm
}

// fold compares titles: accents dropped, lower case, every run of punctuation and space one space.
func fold(s string) string {
	s, _, _ = transform.String(transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn))), s)
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

var languageWord = regexp.MustCompile(`(?i)\b(german|deutsch|french|francais|spanish|espanol|castellano|latino|italian|italiano|` +
	`portuguese|portugues|brazilian|polish|polski|czech|dutch|nederlands|russian|japanese|chinese|korean|turkish|` +
	`hungarian|swedish|danish|norwegian|finnish|greek|hindi)\b`)

// Foreign is whether the set's name says it is made in a language other than English: it names a language, or spells
// a word with a letter beyond ASCII ("Zurück in die Zukunft"). A word or letter the collection's or its films' own
// names hold does not count ("The French Connection", "Amélie").
func (s Set) Foreign(ownNames ...string) bool {
	own := strings.ToLower(strings.Join(ownNames, " "))
	for _, w := range languageWord.FindAllString(s.Name, -1) {
		if !strings.Contains(own, strings.ToLower(w)) {
			return true
		}
	}
	for _, r := range s.Name {
		if r > unicode.MaxASCII && unicode.IsLetter(r) && !strings.ContainsRune(own, unicode.ToLower(r)) {
			return true
		}
	}
	return false
}

// CollectionSets is every set MediUX lists for the TMDB collection.
func (c *Client) CollectionSets(ctx context.Context, tmdbCollection int) (Collection, error) {
	id := strconv.Itoa(tmdbCollection)
	body, err := c.page(ctx, "/collections/"+id)
	if err != nil {
		return Collection{}, err
	}
	return parse(body, id)
}

// page is a page's HTML, from the cache while it is younger than PageTTL. A page MediUX did not serve is remembered
// (an empty body) for failTTL; a request that got no answer at all is not.
func (c *Client) page(ctx context.Context, path string) ([]byte, error) {
	key, now := cachePrefix+path, c.Now()
	c.pruned.Do(func() { c.Cache.CachePrune(ctx, cachePrefix, now.Add(-PageTTL)) })
	if body, ok, err := c.Cache.CacheGet(ctx, key, now.Add(-PageTTL)); err == nil && ok {
		if len(body) == 0 {
			return nil, ErrUnavailable
		}
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	}
	body, err := c.fetch(ctx, c.Site+path)
	var status httpStatus
	if errors.As(err, &status) {
		// Stored as fetched failTTL-PageTTL ago, so the cache gives it up failTTL from now.
		c.Cache.CachePut(ctx, key, []byte{}, now.Add(failTTL-PageTTL))
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	var packed bytes.Buffer
	w := gzip.NewWriter(&packed)
	w.Write(body)
	w.Close()
	c.Cache.CachePut(ctx, key, packed.Bytes(), now)
	return body, nil
}

type httpStatus int

func (s httpStatus) Error() string { return "HTTP " + strconv.Itoa(int(s)) }

// fetch GETs u on the page pacer; a 429 or 5xx is retried once.
func (c *Client) fetch(ctx context.Context, u string) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		if err := c.turn(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", UserAgent)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		switch {
		case err != nil:
			return nil, err
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 || attempt == 2:
			return nil, httpStatus(resp.StatusCode)
		}
		wait := backoff
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = min(time.Duration(s)*time.Second, 5*time.Minute)
		}
		if err := c.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// turn waits until the last page request is PageGap old.
func (c *Client) turn(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.last.Add(PageGap).Sub(c.Now()); wait > 0 {
		if err := c.Sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.last = c.Now()
	return nil
}

// rscChunk is one piece of the payload Next.js streams into the page: a JSON string literal.
var rscChunk = regexp.MustCompile(`self\.__next_f\.push\(\[1,"((?:[^"\\]|\\.)*)"\]\)`)

// parse finds, in the page's payload, the object that holds the collection (by its id) beside its sets.
func parse(page []byte, id string) (Collection, error) {
	var payload strings.Builder
	for _, m := range rscChunk.FindAllSubmatch(page, -1) {
		var s string
		if err := json.Unmarshal(append(append([]byte{'"'}, m[1]...), '"'), &s); err != nil {
			return Collection{}, fmt.Errorf("%w: %w", ErrParse, err)
		}
		payload.WriteString(s)
	}
	text := payload.String()
	for i := strings.Index(text, `{"collection":{`); i >= 0; {
		var p struct {
			Collection struct {
				ID   string `json:"id"`
				Name string `json:"collection_name"`
			} `json:"collection"`
			Sets []struct {
				ID   string `json:"id"`
				Name string `json:"set_name"`
				User *struct {
					Username string `json:"username"`
				} `json:"user_created"`
				Files []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
					Type  string `json:"fileType"`
				} `json:"files"`
			} `json:"sets"`
		}
		if json.NewDecoder(strings.NewReader(text[i:])).Decode(&p) == nil && p.Collection.ID == id && p.Sets != nil {
			out := Collection{ID: id, Name: p.Collection.Name}
			for _, s := range p.Sets {
				set := Set{ID: s.ID, Name: s.Name}
				if s.User != nil {
					set.Creator = s.User.Username
				}
				for _, f := range s.Files {
					set.Files = append(set.Files, File{ID: f.ID, Title: f.Title, Type: f.Type})
				}
				out.Sets = append(out.Sets, set)
			}
			return out, nil
		}
		next := strings.Index(text[i+1:], `{"collection":{`)
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return Collection{}, ErrParse
}
