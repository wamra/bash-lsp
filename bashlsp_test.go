package bashlsp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp"
)

func TestLoad(t *testing.T) {
	s, err := bashlsp.Load(filepath.Join("testdata", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	got := s.WorkspaceSymbols("build_release", 10)
	if len(got) == 0 || got[0].Name != "build_release" {
		t.Fatalf("WorkspaceSymbols = %+v", got)
	}
	if p := filepath.ToSlash(got[0].Path); !strings.HasSuffix(p, "/testdata/corpus/lib/util.sh") {
		t.Fatalf("build_release found in %s, want lib/util.sh", p)
	}
}

func TestLoadMissingRootIsAnError(t *testing.T) {
	if _, err := bashlsp.Load(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want an error for a missing root")
	}
}

func TestLoadBrokenFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte("if then fi ((\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bashlsp.Load(dir); err != nil {
		t.Fatal(err)
	}
}
