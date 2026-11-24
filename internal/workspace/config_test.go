package workspace

import (
	"fmt"
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

// A repository controls its .gitignore files, so the rules compiled and the
// evaluations made in one walk are bounded by counts. Past the bound rules
// are not applied (files are included, not hidden) and a limits line says so.
func TestGitignoreWorkIsBounded(t *testing.T) {
	t.Run("rules", func(t *testing.T) {
		root := t.TempDir()
		var gi strings.Builder
		gi.WriteString("early.sh\n")
		for i := 0; i < maxIgnoreRules; i++ {
			fmt.Fprintf(&gi, "pad%d.sh\n", i)
		}
		gi.WriteString("late.sh\n")
		write(t, filepath.Join(root, ".gitignore"), gi.String())
		write(t, filepath.Join(root, "early.sh"), "echo\n")
		write(t, filepath.Join(root, "late.sh"), "echo\n")
		w := open(t, root)
		if got := rels(w); !reflect.DeepEqual(got, []string{"late.sh"}) {
			t.Fatalf("files %v: rules before the limit apply, the one after does not", got)
		}
		hit := w.Report().LimitsHit
		if len(hit) != 1 || !strings.Contains(hit[0], "ignore rule limit of") {
			t.Fatalf("LimitsHit %q", hit)
		}
	})
	t.Run("evaluations", func(t *testing.T) {
		root := t.TempDir()
		var gi strings.Builder
		for i := 0; i < maxIgnoreRules/2; i++ {
			fmt.Fprintf(&gi, "pad%d.sh\n", i)
		}
		write(t, filepath.Join(root, ".gitignore"), gi.String())
		// Nested sets add their own rules; the walk cannot do more than the
		// budget however many files there are.
		files := 2*maxIgnoreEvals/(maxIgnoreRules/2) + 20
		for i := 0; i < files; i++ {
			write(t, filepath.Join(root, fmt.Sprintf("f%d.sh", i)), "echo\n")
		}
		w := open(t, root)
		if n := len(rels(w)); n != files {
			t.Fatalf("%d of %d files", n, files)
		}
		w.mu.Lock()
		evals := w.ignBudget.Evals()
		w.mu.Unlock()
		if evals > maxIgnoreEvals {
			t.Fatalf("%d evaluations over the budget of %d", evals, maxIgnoreEvals)
		}
		hit := w.Report().LimitsHit
		if len(hit) != 1 || !strings.Contains(hit[0], "ignore evaluation limit of") {
			t.Fatalf("LimitsHit %q", hit)
		}
	})
}
