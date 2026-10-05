// Package library reads identity from the library's naming scheme, so stages need neither Plex's API nor its DB:
//
//	Movies/Title (Year) {tmdb-N}/Title (Year) {tmdb-N}[ - label].ext
//	TV/Show (Year) {tmdb-N}/Season NN/Show (Year) - SxxEyy[-Eyy][ - Title].ext
//
// Movie folders may carry {imdb-ttN} and show folders {tvdb-N} when TMDB had no match. The rename engine leaves the
// episode title out when TMDB only has "Episode N" or a multi-episode file has no title of its own.
package library

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type ID struct {
	Source string // tmdb, imdb, tvdb
	Value  string
}

func (id ID) String() string { return id.Source + "-" + id.Value }

type Movie struct {
	Title string
	Year  int
	ID    ID
	Label string // version label after " - ", e.g. "2160p 14Mbps"; "" for the main file
	Ext   string // with the dot
}

type Episode struct {
	Show     string
	Year     int
	ID       ID
	Season   int
	Episodes []int // one, or a range like S01E01-E02 → [1 2]
	Title    string
	Ext      string
}

var (
	movieFolder = regexp.MustCompile(`^(.+) \((\d{4})\) \{(tmdb|imdb)-([0-9a-z]+)\}$`)
	showFolder  = regexp.MustCompile(`^(.+) \((\d{4})\) \{(tmdb|tvdb)-(\d+)\}$`)
	seasonDir   = regexp.MustCompile(`^Season (\d{2,})$`)
	episodeFile = regexp.MustCompile(`^(.+) \((\d{4})\) - S(\d{2,})E(\d{2,})(?:-E(\d{2,}))?(?: - (.*))?$`)
)

// IsMovieFolder, IsShowFolder and IsSeasonFolder report whether one path element is in the scheme.
func IsMovieFolder(name string) bool  { return movieFolder.MatchString(name) }
func IsShowFolder(name string) bool   { return showFolder.MatchString(name) }
func IsSeasonFolder(name string) bool { return seasonDir.MatchString(name) }

// ParseMovie reads a path ending in <movie folder>/<file>; ok is false when it is not in the scheme.
func ParseMovie(path string) (m Movie, ok bool) {
	folder := filepath.Base(filepath.Dir(path))
	f := movieFolder.FindStringSubmatch(folder)
	if f == nil {
		return Movie{}, false
	}
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if !strings.HasPrefix(stem, folder) {
		return Movie{}, false
	}
	rest := stem[len(folder):]
	label := strings.TrimPrefix(rest, " - ")
	if rest != "" && label == rest {
		return Movie{}, false // something follows the folder name but not " - label"
	}
	year, _ := strconv.Atoi(f[2])
	return Movie{Title: f[1], Year: year, ID: ID{Source: f[3], Value: f[4]}, Label: label, Ext: ext}, true
}

// ParseEpisode reads a path ending in <show folder>/Season NN/<file>; ok is false when it is not in the scheme.
func ParseEpisode(path string) (e Episode, ok bool) {
	seasonPath := filepath.Dir(path)
	sh := showFolder.FindStringSubmatch(filepath.Base(filepath.Dir(seasonPath)))
	sd := seasonDir.FindStringSubmatch(filepath.Base(seasonPath))
	if sh == nil || sd == nil {
		return Episode{}, false
	}
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	ef := episodeFile.FindStringSubmatch(strings.TrimSuffix(base, ext))
	if ef == nil || ef[1] != sh[1] || ef[2] != sh[2] {
		return Episode{}, false
	}
	season, _ := strconv.Atoi(sd[1])
	if fileSeason, _ := strconv.Atoi(ef[3]); fileSeason != season {
		return Episode{}, false
	}
	year, _ := strconv.Atoi(sh[2])
	first, _ := strconv.Atoi(ef[4])
	eps := []int{first}
	if ef[5] != "" {
		last, _ := strconv.Atoi(ef[5])
		for n := first + 1; n <= last; n++ {
			eps = append(eps, n)
		}
	}
	return Episode{Show: sh[1], Year: year, ID: ID{Source: sh[3], Value: sh[4]}, Season: season, Episodes: eps, Title: ef[6], Ext: ext}, true
}
