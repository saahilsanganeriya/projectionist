package details

import (
	"os"
	"testing"

	"projectionist/internal/releasesites"
)

// The tests' release names carry placeholder sites; a deployment lists its own in [release_sites].
func TestMain(m *testing.M) {
	releasesites.Set([]string{"SITEA", "SITEB", "SITEC"}, []string{"sitea.com", "sitee."})
	os.Exit(m.Run())
}
