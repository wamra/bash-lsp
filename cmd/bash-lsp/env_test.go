package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the user settings file at a path that does not exist, so
// no test reads the real user configuration. A copy of the test binary named
// code-index is the fake program of the search tests.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "code-index" { // this binary, copied as the fake code-index (search_test.go)
		runFakeCodeIndex()
	}
	dir, err := os.MkdirTemp("", "bash-lsp-cmd-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", filepath.Join(dir, "none.toml"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
