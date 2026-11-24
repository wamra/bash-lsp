package bashlsp_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The parser is a patched copy of mvdan.cc/sh/v3/syntax (see
// internal/front/syntax/UPGRADING.md). Nothing imports the upstream module.
const (
	upstreamParser = "mvdan.cc/sh"
	vendoredParser = "github.com/wamra/bash-lsp/internal/front/syntax"
)

// execPackage is the one package whose non-test files may import os/exec:
// every external program the server starts goes through its runner.
const execPackage = "internal/extool"

// forbidden reports why the non-test file at slashed (a slash-separated path
// relative to the repo root) may not import p: only execPackage starts a
// subprocess, and nothing opens a network connection (net/url only parses
// URIs).
func forbidden(slashed, p string) string {
	switch {
	case p == "os/exec":
		if path.Dir(slashed) == execPackage {
			return ""
		}
		return "only " + execPackage + " starts a subprocess"
	case p == "net" || strings.HasPrefix(p, "net/") && p != "net/url":
		return "the server makes no network access"
	}
	return ""
}

// TestForbiddenExec plants os/exec imports in a list of file paths: only a
// file directly in internal/extool is allowed one.
func TestForbiddenExec(t *testing.T) {
	for _, c := range []struct {
		file string
		ok   bool
	}{
		{"internal/extool/run.go", true},
		{"internal/extool/find.go", true},
		{"internal/shellcheck/shellcheck.go", false},
		{"internal/extool/sub/run.go", false},
		{"internal/extoolx/run.go", false},
		{"extool/run.go", false},
		{"analysis/diag.go", false},
		{"cmd/bash-lsp/main.go", false},
		{"run.go", false},
	} {
		if got := forbidden(c.file, "os/exec") == ""; got != c.ok {
			t.Errorf("os/exec in %s: allowed = %v, want %v", c.file, got, c.ok)
		}
	}
	for _, p := range []string{"net", "net/http", "net/rpc"} {
		if forbidden("internal/extool/run.go", p) == "" {
			t.Errorf("%s is allowed in internal/extool", p)
		}
	}
	if forbidden("analysis/x.go", "net/url") != "" {
		t.Error("net/url is refused")
	}
}

// TestOnlyFrontImportsParser walks every .go file in the repo and fails if a
// file outside internal/front imports the vendored parser, if any file
// imports the upstream module it was copied from, or if a non-test file
// imports a network package or, outside internal/extool, os/exec.
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
			if p == upstreamParser || strings.HasPrefix(p, upstreamParser+"/") {
				t.Errorf("%s imports %q: the parser is vendored in internal/front/syntax", slashed, p)
			}
			if !front && (p == vendoredParser || strings.HasPrefix(p, vendoredParser+"/")) {
				t.Errorf("%s imports %q: only internal/front may import the parser", slashed, p)
			}
			if why := forbidden(slashed, p); why != "" && !test {
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

// TestAnalysisImportsNoInternal fails if a non-test file of package analysis
// imports an internal/ package: library consumers get analysis without the
// parser behind it.
func TestAnalysisImportsNoInternal(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("analysis", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no analysis files (%v)", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(p, "/internal/") {
				t.Errorf("%s imports %q: analysis may not import internal packages", filepath.ToSlash(path), p)
			}
		}
	}
}

// TestNoPlanReferencesInComments fails on a comment in a non-test Go file
// that points at a plan task ("(Task 2)"): the code outlives the plan, and
// exported doc comments are what `go doc` shows.
func TestNoPlanReferencesInComments(t *testing.T) {
	planRef := regexp.MustCompile(`(?i)\btask [0-9]+\b`)
	skip := map[string]bool{".git": true, ".scratch": true, "testdata": true}
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Errorf("%s: %v", filepath.ToSlash(path), err)
			return nil
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if m := planRef.FindString(c.Text); m != "" {
					t.Errorf("%s: comment refers to a plan task (%q): %s", fset.Position(c.Pos()), m, c.Text)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGoModHasNoRequire fails when go.mod requires any module: the module
// has no dependencies (the parser is vendored under internal/front/syntax).
func TestGoModHasNoRequire(t *testing.T) {
	// The detector itself, on go.mod texts that do require something.
	for _, src := range []string{"module m\n\nrequire example.com/x v1.0.0\n", "module m\nrequire (\n\texample.com/x v1.0.0\n)\n",
		"module m\nrequire(\n\tx v1\n)\n", "module m\nrequire(x v1)\n"} {
		if len(goModRequires(src)) == 0 {
			t.Fatalf("a require is not detected in %q", src)
		}
	}
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range goModRequires(string(b)) {
		t.Errorf("go.mod: %s (the module has no dependencies)", line)
	}
}

// goModRequires lists the lines of a go.mod text that open a require
// directive (single or block form).
func goModRequires(src string) []string {
	var out []string
	for i, line := range strings.Split(src, "\n") {
		if f := strings.Fields(line); len(f) > 0 && (f[0] == "require" || strings.HasPrefix(f[0], "require(")) {
			out = append(out, strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return out
}
