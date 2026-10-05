package library

import (
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
		{"/tv/Fleabag (2016) {tmdb-67070}/Season 01/Fleabag (2016) - S01E01.mkv",
			Episode{"Fleabag", 2016, ID{"tmdb", "67070"}, 1, []int{1}, "", ".mkv"}, true},
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
