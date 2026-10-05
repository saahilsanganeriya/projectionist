package lbpace

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Catches: a package that talks to Letterboxd (a string naming letterboxd.com or boxd.it) with its own pacing instead
// of waiting on Shared — two such stages in one process would together send faster than one request per 2 s. It
// reads every non-test Go file of the module, so a stage merged without the shared limiter fails here.
func TestEveryLetterboxdClientWaitsOnShared(t *testing.T) {
	host := regexp.MustCompile(`letterboxd\.com|boxd\.it`)
	names, waits := map[string]bool{}, map[string]bool{} // package dir → names a host, uses lbpace.Shared
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path != "../.." && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")):
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING && host.MatchString(n.Value) {
					names[dir] = true
				}
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "lbpace" && n.Sel.Name == "Shared" {
					waits[dir] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no package names Letterboxd: the walk missed the module")
	}
	for dir := range names {
		if !waits[dir] && filepath.Base(dir) != "lbpace" {
			t.Errorf("%s talks to Letterboxd but never waits on lbpace.Shared", dir)
		}
	}
}
