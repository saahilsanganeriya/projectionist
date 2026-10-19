package rename

import (
	"context"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/tmdb"
)

// Catches release names parsed into the wrong search: a year taken from the title ('1917', '2001: ...'), quality or
// season words left in the title, a scheme-style 'Title (Year)' name read as anything else, a torrent site's tag at the
// front kept in the title ('www.SiteF.org - ', '[SITEB.MX]'), and a title's own leading word or bracket ('M - ', '[REC]')
// taken for one.
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
		{"www.SiteF.org    -    Slow Horses S06E04 Lost and Found 1080p ATVP WEB-DL DDP5 1 Atmos H 264-FLUX", "Slow Horses", 0},
		{"www.SiteG.com.Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1", "Dune Part Two", 2024},
		{"[ www.SiteH.com ] - Oppenheimer.2023.1080p.BluRay.x264-SPARKS", "Oppenheimer", 2023},
		{"[SITEB.MX] Past Lives (2023) [1080p]", "Past Lives", 2023},
		{"M - Eine Stadt sucht einen Mörder (1931) 1080p", "M - Eine Stadt sucht einen Mörder", 1931},
		{"[REC] (2007) 1080p BluRay", "[REC]", 2007},
	}
	for _, c := range cases {
		if title, year := parseRelease(c.in); title != c.title || year != c.year {
			t.Errorf("parseRelease(%q) = %q, %d; want %q, %d", c.in, title, year, c.title, c.year)
		}
	}
}

// Catches the acceptance rule letting a guess through: two exact titles within a year (remakes) must be refused, also
// when both are rated, an exact title two years off is not exact, and a lone hit is accepted whatever its title; and the
// reverse, a film left for review beside namesakes nobody has rated (a household member's "Animals.2026" sat in review
// beside TMDB's "Animals", 304 votes, and "Animals." and "ANIMALS", none, all 2026): the rated one is taken; and a
// show pack named without a year left for review beside a longer title (a "Friday.Night.Lights.Season.01-05" pack
// sat in review beside "Murder Under the Friday Night Lights").
func TestConfident(t *testing.T) {
	r := func(id int, name string, year int) tmdb.Result {
		return tmdb.Result{ID: id, Title: name, Date: tmdb.Date{Time: time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)}}
	}
	rated := func(votes int, h tmdb.Result) tmdb.Result { h.VoteCount = votes; return h }
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
		{"no year, one exact title", []tmdb.Result{rated(335, r(4278, "Friday Night Lights", 2006)),
			rated(4, r(154877, "Murder Under the Friday Night Lights", 2022))}, "Friday Night Lights", 0, 4278},
		{"single hit", []tmdb.Result{r(30983, "Detective Conan", 1996)}, "Case Closed", 0, 30983},
		{"accents folded", []tmdb.Result{r(126308, "Shōgun", 2024), r(2, "Shogun", 1980)}, "Shogun", 2024, 126308},
		{"one rated among namesakes", []tmdb.Result{rated(304, r(1236045, "Animals", 2026)), r(1713368, "Animals.", 2026),
			rated(2, r(1763498, "ANIMALS", 2026)), r(1747444, "The Animals", 2026)}, "Animals", 2026, 1236045},
		{"two rated namesakes", []tmdb.Result{rated(900, r(10, "Mystery", 2001)), rated(120, r(11, "Mystery", 2002))}, "Mystery", 2001, 0},
		{"namesakes too little rated", []tmdb.Result{rated(40, r(10, "Mystery", 2001)), r(11, "Mystery", 2002)}, "Mystery", 2001, 0},
	}
	for _, c := range cases {
		got, ok := confident(c.hits, c.title, c.year)
		if (ok && got.ID != c.want) || (!ok && c.want != 0) {
			t.Errorf("%s: got %d (ok=%v), want %d", c.name, got.ID, ok, c.want)
		}
	}
}

// Catches a show taking the name of a film with the same TMDB id (the two id spaces overlap); a new arrival named
// after a same-titled show already in the library instead of through TMDB's acceptance rule (the UK "The Office" pack
// is not the US show the library holds: TMDB lists both, so it goes to review with both); an {imdb-tt} tag not
// becoming the TMDB id TMDB maps it to, under the folder's own spelling; a folder without a year not taking it
// from its episodes (two "Shōgun" on TMDB: only the year tells them apart); and a {tmdb-N} tag on a name without a
// year leaving the show without one ("The Great British Bake Off {tmdb-87012}" was filed outside the scheme).
func TestResolveOrder(t *testing.T) {
	h := fake.NewEnv(t, nil)
	tmdbRoutes(h, map[string]string{
		"/find/tt0113277": `{"movie_results":[{"id":949,"title":"Heat","release_date":"1995-12-15"}]}`,
		"/search/tv?Shogun": `{"results":[{"id":126308,"name":"Shōgun","first_air_date":"2024-02-27"},
			{"id":13862,"name":"Shōgun","first_air_date":"1980-09-15"}]}`,
		"/search/tv?The Office": `{"results":[{"id":2316,"name":"The Office","first_air_date":"2005-03-24"},
			{"id":2996,"name":"The Office","first_air_date":"2001-07-09"}]}`,
		"/tv/87012": `{"id":87012,"name":"The Great British Bake Off","first_air_date":"2017-08-29"}`,
	})
	ctx := context.Background()
	id := newIdentifier(h.Env.TMDB)
	id.learn("movie", []string{"/lib/Movies/Stalker (1979) {tmdb-1398}"}) // TMDB film 1398 is not TMDB show 1398
	id.learn("show", []string{"/lib/TV/The Office (2005) {tmdb-2316}", "/lib/TV/The Sopranos (1999) {tmdb-1398}"})
	if c, err := id.resolve(ctx, "show", "/lib/TV/The Sopranos (1999) {tmdb-1398}", nil); err != nil || c.Title.Name != "The Sopranos" {
		t.Fatalf("show named after a film with the same id: %+v, %v", c, err)
	}
	if c, err := id.resolve(ctx, "show", "/lib/TV/The.Office.S01.1080p.BluRay.x264-UK", nil); err != nil || c.Title.ID != "" || len(c.Candidates) != 2 {
		t.Fatalf("UK pack beside the US show: %+v, %v; want review with both candidates", c, err)
	}
	c, err := id.resolve(ctx, "movie", "/lib/Movies/Remote Versions/Heat (1995) {imdb-tt0113277}", nil)
	if err != nil || c.Title.ID != "tmdb-949" || c.Title.Name != "Heat" || c.Title.Year != 1995 {
		t.Fatalf("imdb tag: %+v, %v", c, err)
	}
	c, err = id.resolve(ctx, "show", "/lib/TV/Shogun", []string{"/lib/TV/Shogun/S01/Shogun.2024.S01E01.1080p.x265-AMBER.mkv"})
	if err != nil || c.Title.ID != "tmdb-126308" {
		t.Fatalf("year from the episode names: %+v, %v", c, err)
	}
	c, err = id.resolve(ctx, "show", "/lib/TV/The Great British Bake Off S01E01 Cake Week {tmdb-87012}", nil)
	if err != nil || c.Title.ID != "tmdb-87012" || c.Title.Name != "The Great British Bake Off" || c.Title.Year != 2017 {
		t.Fatalf("tmdb tag without a year: %+v, %v", c, err)
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
