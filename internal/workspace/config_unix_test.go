//go:build !windows

package workspace

import (
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// openWithin opens root, failing the test instead of hanging when Open blocks.
func openWithin(t *testing.T, root string) *Workspace {
	t.Helper()
	done := make(chan *Workspace, 1)
	go func() { done <- open(t, root) }()
	select {
	case w := <-done:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("Open blocked")
		return nil
	}
}

// review I2: a FIFO named .gitignore or .shellcheckrc blocked the open(2)
// forever, and every request waits for the first build. Neither is opened.
func TestConfigFIFOsAreNotOpened(t *testing.T) {
	for _, name := range []string{".gitignore", ".shellcheckrc"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, filepath.Join(root, "a.sh"), "echo\n")
			if err := syscall.Mkfifo(filepath.Join(root, name), 0o644); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			w := openWithin(t, root)
			if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
				t.Fatalf("paths %v", got)
			}
			done := make(chan struct{})
			go func() { w.Refresh(filepath.Join(w.Root(), "a.sh")); w.Refresh(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Refresh blocked")
			}
		})
	}
}
