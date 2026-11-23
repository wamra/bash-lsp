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
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Logf("symlink unsupported: %v", err)
	}
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
