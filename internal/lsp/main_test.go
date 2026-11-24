package lsp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the user settings file at a path that does not exist, so
// no test reads the real user configuration.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bash-lsp-lsp-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", filepath.Join(dir, "none.toml"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
