package library

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Catches: identity misread from the naming scheme (wrong id, label or episode range) or a non-scheme path
// accepted, which would let a stage act on the wrong title.
func TestParseMovie(t *testing.T) {
	cases := []struct {
		path string
		want Movie
		ok   bool
	}{
		{"/volume1/media/Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv", Movie{"Heat", 1995, ID{"tmdb", "949"}, "", ".mkv"}, true},
		{"/m/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 2160p 14Mbps.mkv", Movie{"Heat", 1995, ID{"tmdb", "949"}, "2160p 14Mbps", ".mkv"}, true},
		{"/m/Se7en (1995) {imdb-tt0114369}/Se7en (1995) {imdb-tt0114369}.mp4", Movie{"Se7en", 1995, ID{"imdb", "tt0114369"}, "", ".mp4"}, true},
		{"/m/Heat (1995) {tmdb-949}/Heat.1995.1080p.mkv", Movie{}, false},
		{"/m/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}extra.mkv", Movie{}, false},
		{"/m/Heat (1995)/Heat (1995).mkv", Movie{}, false},
	}
	for _, c := range cases {
		got, ok := ParseMovie(c.path)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseMovie(%q) = %+v, %v; want %+v, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestParseEpisode(t *testing.T) {
	cases := []struct {
		path string
		want Episode
		ok   bool
	}{
		{"/tv/The Wire (2002) {tmdb-1438}/Season 01/The Wire (2002) - S01E01 - The Target.mkv",
			Episode{"The Wire", 2002, ID{"tmdb", "1438"}, 1, []int{1}, "The Target", ".mkv"}, true},
		{"/tv/Seinfeld (1989) {tvdb-79169}/Season 09/Seinfeld (1989) - S09E23-E24 - The Finale.mkv",
			Episode{"Seinfeld", 1989, ID{"tvdb", "79169"}, 9, []int{23, 24}, "The Finale", ".mkv"}, true},
		{"/tv/Adolescence (2025) {tmdb-249042}/Season 01/Adolescence (2025) - S01E01.mkv",
			Episode{"Adolescence", 2025, ID{"tmdb", "249042"}, 1, []int{1}, "", ".mkv"}, true},
		{"/tv/The Wire (2002) {tmdb-1438}/Season 02/The Wire (2002) - S01E01 - The Target.mkv", Episode{}, false},
		{"/tv/The Wire (2002) {tmdb-1438}/Season 01/The Wire (2002) - S01E01The Target.mkv", Episode{}, false},
		{"/tv/The Wire (2002) {tmdb-1438}/Specials/The Wire (2002) - S00E01 - x.mkv", Episode{}, false},
		{"/tv/The Wire (2002) {tmdb-1438}/Season 01/Other (2002) - S01E01 - x.mkv", Episode{}, false},
	}
	for _, c := range cases {
		got, ok := ParseEpisode(c.path)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseEpisode(%q) = %+v, %v; want %+v, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

// Catches: a file renamed twice showing its middle name instead of its first, archive rows read as moves, a
// missing log (one appears on first use) failing the run, and the rename stage's legacy copy of a log replaying
// its moves: after P→Q then R→P, a second P→Q would give Q the original name R.
func TestOriginals(t *testing.T) {
	dir := t.TempDir()
	first, second, legacy := filepath.Join(dir, "undo.tsv"), filepath.Join(dir, "undo_auto.tsv"), filepath.Join(dir, "legacy-undo.tsv")
	log := "t1\tmove\t/m/Heat.1995.BluRay-GRP.mkv\t/m/Heat (1995) {tmdb-949}/Heat.mkv\tmovie:1\n" +
		"t1\tarchive\t/m/old.nfo\t/m/_archive/old.nfo\tmovie:1\n" +
		"t1\tunit-done\t\t\tmovie:1\n" +
		"t2\tmove\t/m/P.mkv\t/m/Q.mkv\tmovie:2\n" +
		"t3\tmove\t/m/R.mkv\t/m/P.mkv\tmovie:3\n"
	os.WriteFile(first, []byte(log), 0o644)
	os.WriteFile(legacy, []byte(log), 0o644)
	os.WriteFile(second, []byte("t4\tmove\t/m/Heat (1995) {tmdb-949}/Heat.mkv\t/m/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv\tmovie:1\n"), 0o644)
	orig, moves, err := Originals([]string{first, filepath.Join(dir, "missing.tsv"), second, legacy})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/m/Heat (1995) {tmdb-949}/Heat.mkv":                   "/m/Heat.1995.BluRay-GRP.mkv",
		"/m/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv": "/m/Heat.1995.BluRay-GRP.mkv",
		"/m/Q.mkv": "/m/P.mkv", "/m/P.mkv": "/m/R.mkv",
	}
	if !maps.Equal(orig, want) || moves != 4 {
		t.Fatalf("Originals = %v, %d moves", orig, moves)
	}
}
