package posters

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // fanart.tv and the owner's posters are sometimes PNG
	"io"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // a few of the owner's backed-up posters are WebP behind a .jpg name

	"projectionist/internal/tmdb"
)

// Candidate is one poster in a title's pool. The owner's poster, when the backup has one, is always first.
type Candidate struct {
	URL    string  `json:"url"`   // full size: https://… or owner:<kind>/<ratingKey>.jpg
	Thumb  string  `json:"thumb"` // what the judge sees, and the thumbnail cache key
	Source string  `json:"source"`
	Owner  string  `json:"owner,omitempty"` // upload | pick | default, on the owner's poster only
	Lang   string  `json:"lang,omitempty"`
	Votes  int     `json:"votes,omitempty"`
	Rating float64 `json:"rating,omitempty"`
	Likes  int     `json:"likes,omitempty"`
	Hash   uint64  `json:"dhash"`
	W      int     `json:"w"` // thumbnail size: the judge's token cost
	H      int     `json:"h"`
}

const (
	srcOwner  = "owner"
	srcTMDB   = "tmdb"
	srcFanart = "fanart"

	poolSize   = 6  // the owner's + 5 others: about 1,000 input tokens a title with thumbBox
	tryAtMost  = 12 // remote posters fetched per title before giving up on filling the pool
	sameArt    = 6  // dHash bits within which two posters are the same art (another crop, size or compression)
	priorVotes = 5  // TMDB ratings are pulled towards 5/10 as if by this many votes: sparse votes count less
)

// thumbBox is the most a thumbnail measures: ⌈252/28⌉×⌈392/28⌉ = 126 visual tokens at most.
var thumbBox = image.Rect(0, 0, 252, 392)

// gather builds a title's candidate pool. An owner upload is never replaced, so it needs no other candidate. An owner
// poster that cannot be read stops the title: without it any candidate would win, with no margin over the owner's.
func (r *run) gather(ctx context.Context, it *row) error {
	t := it.Title
	var pool []Candidate
	if p, ok := r.owners[t.Key]; ok {
		owner := Candidate{URL: "owner:" + p.File, Thumb: "owner:" + p.File, Source: srcOwner, Owner: p.State}
		if p.State == ownerUpload {
			it.Cands = []Candidate{owner}
			return nil
		}
		if err := r.withThumb(ctx, &owner); err != nil {
			r.alert("posters: %s waits: the owner's %s poster %s is unreadable: %v", t.Key, p.State, p.File, err)
			return err
		}
		pool = append(pool, owner)
	}
	info, err := r.tmdbInfo(ctx, t)
	if err != nil {
		return err
	}
	var fan []Candidate
	if r.fanartKey != "" {
		if fan, err = r.fanartPosters(ctx, t, info); err != nil {
			return err
		}
	}
	langs := []string{"", "xx", "00", "en", info.lang}
	keep := func(c Candidate) bool { return slices.Contains(langs, c.Lang) }
	it.Cands = r.fill(ctx, pool, slices.DeleteFunc(info.posters, func(c Candidate) bool { return !keep(c) }),
		slices.DeleteFunc(fan, func(c Candidate) bool { return !keep(c) }))
	return nil
}

// fill takes TMDB's and fanart.tv's posters in turn, each list best first, and keeps those that are not the same art
// as one already kept, until the pool is full. A poster that cannot be fetched is skipped, never fatal.
func (r *run) fill(ctx context.Context, pool, tmdbPosters, fanart []Candidate) []Candidate {
	var order []Candidate
	for i := 0; i < max(len(tmdbPosters), len(fanart)); i++ {
		if i < len(tmdbPosters) {
			order = append(order, tmdbPosters[i])
		}
		if i < len(fanart) {
			order = append(order, fanart[i])
		}
	}
	for i, c := range order {
		if len(pool) == poolSize || i == tryAtMost || ctx.Err() != nil {
			break
		}
		if err := r.withThumb(ctx, &c); err != nil {
			r.env.Log.Info("candidate skipped", "event", "candidate_skipped", "url", c.URL, "err", err)
			continue
		}
		if !slices.ContainsFunc(pool, func(o Candidate) bool { return bits.OnesCount64(o.Hash^c.Hash) <= sameArt }) {
			pool = append(pool, c)
		}
	}
	return pool
}

// ---- TMDB ----

type tmdbTitle struct {
	posters []Candidate // best first
	lang    string      // original language, ISO 639-1
	tvdb    int         // shows only
}

type tmdbPoster struct {
	FilePath string  `json:"file_path"`
	Language *string `json:"iso_639_1"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Average  float64 `json:"vote_average"`
	Count    int     `json:"vote_count"`
}

type tmdbImages struct {
	Images struct {
		Posters []tmdbPoster `json:"posters"`
	} `json:"images"`
}

type tmdbTop struct {
	tmdbImages
	Lang     string `json:"original_language"`
	External struct {
		TVDB int `json:"tvdb_id"`
	} `json:"external_ids"`
}

// tmdbInfo asks TMDB for the title's posters in every language, plus what filtering them needs. A title TMDB does
// not know has no TMDB posters; the owner's and fanart.tv's still compete.
func (r *run) tmdbInfo(ctx context.Context, t Title) (tmdbTitle, error) {
	id, err := once(r, "find "+t.ID.String(), func() (string, error) { return r.tmdbID(ctx, t) })
	if errors.Is(err, tmdb.ErrNotFound) {
		return tmdbTitle{}, nil
	}
	if err != nil {
		return tmdbTitle{}, err
	}
	path := "/movie/" + id
	if t.Kind != movie {
		path = "/tv/" + id
	}
	top, err := once(r, "tmdb "+path, func() (tmdbTop, error) {
		var top tmdbTop // no language asked: TMDB then lists the images in all of them
		err := r.tmdb.Get(ctx, path, url.Values{"append_to_response": {"images,external_ids"}}, &top)
		if errors.Is(err, tmdb.ErrNotFound) {
			err = nil
		}
		return top, err
	})
	if err != nil {
		return tmdbTitle{}, err
	}
	out := tmdbTitle{lang: top.Lang, tvdb: top.External.TVDB}
	if t.ID.Source == "tvdb" {
		out.tvdb, _ = strconv.Atoi(t.ID.Value)
	}
	posters := top.Images.Posters
	if t.Kind == season {
		var s tmdbImages
		err := r.tmdb.Get(ctx, fmt.Sprintf("%s/season/%d", path, t.Season), url.Values{"append_to_response": {"images"}}, &s)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return tmdbTitle{}, err
		}
		posters = s.Images.Posters
	}
	out.posters = r.tmdbCandidates(posters)
	return out, nil
}

func (r *run) tmdbCandidates(ps []tmdbPoster) []Candidate {
	var out []Candidate
	for _, p := range ps {
		if p.Width < 600 || p.Height == 0 || float64(p.Width)/float64(p.Height) < 0.62 || float64(p.Width)/float64(p.Height) > 0.72 {
			continue // a scan, a banner or a square
		}
		c := Candidate{URL: r.opt.TMDBImages + "/original" + p.FilePath, Thumb: r.opt.TMDBImages + "/w500" + p.FilePath,
			Source: srcTMDB, Votes: p.Count, Rating: p.Average}
		if p.Language != nil {
			c.Lang = *p.Language
		}
		out = append(out, c)
	}
	prior := func(c Candidate) float64 {
		return (float64(c.Votes)*c.Rating + priorVotes*5) / float64(c.Votes+priorVotes)
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(prior(b), prior(a)) })
	return out
}

// tmdbID is the movie's or show's TMDB id; a folder named by an IMDb or TVDB id is looked up once per run.
func (r *run) tmdbID(ctx context.Context, t Title) (string, error) {
	if t.ID.Source == "tmdb" {
		return t.ID.Value, nil
	}
	var found struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		Shows []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	source := map[string]string{"imdb": "imdb_id", "tvdb": "tvdb_id"}[t.ID.Source]
	if err := r.tmdb.Get(ctx, "/find/"+t.ID.Value, url.Values{"external_source": {source}}, &found); err != nil {
		return "", err
	}
	switch {
	case t.Kind == movie && len(found.Movies) > 0:
		return strconv.Itoa(found.Movies[0].ID), nil
	case t.Kind != movie && len(found.Shows) > 0:
		return strconv.Itoa(found.Shows[0].ID), nil
	}
	return "", tmdb.ErrNotFound
}

// ---- fanart.tv ----

type fanartPoster struct {
	URL    string `json:"url"`
	Lang   string `json:"lang"` // "00": no language
	Likes  string `json:"likes"`
	Season string `json:"season"`
}

type fanartList struct {
	Movie  []fanartPoster `json:"movieposter"`
	Show   []fanartPoster `json:"tvposter"`
	Season []fanartPoster `json:"seasonposter"`
}

// fanartPosters lists fanart.tv's posters for the title, most liked first. Shows are keyed by TVDB id there.
func (r *run) fanartPosters(ctx context.Context, t Title, info tmdbTitle) ([]Candidate, error) {
	var path string
	switch {
	case t.Kind == movie:
		path = "/movies/" + t.ID.Value
	case info.tvdb != 0:
		path = "/tv/" + strconv.Itoa(info.tvdb)
	default:
		return nil, nil
	}
	resp, err := once(r, "fanart "+path, func() (fanartList, error) {
		var resp fanartList
		raw, err := r.fetch(ctx, r.opt.FanartAPI+path+"?"+url.Values{"api_key": {r.fanartKey}}.Encode())
		var status httpStatus
		if errors.As(err, &status) && status == http.StatusNotFound {
			return resp, nil
		}
		if err == nil {
			err = json.Unmarshal(raw, &resp)
		}
		return resp, err
	})
	if err != nil {
		return nil, fmt.Errorf("fanart.tv %s: %w", path, err)
	}
	list := map[kind][]fanartPoster{movie: resp.Movie, show: resp.Show, season: resp.Season}[t.Kind]
	var out []Candidate
	for _, p := range list {
		if t.Kind == season && p.Season != strconv.Itoa(t.Season) {
			continue
		}
		likes, _ := strconv.Atoi(p.Likes)
		out = append(out, Candidate{URL: p.URL, Thumb: p.URL, Source: srcFanart, Lang: p.Lang, Likes: likes})
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(b.Likes, a.Likes) })
	return out, nil
}

// once runs fn the first time key is asked for in this run and shares its answer: every season of a show needs the
// show's TMDB details and fanart.tv list.
func once[T any](r *run, key string, fn func() (T, error)) (T, error) {
	e, _ := r.memo.LoadOrStore(key, &memoEntry{})
	m := e.(*memoEntry)
	m.once.Do(func() { m.val, m.err = fn() })
	v, _ := m.val.(T)
	return v, m.err
}

type memoEntry struct {
	once sync.Once
	val  any
	err  error
}

// ---- images ----

// withThumb makes sure c's thumbnail is cached and fills in its size and hash.
func (r *run) withThumb(ctx context.Context, c *Candidate) error {
	thumb, err := r.thumb(ctx, *c)
	if err != nil {
		return err
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		return err
	}
	c.W, c.H, c.Hash = img.Bounds().Dx(), img.Bounds().Dy(), dhash(img)
	return nil
}

// thumb returns c's thumbnail (JPEG within thumbBox), from the cache when it is there.
func (r *run) thumb(ctx context.Context, c Candidate) ([]byte, error) {
	sum := sha256.Sum256([]byte(c.Thumb))
	path := filepath.Join(r.opt.CacheDir, "thumbs", hex.EncodeToString(sum[:12])+".jpg")
	if b, err := os.ReadFile(path); err == nil {
		return b, nil
	}
	src, err := r.original(ctx, c.Thumb)
	if err != nil {
		return nil, err
	}
	img, err := decode(src)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	scale := min(1, float64(thumbBox.Dx())/float64(b.Dx()), float64(thumbBox.Dy())/float64(b.Dy()))
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decode refuses images over 50 megapixels before allocating them: the container has 1 GB.
func decode(b []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > 50e6 {
		return nil, fmt.Errorf("image %dx%d too large", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// dhash is a 64-bit difference hash: the same art at another size or compression lands within a few bits.
func dhash(img image.Image) uint64 {
	g := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.CatmullRom.Scale(g, g.Bounds(), img, img.Bounds(), draw.Src, nil)
	var h uint64
	for i := 0; i < 64; i++ {
		x, y := i%8, i/8
		if g.GrayAt(x, y).Y < g.GrayAt(x+1, y).Y {
			h |= 1 << i
		}
	}
	return h
}

// original returns the bytes behind a candidate URL: the owner's backed-up file, or a download.
func (r *run) original(ctx context.Context, u string) ([]byte, error) {
	if file, ok := strings.CutPrefix(u, "owner:"); ok {
		return os.ReadFile(filepath.Join(r.opt.OwnerPosters, filepath.Clean("/"+file)))
	}
	return r.fetch(ctx, u)
}

type httpStatus int

func (s httpStatus) Error() string { return "HTTP " + strconv.Itoa(int(s)) }

// FetchDelay is the pause before retry n (1, 2); tests set it to zero.
var FetchDelay = func(n int) time.Duration {
	return time.Duration(1<<n)*time.Second + rand.N(time.Second)
}

// fetch GETs u; 429, 5xx and dropped connections are retried twice. Errors never carry the URL, which may hold a key.
func (r *run) fetch(ctx context.Context, u string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, errors.New("bad image URL")
		}
		req.Header.Set("User-Agent", "projectionist-posters/1")
		resp, err := r.http.Do(req)
		var body []byte
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			if err == nil && resp.StatusCode/100 != 2 {
				err = httpStatus(resp.StatusCode)
			}
		}
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		var status httpStatus
		permanent := errors.As(err, &status) && status != http.StatusTooManyRequests && status < 500
		if err == nil {
			return body, nil
		}
		if permanent || attempt == 2 || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(FetchDelay(attempt + 1)):
		}
	}
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
