package rename

// Episode numbering for a TV file, from its own name and folders only; titles come from the show's episode list
// (TMDB). The rows it returns are what Plex's database used to give the Python planner: one per episode the file
// holds, so the planner's rules (file range wins, the file's own title when TMDB disagrees, specials) apply unchanged.

import (
	"path"
	"regexp"
	"strings"
)

var (
	codeNxMM       = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])(\d{1,2})x(\d{2,3})(?:[^0-9]|$)`)
	seasonEpisode  = regexp.MustCompile(`(?i)\bSeason[ ._]?(\d{1,2})[ ._]?Episode[ ._]?(\d{1,3})`)
	episodeOnly    = regexp.MustCompile(`(?i)(?:^|[ ._\-\[])E(?:p|pisode)?[ ._]?(\d{1,3})(?:[^0-9a-z]|$)`)
	bracketGroups  = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	trailingNumber = regexp.MustCompile(`[-–]` + pyS + `*(\d{1,4})` + pyS + `*$`)
	folderSeason   = regexp.MustCompile(`(?i)(?:^|[ ._\-\[(])(?:season|series|S)[ ._-]?(\d{1,2})(?:$|[ ._\-\])]|E\d)`)
)

// episodeNumbers reads (season, first, last, the file's own title) from a TV file's path. season is -1 when only an
// absolute number is known. The extended spellings rename_engine.py left to Plex (4x01, "Season 3 Episode 1", E01
// under an S01 folder, "Show- 01") carry no title: the source's title is used for them, as Plex's was.
func episodeNumbers(tvRoot, f string) (fileEp, bool) {
	name := path.Base(f)
	if e, ok := fileEpisode(name); ok {
		return e, true
	}
	s := stem(name)
	if m := codeNxMM.FindStringSubmatch(s); m != nil {
		return fileEp{atoi(m[1]), atoi(m[2]), atoi(m[2]), ""}, true
	}
	if m := seasonEpisode.FindStringSubmatch(s); m != nil {
		return fileEp{atoi(m[1]), atoi(m[2]), atoi(m[2]), ""}, true
	}
	season := seasonFromFolders(tvRoot, f)
	if m := episodeOnly.FindStringSubmatch(s); m != nil {
		return fileEp{max(season, 1), atoi(m[1]), atoi(m[1]), ""}, true
	}
	if m := trailingNumber.FindStringSubmatch(bracketGroups.ReplaceAllString(s, "")); m != nil {
		return fileEp{-1, atoi(m[1]), atoi(m[1]), ""}, true
	}
	return fileEp{}, false
}

// seasonFromFolders is the season the folders between the show's top folder and the file name, or -1.
func seasonFromFolders(tvRoot, f string) int {
	parts := relParts(tvRoot, path.Dir(f))
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.EqualFold(parts[i], "specials") {
			return 0
		}
		if m := folderSeason.FindStringSubmatch(parts[i]); m != nil {
			return atoi(m[1])
		}
	}
	return -1
}

// resolveEpisodes numbers file f against its show's episode list (which may be empty when the source has none).
// An absolute number maps to the folder's season when that season lists it, else by counting through the regular
// seasons in order.
func resolveEpisodes(tvRoot, f string, list []Episode) ([]Episode, bool) {
	e, ok := episodeNumbers(tvRoot, f)
	if !ok {
		return nil, false
	}
	titles := map[[2]int]string{}
	for _, x := range list {
		titles[[2]int{x.Season, x.Episode}] = x.Title
	}
	if e.season < 0 {
		span, season := e.last-e.first, seasonFromFolders(tvRoot, f)
		if _, listed := titles[[2]int{season, e.first}]; !listed {
			if s, n, found := countThrough(list, e.first); found {
				season, e.first = s, n
			} else if season < 0 {
				season = 1
			}
		}
		e.season, e.last = season, e.first+span
	}
	rows := make([]Episode, 0, e.last-e.first+1)
	for n := e.first; n <= e.last; n++ {
		rows = append(rows, Episode{e.season, n, titles[[2]int{e.season, n}]})
	}
	return rows, true
}

// countThrough finds the n-th episode (1-based) across seasons 1.. in list order.
func countThrough(list []Episode, n int) (season, episode int, ok bool) {
	i := 0
	for _, x := range list {
		if x.Season < 1 {
			continue
		}
		if i++; i == n {
			return x.Season, x.Episode, true
		}
	}
	return 0, 0, false
}
