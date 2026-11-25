package query

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/redact"
)

func mkfile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func canon(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// indexDB finds the database as code-index's own lookup does: the nearest
// .git upward names the repository; with none, the nearest ancestor that holds
// .Wammy/code.sqlite; else the root itself.
func TestIndexDBFollowsCodeIndexLookup(t *testing.T) {
	t.Setenv(IndexEnv, "")
	// a git repository, the workspace one directory below its root
	repo := canon(t, t.TempDir())
	mkfile(t, filepath.Join(repo, ".git", "HEAD"))
	mkfile(t, filepath.Join(repo, "scripts", "a.sh"))
	got, err := indexDB(filepath.Join(repo, "scripts"))
	if err != nil || got != filepath.Join(repo, ".Wammy", "code.sqlite") {
		t.Errorf("git repo: %q, %v", got, err)
	}
	// .git as a file (worktree) counts, the repository root itself too
	if got, _ := indexDB(repo); got != filepath.Join(repo, ".Wammy", "code.sqlite") {
		t.Errorf("repo root: %q", got)
	}
	// no git: the nearest ancestor with an index
	plain := canon(t, t.TempDir())
	mkfile(t, filepath.Join(plain, ".Wammy", "code.sqlite"))
	mkfile(t, filepath.Join(plain, "a", "b", "x.sh"))
	if got, _ := indexDB(filepath.Join(plain, "a", "b")); got != filepath.Join(plain, ".Wammy", "code.sqlite") {
		t.Errorf("plain, index above: %q", got)
	}
	// no git, no index anywhere: the root's own place
	none := canon(t, t.TempDir())
	if got, _ := indexDB(none); got != filepath.Join(none, ".Wammy", "code.sqlite") {
		t.Errorf("no index: %q", got)
	}
	// the user's own value wins
	t.Setenv(IndexEnv, filepath.Join(plain, "mine.sqlite"))
	if got, _ := indexDB(repo); got != filepath.Join(plain, "mine.sqlite") {
		t.Errorf("env: %q", got)
	}
}

func TestIndexDBRefusesNonRegularDatabase(t *testing.T) {
	t.Setenv(IndexEnv, "")
	repo := canon(t, t.TempDir())
	mkfile(t, filepath.Join(repo, ".git", "HEAD"))
	other := filepath.Join(t.TempDir(), "other.sqlite")
	mkfile(t, other)
	if err := os.MkdirAll(filepath.Join(repo, ".Wammy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(repo, ".Wammy", "code.sqlite")); err != nil {
		t.Skip("no symlinks")
	}
	if _, err := indexDB(repo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlinked database: %v", err)
	}
	// the same under a plain directory found by walking up
	plain := canon(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(plain, ".Wammy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(plain, ".Wammy", "code.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := indexDB(plain); err == nil {
		t.Error("walk-up: a symlinked database was accepted")
	}
}

func TestClipRedactsFirst(t *testing.T) {
	token := "ghp_" + strings.Repeat("Q", 36)
	for pad := 150; pad < maxReason+2; pad += 7 {
		in := strings.Repeat("é", pad/2) + " " + token
		got := clip(in)
		if strings.Contains(got, "ghp_") || strings.Contains(got, "QQ") {
			t.Fatalf("pad %d: partial token in %q", pad, got)
		}
		if len(got) > maxReason {
			t.Fatalf("%d bytes", len(got))
		}
	}
}

func TestSourceLineRedactsBeforeCut(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	for pad := 140; pad < maxSignature+2; pad += 3 {
		src := []byte("f() { : " + strings.Repeat("p", pad) + " " + token + "; }\n")
		got := sourceLine(src, 0)
		if strings.Contains(got, "ghp_") || strings.Contains(got, "AAAA") {
			t.Fatalf("pad %d: partial token in %q", pad, got)
		}
		if len(got) > maxSignature || !redactedOrShort(got) {
			t.Fatalf("pad %d: %d bytes", pad, len(got))
		}
	}
	// a very long line is judged by a bounded window
	long := []byte("f() { " + strings.Repeat("x ", 500000) + " }\n")
	if got := sourceLine(long, 0); len(got) > maxSignature {
		t.Errorf("%d bytes", len(got))
	}
}

func redactedOrShort(s string) bool { return len(s) <= maxSignature && redact.String(s) == s }

// A .Wammy that is a link, even to a directory, would point the program at an
// index elsewhere: each component below the directory that holds the index is
// checked, not only the database.
func TestIndexDBRefusesLinkedIndexDirectory(t *testing.T) {
	t.Setenv(IndexEnv, "")
	outside := t.TempDir()
	mkfile(t, filepath.Join(outside, "code.sqlite"))
	repo := canon(t, t.TempDir())
	mkfile(t, filepath.Join(repo, ".git", "HEAD"))
	if err := os.Symlink(outside, filepath.Join(repo, ".Wammy")); err != nil {
		t.Skip("no symlinks")
	}
	if _, err := indexDB(repo); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("linked .Wammy under a repository: %v", err)
	}
	plain := canon(t, t.TempDir())
	if err := os.Symlink(outside, filepath.Join(plain, ".Wammy")); err != nil {
		t.Fatal(err)
	}
	mkfile(t, filepath.Join(plain, "sub", "x.sh"))
	if _, err := indexDB(filepath.Join(plain, "sub")); err == nil {
		t.Error("walk-up: a linked .Wammy was accepted")
	}
	if _, err := indexDB(plain); err == nil {
		t.Error("a linked .Wammy was accepted")
	}
	// a real .Wammy directory without a database is fine
	ok := canon(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(ok, ".Wammy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := indexDB(ok); err != nil {
		t.Errorf("plain .Wammy directory: %v", err)
	}
}
