package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
)

// renameRefusal plans the rename of f in lib.sh and returns the refusal, or
// nil when the plan is made.
func renameRefusal(t *testing.T, w *Workspace) *analysis.RenameRefusal {
	t.Helper()
	snap := w.Snapshot()
	lib := filepath.Join(w.Root(), "lib.sh")
	_, err := snap.PlanRename(lib, 0, "g")
	var ref *analysis.RenameRefusal
	if err != nil && !errors.As(err, &ref) {
		t.Fatalf("rename: %v", err)
	}
	return ref
}

func wantSkippedFile(t *testing.T, w *Workspace, names ...string) {
	t.Helper()
	ref := renameRefusal(t, w)
	if ref == nil || ref.Code != analysis.RefuseSkippedFile {
		t.Fatalf("refusal %v, want skipped-file", ref)
	}
	var got []string
	for _, s := range ref.Sites {
		got = append(got, filepath.Base(s.Path))
	}
	if strings.Join(got, " ") != strings.Join(names, " ") {
		t.Fatalf("sites %v, want %v", got, names)
	}
}

func callerRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	write(t, filepath.Join(root, "lib.sh"), "f() { :; }\n")
	write(t, filepath.Join(root, "main.sh"), "source ./lib.sh\nf\n")
	return root
}

func TestRenameRefusedWhenACallerWasOverTheSizeLimit(t *testing.T) {
	root := callerRoot(t)
	write(t, filepath.Join(root, "big.sh"), "source ./lib.sh\nf\n"+strings.Repeat("# pad\n", 400))
	w, err := Open(root, Limits{MaxFileBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	wantSkippedFile(t, w, "big.sh")
}

func TestRenameRefusedWhenTheFileCountCutTheWorkspace(t *testing.T) {
	root := callerRoot(t)
	write(t, filepath.Join(root, "zcaller.sh"), "source ./lib.sh\nf\n")
	w, err := Open(root, Limits{MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	wantSkippedFile(t, w, "zcaller.sh")
}

func TestRenameRefusedWhenACallerIsUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this reader")
	}
	root := callerRoot(t)
	z := filepath.Join(root, "zcaller.sh")
	write(t, z, "source ./lib.sh\nf\n")
	if err := os.Chmod(z, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(z, 0o644)
	w, err := Open(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantSkippedFile(t, w, "zcaller.sh")
	if err := os.Chmod(z, 0o644); err != nil {
		t.Fatal(err)
	}
	w.Refresh(z)
	if ref := renameRefusal(t, w); ref != nil {
		t.Fatalf("readable again, still refused: %v", ref)
	}
}

func TestRenameRefusalFollowsRefresh(t *testing.T) {
	root := callerRoot(t)
	w, err := Open(root, Limits{MaxFileBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if ref := renameRefusal(t, w); ref != nil {
		t.Fatalf("refused with nothing skipped: %v", ref)
	}
	big := filepath.Join(root, "big.sh")
	write(t, big, "f\n"+strings.Repeat("# pad\n", 400))
	w.Refresh(big)
	wantSkippedFile(t, w, "big.sh")
	if err := os.Remove(big); err != nil {
		t.Fatal(err)
	}
	w.Refresh(big)
	if ref := renameRefusal(t, w); ref != nil {
		t.Fatalf("file removed, still refused: %v", ref)
	}
}

func TestRenameRefusedForAnUnsupportedDialectFile(t *testing.T) {
	root := callerRoot(t)
	write(t, filepath.Join(root, "rc.zsh"), "source ./lib.sh\nf\n")
	w, err := Open(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	wantSkippedFile(t, w, "rc.zsh")
}
