package releasesites

import "testing"

// Catches an empty list matching anything (an empty alternation matches every string), a site name read as a regexp
// ('.' as any character), and a Pattern made before Set keeping the lists it was compiled with.
func TestPatternFollowsSet(t *testing.T) {
	tag := New(`(?i)\b({sites})\b`)
	clip := New(`(?i)^(?:{junk})`)
	Set(nil, nil)
	for _, s := range []string{"", "Movie.2001.1080p-GROUP", "site.com.mp4"} {
		if tag.Re().MatchString(s) || clip.Re().MatchString(s) {
			t.Fatalf("%q matched with no sites configured", s)
		}
	}
	Set([]string{"SiteA", "Site.B"}, []string{"sitea.com"})
	defer Set(nil, nil)
	for s, want := range map[string]bool{"Movie.2001.x264-SITEA.mkv": true, "Movie [Site.B]": true, "Movie [SiteXB]": false} {
		if got := tag.Re().MatchString(s); got != want {
			t.Errorf("tag %q = %v, want %v", s, got, want)
		}
	}
	if !clip.Re().MatchString("SiteA.com.mp4") || clip.Re().MatchString("SiteA.mp4") {
		t.Errorf("junk prefix: %s", clip.Re())
	}
}
