package bashlsp_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const parserPrefix = "mvdan.cc/sh/"

// forbidden reports why a non-test file may not import p: milestone 1 starts
// no subprocess and opens no network connection (net/url only parses URIs).
func forbidden(p string) string {
	switch {
	case p == "os/exec":
		return "M1 starts no subprocess"
	case p == "net" || strings.HasPrefix(p, "net/") && p != "net/url":
		return "M1 makes no network access"
	}
	return ""
}

// TestOnlyFrontImportsParser walks every .go file in the repo and fails if a
// file outside internal/front imports the mvdan parser, or if a non-test
// file imports os/exec or a network package.
func TestOnlyFrontImportsParser(t *testing.T) {
	skip := map[string]bool{".git": true, ".scratch": true, "testdata": true}
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		checked++
		slashed := filepath.ToSlash(path)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("%s: %v", slashed, err)
			return nil
		}
		front := strings.HasPrefix(slashed, "internal/front/")
		test := strings.HasSuffix(slashed, "_test.go")
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Errorf("%s: bad import literal %s", slashed, imp.Path.Value)
				continue
			}
			if !front && strings.HasPrefix(p, parserPrefix) {
				t.Errorf("%s imports %q: only internal/front may import the parser", slashed, p)
			}
			if why := forbidden(p); why != "" && !test {
				t.Errorf("%s imports %q: %s", slashed, p, why)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no .go files found; the walk is not reaching the repo")
	}
}
