//go:build unix

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// loadWithin runs Load and fails the test if it does not return in time.
func loadWithin(t *testing.T, root string) Settings {
	t.Helper()
	done := make(chan Settings, 1)
	go func() { done <- Load(root, nil) }()
	select {
	case s := <-done:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("Load blocked on a non-regular settings file")
		return Settings{}
	}
}

// unblock releases a reader stuck on the fifo, so a failing run does not leak it.
func unblock(t *testing.T, fifo string) {
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
}

func TestLoadRepoFifoDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, ".bash-lsp.toml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	unblock(t, fifo)
	s := loadWithin(t, root)
	if !hasWarning(s, ".bash-lsp.toml", "not a regular file") || len(s.Files) != 0 {
		t.Fatalf("%q %v", s.Warnings, s.Files)
	}
}

func TestLoadUserFifoDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "c.toml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	unblock(t, fifo)
	t.Setenv("BASH_LSP_CONFIG", fifo)
	s := loadWithin(t, t.TempDir())
	if !hasWarning(s, "not a regular file") || len(s.Files) != 0 {
		t.Fatalf("%q %v", s.Warnings, s.Files)
	}
}

func TestLoadSymlinkToFifoDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	unblock(t, fifo)
	if err := os.Symlink(fifo, filepath.Join(root, ".bash-lsp.toml")); err != nil {
		t.Skip(err)
	}
	s := loadWithin(t, root)
	if !hasWarning(s, "not a regular file") {
		t.Fatalf("%q", s.Warnings)
	}
}
