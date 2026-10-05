// Package tmdb lists TMDB's posters for a title named by its TMDB id. Two sources answer the same question:
//
//   - Jellyfin, through its built-in TMDB provider (no key needed). It can only list titles Jellyfin has already
//     scanned; for the others it answers ErrNotReady, and the caller retries on a later run.
//   - the TMDB v3 API, once the owner has created a key file. It knows every title at once.
//
// Neither path ever touches Plex.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"projectionist/internal/jellyfin"
)

type Kind string

const (
	Movie  Kind = "movie"
	Show   Kind = "show"
	Season Kind = "season"
)

// Ref names a title: ID is the movie's or the show's TMDB id; Season is the season number for Kind Season.
type Ref struct {
	Kind   Kind
	ID     string
	Season int
}

// Image is one poster TMDB lists for a title.
type Image struct {
	URL      string // full size
	ThumbURL string // the w500 rendition: enough to judge from, a tenth of the bytes
	Provider string // as the source names it ("TheMovieDb", or OMDb through Jellyfin)
	Language string // ISO 639-1; "" when untagged, which on TMDB usually means textless
	Width    int    // 0 when the source does not say
	Height   int
	Votes    int
	Rating   float64 // community rating, 0-10
}

// ErrNotReady means this source cannot list the title yet (Jellyfin has not scanned it); try a later run.
var ErrNotReady = errors.New("tmdb: title not listed by this source yet")

// Source lists a title's posters, best first in the source's own order.
type Source interface {
	Posters(ctx context.Context, ref Ref) ([]Image, error)
}

// ---- Jellyfin ----

// Jellyfin answers from Jellyfin's TMDB provider. Its item index is loaded once per value, so make one per run.
type Jellyfin struct {
	JF *jellyfin.Client

	once  sync.Once
	err   error
	items map[Ref]string // Jellyfin item id per title
}

func (j *Jellyfin) Posters(ctx context.Context, ref Ref) ([]Image, error) {
	id, err := j.ItemID(ctx, ref)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Images []struct {
			ProviderName    string  `json:"ProviderName"`
			URL             string  `json:"Url"`
			ThumbnailURL    string  `json:"ThumbnailUrl"`
			Height          int     `json:"Height"`
			Width           int     `json:"Width"`
			CommunityRating float64 `json:"CommunityRating"`
			VoteCount       int     `json:"VoteCount"`
			Language        string  `json:"Language"`
		} `json:"Images"`
	}
	if err := j.JF.GetJSON(ctx, "/Items/"+id+"/RemoteImages?type=Primary&includeAllLanguages=true", &resp); err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(resp.Images))
	for _, im := range resp.Images {
		out = append(out, Image{URL: im.URL, ThumbURL: im.ThumbnailURL, Provider: im.ProviderName, Language: im.Language,
			Width: im.Width, Height: im.Height, Votes: im.VoteCount, Rating: im.CommunityRating})
	}
	return out, nil
}

// ItemID is the Jellyfin item for a title, or ErrNotReady while Jellyfin has not scanned it.
func (j *Jellyfin) ItemID(ctx context.Context, ref Ref) (string, error) {
	j.once.Do(func() { j.items, j.err = j.index(ctx) })
	if j.err != nil {
		return "", j.err
	}
	id, ok := j.items[ref]
	if !ok {
		return "", ErrNotReady
	}
	return id, nil
}

func (j *Jellyfin) index(ctx context.Context) (map[Ref]string, error) {
	type listing struct {
		Items []struct {
			ID          string            `json:"Id"`
			Type        string            `json:"Type"`
			ProviderIDs map[string]string `json:"ProviderIds"`
			SeriesID    string            `json:"SeriesId"`
			IndexNumber *int              `json:"IndexNumber"`
		} `json:"Items"`
	}
	const common = "&Recursive=true&Fields=ProviderIds&EnableImages=false&EnableUserData=false"
	var titles, seasons listing
	if err := j.JF.GetJSON(ctx, "/Items?IncludeItemTypes=Movie,Series"+common, &titles); err != nil {
		return nil, err
	}
	if err := j.JF.GetJSON(ctx, "/Items?IncludeItemTypes=Season"+common, &seasons); err != nil {
		return nil, err
	}
	items := map[Ref]string{}
	showTMDB := map[string]string{} // Jellyfin series id -> TMDB id
	for _, it := range titles.Items {
		tmdbID := it.ProviderIDs["Tmdb"]
		switch {
		case tmdbID == "":
		case it.Type == "Movie":
			items[Ref{Kind: Movie, ID: tmdbID}] = it.ID
		case it.Type == "Series":
			items[Ref{Kind: Show, ID: tmdbID}] = it.ID
			showTMDB[it.ID] = tmdbID
		}
	}
	for _, s := range seasons.Items {
		if show := showTMDB[s.SeriesID]; show != "" && s.IndexNumber != nil {
			items[Ref{Kind: Season, ID: show, Season: *s.IndexNumber}] = s.ID
		}
	}
	return items, nil
}

// ---- TMDB v3 API ----

// Direct asks TMDB itself. The key file holds either a v3 API key or a v4 read access token (a JWT); a token goes
// in a header, a v3 key can only go in the query string, so no error or log line ever carries a request URL.
type Direct struct {
	KeyFile string
	API     string // "https://api.themoviedb.org/3"
	Images  string // "https://image.tmdb.org/t/p"
	HTTP    *http.Client
}

func (d *Direct) Posters(ctx context.Context, ref Ref) ([]Image, error) {
	path := map[Kind]string{Movie: "/movie/" + ref.ID, Show: "/tv/" + ref.ID,
		Season: fmt.Sprintf("/tv/%s/season/%d", ref.ID, ref.Season)}[ref.Kind] + "/images"
	var resp struct {
		Posters []struct {
			FilePath    string  `json:"file_path"`
			Language    *string `json:"iso_639_1"`
			Width       int     `json:"width"`
			Height      int     `json:"height"`
			VoteAverage float64 `json:"vote_average"`
			VoteCount   int     `json:"vote_count"`
		} `json:"posters"`
	}
	found, err := d.get(ctx, path, &resp)
	if err != nil || !found {
		return nil, err
	}
	out := make([]Image, 0, len(resp.Posters))
	for _, p := range resp.Posters {
		lang := ""
		if p.Language != nil {
			lang = *p.Language
		}
		out = append(out, Image{URL: d.Images + "/original" + p.FilePath, ThumbURL: d.Images + "/w500" + p.FilePath,
			Provider: "TheMovieDb", Language: lang, Width: p.Width, Height: p.Height, Votes: p.VoteCount, Rating: p.VoteAverage})
	}
	return out, nil
}

// RetryDelay is the pause before retry n (1, 2) after a 429, a 5xx or a dropped connection; tests set it to 0.
var RetryDelay = func(n int) time.Duration {
	return time.Duration(n)*time.Second + rand.N(500*time.Millisecond)
}

// get decodes path's JSON into out; found is false on 404 (TMDB has no such title or season).
func (d *Direct) get(ctx context.Context, path string, out any) (found bool, err error) {
	raw, err := os.ReadFile(d.KeyFile)
	if err != nil {
		return false, fmt.Errorf("tmdb key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.API+path, nil)
		if err != nil {
			return false, err
		}
		if strings.Count(key, ".") == 2 {
			req.Header.Set("Authorization", "Bearer "+key)
		} else {
			req.URL.RawQuery = url.Values{"api_key": {key}}.Encode()
		}
		req.Header.Set("Accept", "application/json")
		status, body, err := d.do(req)
		retry := err != nil || status == http.StatusTooManyRequests || status >= 500
		if retry && attempt < 2 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(RetryDelay(attempt + 1)):
			}
			continue
		}
		switch {
		case err != nil:
			return false, fmt.Errorf("tmdb GET %s: %w", path, err)
		case status == http.StatusNotFound:
			return false, nil
		case status/100 != 2:
			return false, fmt.Errorf("tmdb GET %s: HTTP %d", path, status)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return false, fmt.Errorf("tmdb GET %s: %w", path, err)
		}
		return true, nil
	}
}

// do strips the request URL from transport errors: with a v3 key it holds the secret.
func (d *Direct) do(req *http.Request) (int, []byte, error) {
	resp, err := d.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, err
}
