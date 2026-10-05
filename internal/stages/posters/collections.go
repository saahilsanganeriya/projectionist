package posters

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/image/draw"

	"projectionist/internal/library"
	"projectionist/internal/stages/collections"
	"projectionist/internal/tmdb"
)

// ---- collections: the Movies collections that have no poster of the owner's ----
//
// The collections stage's desired set, less the owner's collections (their posters are his and are never touched),
// is two more kinds of title here. A franchise collection pools its TMDB collections' posters, fanart.tv's art for
// them and the poster of its most acclaimed film; a director collection pools photos of the director (Wikimedia
// Commons, on-set shots first, then TMDB's tagged images and portraits), and the chosen photo is composed into a 2:3
// poster. The owner's own director posters are textless photos, so the composition adds no text.

const collectionPrefix = "collection:"

// collectionKey names a collection the way the collections stage compares names: case-insensitive, without the
// U+200E marks some Plex titles carry.
func collectionKey(name string) string {
	return collectionPrefix + strings.ToLower(strings.TrimSpace(strings.ReplaceAll(name, "\u200e", "")))
}

func (k kind) collection() bool { return k == franchise || k == director }

// collectionTitles lists the collections to give a poster, and the Plex ratingKeys of the owner's collections (his file
// names them), which are never a target whatever they are called.
func collectionTitles(ctx context.Context, db *sql.DB, opt Options) ([]Title, map[string]bool, error) {
	cs, err := collections.PosterlessCollections(ctx, db, opt.CollectionsOwner, opt.CollectionsApproved)
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(opt.CollectionsOwner)
	if err != nil {
		return nil, nil, err
	}
	var owner []struct {
		RatingKey string `json:"ratingKey"`
	}
	if err := json.Unmarshal(raw, &owner); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", opt.CollectionsOwner, err)
	}
	keys := map[string]bool{}
	for _, o := range owner {
		keys[o.RatingKey] = true
	}
	out := make([]Title, 0, len(cs))
	for _, c := range cs {
		k := franchise
		if c.Director {
			k = director
		}
		out = append(out, Title{Key: collectionKey(c.Name), Kind: k, Name: c.Name, Members: c.Members, Sets: c.Sets})
	}
	return out, keys, nil
}

// ---- franchise collections ----

// gatherFranchise pools a franchise collection's posters: its TMDB collections' own, fanart.tv's for them, and the
// poster of its most acclaimed film (the one this stage chose for that film, when it has judged it). A TMDB
// collection is one a member film belongs to whose name the approved list gives.
func (r *run) gatherFranchise(ctx context.Context, it *row) error {
	t := it.Title
	sets := map[int]string{}
	var setIDs []int
	var lang string
	var top tmdb.Details
	for _, id := range t.Members {
		d, err := r.env.TMDB.Details(ctx, tmdb.Movie, id)
		if errors.Is(err, tmdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		lang = cmp.Or(lang, d.Language)
		if top.ID == 0 || acclaim(d) > acclaim(top) {
			top = d
		}
		c := d.Collection
		if c == nil || sets[c.ID] != "" || len(t.Sets) > 0 && !slices.ContainsFunc(t.Sets, func(s string) bool { return strings.EqualFold(s, c.Name) }) {
			continue
		}
		sets[c.ID] = c.Name
		setIDs = append(setIDs, c.ID)
	}
	var own, fan []Candidate
	for _, id := range setIDs {
		imgs, err := r.env.TMDB.CollectionImages(ctx, id)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return err
		}
		for _, c := range r.tmdbCandidates(imgs.Posters) {
			c.Source, c.Caption = srcTMDBCollection, "TMDB, made for the "+sets[id]
			own = append(own, c)
		}
		name, art, err := r.env.Fanart.Collection(ctx, id)
		if err != nil {
			return err
		}
		if !strings.EqualFold(name, sets[id]) {
			continue // a film filed under the same number
		}
		for _, p := range art {
			if p.Type == "movieposter" {
				fan = append(fan, Candidate{URL: p.URL, Thumb: p.URL, Source: srcFanartCollection, Lang: p.Lang, Likes: p.Likes,
					Caption: "fanart.tv, made for the " + sets[id]})
			}
		}
	}
	slices.SortStableFunc(fan, func(a, b Candidate) int { return cmp.Compare(b.Likes, a.Likes) })
	film, err := r.filmPoster(ctx, top, lang)
	if err != nil {
		return err
	}
	langs := []string{"", "xx", "00", "en", lang}
	other := func(c Candidate) bool { return !slices.Contains(langs, c.Lang) }
	it.Cands = r.fill(ctx, nil, slices.DeleteFunc(own, other), slices.DeleteFunc(fan, other), film)
	return nil
}

// acclaim is a film's TMDB rating pulled towards 6/10 as if by 1,000 votes, so a little-seen film rated 9 does not
// outrank a classic.
func acclaim(d tmdb.Details) float64 {
	return (float64(d.VoteCount)*d.VoteAverage + 1000*6) / float64(d.VoteCount+1000)
}

// filmPoster is the candidate for a film's own poster: the one this stage chose for it, else TMDB's best voted in an
// allowed language.
func (r *run) filmPoster(ctx context.Context, d tmdb.Details, lang string) ([]Candidate, error) {
	if d.ID == 0 {
		return nil, nil
	}
	caption := fmt.Sprintf("the poster of %s (%d), the series' most acclaimed film", d.Title, d.Date.Year())
	var cands, verdict sql.NullString
	err := r.env.Store.DB().QueryRowContext(ctx, `SELECT cands, verdict FROM posters_items WHERE key=?`,
		key(movie, library.ID{Source: "tmdb", Value: strconv.Itoa(d.ID)}, 0)).Scan(&cands, &verdict)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var v Verdict
	var pool []Candidate
	if verdict.Valid && json.Unmarshal([]byte(verdict.String), &v) == nil && json.Unmarshal([]byte(cands.String), &pool) == nil {
		if i := slices.IndexFunc(pool, func(c Candidate) bool { return c.URL == v.Choice }); i >= 0 {
			c := pool[i]
			return []Candidate{{URL: c.URL, Thumb: c.Thumb, Source: srcFilm, Lang: c.Lang, Caption: caption}}, nil
		}
	}
	imgs, err := r.env.TMDB.Images(ctx, tmdb.Movie, d.ID)
	if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
		return nil, err
	}
	for _, c := range r.tmdbCandidates(imgs.Posters) {
		if slices.Contains([]string{"", "xx", "00", "en", lang}, c.Lang) {
			c.Source, c.Caption = srcFilm, caption
			return []Candidate{c}, nil
		}
	}
	return nil, nil
}

// ---- director collections ----

// gatherDirector pools photos of the collection's director: Wikimedia Commons files that depict them, sit in their
// category or (by their caption) show them filming, on-set shots first; and TMDB's images tagged with them and their
// profile portraits. The two lists take turns, so the pool always holds a portrait TMDB vouches for when it has one.
func (r *run) gatherDirector(ctx context.Context, it *row) error {
	people, err := r.directors(ctx, it.Title)
	if err != nil {
		return err
	}
	var commons, fromTMDB []Candidate
	for _, p := range people {
		ids, err := r.env.TMDB.PersonExternalIDs(ctx, p.ID)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return err
		}
		cs, err := r.commonsPhotos(ctx, p.Name, ids.Wikidata)
		if err != nil {
			return err
		}
		commons = append(commons, cs...)
		tagged, err := r.env.TMDB.TaggedImages(ctx, p.ID)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return err
		}
		for _, im := range tagged {
			if im.ImageType == "poster" || im.ImageType == "logo" || im.Width < 1000 {
				continue // a film's poster or logo, not a photo of the director
			}
			u := r.opt.TMDBImages + "/original" + im.FilePath
			fromTMDB = append(fromTMDB, Candidate{URL: u, Thumb: u, Source: srcTMDBTagged, Votes: im.VoteCount, Rating: im.VoteAverage,
				Caption: fmt.Sprintf("TMDB image tagged with %s (a %s from %s), %d×%d pixels", p.Name, im.ImageType,
					cmp.Or(im.Media.Title, im.Media.Name), im.Width, im.Height)})
		}
		profiles, err := r.env.TMDB.PersonImages(ctx, p.ID)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return err
		}
		slices.SortStableFunc(profiles, func(a, b tmdb.Image) int { return cmp.Compare(b.VoteAverage, a.VoteAverage) })
		for _, im := range profiles {
			if im.Width >= 500 {
				u := r.opt.TMDBImages + "/original" + im.FilePath
				fromTMDB = append(fromTMDB, Candidate{URL: u, Thumb: u, Source: srcTMDBProfile, Votes: im.VoteCount, Rating: im.VoteAverage,
					Caption: fmt.Sprintf("TMDB profile photo of %s, %d×%d pixels", p.Name, im.Width, im.Height)})
			}
		}
	}
	if len(people) == 0 {
		r.env.Log.Info("no director found for the collection", "event", "collection_no_director", "item", it.Title.Key)
	}
	it.Cands = r.fill(ctx, nil, commons, fromTMDB)
	return nil
}

// films lists a director collection's films as "Title (year)" for the judge, who checks a caption against them; a
// film TMDB cannot answer for is left out.
func (r *run) films(ctx context.Context, t Title) string {
	var out []string
	for _, id := range t.Members[:min(len(t.Members), 12)] {
		if d, err := r.env.TMDB.Details(ctx, tmdb.Movie, id); err == nil {
			out = append(out, fmt.Sprintf("%s (%d)", d.Title, d.Date.Year()))
		}
	}
	return strings.Join(out, "; ")
}

// directors are the people TMDB credits with directing most of the collection's films (both halves of a duo), or the
// one named like the collection; at most two.
func (r *run) directors(ctx context.Context, t Title) ([]tmdb.Person, error) {
	films := map[int]int{}
	people := map[int]tmdb.Person{}
	for _, id := range t.Members {
		ps, err := r.env.TMDB.Directors(ctx, id)
		if errors.Is(err, tmdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			films[p.ID]++
			people[p.ID] = p
		}
	}
	var out []tmdb.Person
	for id, n := range films {
		if 2*n > len(t.Members) || strings.EqualFold(people[id].Name, t.Name) {
			out = append(out, people[id])
		}
	}
	slices.SortFunc(out, func(a, b tmdb.Person) int {
		return cmp.Or(cmp.Compare(films[b.ID], films[a.ID]), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), 2)], nil
}

// ---- Wikimedia Commons (keyless; https://www.mediawiki.org/wiki/Wikimedia_APIs/Rate_limits) ----

// onSet finds the words a Commons title, description or category uses for a photo taken at work on a film.
var onSet = regexp.MustCompile(`(?i)\b(on (the )?set|set of|film set|filming|shooting|directing|behind the scenes|making of|on location|dreharbeiten|rodaje|tournage|na planie|clapperboard)\b`)

var htmlTag = regexp.MustCompile(`<[^>]*>`)

type commonsFile struct {
	Title     string `json:"title"`
	ImageInfo []struct {
		URL         string                     `json:"url"`
		ThumbURL    string                     `json:"thumburl"`
		Mime        string                     `json:"mime"`
		Width       int                        `json:"width"`
		Height      int                        `json:"height"`
		ExtMetadata map[string]json.RawMessage `json:"extmetadata"` // {"value": ..., "source": ...}
	} `json:"imageinfo"`
}

// meta is an extmetadata field as text, HTML tags stripped.
func (f commonsFile) meta(field string) string {
	var v struct {
		Value any `json:"value"`
	}
	if len(f.ImageInfo) == 0 || json.Unmarshal(f.ImageInfo[0].ExtMetadata[field], &v) != nil || v.Value == nil {
		return ""
	}
	s := htmlTag.ReplaceAllString(fmt.Sprint(v.Value), " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// commonsPhotos lists Commons photos of a person, ranked: the files that certainly show them first (those that state
// they depict the person's Wikidata item, or sit in a category Commons ties to that item), then text search hits,
// which count only when their title or caption names them and may show a namesake; within each, on-set words first.
// Small images, drawings and documents are left out.
func (r *run) commonsPhotos(ctx context.Context, name, qid string) ([]Candidate, error) {
	type ranked struct {
		c    Candidate
		rank int
	}
	var found []ranked
	seen := map[string]bool{}
	add := func(files []commonsFile, sure bool) {
		for _, f := range files {
			if seen[f.Title] || len(f.ImageInfo) == 0 {
				continue
			}
			ii := f.ImageInfo[0]
			title := strings.TrimSuffix(strings.TrimPrefix(f.Title, "File:"), filepath.Ext(f.Title))
			desc := f.meta("ImageDescription")
			text := title + " " + desc + " " + f.meta("Categories")
			if ii.ThumbURL == "" || !strings.HasPrefix(ii.Mime, "image/") || ii.Mime == "image/svg+xml" || ii.Mime == "image/gif" ||
				max(ii.Width, ii.Height) < 1000 || min(ii.Width, ii.Height) < 600 ||
				!sure && !strings.Contains(strings.ToLower(title+" "+desc), strings.ToLower(name)) {
				continue
			}
			seen[f.Title] = true
			caption := title
			if desc != "" {
				caption += ": " + desc
			}
			if len(caption) > 200 {
				caption = caption[:200] + "…"
			}
			rank := 0
			if sure {
				rank += 2
			}
			if onSet.MatchString(text) {
				rank++
			}
			source, site := srcCommons, "Wikimedia Commons"
			if !sure {
				source, site = srcCommonsText, "Wikimedia Commons text search (may show someone of the same name)"
			}
			found = append(found, ranked{Candidate{URL: rendering(ii.ThumbURL, ii.Width), Thumb: ii.ThumbURL, Source: source,
				License: f.meta("LicenseShortName"), Caption: fmt.Sprintf("%s, %d×%d pixels: %q", site, ii.Width, ii.Height, caption)}, rank})
		}
	}
	if qid != "" {
		files, err := r.commonsSearch(ctx, "haswbstatement:P180="+qid+" filetype:bitmap")
		if err != nil {
			return nil, err
		}
		add(files, true)
		var cat struct {
			Query struct {
				Pages []struct {
					PageProps struct {
						Item string `json:"wikibase_item"`
					} `json:"pageprops"`
				} `json:"pages"`
			} `json:"query"`
		}
		if err := r.commons(ctx, url.Values{"action": {"query"}, "prop": {"pageprops"}, "ppprop": {"wikibase_item"},
			"titles": {"Category:" + name}}, &cat); err != nil {
			return nil, err
		}
		if p := cat.Query.Pages; len(p) == 1 && p[0].PageProps.Item == qid { // not a namesake's category
			if files, err = r.commonsSearch(ctx, `incategory:"`+name+`" filetype:bitmap`); err != nil {
				return nil, err
			}
			add(files, true)
		}
	}
	files, err := r.commonsSearch(ctx, `"`+name+`" filming filetype:bitmap`)
	if err != nil {
		return nil, err
	}
	add(files, false)
	slices.SortStableFunc(found, func(a, b ranked) int { return cmp.Compare(b.rank, a.rank) })
	out := make([]Candidate, len(found))
	for i, f := range found {
		out[i] = f.c
	}
	return out, nil
}

// rendering is the largest standard Commons rendering (1920, 1280 or 960 pixels wide) no wider than the photo, made
// from its 500-pixel one by the width in the file name; else the 500-pixel one. Wikimedia throttles downloads of
// original files hard (429s on the NAS), while renderings come from its thumbnail service.
func rendering(thumb500 string, width int) string {
	i := strings.LastIndex(thumb500, "/") + 1
	for _, w := range []int{1920, 1280, 960} {
		if width >= w && strings.Contains(thumb500[i:], "500px-") {
			return thumb500[:i] + strings.Replace(thumb500[i:], "500px-", strconv.Itoa(w)+"px-", 1)
		}
	}
	return thumb500
}

// commonsSearch lists the File pages a CirrusSearch query finds (at most 50) with their image info: a 500-pixel
// rendering, size, type, licence and caption.
func (r *run) commonsSearch(ctx context.Context, query string) ([]commonsFile, error) {
	var resp struct {
		Query struct {
			Pages []commonsFile `json:"pages"`
		} `json:"query"`
	}
	err := r.commons(ctx, url.Values{"action": {"query"}, "generator": {"search"}, "gsrnamespace": {"6"}, "gsrsearch": {query},
		"gsrlimit": {"50"}, "prop": {"imageinfo"}, "iiprop": {"url|size|mime|extmetadata"}, "iiurlwidth": {"500"},
		"iiextmetadatafilter": {"ImageDescription|Categories|LicenseShortName"}}, &resp)
	return resp.Query.Pages, err
}

// commons makes one Commons API call (paced by fetch, like every request to Wikimedia).
func (r *run) commons(ctx context.Context, q url.Values, out any) error {
	q.Set("format", "json")
	q.Set("formatversion", "2")
	body, err := r.fetch(ctx, r.opt.CommonsAPI+"?"+q.Encode())
	if err != nil {
		return fmt.Errorf("commons: %w", err)
	}
	var apiErr struct {
		Error *struct{ Code, Info string } `json:"error"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != nil {
		return fmt.Errorf("commons: %s: %s", apiErr.Error.Code, apiErr.Error.Info)
	}
	return json.Unmarshal(body, out)
}

// ---- composing a director's poster ----

// posterW × posterH is the 2:3 poster a director collection gets, a size both apps show without scaling it up.
const posterW, posterH = 1000, 1500

const composePrefix = "compose:" // an outbox URL "compose:<box> <photo URL>": the poster composed from that photo

// composeURL is what a director collection's outbox row sends: the photo with the judge's box around the director
// ("-" when there is none), so a new box or photo is a new upload.
func composeURL(photo string, box []float64) string {
	spec := "-"
	if len(box) == 4 {
		spec = fmt.Sprintf("%.4f,%.4f,%.4f,%.4f", box[0], box[1], box[2], box[3])
	}
	return composePrefix + spec + " " + photo
}

func parseBox(spec string) []float64 {
	var box []float64
	for _, f := range strings.Split(spec, ",") {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil
		}
		box = append(box, v)
	}
	if !validBox(box) {
		return nil
	}
	return box
}

// validBox is a [left top right bottom] box in fractions of the image, of some size.
func validBox(b []float64) bool {
	return len(b) == 4 && b[0] >= 0 && b[1] >= 0 && b[2] <= 1 && b[3] <= 1 && b[2]-b[0] > 0.01 && b[3]-b[1] > 0.01
}

// compose frames a photo as a 2:3 poster around box (the director; nil = the middle of the photo). A tall photo is
// cropped to 2:3 around them, with a little room above the head. A wide one keeps its context: it is cropped to 4:5
// around them (wider when they need it) and set across the width, and the bands above and below are the same crop,
// enlarged, blurred and darkened, with the photo's edges feathered into them.
func compose(img image.Image, box []float64) *image.RGBA {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	bx0, by0, bx1, by1 := 0.25*w, 0.25*h, 0.75*w, 0.75*h
	if validBox(box) {
		bx0, by0, bx1, by1 = box[0]*w, box[1]*h, box[2]*w, box[3]*h
	}
	cx, cy := (bx0+bx1)/2, (by0+by1)/2
	clamp := func(v, lo, hi float64) float64 { return math.Max(lo, math.Min(v, hi)) }
	rect := func(x0, y0, cw, ch float64) image.Rectangle {
		return image.Rect(int(x0), int(y0), int(math.Round(x0+cw)), int(math.Round(y0+ch))).Add(b.Min)
	}
	dst := image.NewRGBA(image.Rect(0, 0, posterW, posterH))
	if w/h <= 0.8 {
		cw, ch := w, w*1.5
		if ch > h {
			cw, ch = h/1.5, h
		}
		x0 := clamp(cx-cw/2, 0, w-cw)
		y0 := clamp(math.Min(cy-ch/2, by0-0.04*ch), 0, h-ch)
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, rect(x0, y0, cw, ch), draw.Src, nil)
		return dst
	}
	cw := math.Min(w, math.Max(h*0.8, (bx1-bx0)*1.1))
	crop := rect(clamp(cx-cw/2, 0, w-cw), 0, cw, h)

	// The backdrop: the middle 2:3 of the crop, through a 40×60 copy and back, which blurs it, then darkened.
	bw := h / 1.5
	cover := rect(clamp(cx-bw/2, float64(crop.Min.X-b.Min.X), float64(crop.Max.X-b.Min.X)-bw), 0, bw, h)
	small := image.NewRGBA(image.Rect(0, 0, posterW/25, posterH/25))
	draw.CatmullRom.Scale(small, small.Bounds(), img, cover, draw.Src, nil)
	draw.CatmullRom.Scale(dst, dst.Bounds(), small, small.Bounds(), draw.Src, nil)
	for i := range dst.Pix {
		if i%4 != 3 {
			dst.Pix[i] = uint8(int(dst.Pix[i]) * 45 / 100)
		}
	}
	fh := int(math.Round(posterW * h / cw))
	fg := image.NewRGBA(image.Rect(0, 0, posterW, fh))
	draw.CatmullRom.Scale(fg, fg.Bounds(), img, crop, draw.Src, nil)
	const feather = 48
	top := (posterH - fh) / 2
	for y := 0; y < fh; y++ {
		alpha := min(1, float64(min(y, fh-1-y))/feather)
		row, out := fg.Pix[y*fg.Stride:y*fg.Stride+posterW*4], dst.Pix[(top+y)*dst.Stride:(top+y)*dst.Stride+posterW*4]
		for i := range row {
			out[i] = uint8(alpha*float64(row[i]) + (1-alpha)*float64(out[i]) + 0.5)
		}
	}
	return dst
}
