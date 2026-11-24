package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/tree"
)

func put(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rels(w *workspace.Workspace) []string {
	var out []string
	for _, p := range w.Snapshot().Paths() {
		r, _ := filepath.Rel(w.Root(), p)
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

func openSettings(t *testing.T, root string, s Settings, cmds func(string) (string, bool)) *workspace.Workspace {
	t.Helper()
	w, err := workspace.OpenWith(root, s.Limits(), nil, s.Workspace(cmds))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// [limits] max_files = 2 analyses two files and reports the count limit.
func TestLimitsKey(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.sh", "b.sh", "c.sh"} {
		put(t, root, n, "x() { :; }\n")
	}
	s := Merge(nil, []byte("[limits]\nmax_files = 2\nmax_file_bytes = 100\n"), nil)
	if s.Limits() != (workspace.Limits{MaxFiles: 2, MaxFileBytes: 100}) {
		t.Fatalf("Limits() = %+v", s.Limits())
	}
	w := openSettings(t, root, s, nil)
	r := w.Report()
	if r.Files != 2 || len(r.LimitsHit) != 1 || r.LimitsHit[0] != "file limit of 2 reached: further files skipped, first: c.sh" {
		t.Errorf("report = %+v", r)
	}
	put(t, root, "big.sh", "# "+string(make([]byte, 200))+"\n")
	w = openSettings(t, root, s, nil)
	if len(w.Skipped()) == 0 {
		t.Error("max_file_bytes = 100 skipped nothing")
	}
	// No [limits]: the built-in caps.
	if got := Merge(nil, nil, nil).Limits(); got != (workspace.Limits{}) {
		t.Errorf("default Limits() = %+v", got)
	}
}

func TestWorkspaceKeysFromSettings(t *testing.T) {
	root := t.TempDir()
	put(t, root, "vendor/v.sh", "v() { :; }\n")
	put(t, root, "bin/run.sh", "#!/bin/bash\nsource util.sh\n")
	put(t, root, "lib/util.sh", "util() { :; }\n")
	put(t, root, "docs/note.sh", "echo\n")
	repo := "[bash]\ndialect = \"sh\"\n[files]\ninclude = [\"bin/*.sh\", \"lib/*.sh\", \"vendor/*.sh\"]\nexclude = [\"vendor/\"]\n[sources]\npaths = [\"lib\", \"../x\", \"/etc\"]\n"
	s := Merge(nil, []byte(repo), nil)
	w := openSettings(t, root, s, nil)
	if got, want := rels(w), []string{"bin/run.sh", "lib/util.sh"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	run := filepath.Join(w.Root(), "bin", "run.sh")
	if e := w.Snapshot().SourceEdges(run); len(e) != 1 || len(e[0].Targets) != 1 {
		t.Errorf("source util.sh through sources.paths: %+v", e)
	}
	if n := w.Report().Notices; len(n) != 2 {
		t.Errorf("notices = %q, want the two dropped entries", n)
	}
	w.SetOverlay(filepath.Join(w.Root(), "script"), 1, []byte("echo hi\n"))
	if d := w.Snapshot().Facts(filepath.Join(w.Root(), "script")).File.Dialect; d != tree.POSIX {
		t.Errorf("bash.dialect = sh gave %v", d)
	}
	// A user's include and exclude carry through the same way.
	u := Merge([]byte("[files]\nexclude = [\"docs/\"]\n"), nil, nil)
	if got := rels(openSettings(t, root, u, nil)); slices.Contains(got, "docs/note.sh") || !slices.Contains(got, "bin/run.sh") {
		t.Errorf("user exclude: %v", got)
	}
}

// The repo file adds to the user's excludes; it cannot take one away, and a
// negation in it cannot bring back what the user excluded.
func TestRepoExcludeAddsToUserExclude(t *testing.T) {
	root := t.TempDir()
	put(t, root, "vendor/x.sh", "x() { :; }\n")
	put(t, root, "gen/g.sh", "g() { :; }\n")
	put(t, root, "src/s.sh", "s() { :; }\n")
	user := "[files]\nexclude = [\"vendor/**\"]\n"
	repo := "[files]\nexclude = [\"gen/**\", \"!vendor/x.sh\"]\n"
	s := Merge([]byte(user), []byte(repo), nil)
	if got, want := rels(openSettings(t, root, s, nil)), []string{"src/s.sh"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	found := false
	for _, w := range s.Warnings {
		found = found || strings.Contains(w, "negat")
	}
	if !found {
		t.Errorf("no warning for the dropped negation: %q", s.Warnings)
	}
	// With no user file the repo's own excludes apply as before.
	r := Merge(nil, []byte("[files]\nexclude = [\"gen/**\"]\n"), nil)
	if got := rels(openSettings(t, root, r, nil)); slices.Contains(got, "gen/g.sh") || !slices.Contains(got, "vendor/x.sh") {
		t.Errorf("repo only: %v", got)
	}
}
