package lsp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points the user settings file at a file of its own, so no test
// reads the real user configuration or the real $PATH: the file names a PATH
// profile directory holding a few programs.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bash-lsp-lsp-test")
	if err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		panic(err)
	}
	for _, n := range []string{"ls", "cat", "grep", "rm", "mkdir"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			panic(err)
		}
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[path]\nprofile = [\""+bin+"\"]\n"), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", cfg)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
