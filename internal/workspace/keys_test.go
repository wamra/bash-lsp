package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

func openWith(t *testing.T, root string, lim Limits, cfg Config) *Workspace {
	t.Helper()
	w, err := OpenWith(root, lim, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestOpenWithZeroConfigIsOpenExcluding(t *testing.T) {
	root := copyCorpus(t)
	a, err := OpenExcluding(root, Limits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := openWith(t, root, Limits{}, Config{})
	if !slices.Equal(rels(a), rels(b)) || len(a.Snapshot().Edges()) != len(b.Snapshot().Edges()) {
		t.Fatal("OpenWith with a zero Config differs from OpenExcluding")
	}
}

func TestExcludeKey(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "a() { :; }\n")
	write(t, filepath.Join(root, "vendor", "a.sh"), "v() { :; }\n")
	write(t, filepath.Join(root, "deep", "vendor", "b.sh"), "w() { :; }\n")
	write(t, filepath.Join(root, "deep", "keep.sh"), "k() { :; }\n")
	write(t, filepath.Join(root, "gen_x.sh"), "g() { :; }\n")
	write(t, filepath.Join(root, ".gitignore"), "!vendor/\n")
	w := openWith(t, root, Limits{}, Config{Exclude: []string{"vendor/", "gen_*.sh"}})
	if got, want := rels(w), []string{"a.sh", "deep/keep.sh"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v (a .gitignore negation must not bring an excluded directory back)", got, want)
	}
	// Excluded paths are skipped as .gitignore'd ones are: not files, not listed as skipped.
	if r := w.Report(); r.Files != 2 || r.Skipped != 0 || len(w.Skipped()) != 0 {
		t.Errorf("report = %+v, skipped %v", r, w.Skipped())
	}
	// The reach test, and Refresh, follow the key.
	if w.Reaches(filepath.Join(w.Root(), "vendor")) || w.Reaches(filepath.Join(w.Root(), "deep", "vendor")) || !w.Reaches(filepath.Join(w.Root(), "deep")) {
		t.Error("Reaches does not follow files.exclude")
	}
	write(t, filepath.Join(root, "vendor", "new.sh"), "n() { :; }\n")
	write(t, filepath.Join(root, "deep", "new.sh"), "d() { :; }\n")
	w.Refresh(filepath.Join(w.Root(), "vendor", "new.sh"), filepath.Join(w.Root(), "deep", "new.sh"))
	if got, want := rels(w), []string{"a.sh", "deep/keep.sh", "deep/new.sh"}; !slices.Equal(got, want) {
		t.Errorf("after Refresh: %v, want %v", got, want)
	}
	// A directory event re-walks, and must give the same answer.
	w.Refresh()
	if got, want := rels(w), []string{"a.sh", "deep/keep.sh", "deep/new.sh"}; !slices.Equal(got, want) {
		t.Errorf("after a full refresh: %v, want %v", got, want)
	}
}

func TestIncludeKey(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"bin/a.sh", "bin/b.sh", "bin/sub/c.sh", "lib/d.sh", "top.sh"} {
		write(t, filepath.Join(root, f), "x() { :; }\n")
	}
	w := openWith(t, root, Limits{}, Config{Include: []string{"bin/*.sh"}})
	if got, want := rels(w), []string{"bin/a.sh", "bin/b.sh"}; !slices.Equal(got, want) {
		t.Fatalf("include bin/*.sh: %v, want %v", got, want)
	}
	write(t, filepath.Join(root, "lib", "e.sh"), "e() { :; }\n")
	write(t, filepath.Join(root, "bin", "f.sh"), "f() { :; }\n")
	w.Refresh(filepath.Join(w.Root(), "lib", "e.sh"), filepath.Join(w.Root(), "bin", "f.sh"))
	if got, want := rels(w), []string{"bin/a.sh", "bin/b.sh", "bin/f.sh"}; !slices.Equal(got, want) {
		t.Errorf("after Refresh: %v, want %v", got, want)
	}
	// A directory pattern includes everything below it; the walk still descends everywhere to find matches.
	if got, want := rels(openWith(t, root, Limits{}, Config{Include: []string{"bin/"}})), []string{"bin/a.sh", "bin/b.sh", "bin/f.sh", "bin/sub/c.sh"}; !slices.Equal(got, want) {
		t.Errorf("include bin/: %v, want %v", got, want)
	}
	if got := rels(openWith(t, root, Limits{}, Config{Include: []string{"**/c.sh"}})); !slices.Equal(got, []string{"bin/sub/c.sh"}) {
		t.Errorf("include **/c.sh: %v", got)
	}
	// Present and empty includes nothing; absent is today's discovery.
	if got := rels(openWith(t, root, Limits{}, Config{Include: []string{}})); len(got) != 0 {
		t.Errorf("empty include: %v", got)
	}
	if got := rels(openWith(t, root, Limits{}, Config{})); len(got) != 7 {
		t.Errorf("no include: %v", got)
	}
	// Exclude wins over include.
	if got := rels(openWith(t, root, Limits{}, Config{Include: []string{"bin/"}, Exclude: []string{"sub/"}})); slices.Contains(got, "bin/sub/c.sh") {
		t.Errorf("exclude lost to include: %v", got)
	}
}

func TestSourcePathsKey(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "lib", "util.sh"), "util() { :; }\n")
	write(t, filepath.Join(root, "bin", "run.sh"), "#!/bin/bash\nsource util.sh\nutil\n")
	run := func(w *Workspace) []analysis.SourceEdge {
		return w.Snapshot().SourceEdges(filepath.Join(w.Root(), "bin", "run.sh"))
	}
	if e := run(openWith(t, root, Limits{}, Config{})); len(e) != 1 || e[0].Status != analysis.SourceUnresolved {
		t.Fatalf("without sources.paths: %+v", e)
	}
	w := openWith(t, root, Limits{}, Config{SourcePaths: []string{"lib"}})
	e := run(w)
	if len(e) != 1 || e[0].Status != analysis.SourceResolved || !strings.HasSuffix(filepath.ToSlash(e[0].Targets[0]), "lib/util.sh") {
		t.Fatalf("sources.paths = lib: %+v", e)
	}
	if n := w.Report().Notices; len(n) != 0 {
		t.Errorf("notices for a good entry: %q", n)
	}
	// With a .shellcheckrc, sources.paths follows it.
	write(t, filepath.Join(root, ".shellcheckrc"), "source-path=SCRIPTDIR\n")
	w2 := openWith(t, root, Limits{}, Config{SourcePaths: []string{"lib"}})
	got := w2.searchPaths(filepath.Join(w2.Root(), "bin", "run.sh"))
	if want := []string{filepath.Join(w2.Root(), "bin"), filepath.Join(w2.Root(), "lib")}; !slices.Equal(got, want) {
		t.Errorf("search paths = %v, want %v", got, want)
	}
	// "../x" is reported and dropped.
	w3 := openWith(t, root, Limits{}, Config{SourcePaths: []string{"../x", "lib"}})
	if n := w3.Report().Notices; len(n) != 1 || !strings.Contains(n[0], `"../x"`) || !strings.Contains(n[0], "outside the workspace root (ignored)") {
		t.Errorf("notices = %q", n)
	}
}

// sources.paths comes from the repo file: nothing in it may widen what the
// server reads. Every entry that is absolute, spelled with ~ or $, or that
// resolves outside the root (by dots, or by a link) is dropped with a notice;
// ~ and $HOME are never expanded.
func TestSourcePathsContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.sh"), "secret() { :; }\n")
	write(t, filepath.Join(root, "lib", "util.sh"), "util() { :; }\n")
	write(t, filepath.Join(root, "~", "tilde.sh"), "tilde() { :; }\n")
	write(t, filepath.Join(root, "$HOME", "home.sh"), "home() { :; }\n")
	write(t, filepath.Join(root, "main.sh"), "source secret.sh\nsource tilde.sh\nsource home.sh\nsource util.sh\n")
	if err := os.Symlink(outside, filepath.Join(root, "lnk")); err != nil {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	entries := []string{"/etc", "../..", "~", "$HOME", "~/lib", "${HOME}/lib", "lnk", "lib/../../x", outside, filepath.Join(rootReal, "lib"), "", "lib"}
	w := openWith(t, root, Limits{}, Config{SourcePaths: entries})
	main := filepath.Join(w.Root(), "main.sh")
	got := w.searchPaths(main)
	if want := []string{filepath.Join(w.Root(), "lib")}; !slices.Equal(got, want) {
		t.Fatalf("search paths = %v, want only %v", got, want)
	}
	for _, d := range got {
		if !w.within(d) {
			t.Errorf("search path %s is outside the root", d)
		}
	}
	// Only util.sh resolves: not the link's target, not the ~ and $HOME directories.
	var resolved []string
	for _, e := range w.Snapshot().SourceEdges(main) {
		for _, tg := range e.Targets {
			resolved = append(resolved, filepath.Base(tg))
		}
	}
	if !slices.Equal(resolved, []string{"util.sh"}) {
		t.Errorf("resolved %v, want [util.sh]", resolved)
	}
	n := w.Report().Notices
	if len(n) != len(entries)-1 { // all but "lib"
		t.Fatalf("%d notices, want %d: %q", len(n), len(entries)-1, n)
	}
	for _, e := range entries[:len(entries)-1] {
		if e == "" {
			continue
		}
		found := false
		for _, line := range n {
			found = found || strings.Contains(line, "sources.paths") && strings.Contains(line, strconv.Quote(e)) && strings.HasSuffix(line, "(ignored)")
		}
		if !found {
			t.Errorf("no notice for %q in %q", e, n)
		}
	}
}

// An absolute source-path in a .shellcheckrc, or one that climbs out of the
// root, is dropped the same way and reported once.
func TestShellcheckrcSourcePathContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "o.sh"), "o() { :; }\n")
	write(t, filepath.Join(root, "lib", "util.sh"), "util() { :; }\n")
	write(t, filepath.Join(root, "bin", "run.sh"), "source o.sh\nsource util.sh\n")
	if err := os.Symlink(outside, filepath.Join(root, "lnk")); err != nil {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	rc := "source-path=/etc\nsource-path=" + outside + "\nsource-path=SCRIPTDIR/../../..\nsource-path=../../x\nsource-path=" + filepath.Join(root, "lnk") + "\nsource-path=" + rootReal + "/lib\nsource-path=SCRIPTDIR/../lib\n"
	write(t, filepath.Join(root, ".shellcheckrc"), rc)
	w := openWith(t, root, Limits{}, Config{})
	run := filepath.Join(w.Root(), "bin", "run.sh")
	got := w.searchPaths(run)
	lib := filepath.Join(w.Root(), "lib")
	if want := []string{lib, lib}; !slices.Equal(got, want) {
		t.Fatalf("search paths = %v, want %v", got, want)
	}
	if e := w.Snapshot().SourceEdges(run); e[0].Status != analysis.SourceUnresolved || e[1].Status != analysis.SourceResolved {
		t.Errorf("edges %+v: o.sh outside the root must stay unresolved, util.sh resolves", e)
	}
	n := w.Report().Notices
	if len(n) != 5 {
		t.Fatalf("notices = %q, want 5", n)
	}
	for _, line := range n {
		if !strings.Contains(line, ".shellcheckrc") || !strings.Contains(line, "source-path=") || !strings.HasSuffix(line, "resolves outside the workspace root (ignored)") {
			t.Errorf("notice = %q", line)
		}
	}
	// Reading again (a rebuild) does not repeat them; fixing the file clears them.
	w.Refresh(run)
	if len(w.Report().Notices) != 5 {
		t.Errorf("notices repeated: %q", w.Report().Notices)
	}
	write(t, filepath.Join(root, ".shellcheckrc"), "source-path=SCRIPTDIR/../lib\n")
	w.Refresh()
	if n := w.Report().Notices; len(n) != 0 {
		t.Errorf("notices after the fix: %q", n)
	}
}

func TestDialectKey(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "plain.sh"), "echo hi\n")
	write(t, filepath.Join(root, "bashy.sh"), "#!/bin/bash\necho hi\n")
	w := openWith(t, root, Limits{}, Config{Dialect: tree.POSIX})
	dialect := func(name string) tree.Dialect { return w.Snapshot().Facts(filepath.Join(w.Root(), name)).File.Dialect }
	if dialect("plain.sh") != tree.POSIX || dialect("bashy.sh") != tree.Bash {
		t.Errorf("walked files: plain.sh %v, bashy.sh %v", dialect("plain.sh"), dialect("bashy.sh"))
	}
	// The walk never lists an extensionless file without a shebang; an open document can be one.
	w.SetOverlay(filepath.Join(w.Root(), "script"), 1, []byte("echo hi\n"))
	w.SetOverlay(filepath.Join(w.Root(), "bashscript"), 1, []byte("#!/bin/bash\necho hi\n"))
	if dialect("script") != tree.POSIX || dialect("bashscript") != tree.Bash {
		t.Errorf("overlays: script %v, bashscript %v", dialect("script"), dialect("bashscript"))
	}
	// A document over the size limit keeps its dialect too.
	big := openWith(t, t.TempDir(), Limits{MaxFileBytes: 8}, Config{Dialect: tree.POSIX})
	big.SetOverlay(filepath.Join(big.Root(), "huge"), 1, []byte("echo hello world\n"))
	if d := big.Snapshot().Facts(filepath.Join(big.Root(), "huge")).File.Dialect; d != tree.POSIX {
		t.Errorf("oversize overlay dialect = %v", d)
	}
	// Default: Bash.
	w2 := openWith(t, root, Limits{}, Config{})
	if d := w2.Snapshot().Facts(filepath.Join(w2.Root(), "plain.sh")).File.Dialect; d != tree.Bash {
		t.Errorf("default dialect = %v", d)
	}
}

func TestCommandsConfig(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.sh"), "ls\ndeplyo\n")
	cmds := func(name string) (string, bool) { return "/bin/" + name, name == "ls" }
	w := openWith(t, root, Limits{}, Config{Commands: cmds})
	count := func() (found, missing int) {
		for _, e := range w.Snapshot().Edges() {
			if e.IsCommand() {
				if e.Res == analysis.Candidate {
					found++
				} else {
					missing++
				}
			}
		}
		return
	}
	if f, m := count(); f != 1 || m != 1 {
		t.Fatalf("command edges: %d found, %d missing", f, m)
	}
	w.SetOverlay(filepath.Join(w.Root(), "a.sh"), 2, []byte("ls\nls\ndeplyo\n"))
	if f, m := count(); f != 2 || m != 1 {
		t.Errorf("after an edit: %d found, %d missing", f, m)
	}
	if f, m := func() (int, int) { x := openWith(t, root, Limits{}, Config{}); w = x; return count() }(); f != 0 || m != 0 {
		t.Errorf("Config without Commands has command edges: %d, %d", f, m)
	}
}

func TestLimitsArgument(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.sh", "b.sh", "c.sh"} {
		write(t, filepath.Join(root, n), "x() { :; }\n")
	}
	w := openWith(t, root, Limits{MaxFiles: 2}, Config{})
	r := w.Report()
	if r.Files != 2 || len(r.LimitsHit) != 1 || !strings.Contains(r.LimitsHit[0], "file limit of 2") {
		t.Errorf("report = %+v", r)
	}
}

// A relative source-path of a .shellcheckrc that passes through a symlink out
// of the root is dropped and reported, like the other entries.
func TestShellcheckrcRelativeSymlinkEntryNoticed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "o.sh"), "o() { :; }\n")
	write(t, filepath.Join(root, "main.sh"), "source o.sh\n")
	if err := os.Symlink(outside, filepath.Join(root, "lnk")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, ".shellcheckrc"), "source-path=lnk\nsource-path=SCRIPTDIR/lnk\n")
	w := openWith(t, root, Limits{}, Config{})
	main := filepath.Join(w.Root(), "main.sh")
	if got := w.searchPaths(main); len(got) != 0 {
		t.Errorf("search paths = %v, want none", got)
	}
	n := w.Report().Notices
	if len(n) != 2 || !strings.Contains(n[0], "source-path=") || !strings.HasSuffix(n[0], "resolves outside the workspace root (ignored)") {
		t.Errorf("notices = %q, want one per entry", n)
	}
	w.searchPaths(main)
	if len(w.Report().Notices) != 2 {
		t.Errorf("notices repeated: %q", w.Report().Notices)
	}
}

func TestKeyLongPathAndDrive(t *testing.T) {
	want := keyFor(`c:\x`, true)
	for _, p := range []string{`\\?\C:\x`, `C:\x`, `c:\x`, `\\?\c:\X`} {
		if got := keyFor(p, true); got != want {
			t.Errorf("keyFor(%q, true) = %q, want %q", p, got, want)
		}
	}
	if got := keyFor(`\\?\UNC\Srv\Share\x`, true); got != `\\srv\share\x` {
		t.Errorf("UNC long path key = %q", got)
	}
	// Case-sensitive keys are the path as given.
	if got := keyFor(`\\?\C:\x`, false); got != `\\?\C:\x` {
		t.Errorf("keyFor without fold changed the path: %q", got)
	}
}
