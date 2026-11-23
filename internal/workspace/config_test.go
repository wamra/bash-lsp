package workspace

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// review I2: .gitignore is read through readConfig: at most maxIgnoreBytes,
// and a symlink only when it resolves to a regular file inside the root.
func TestGitignoreReadIsBounded(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, ".gitignore"), "ignored/\n"+strings.Repeat("#\n", maxIgnoreBytes/2+1))
		write(t, filepath.Join(root, "ignored", "x.sh"), "echo\n")
		if got := rels(open(t, root)); !reflect.DeepEqual(got, []string{"ignored/x.sh"}) {
			t.Fatalf("an oversize .gitignore was applied: %v", got)
		}
	})
	t.Run("symlink outside the root", func(t *testing.T) {
		outside := t.TempDir()
		write(t, filepath.Join(outside, "gi"), "x.sh\n")
		root := t.TempDir()
		write(t, filepath.Join(root, "x.sh"), "echo\n")
		symlink(t, filepath.Join(outside, "gi"), filepath.Join(root, ".gitignore"))
		w := open(t, root)
		if got := rels(w); !reflect.DeepEqual(got, []string{"x.sh"}) {
			t.Fatalf("a .gitignore outside the root was applied: %v", got)
		}
		w.Refresh(filepath.Join(w.Root(), "x.sh"))
		if got := rels(w); !reflect.DeepEqual(got, []string{"x.sh"}) {
			t.Fatalf("Refresh applied a .gitignore outside the root: %v", got)
		}
	})
	t.Run("symlink inside the root", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "conf", "gi"), "x.sh\n")
		write(t, filepath.Join(root, "x.sh"), "echo\n")
		write(t, filepath.Join(root, "y.sh"), "echo\n")
		symlink(t, filepath.Join(root, "conf", "gi"), filepath.Join(root, ".gitignore"))
		if got := rels(open(t, root)); !reflect.DeepEqual(got, []string{"y.sh"}) {
			t.Fatalf("a .gitignore linked inside the root was not applied: %v", got)
		}
	})
}
