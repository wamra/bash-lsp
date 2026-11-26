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

// reasonsOf lists "name: reason" of the sites of a skipped-file refusal.
func reasonsOf(t *testing.T, w *Workspace) []string {
	t.Helper()
	ref := renameRefusal(t, w)
	if ref == nil || ref.Code != analysis.RefuseSkippedFile {
		t.Fatalf("refusal %v, want skipped-file", ref)
	}
	var out []string
	for _, s := range ref.Sites {
		out = append(out, filepath.Base(s.Path)+": "+s.Why)
	}
	return out
}

func TestRenameRefusedForADirectoryCutByTheDepthLimit(t *testing.T) {
	root := callerRoot(t)
	write(t, filepath.Join(root, "a", "b", "caller.sh"), "source ../../lib.sh\nf\n")
	w, err := Open(root, Limits{MaxDirDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := reasonsOf(t, w)
	if len(got) != 1 || got[0] != "b: directory too deep" {
		t.Fatalf("sites %v", got)
	}
	found := false
	for _, l := range w.Report().LimitsHit {
		found = found || strings.Contains(l, "depth limit")
	}
	if !found {
		t.Errorf("the report does not say the depth limit was hit: %v", w.Report().LimitsHit)
	}
}

func TestRenameRefusedForAnUnreadableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this reader")
	}
	root := callerRoot(t)
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "caller.sh"), "source ../lib.sh\nf\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	w, err := Open(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reasonsOf(t, w); len(got) != 1 || got[0] != "locked: directory unreadable" {
		t.Fatalf("sites %v", got)
	}
	found := false
	for _, l := range w.Report().LimitsHit {
		found = found || (strings.Contains(l, "locked") && strings.Contains(l, "could not be read"))
	}
	if !found {
		t.Errorf("the report does not name the unreadable directory: %v", w.Report().LimitsHit)
	}
}

func TestRenameFilesSayWhy(t *testing.T) {
	root := callerRoot(t)
	write(t, filepath.Join(root, "big.sh"), "f\n"+strings.Repeat("# pad\n", 400))
	write(t, filepath.Join(root, "rc.zsh"), "f\n")
	w, err := Open(root, Limits{MaxFileBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(reasonsOf(t, w), "|")
	if got != "big.sh: too large|rc.zsh: unsupported dialect" {
		t.Fatalf("reasons %q", got)
	}
	w, err = Open(root, Limits{MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range reasonsOf(t, w) {
		if !strings.HasSuffix(s, ": over the file limit") && !strings.HasSuffix(s, ": unsupported dialect") {
			t.Errorf("reason %q", s)
		}
	}
}

// Excluded files are a choice made in the settings: the plan is made, with a warning
// that says they were not checked.
func TestRenamePlanWarnsOfExcludedAndIgnoredFiles(t *testing.T) {
	root := callerRoot(t)
	write(t, filepath.Join(root, "vendor", "x.sh"), "f\n")
	write(t, filepath.Join(root, "skip.sh"), "f\n")
	write(t, filepath.Join(root, ".gitignore"), "vendor/\nskip.sh\n")
	w, err := Open(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := w.Snapshot().PlanRename(filepath.Join(root, "lib.sh"), 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "1 file and 1 directory excluded by settings or .gitignore were not checked") {
		t.Fatalf("warnings %q", plan.Warnings)
	}
	// nothing excluded, nothing said
	clean := callerRoot(t)
	w, err = Open(clean, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = w.Snapshot().PlanRename(filepath.Join(clean, "lib.sh"), 0, "g")
	if err != nil || len(plan.Warnings) != 0 {
		t.Fatalf("plan %v, %v", plan, err)
	}
	// files.exclude counts the same way
	ex := callerRoot(t)
	write(t, filepath.Join(ex, "old", "y.sh"), "f\n")
	w, err = OpenWith(ex, Limits{}, nil, Config{Exclude: []string{"old/"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = w.Snapshot().PlanRename(filepath.Join(ex, "lib.sh"), 0, "g")
	if err != nil || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "1 directory excluded") {
		t.Fatalf("plan %v, %v", plan, err)
	}
}
