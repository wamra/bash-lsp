//go:build unix

package format

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A .editorconfig that is a FIFO must not hang the lookup, and one that links
// out of the workspace is not read.
func TestOptionsForEditorconfigIsRepoControlled(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "a.sh")

	if err := syscall.Mkfifo(filepath.Join(root, ".editorconfig"), 0o600); err != nil {
		t.Skip("no mkfifo")
	}
	done := make(chan Options, 1)
	go func() { done <- OptionsFor(file, root, Settings{}, nil) }()
	select {
	case got := <-done:
		if got != (Options{}) {
			t.Fatalf("FIFO: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OptionsFor hangs on a FIFO .editorconfig")
	}
	os.Remove(filepath.Join(root, ".editorconfig"))

	outside := filepath.Join(dir, "outside.ec")
	if err := os.WriteFile(outside, []byte("[*]\nindent_style=space\nindent_size=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".editorconfig")); err != nil {
		t.Skip("no symlinks")
	}
	if got := OptionsFor(file, root, Settings{}, nil); got != (Options{}) {
		t.Fatalf("a link out of the workspace was followed: %+v", got)
	}
	os.Remove(filepath.Join(root, ".editorconfig"))

	// A link that stays inside the workspace is read.
	if err := os.WriteFile(filepath.Join(root, "shared.ec"), []byte("[*]\nindent_style=space\nindent_size=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared.ec", filepath.Join(root, ".editorconfig")); err != nil {
		t.Fatal(err)
	}
	if got := OptionsFor(file, root, Settings{}, nil); got != (Options{Indent: 2}) {
		t.Fatalf("a link inside the workspace: %+v", got)
	}
	// A directory named .editorconfig is not a file.
	os.Remove(filepath.Join(root, ".editorconfig"))
	if err := os.Mkdir(filepath.Join(root, ".editorconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := OptionsFor(file, root, Settings{}, nil); got != (Options{}) {
		t.Fatalf("a directory: %+v", got)
	}
}
