package posters

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // a few of the owner's backed-up posters are WebP behind a .jpg name

	"projectionist/internal/fanart"
	"projectionist/internal/tmdb"
)

// Candidate is one poster in a title's pool. The owner's poster, when the backup has one, is always first.
type Candidate struct {
	URL    string  `json:"url"`   // full size: https://… or owner:<owner_posters>/<kind>/<ratingKey>.jpg
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

// gather builds a title's candidate pool. The owner's upload and pick are never replaced, so they need no other
// candidate. An owner poster that cannot be read skips the title (ownerUnreadable): without it Plex's default would
// lose to any candidate, with no margin over it, and an unreadable upload or pick must not be sent to the apps.
func (r *run) gather(ctx context.Context, it *row) error {
	t := it.Title
	var pool []Candidate
	if p, ok := r.owners[t.Key]; ok {
		owner := Candidate{URL: "owner:" + p.File, Thumb: "owner:" + p.File, Source: srcOwner, Owner: p.State}
		if err := r.withThumb(ctx, &owner); err != nil {
			return r.ownerUnreadable(ctx, t, p, err)
		}
		if p.State == ownerUpload || p.State == ownerPick {
			it.Cands = []Candidate{owner}
			return nil
		}
		pool = append(pool, owner)
	}
	info, err := r.tmdbInfo(ctx, t)
	if err != nil {
		return err
	}
	fan, err := r.fanartPosters(ctx, t, info)
	if err != nil {
		return err
	}
	langs := []string{"", "xx", "00", "en", info.lang}
	keep := func(c Candidate) bool { return slices.Contains(langs, c.Lang) }
	it.Cands = r.fill(ctx, pool, slices.DeleteFunc(info.posters, func(c Candidate) bool { return !keep(c) }),
		slices.DeleteFunc(fan, func(c Candidate) bool { return !keep(c) }))
	return nil
}

// ownerUnreadable alerts once about a title whose owner poster cannot be read (a truncated JPEG in the export) and
// returns the error that skips it: it gets no verdict, so the apps keep the poster they show. The alert is recorded
// with the title's inputs (the export's file, its size and time) and raised again only when they change, so one bad
// file does not hold /health at 503 run after run.
func (r *run) ownerUnreadable(ctx context.Context, t Title, p ownerPoster, readErr error) error {
	inputs := r.poolSig(t)
	if fi, err := os.Stat(p.File); err == nil {
		inputs += fmt.Sprintf(" size=%d mtime=%d", fi.Size(), fi.ModTime().UnixNano())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	told := map[string]string{} // title key → the inputs it was alerted with
	if _, err := r.env.Store.GetJSON(ctx, name, "owner_unreadable", &told); err != nil {
		return err
	}
	if told[t.Key] != inputs {
		told[t.Key] = inputs
		if err := r.env.Store.PutJSON(context.WithoutCancel(ctx), name, "owner_unreadable", told); err != nil {
			return err
		}
		r.rep.Alert("posters: %s is skipped and keeps the poster the apps show: the owner's %s poster %s is unreadable (%v); "+
			"this alert comes again only when that file changes", t.Key, p.State, p.File, readErr)
	}
	r.rep.Count("owner_unreadable", 1)
	return fmt.Errorf("the owner's %s poster is unreadable: %w", p.State, readErr)
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

// tmdbInfo asks TMDB for the title's posters in every language, plus what filtering them needs. A title TMDB does
// not know has no TMDB posters; the owner's and fanart.tv's still compete. Every season of a show asks for the
// show's details again: the shared client answers those from its cache.
func (r *run) tmdbInfo(ctx context.Context, t Title) (tmdbTitle, error) {
	k := tmdb.Movie
	if t.Kind != movie {
		k = tmdb.TV
	}
	id, err := r.tmdbID(ctx, t)
	if err != nil {
		return tmdbTitle{}, ignoreNotFound(err)
	}
	d, err := r.env.TMDB.Details(ctx, k, id)
	if err != nil {
		return tmdbTitle{}, ignoreNotFound(err)
	}
	out := tmdbTitle{lang: d.Language, tvdb: d.ExternalIDs.TVDB}
	if t.ID.Source == "tvdb" {
		out.tvdb, _ = strconv.Atoi(t.ID.Value)
	}
	var imgs tmdb.Images // no language asked: TMDB then lists the images in all of them
	if t.Kind == season {
		imgs, err = r.env.TMDB.SeasonImages(ctx, id, t.Season)
	} else {
		imgs, err = r.env.TMDB.Images(ctx, k, id)
	}
	if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
		return tmdbTitle{}, err
	}
	out.posters = r.tmdbCandidates(imgs.Posters)
	return out, nil
}

func ignoreNotFound(err error) error {
	if errors.Is(err, tmdb.ErrNotFound) {
		return nil
	}
	return err
}

func (r *run) tmdbCandidates(ps []tmdb.Image) []Candidate {
	var out []Candidate
	for _, p := range ps {
		if p.Width < 600 || p.Height == 0 || float64(p.Width)/float64(p.Height) < 0.62 || float64(p.Width)/float64(p.Height) > 0.72 {
			continue // a scan, a banner or a square
		}
		out = append(out, Candidate{URL: r.opt.TMDBImages + "/original" + p.FilePath, Thumb: r.opt.TMDBImages + "/w500" + p.FilePath,
			Source: srcTMDB, Lang: p.Language, Votes: p.VoteCount, Rating: p.VoteAverage})
	}
	prior := func(c Candidate) float64 {
		return (float64(c.Votes)*c.Rating + priorVotes*5) / float64(c.Votes+priorVotes)
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(prior(b), prior(a)) })
	return out
}

// tmdbID is the movie's or show's TMDB id; a folder named by an IMDb or TVDB id is looked up with /find.
func (r *run) tmdbID(ctx context.Context, t Title) (int, error) {
	if t.ID.Source == "tmdb" {
		return strconv.Atoi(t.ID.Value)
	}
	source := map[string]string{"imdb": "imdb_id", "tvdb": "tvdb_id"}[t.ID.Source]
	movies, shows, err := r.env.TMDB.Find(ctx, t.ID.Value, source)
	switch {
	case err != nil:
		return 0, err
	case t.Kind == movie && len(movies) > 0:
		return movies[0].ID, nil
	case t.Kind != movie && len(shows) > 0:
		return shows[0].ID, nil
	}
	return 0, tmdb.ErrNotFound
}

// ---- fanart.tv ----

// fanartPosters lists fanart.tv's posters for the title, most liked first. Shows are keyed by TVDB id there.
func (r *run) fanartPosters(ctx context.Context, t Title, info tmdbTitle) ([]Candidate, error) {
	var imgs []fanart.Image
	var err error
	switch {
	case t.Kind == movie:
		imgs, err = r.env.Fanart.Movie(ctx, t.ID.Value)
	case info.tvdb != 0:
		imgs, err = r.env.Fanart.TV(ctx, info.tvdb)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	want := map[kind]string{movie: "movieposter", show: "tvposter", season: "seasonposter"}[t.Kind]
	var out []Candidate
	for _, p := range imgs {
		if p.Type != want || t.Kind == season && p.Season != strconv.Itoa(t.Season) {
			continue
		}
		out = append(out, Candidate{URL: p.URL, Thumb: p.URL, Source: srcFanart, Lang: p.Lang, Likes: p.Likes})
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(b.Likes, a.Likes) })
	return out, nil
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
		return os.ReadFile(file)
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
