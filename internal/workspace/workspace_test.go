package workspace

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
)

func open(t *testing.T, root string) *Workspace {
	t.Helper()
	w, err := Open(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func copyCorpus(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "corpus")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rels(w *Workspace) []string {
	var out []string
	for _, p := range w.Snapshot().Paths() {
		r, _ := filepath.Rel(w.Root(), p)
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

func TestOpenCorpus(t *testing.T) {
	w := open(t, copyCorpus(t))
	if n := len(w.Snapshot().Paths()); n != 9 {
		t.Fatalf("%d files", n)
	}
	if w.Snapshot().Generation() != 1 {
		t.Fatal(w.Snapshot().Generation())
	}
	r := w.Report()
	if r.Files != 9 || r.Skipped != 0 || len(r.LimitsHit) != 0 {
		t.Fatalf("%+v", r)
	}
	if r.Dialects["bash"] == 0 {
		t.Fatalf("dialects %v", r.Dialects)
	}
}

func TestDiscoveryRules(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "#!/bin/bash\necho a\n")
	write(t, filepath.Join(root, "noext"), "#!/usr/bin/env bash\necho n\n")
	write(t, filepath.Join(root, "notshell"), "#!/usr/bin/python3\nprint(1)\n")
	write(t, filepath.Join(root, "plain"), "echo no shebang\n")
	write(t, filepath.Join(root, "big.sh"), "#!/bin/bash\n"+strings.Repeat("# pad\n", 3<<20/6))
	write(t, filepath.Join(root, "z.zsh"), "echo z\n")
	write(t, filepath.Join(root, ".gitignore"), "ignored/\n")
	write(t, filepath.Join(root, "ignored", "x.sh"), "echo x\n")
	write(t, filepath.Join(root, "node_modules", "y.sh"), "echo y\n")
	write(t, filepath.Join(root, ".git", "h.sh"), "echo h\n")
	write(t, filepath.Join(root, ".Wammy", "w.sh"), "echo w\n")
	check := func(t *testing.T) {
		t.Helper()
		w := open(t, root)
		if got, want := rels(w), []string{"a.sh", "noext"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("paths %v, want %v", got, want)
		}
		r := w.Report()
		if r.Skipped != 2 || r.Files != 2 {
			t.Fatalf("%+v", r)
		}
		if len(r.LimitsHit) != 1 || !strings.Contains(r.LimitsHit[0], "big.sh") {
			t.Fatalf("LimitsHit %v", r.LimitsHit)
		}
	}
	check(t)
	// A directory symlink leading out of the root is not followed. Skipped,
	// visibly, where links cannot be made (Windows without the privilege).
	t.Run("symlink out of root", func(t *testing.T) {
		outside := t.TempDir()
		write(t, filepath.Join(outside, "out.sh"), "echo out\n")
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		check(t)
	})
}

func TestDiscoverySymlinks(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real", "a.sh"), "echo a\n")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink("..", filepath.Join(root, "real", "up")); err != nil {
		t.Fatal(err)
	}
	w := open(t, root)
	// "alias" sorts before "real", so the directory is visited once, through it.
	if got := rels(w); len(got) != 1 {
		t.Fatalf("directory visited twice: %v", got)
	}
}

func TestLimits(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.sh", "b.sh", "c.sh"} {
		write(t, filepath.Join(root, n), "echo\n")
	}
	w, err := Open(root, Limits{MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	r := w.Report()
	if r.Files != 2 || r.Skipped != 1 || len(r.LimitsHit) != 1 || !strings.Contains(r.LimitsHit[0], "2") {
		t.Fatalf("%+v", r)
	}
}

func TestOverlayChangesAnswers(t *testing.T) {
	w := open(t, copyCorpus(t))
	p := filepath.Join(w.Root(), "lib/util.sh")
	g := w.Snapshot().Generation()
	w.SetOverlay(p, 2, []byte("#!/usr/bin/env bash\nrenamed() { :; }\n"))
	s := w.Snapshot()
	if s.Generation() != g+1 {
		t.Fatal(s.Generation())
	}
	if len(s.WorkspaceSymbols("build_release", 5)) != 1 {
		t.Fatal("only orphan.sh should still define it")
	}
	old := w.Snapshot()
	w.CloseOverlay(p)
	if len(w.Snapshot().WorkspaceSymbols("build_release", 5)) != 2 {
		t.Fatal("disk content not restored")
	}
	if len(old.WorkspaceSymbols("renamed", 5)) != 1 {
		t.Fatal("earlier snapshot must stay unchanged")
	}
}

func TestOverlayNotOnDisk(t *testing.T) {
	w := open(t, copyCorpus(t))
	p := filepath.Join(w.Root(), "scratch.sh")
	w.SetOverlay(p, 1, []byte("fresh() { :; }\n"))
	if len(w.Snapshot().WorkspaceSymbols("fresh", 5)) != 1 {
		t.Fatal("overlay-only file missing")
	}
	w.CloseOverlay(p)
	if len(w.Snapshot().WorkspaceSymbols("fresh", 5)) != 0 {
		t.Fatal("file not removed on close")
	}
}

func TestRefreshCreateDelete(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "user.sh"), "#!/usr/bin/env bash\nsource ./new.sh\n")
	w := open(t, root)
	user := filepath.Join(w.Root(), "user.sh")
	newp := filepath.Join(w.Root(), "new.sh")
	status := func() analysis.SourceStatus {
		e := w.Snapshot().SourceEdges(user)
		if len(e) != 1 {
			t.Fatalf("edges %v", e)
		}
		return e[0].Status
	}
	if status() != analysis.SourceUnresolved {
		t.Fatal("expected unresolved before creation")
	}
	write(t, newp, "#!/usr/bin/env bash\nhello() { :; }\n")
	g := w.Snapshot().Generation()
	w.Refresh(newp)
	if w.Snapshot().Generation() != g+1 || status() != analysis.SourceResolved || len(w.Snapshot().Paths()) != 2 {
		t.Fatal("created file not picked up")
	}
	if err := os.Remove(newp); err != nil {
		t.Fatal(err)
	}
	w.Refresh(newp)
	if status() != analysis.SourceUnresolved || len(w.Snapshot().Paths()) != 1 {
		t.Fatal("deleted file still present")
	}
}

func TestRefreshReconcileAll(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "one() { :; }\n")
	write(t, filepath.Join(root, "b.sh"), "two() { :; }\n")
	w := open(t, root)
	g := w.Snapshot().Generation()
	w.Refresh()
	if w.Snapshot().Generation() != g {
		t.Fatal("no-op reconcile must not bump the generation")
	}
	write(t, filepath.Join(root, "a.sh"), "uno() { :; }\n") // same size, content differs
	write(t, filepath.Join(root, "c.sh"), "three() { :; }\n")
	os.Remove(filepath.Join(root, "b.sh"))
	w.Refresh()
	s := w.Snapshot()
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "c.sh"}) {
		t.Fatal(got)
	}
	if len(s.WorkspaceSymbols("uno", 5)) != 1 || len(s.WorkspaceSymbols("one", 5)) != 0 || len(s.WorkspaceSymbols("two", 5)) != 0 {
		t.Fatal("reconcile did not apply changes")
	}
}

func TestShellcheckrcSourcePath(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".shellcheckrc"), "# comment\nsource-path=SCRIPTDIR/../lib\n")
	write(t, filepath.Join(root, "lib", "util.sh"), "util() { :; }\n")
	write(t, filepath.Join(root, "bin", "run.sh"), "#!/bin/bash\nsource util.sh\nutil\n")
	w := open(t, root)
	run := filepath.Join(w.Root(), "bin", "run.sh")
	e := w.Snapshot().SourceEdges(run)
	if len(e) != 1 || e[0].Status != analysis.SourceResolved {
		t.Fatalf("source via .shellcheckrc not resolved: %+v", e)
	}
	got := w.searchPaths(run)
	if want := []string{filepath.Join(w.Root(), "lib")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	// A .shellcheckrc above the root is never read.
	outer := t.TempDir()
	write(t, filepath.Join(outer, ".shellcheckrc"), "source-path=/nowhere\n")
	write(t, filepath.Join(outer, "ws", "a.sh"), "echo\n")
	w2 := open(t, filepath.Join(outer, "ws"))
	if got := w2.searchPaths(filepath.Join(w2.Root(), "a.sh")); len(got) != 0 {
		t.Fatalf("rc above root read: %v", got)
	}
}

func TestKeyCaseFolding(t *testing.T) {
	if keyFor(`C:\A\B.sh`, true) != keyFor(`c:\a\b.SH`, true) {
		t.Fatal("windows keys must fold case")
	}
	if keyFor("/A/b.sh", false) == keyFor("/a/b.sh", false) {
		t.Fatal("posix keys are case-sensitive")
	}
}

func TestConcurrentUse(t *testing.T) {
	w := open(t, copyCorpus(t))
	p := filepath.Join(w.Root(), "lib/util.sh")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if (i+j)%2 == 0 {
					text := bytes.Repeat([]byte("x=1\n"), j+1)
					w.SetOverlay(p, j, append([]byte("#!/bin/bash\nbuild_release() { :; }\n"), text...))
				} else {
					w.Snapshot().References(p, 0, true)
					w.Report()
				}
			}
		}(i)
	}
	wg.Wait()
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
}

func TestSymlinksOutOfRootNeverListed(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "out.sh"), "outside() { :; }\n")
	root := t.TempDir()
	write(t, filepath.Join(root, "real.sh"), "real() { :; }\n")
	symlink(t, filepath.Join(outside, "out.sh"), filepath.Join(root, "link.sh"))
	symlink(t, outside, filepath.Join(root, "linkdir"))
	symlink(t, "real.sh", filepath.Join(root, "dup.sh")) // in-root file link: a duplicate
	w := open(t, root)
	want := []string{"real.sh"}
	if got := rels(w); !reflect.DeepEqual(got, want) {
		t.Fatalf("Open listed %v", got)
	}
	g := w.Snapshot().Generation()
	for _, p := range []string{"link.sh", "linkdir/out.sh", "dup.sh"} {
		w.Refresh(filepath.Join(w.Root(), p))
		if got := rels(w); !reflect.DeepEqual(got, want) {
			t.Fatalf("Refresh(%s) listed %v", p, got)
		}
	}
	if w.Snapshot().Generation() != g {
		t.Fatal("rejected refreshes must not rebuild")
	}
	w.Refresh() // full reconcile agrees
	if got := rels(w); !reflect.DeepEqual(got, want) {
		t.Fatalf("Refresh() listed %v", got)
	}
}

func TestOverlayThroughSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "real", "lib", "a.sh"), "old() { :; }\n")
	symlink(t, filepath.Join(base, "real"), filepath.Join(base, "link"))
	w := open(t, filepath.Join(base, "link"))
	viaLink := filepath.Join(base, "link", "lib", "a.sh")
	w.SetOverlay(viaLink, 1, []byte("new() { :; }\n"))
	s := w.Snapshot()
	if len(s.Paths()) != 1 || len(s.WorkspaceSymbols("new", 5)) != 1 || len(s.WorkspaceSymbols("old", 5)) != 0 {
		t.Fatalf("overlay did not replace its disk entry: %v", s.Paths())
	}
	w.CloseOverlay(viaLink)
	if len(w.Snapshot().WorkspaceSymbols("old", 5)) != 1 || len(w.Snapshot().Paths()) != 1 {
		t.Fatal("close through the unresolved spelling failed")
	}
}

func TestOverlayThroughDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real", "a.sh"), "old() { :; }\n")
	symlink(t, "real", filepath.Join(root, "alias")) // sorts first: the walk names it alias/a.sh
	w := open(t, root)
	if got := rels(w); !reflect.DeepEqual(got, []string{"alias/a.sh"}) {
		t.Fatal(got)
	}
	for _, p := range []string{"real/a.sh", "alias/a.sh"} {
		w.SetOverlay(filepath.Join(w.Root(), p), 1, []byte("new() { :; }\n"))
		s := w.Snapshot()
		if len(s.Paths()) != 1 || len(s.WorkspaceSymbols("new", 5)) != 1 || len(s.WorkspaceSymbols("old", 5)) != 0 {
			t.Fatalf("overlay %s duplicated the file: %v", p, s.Paths())
		}
		w.CloseOverlay(filepath.Join(w.Root(), "real", "a.sh"))
		if len(w.Snapshot().WorkspaceSymbols("old", 5)) != 1 {
			t.Fatalf("close of %s via real path did not drop overlay", p)
		}
	}
	// Refresh by the real spelling updates the aliased entry.
	write(t, filepath.Join(root, "real", "a.sh"), "newer() { :; }\n")
	w.Refresh(filepath.Join(w.Root(), "real", "a.sh"))
	if len(w.Snapshot().WorkspaceSymbols("newer", 5)) != 1 || len(w.Snapshot().Paths()) != 1 {
		t.Fatal("Refresh via real path missed the aliased entry")
	}
}

func TestCanonMatchesOverlayKey(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real", "a.sh"), "old() { :; }\n")
	symlink(t, "real", filepath.Join(root, "alias"))
	w := open(t, root)
	for _, p := range []string{"real/a.sh", "alias/a.sh"} {
		in := filepath.Join(root, p)
		w.SetOverlay(in, 1, []byte("new() { :; }\n"))
		c := w.Canon(in)
		if w.Snapshot().Facts(c) == nil {
			t.Fatalf("Canon(%s) = %s is not a snapshot key: %v", p, c, w.Snapshot().Paths())
		}
	}
	if a, b := w.Canon(filepath.Join(root, "real", "a.sh")), w.Canon(filepath.Join(root, "alias", "a.sh")); a != b {
		t.Fatalf("aliases disagree: %s vs %s", a, b)
	}
}

func TestShellcheckrcEditTakesEffect(t *testing.T) {
	for _, mode := range []string{"path", "all"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			write(t, filepath.Join(root, "lib", "util.sh"), "util() { :; }\n")
			write(t, filepath.Join(root, "bin", "run.sh"), "#!/bin/bash\nsource util.sh\n")
			w := open(t, root)
			run := filepath.Join(w.Root(), "bin", "run.sh")
			st := func() analysis.SourceStatus { return w.Snapshot().SourceEdges(run)[0].Status }
			if st() != analysis.SourceUnresolved {
				t.Fatal("should start unresolved")
			}
			rc := filepath.Join(w.Root(), ".shellcheckrc")
			write(t, rc, "source-path=SCRIPTDIR/../lib\n")
			g := w.Snapshot().Generation()
			if mode == "path" {
				w.Refresh(rc)
			} else {
				w.Refresh()
			}
			if st() != analysis.SourceResolved || w.Snapshot().Generation() != g+1 {
				t.Fatalf("rc creation ignored (gen %d -> %d)", g, w.Snapshot().Generation())
			}
			write(t, rc, "# emptied, different size\n")
			if mode == "path" {
				w.Refresh(rc)
			} else {
				w.Refresh()
			}
			if st() != analysis.SourceUnresolved {
				t.Fatal("rc edit ignored")
			}
		})
	}
}

func TestShellcheckrcSafety(t *testing.T) {
	root := t.TempDir()
	big := "source-path=/x\n" + strings.Repeat("# pad\n", maxRCBytes/6+1)
	write(t, filepath.Join(root, ".shellcheckrc"), big)
	write(t, filepath.Join(root, "a.sh"), "echo\n")
	w := open(t, root)
	if got := w.searchPaths(filepath.Join(w.Root(), "a.sh")); len(got) != 0 {
		t.Fatalf("oversize rc honoured: %v", got)
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "rc"), "source-path=/evil\n")
	root2 := t.TempDir()
	write(t, filepath.Join(root2, "a.sh"), "echo\n")
	symlink(t, filepath.Join(outside, "rc"), filepath.Join(root2, ".shellcheckrc"))
	w2 := open(t, root2)
	if got := w2.searchPaths(filepath.Join(w2.Root(), "a.sh")); len(got) != 0 {
		t.Fatalf("rc symlinked outside root honoured: %v", got)
	}
}

func TestRefreshPathsEnforceLimits(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "echo a\n")
	write(t, filepath.Join(root, "b.sh"), "echo b\n")
	w, err := Open(root, Limits{MaxFiles: 2, MaxFileBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	c := filepath.Join(w.Root(), "c.sh")
	write(t, c, "echo c\n")
	w.Refresh(c)
	r := w.Report()
	if len(w.Snapshot().Paths()) != 2 || r.Skipped != 1 || len(r.LimitsHit) != 1 {
		t.Fatalf("MaxFiles not enforced by Refresh(path): %v %+v", rels(w), r)
	}
	w.Refresh(c) // repeat must not double count
	if w.Report().Skipped != 1 {
		t.Fatal("double counted")
	}
	os.Remove(c)
	w.Refresh(c)
	if r := w.Report(); r.Skipped != 0 || len(r.LimitsHit) != 0 {
		t.Fatalf("stale skip: %+v", r)
	}
	a := filepath.Join(w.Root(), "a.sh")
	write(t, a, "#!/bin/bash\n"+strings.Repeat("# pad\n", 40))
	w.Refresh(a)
	r = w.Report()
	if len(w.Snapshot().Paths()) != 1 || r.Skipped != 1 || len(r.LimitsHit) != 1 || !strings.Contains(r.LimitsHit[0], "a.sh") {
		t.Fatalf("oversize not dropped: %v %+v", rels(w), r)
	}
	write(t, a, "echo a\n")
	w.Refresh(a)
	if r := w.Report(); r.Skipped != 0 || len(w.Snapshot().Paths()) != 2 {
		t.Fatalf("shrunk file not restored: %+v", r)
	}
}

func TestReadBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	write(t, p, strings.Repeat("x", 50))
	if d, big, err := readBounded(p, 50); err != nil || big || len(d) != 50 {
		t.Fatal(len(d), big, err)
	}
	if d, big, err := readBounded(p, 49); err != nil || !big || d != nil {
		t.Fatal(len(d), big, err)
	}
}

// review C1 / T9: an overlay over MaxFileBytes is skipped like a disk file of
// that size, and hides the disk entry while it lasts.
func TestOverlayOverSizeLimit(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.sh")
	write(t, p, "on_disk() { :; }\n")
	w, err := Open(root, Limits{MaxFileBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	w.SetOverlay(p, 1, []byte("in_buffer() { :; }\n"+strings.Repeat("# pad\n", 20)))
	s := w.Snapshot()
	ff := s.Facts(w.Canon(p))
	if ff == nil || ff.File == nil || !strings.Contains(ff.File.Skipped, "a.sh: 139 bytes exceeds the 64 byte file limit") {
		t.Fatalf("facts %+v", ff)
	}
	if len(s.WorkspaceSymbols("in_buffer", 5))+len(s.WorkspaceSymbols("on_disk", 5)) != 0 {
		t.Fatal("an oversize overlay must not be parsed, nor show the disk content")
	}
	w.CloseOverlay(p)
	if len(w.Snapshot().WorkspaceSymbols("on_disk", 5)) != 1 {
		t.Fatal("disk content not back after close")
	}
}

// review I1: closing a document re-reads its file. It used to revert to the
// disk entry read at start, and a file created during the session vanished.
func TestCloseOverlayRereadsDisk(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.sh"), filepath.Join(root, "b.sh")
	write(t, a, "old_fn() { :; }\n")
	w := open(t, root)
	count := func(name string) int { return len(w.Snapshot().WorkspaceSymbols(name, 5)) }

	w.SetOverlay(a, 1, []byte("new_fn() { :; }\n"))
	write(t, a, "new_fn() { :; }\n") // the editor saves
	w.CloseOverlay(a)
	if count("new_fn") != 1 || count("old_fn") != 0 {
		t.Fatalf("after close: new_fn %d, old_fn %d", count("new_fn"), count("old_fn"))
	}

	w.SetOverlay(b, 1, []byte("b_fn() { :; }\n"))
	write(t, b, "b_fn() { :; }\n") // created during the session
	w.CloseOverlay(b)
	if count("b_fn") != 1 {
		t.Fatal("a file created during the session left the index on close")
	}

	w.SetOverlay(a, 2, []byte("new_fn() { :; }\n"))
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	w.CloseOverlay(a)
	if count("new_fn") != 0 {
		t.Fatal("a file deleted while open is still indexed after close")
	}
}
