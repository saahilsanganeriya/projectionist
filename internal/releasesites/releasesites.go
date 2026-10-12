// Package releasesites holds the config's [release_sites]: the trackers and indexers whose names ride on release names
// and subtitle files. A site's name is a tag or an advert, never part of a title. The lists are empty until Set, which
// runs once at startup before any stage; every Pattern follows the latest Set.
package releasesites

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// none stands in for an empty list: an empty character class, which matches nothing.
const none = `[^\x00-\x{10FFFF}]`

var (
	mu       sync.Mutex // guards names, junk and patterns
	names    = none
	junk     = none
	patterns []*Pattern
)

// Set replaces the lists. siteNames tag a release ('[SITE.MX]', 'x264-SITE') and mark a subtitle line as an advert;
// junkFilePrefixes begin the file names of a site's bundled clip or notice ('site.com', 'group.'). Matching ignores case.
func Set(siteNames, junkFilePrefixes []string) {
	mu.Lock()
	defer mu.Unlock()
	names, junk = alternation(siteNames), alternation(junkFilePrefixes)
	for _, p := range patterns {
		p.compile()
	}
}

func alternation(words []string) string {
	if len(words) == 0 {
		return none
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = regexp.QuoteMeta(w)
	}
	return strings.Join(quoted, "|")
}

// Pattern is a regexp whose source names the lists: {sites} stands for the site names and {junk} for the junk file
// prefixes, each as one alternation (wrap it in a group).
type Pattern struct {
	src string
	re  atomic.Pointer[regexp.Regexp]
}

// New compiles src now and again on every Set; it panics on a bad source, as regexp.MustCompile does.
func New(src string) *Pattern {
	mu.Lock()
	defer mu.Unlock()
	p := &Pattern{src: src}
	p.compile()
	patterns = append(patterns, p)
	return p
}

func (p *Pattern) compile() {
	p.re.Store(regexp.MustCompile(strings.NewReplacer("{sites}", names, "{junk}", junk).Replace(p.src)))
}

// Re is the pattern as the lists stand now.
func (p *Pattern) Re() *regexp.Regexp { return p.re.Load() }
