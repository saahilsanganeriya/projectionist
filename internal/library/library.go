// Package library reads identity from the library's naming scheme, so stages need neither Plex's API nor its DB:
//
//	Movies/Title (Year) {tmdb-N}/Title (Year) {tmdb-N}[ - label].ext
//	TV/Show (Year) {tmdb-N}/Season NN/Show (Year) - SxxEyy[-Eyy][ - Title].ext
//
// Movie folders may carry {imdb-ttN} and show folders {tvdb-N} when TMDB had no match.
package library

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	episodeFile = regexp.MustCompile(`^(.+) \((\d{4})\) - S(\d{2,})E(\d{2,})(?:-E(\d{2,}))?(?: - (.*))?$`) // TMDB has no title for some episodes
)

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

// Folder is a movie or show folder name read from the scheme ("Title (Year) {tmdb-N}").
type Folder struct {
	Title string
	Year  int
	ID    ID
}

// ParseMovieFolder reads a movie folder's name; ok is false when it is not in the scheme.
func ParseMovieFolder(name string) (f Folder, ok bool) { return parseFolder(movieFolder, name) }

// ParseShowFolder reads a show folder's name; ok is false when it is not in the scheme.
func ParseShowFolder(name string) (f Folder, ok bool) { return parseFolder(showFolder, name) }

// ParseSeasonFolder reads "Season NN" (00 holds specials); ok is false for any other name.
func ParseSeasonFolder(name string) (season int, ok bool) {
	m := seasonDir.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	season, _ = strconv.Atoi(m[1])
	return season, true
}

func parseFolder(re *regexp.Regexp, name string) (Folder, bool) {
	m := re.FindStringSubmatch(name)
	if m == nil {
		return Folder{}, false
	}
	year, _ := strconv.Atoi(m[2])
	return Folder{Title: m[1], Year: year, ID: ID{Source: m[3], Value: m[4]}}, true
}

// ---- rename logs (TSV: time, op, old path, new path, unit) ----

// Originals maps each moved path to the first name it had: logs are read in order, so a file moved twice keeps
// the name from before its first move. A row read before is skipped: the rename stage's legacy-undo*.tsv are
// copies of the Python logs, and replaying a move breaks a chain once its path was reused (P→Q, then R→P: a second
// P→Q would give Q the original name R). A missing log is skipped (one appears on first use); moves counts the
// distinct move rows.
func Originals(logs []string) (orig map[string]string, moves int, err error) {
	orig = map[string]string{}
	seen := map[string]bool{}
	for _, path := range logs {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			col := strings.Split(line, "\t")
			if len(col) != 5 || col[1] != "move" || seen[line] {
				continue
			}
			seen[line] = true
			moves++
			if first, ok := orig[col[2]]; ok {
				orig[col[3]] = first
			} else {
				orig[col[3]] = col[2]
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	return orig, moves, nil
}
