package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---- Refresh as the target of watched-file events ----

// An event for a file with an open overlay is ignored: the editor's text wins
// until CloseOverlay, which re-reads the file. No rebuild for the ignored path.
func TestRefreshSkipsOverlaidPath(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "disk_one() { :; }\n")
	w := open(t, root)
	a := filepath.Join(w.Root(), "a.sh")
	w.SetOverlay(a, 1, []byte("buffer_fn() { :; }\n"))
	write(t, a, "disk_two() { :; }\n")
	g := w.Snapshot().Generation()
	w.Refresh(a)
	if w.Snapshot().Generation() != g {
		t.Fatal("an event for an overlaid path must not rebuild")
	}
	if len(w.Snapshot().WorkspaceSymbols("buffer_fn", 5)) != 1 {
		t.Fatal("overlay lost")
	}
	if err := os.Remove(a); err != nil { // delete of an open file keeps the overlay
		t.Fatal(err)
	}
	w.Refresh(a)
	if len(w.Snapshot().WorkspaceSymbols("buffer_fn", 5)) != 1 {
		t.Fatal("delete of an open file dropped the overlay")
	}
	write(t, a, "disk_two() { :; }\n")
	w.CloseOverlay(a)
	s := w.Snapshot()
	if len(s.WorkspaceSymbols("disk_two", 5)) != 1 || len(s.WorkspaceSymbols("buffer_fn", 5)) != 0 {
		t.Fatal("CloseOverlay must re-read the disk")
	}
}

// A created directory, a deleted directory and a directory renamed (an event
// pair: delete of the old name, create of the new) end in the state a full
// walk gives, though the events name only the directories.
func TestRefreshDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	w := open(t, root)
	sub := filepath.Join(w.Root(), "sub")
	write(t, filepath.Join(sub, "deep", "x.sh"), "x() { :; }\n")
	w.Refresh(sub)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "sub/deep/x.sh"}) {
		t.Fatalf("after create of a directory: %v", got)
	}
	sub2 := filepath.Join(w.Root(), "sub2")
	if err := os.Rename(sub, sub2); err != nil {
		t.Fatal(err)
	}
	w.Refresh(sub, sub2)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "sub2/deep/x.sh"}) {
		t.Fatalf("after rename of a directory: %v", got)
	}
	if err := os.RemoveAll(sub2); err != nil {
		t.Fatal(err)
	}
	w.Refresh(sub2)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
		t.Fatalf("after delete of a directory: %v", got)
	}
	// A file renamed: delete + create in one batch, in either order.
	b := filepath.Join(w.Root(), "b.sh")
	if err := os.Rename(filepath.Join(w.Root(), "a.sh"), b); err != nil {
		t.Fatal(err)
	}
	g := w.Snapshot().Generation()
	w.Refresh(b, filepath.Join(w.Root(), "a.sh"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"b.sh"}) {
		t.Fatalf("after rename of a file: %v", got)
	}
	if w.Snapshot().Generation() != g+1 {
		t.Fatal("one batch must rebuild once")
	}
}

// A .gitignore changes what the walk lists: an event for it applies it.
func TestRefreshGitignore(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, "gen", "b.sh"), "b() { :; }\n")
	w := open(t, root)
	write(t, filepath.Join(root, ".gitignore"), "gen/\n")
	w.Refresh(filepath.Join(w.Root(), ".gitignore"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
		t.Fatalf("after the .gitignore event: %v", got)
	}
}

// Events are untrusted: paths outside the root, through `..`, through a
// symlink out of the root, under a skipped directory, or absurdly long are
// ignored without a rebuild, and nothing outside the root is read.
func TestRefreshHostilePaths(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "out.sh"), "outside() { :; }\n")
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, ".git", "hooks", "pre-commit.sh"), "hook() { :; }\n")
	symlink(t, outside, filepath.Join(root, "linkdir"))
	symlink(t, filepath.Join(outside, "out.sh"), filepath.Join(root, "link.sh"))
	w := open(t, root)
	g := w.Snapshot().Generation()
	r := w.Root()
	w.Refresh(
		filepath.Join(outside, "out.sh"),
		r+"/../"+filepath.Base(outside)+"/out.sh",
		filepath.Join(r, "linkdir", "out.sh"),
		filepath.Join(r, "link.sh"),
		filepath.Join(r, ".git", "hooks", "pre-commit.sh"),
		filepath.Join(r, strings.Repeat("x", 2<<20)),
		"relative/../../../etc/hosts",
		"",
	)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
		t.Fatalf("hostile refresh listed %v", got)
	}
	if w.Snapshot().Generation() != g {
		t.Fatal("ignored paths must not rebuild")
	}
}

// ---- nested roots: a subtree excluded ----

func TestExcludedSubtree(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, "inner", "b.sh"), "b() { :; }\n")
	write(t, filepath.Join(root, "inner2", "c.sh"), "c() { :; }\n")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	// An exclusion that is not strictly inside the root (an enclosing root,
	// the root itself, an unrelated directory) is ignored.
	w, err := OpenExcluding(root, Limits{}, []string{filepath.Join(real, "inner"), filepath.Dir(real), real, t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "inner2/c.sh"}) {
		t.Fatalf("excluded subtree listed: %v", got)
	}
	b := filepath.Join(real, "inner", "b.sh")
	if w.Owns(b) || !w.Owns(filepath.Join(real, "a.sh")) || !w.Owns(filepath.Join(real, "inner2", "c.sh")) {
		t.Fatal("Owns disagrees with the exclusion")
	}
	if !w.Contains(b) || w.Contains(filepath.Dir(real)) {
		t.Fatal("Contains ignores exclusions and stops at the root")
	}
	w.Refresh(b)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "inner2/c.sh"}) {
		t.Fatalf("Refresh listed an excluded file: %v", got)
	}
	w.SetExcluded(nil)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "inner/b.sh", "inner2/c.sh"}) {
		t.Fatalf("after dropping the exclusion: %v", got)
	}
	g := w.Snapshot().Generation()
	w.SetExcluded(nil)
	if w.Snapshot().Generation() != g {
		t.Fatal("an unchanged exclusion must not rebuild")
	}
	w.SetExcluded([]string{filepath.Join(real, "inner2")})
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "inner/b.sh"}) {
		t.Fatalf("after a new exclusion: %v", got)
	}
	if !w.IsRoot(real) || w.IsRoot(filepath.Join(real, "inner")) {
		t.Fatal("IsRoot")
	}
}

// ---- case-insensitive volumes ----

func TestFlipCase(t *testing.T) {
	for in, want := range map[string]string{"/Ab/c.SH": "/aB/C.sh", "/Été/ß": "/éTÉ/ß"} {
		if got, ok := flipCase(in); !ok || got != want {
			t.Errorf("flipCase(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := flipCase("/123/_-/"); ok {
		t.Error("a path with no letters has no case to flip")
	}
	if caseInsensitive("/") {
		t.Error("a root with no letters is treated as case-sensitive")
	}
	if caseInsensitive(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("a root that does not exist is treated as case-sensitive")
	}
}

// volumeFolds reports whether the volume of dir is case-insensitive, by the
// same create-free check Open uses.
func volumeFolds(t *testing.T, dir string) bool {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return caseInsensitive(real)
}

func TestCaseInsensitiveDetection(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "main.sh"), "m() { :; }\n")
	w := open(t, root)
	real := w.Root()
	flipped, _ := flipCase(real)
	a, errA := os.Stat(real)
	b, errB := os.Stat(flipped)
	want := windows || errA == nil && errB == nil && os.SameFile(a, b)
	if w.CaseInsensitive() != want {
		t.Fatalf("CaseInsensitive() = %v, the volume says %v", w.CaseInsensitive(), want)
	}
}

// The forced variant runs everywhere (Linux CI included): the folding logic
// with the detection replaced.
func TestCaseFoldedKeysForced(t *testing.T) {
	old := detectFold
	detectFold = func(string) bool { return true }
	defer func() { detectFold = old }()
	root := t.TempDir()
	write(t, filepath.Join(root, "Lib", "main.sh"), "main_fn() { :; }\n")
	w := open(t, root)
	checkFolded(t, w, root)
}

// On a real case-insensitive volume (the macOS default): MAIN.sh and main.sh
// name one file.
func TestCaseVariantPathsOnInsensitiveVolume(t *testing.T) {
	root := t.TempDir()
	if !volumeFolds(t, root) {
		t.Skip("case-sensitive volume")
	}
	write(t, filepath.Join(root, "Lib", "main.sh"), "main_fn() { :; }\n")
	w := open(t, root)
	if !w.CaseInsensitive() {
		t.Fatal("not detected")
	}
	checkFolded(t, w, root)
}

func checkFolded(t *testing.T, w *Workspace, root string) {
	t.Helper()
	disk := filepath.Join(w.Root(), "Lib", "main.sh")
	variant := filepath.Join(w.Root(), "LIB", "MAIN.sh")
	if got := w.Canon(variant); got != disk {
		t.Fatalf("Canon(%s) = %s, want the on-disk spelling %s", variant, got, disk)
	}
	w.SetOverlay(variant, 1, []byte("main_fn() { :; }\nother_fn() { :; }\n"))
	s := w.Snapshot()
	if got := rels(w); !reflect.DeepEqual(got, []string{"Lib/main.sh"}) {
		t.Fatalf("a case variant made a second file: %v", got)
	}
	if n := len(s.WorkspaceSymbols("main_fn", 5)); n != 1 {
		t.Fatalf("%d main_fn symbols", n)
	}
	w.CloseOverlay(variant)
	if got := rels(w); !reflect.DeepEqual(got, []string{"Lib/main.sh"}) {
		t.Fatalf("after close: %v", got)
	}
	if len(w.Snapshot().WorkspaceSymbols("other_fn", 5)) != 0 {
		t.Fatal("overlay not dropped through the variant spelling")
	}
	if !w.Owns(strings.ToUpper(disk)) && !windows {
		t.Fatal("Owns must fold on a folding workspace")
	}
}

// ---- concurrency: events while queries run ----

func TestConcurrentRefreshAndQueries(t *testing.T) {
	root := copyCorpus(t)
	w := open(t, root)
	lib := filepath.Join(w.Root(), "lib", "util.sh")
	extra := filepath.Join(w.Root(), "extra")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				switch (i + j) % 3 {
				case 0:
					p := filepath.Join(extra, "f.sh")
					if j%2 == 0 {
						_ = os.MkdirAll(extra, 0o755)
						_ = os.WriteFile(p, []byte("extra_fn() { :; }\n"), 0o644)
					} else {
						_ = os.RemoveAll(extra)
					}
					w.Refresh(p, extra, lib)
				case 1:
					w.Snapshot().WorkspaceSymbols("", 0)
					w.Owns(lib)
					w.Canon(lib)
				default:
					w.SetOverlay(lib, j, []byte("build_release() { :; }\n"))
					w.CloseOverlay(lib)
				}
			}
		}(i)
	}
	wg.Wait()
	_ = os.RemoveAll(extra)
	w.Refresh(extra)
	if got := w.Snapshot().WorkspaceSymbols("extra_fn", 5); len(got) != 0 {
		t.Fatalf("stale after the last refresh: %+v", got)
	}
}

// ---- full walks ----

// rewalked reports whether a full walk ran: b.sh was changed on disk before
// the call and only a walk (no event names it) picks the change up.
func rewalkProbe(t *testing.T, w *Workspace) func() bool {
	t.Helper()
	b := filepath.Join(w.Root(), "b.sh")
	write(t, b, "b_new() { :; }\n")
	g := w.Snapshot().Generation()
	return func() bool {
		return w.Snapshot().Generation() != g || len(w.Snapshot().WorkspaceSymbols("b_new", 5)) != 0
	}
}

// Events in skipped or ignored directories (every git operation makes them
// under a `**/*` watch) are dropped before the directory and .gitignore
// checks: no re-walk, no rebuild.
func TestRefreshSkippedDirectoriesDoNotRewalk(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, "b.sh"), "b_old() { :; }\n")
	write(t, filepath.Join(root, ".gitignore"), "build/\n")
	write(t, filepath.Join(root, ".git", "objects", "ab", "cd"), "x")
	write(t, filepath.Join(root, "node_modules", "p", ".gitignore"), "*\n")
	write(t, filepath.Join(root, "build", "sub", "x.sh"), "x() { :; }\n")
	w := open(t, root)
	r := w.Root()
	for _, p := range []string{
		filepath.Join(r, ".git"),
		filepath.Join(r, ".git", "objects", "ab"),
		filepath.Join(r, ".git", "objects", "gone"),
		filepath.Join(r, "node_modules", "p", ".gitignore"),
		filepath.Join(r, "node_modules", "p"),
		filepath.Join(r, "build", "sub"),
		filepath.Join(r, "build", "sub", ".gitignore"),
	} {
		rewalked := rewalkProbe(t, w)
		w.Refresh(p)
		if rewalked() {
			t.Fatalf("an event for %s re-walked the root", p)
		}
		write(t, filepath.Join(r, "b.sh"), "b_old() { :; }\n")
		w.Refresh(filepath.Join(r, "b.sh"))
	}
}

// A symlink to a directory outside the root is not a directory event: the
// walk does not follow it, so no re-walk.
func TestRefreshSymlinkOutIsNotADirectoryEvent(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	write(t, filepath.Join(root, "b.sh"), "b_old() { :; }\n")
	symlink(t, outside, filepath.Join(root, "link"))
	w := open(t, root)
	rewalked := rewalkProbe(t, w)
	w.Refresh(filepath.Join(w.Root(), "link"))
	if rewalked() {
		t.Fatal("an event for a symlink out of the root re-walked")
	}
}

// On a folding workspace skipDirs fold too: `.GIT` is `.git`.
func TestSkipDirsFoldForced(t *testing.T) {
	old := detectFold
	detectFold = func(string) bool { return true }
	defer func() { detectFold = old }()
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, ".GIT", "x.sh"), "x() { :; }\n")
	write(t, filepath.Join(root, "NODE_MODULES", "y.sh"), "y() { :; }\n")
	w := open(t, root)
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
		t.Fatalf("walk listed a skipped directory in another case: %v", got)
	}
	for _, p := range []string{".GIT/x.sh", ".git/x.sh", "NODE_MODULES/y.sh"} {
		w.Refresh(filepath.Join(w.Root(), p))
		if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
			t.Fatalf("Refresh(%s) listed %v", p, got)
		}
	}
}

// A case-only rename on a folding volume takes the new spelling, whichever
// order the delete and create events come in, and through a full walk.
func TestCaseOnlyRename(t *testing.T) {
	root := t.TempDir()
	if !volumeFolds(t, root) {
		t.Skip("case-sensitive volume")
	}
	write(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	write(t, filepath.Join(root, "lib", "c.sh"), "c_fn() { :; }\n")
	w := open(t, root)
	r := w.Root()
	mv := func(from, to string) {
		t.Helper()
		if err := os.Rename(filepath.Join(r, from), filepath.Join(r, to)); err != nil {
			t.Fatal(err)
		}
	}
	mv("a.sh", "A.sh")
	w.Refresh(filepath.Join(r, "a.sh"), filepath.Join(r, "A.sh"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"A.sh", "lib/c.sh"}) {
		t.Fatalf("delete then create: %v", got)
	}
	mv("A.sh", "a.sh")
	w.Refresh(filepath.Join(r, "a.sh"), filepath.Join(r, "A.sh"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "lib/c.sh"}) {
		t.Fatalf("create then delete: %v", got)
	}
	mv("lib", "LIB")
	w.Refresh(filepath.Join(r, "lib"), filepath.Join(r, "LIB"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"LIB/c.sh", "a.sh"}) {
		t.Fatalf("directory renamed: %v", got)
	}
}

// ---- .gitignore files ----

// A .gitignore that ignores itself still applies: git reads every
// .gitignore and so does the walk. Only the directory it sits in decides
// whether its event can be dropped.
func TestRefreshSelfIgnoringGitignore(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, "out", "o.sh"), "o() { :; }\n")
	write(t, filepath.Join(root, "sub", "s.sh"), "s() { :; }\n")
	w := open(t, root)
	r := w.Root()
	write(t, filepath.Join(root, "out", ".gitignore"), "*\n")
	w.Refresh(filepath.Join(r, "out", ".gitignore"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "sub/s.sh"}) {
		t.Fatalf("out/.gitignore with *: %v", got)
	}
	write(t, filepath.Join(root, ".gitignore"), "sub\n.gitignore\n")
	w.Refresh(filepath.Join(r, ".gitignore"))
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh"}) {
		t.Fatalf("root .gitignore naming sub and itself: %v", got)
	}
	for _, p := range []string{filepath.Join(root, "out", ".gitignore"), filepath.Join(root, ".gitignore")} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		w.Refresh(p)
	}
	if got := rels(w); !reflect.DeepEqual(got, []string{"a.sh", "out/o.sh", "sub/s.sh"}) {
		t.Fatalf("after deleting both: %v", got)
	}
}

// countReadDir counts directory listings for the duration of a test.
func countReadDir(t *testing.T) *int {
	t.Helper()
	n := new(int)
	old := readDirN
	readDirN = func(d string, max int) ([]os.DirEntry, error) { *n++; return old(d, max) }
	t.Cleanup(func() { readDirN = old })
	return n
}

// Case-variant events read each directory once per batch, and once at all
// while the directory is unchanged; the stored spelling converges.
func TestCaseVariantEventsReadListingsOnce(t *testing.T) {
	old := detectFold
	detectFold = func(string) bool { return true }
	defer func() { detectFold = old }()
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		write(t, filepath.Join(root, "d", fmt.Sprintf("f%d.sh", i)), "f() { :; }\n")
	}
	w := open(t, root)
	n := countReadDir(t)
	var batch []string
	for i := 0; i < 50; i++ {
		batch = append(batch, filepath.Join(w.Root(), "d", fmt.Sprintf("F%d.SH", i)))
	}
	w.Refresh(batch...)
	first := *n
	if first == 0 || first > 2 {
		t.Fatalf("%d listings for one batch, want at most 2 (root and d)", first)
	}
	for i := 0; i < 20; i++ {
		w.Refresh(filepath.Join(w.Root(), "d", "F7.SH"))
	}
	if *n != first {
		t.Fatalf("%d more listings for repeated events in an unchanged directory", *n-first)
	}
	if got := rels(w); len(got) != 50 || got[0] != "d/f0.sh" {
		t.Fatalf("spelling did not stay the disk's: %v", got[:3])
	}
}

// An event that names a renamed file or directory by its old spelling, or
// a file below a directory renamed by case, takes the spelling on disk.
func TestCaseRenameSingleEvents(t *testing.T) {
	root := t.TempDir()
	if !volumeFolds(t, root) {
		t.Skip("case-sensitive volume")
	}
	write(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	write(t, filepath.Join(root, "lib", "c.sh"), "c_fn() { :; }\n")
	w := open(t, root)
	r := w.Root()
	if err := os.Rename(filepath.Join(r, "a.sh"), filepath.Join(r, "A.sh")); err != nil {
		t.Fatal(err)
	}
	w.Refresh(filepath.Join(r, "a.sh")) // the old spelling only
	if got := rels(w); !reflect.DeepEqual(got, []string{"A.sh", "lib/c.sh"}) {
		t.Fatalf("old-spelling event: %v", got)
	}
	if err := os.Rename(filepath.Join(r, "lib"), filepath.Join(r, "LIB")); err != nil {
		t.Fatal(err)
	}
	w.Refresh(filepath.Join(r, "LIB", "c.sh")) // the file only, not the directory
	if got := rels(w); !reflect.DeepEqual(got, []string{"A.sh", "LIB/c.sh"}) {
		t.Fatalf("file event below a directory renamed by case: %v", got)
	}
}
