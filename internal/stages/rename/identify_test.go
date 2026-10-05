package rename

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"projectionist/internal/tmdb"
)

// Catches release names parsed into the wrong search: a year taken from the title ('1917', '2001: ...'), quality or
// season words left in the title, and a scheme-style 'Title (Year)' name read as anything else.
func TestParseRelease(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
	}{
		{"Alien.1979.Theatrical.Cut.UHD.BluRay.2160p.DTS-HD.MA.5.1.HEVC.REMUX-FraMeSToR", "Alien", 1979},
		{"A Beautiful Day In The Neighborhood (2019) [1080p] [BluRay] [5.1] [SITEB.MX]", "A Beautiful Day In The Neighborhood", 2019},
		{"1917.2019.1080p.BluRay.x264", "1917", 2019},
		{"2001.A.Space.Odyssey.1968.REMASTERED.1080p", "2001 A Space Odyssey", 1968},
		{"Breaking.Bad.S05.1080p.BluRay.x264-ROVERS", "Breaking Bad", 0},
		{"Sonny Boy S01 1080p WEBRip AAC x265-EMBER", "Sonny Boy", 0},
		{"Too Old to Die Young (2019) Season 1 S01 (1080p AMZN WEB-DL x265 HEVC 10bit EAC3 5.1 ImE)", "Too Old to Die Young", 2019},
		{"Heat (1995) {imdb-tt0113277}", "Heat", 1995},
	}
	for _, c := range cases {
		if title, year := parseRelease(c.in); title != c.title || year != c.year {
			t.Errorf("parseRelease(%q) = %q, %d; want %q, %d", c.in, title, year, c.title, c.year)
		}
	}
}

// Catches the acceptance rule letting a guess through: two exact titles within a year (remakes) must be refused,
// an exact title two years off is not exact, and a lone hit is accepted whatever its title.
func TestConfident(t *testing.T) {
	r := func(id int, name string, year int) tmdb.Result { return tmdb.Result{ID: id, Name: name, Year: year} }
	cases := []struct {
		name  string
		hits  []tmdb.Result
		title string
		year  int
		want  int // 0: refused
	}{
		{"exact within a year", []tmdb.Result{r(1, "Heat Wave", 1995), r(949, "Heat", 1996)}, "Heat", 1995, 949},
		{"two exact", []tmdb.Result{r(10, "Mystery", 2001), r(11, "Mystery", 2002)}, "Mystery", 2001, 0},
		{"exact but two years off", []tmdb.Result{r(5, "Heat", 1997), r(6, "Heat 2", 1995)}, "Heat", 1995, 0},
		{"no year, several hits", []tmdb.Result{r(1, "Friends", 1994), r(2, "Friends", 1979)}, "Friends", 0, 0},
		{"single hit", []tmdb.Result{r(30983, "Detective Conan", 1996)}, "Case Closed", 0, 30983},
		{"accents folded", []tmdb.Result{r(126308, "Shōgun", 2024), r(2, "Shogun", 1980)}, "Shogun", 2024, 126308},
	}
	for _, c := range cases {
		got, ok := confident(c.hits, c.title, c.year)
		if (ok && got.ID != c.want) || (!ok && c.want != 0) {
			t.Errorf("%s: got %d (ok=%v), want %d", c.name, got.ID, ok, c.want)
		}
	}
}

type mapCache map[string]any

func (m mapCache) get(_ context.Context, key string, v any) bool { return false }
func (m mapCache) put(_ context.Context, key string, v any)      { m[key] = v }

// Catches identity sources out of order: a season pack of a show already in the library must take that show's folder
// without asking TMDB (which would also list "Breaking Bad: Original Minisodes"), and an {imdb-tt} tag must become
// the TMDB id TMDB maps it to, under the folder's own spelling.
func TestResolveOrder(t *testing.T) {
	srv := fakeTMDB(t, map[string]string{
		"/find/tt0113277": `{"movie_results":[{"id":949,"title":"Heat","release_date":"1995-12-15"}]}`,
	})
	key := filepath.Join(t.TempDir(), "key")
	write(t, key, "test-tmdb-key")
	id := newIdentifier(&tmdb.Client{KeyFile: key, API: srv.URL}, mapCache{}, time.Unix(0, 0), time.Hour)
	id.learn("show", []string{"/lib/TV/Breaking Bad (2008) {tmdb-1396}"})
	c, err := id.resolve(context.Background(), "show", "/lib/TV/Breaking.Bad.S05.1080p.BluRay.x264-ROVERS")
	if err != nil || c.Title != (Title{ID: "tmdb-1396", Name: "Breaking Bad", Year: 2008}) {
		t.Fatalf("season pack: %+v, %v", c, err)
	}
	c, err = id.resolve(context.Background(), "movie", "/lib/Movies/Remote Versions/Heat (1995) {imdb-tt0113277}")
	if err != nil || c.Title.ID != "tmdb-949" || c.Title.Name != "Heat" || c.Title.Year != 1995 {
		t.Fatalf("imdb tag: %+v, %v", c, err)
	}
}

// Catches episode numbers misread from names the Python planner left to Plex: NxMM, "Season N Episode M", an E01
// under an S01 folder, "01 Title", an anime "Show- 01", and an absolute number in a Season folder.
func TestResolveEpisodes(t *testing.T) {
	list := []Episode{ep(1, 1, "Pilot"), ep(1, 2, "Second"), ep(2, 1, "Return")}
	cases := []struct {
		file string
		want []Episode
	}{
		{tv + "Reacher/S04/Reacher.4x01.La.Citta.ITA.1080p.mkv", []Episode{ep(4, 1, "")}},
		{tv + "X/Show.Season 2 Episode 1.mkv", []Episode{ep(2, 1, "Return")}},
		{tv + "Scenes.from.a.Marriage.S01.1973/Scenes.from.a.Marriage.E02.BDRip.mkv", []Episode{ep(1, 2, "Second")}},
		{tv + "Garry Kasparov - MasterClass/02 Garry's Chess Fundamentals.mp4", []Episode{ep(1, 2, "Second")}},
		{tv + "Sonny Boy S01 1080p/[EMBER] Sonny Boy- 01.mkv", []Episode{ep(1, 1, "Pilot")}},
		{tv + "Case Closed/Season 1/Case Closed - 002 - The Kidnapped Debutante   [DarkDream].mkv", []Episode{ep(1, 2, "Second")}},
		{tv + "Show/Show - 003 - Third.mkv", []Episode{ep(2, 1, "Return")}}, // absolute, no season folder: counted through
		{tv + "Show/S01/Show.S01E01E02.1080p.mkv", []Episode{ep(1, 1, "Pilot"), ep(1, 2, "Second")}},
	}
	for _, c := range cases {
		got, ok := resolveEpisodes(tv[:len(tv)-1], c.file, list)
		if !ok || len(got) != len(c.want) {
			t.Errorf("%s: %v (ok=%v), want %v", c.file, got, ok, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, want %v", c.file, got, c.want)
			}
		}
	}
}
