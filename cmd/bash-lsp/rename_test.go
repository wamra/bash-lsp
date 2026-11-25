package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/tree"
)

// noTemps fails when a temporary file of a rewrite is left under root.
func noTemps(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".bash-lsp-fmt-") {
			t.Errorf("temporary file left: %s", p)
		}
		return nil
	})
}

// corpusCopy is a writable copy of the corpus; paths are resolved, the root
// the commands print relative to.
func corpusCopy(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if r, err := filepath.EvalSymlinks(dst); err == nil {
		dst = r
	}
	err := filepath.WalkDir(corpus, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(corpus, p)
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

// treeBytes maps each file under root to its bytes.
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			m[rel] = string(b)
		}
		return nil
	})
	return m
}

func sameTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s changed:\n%s", k, got[k])
		}
	}
	if len(got) != len(want) {
		var extra []string
		for k := range got {
			if _, ok := want[k]; !ok {
				extra = append(extra, k)
			}
		}
		t.Errorf("extra files %v", extra)
	}
}

// corpusDef is the definition of build_release in lib/util.sh: --apply names
// it by position, because the name alone is shared with orphan.sh.
const corpusDef = "lib/util.sh:4:1"

func hooks(t *testing.T) *renameHooks {
	t.Helper()
	h := &renameHooks{}
	prev := applyHooks
	applyHooks = h
	t.Cleanup(func() { applyHooks = prev })
	return h
}

func TestCLIRenamePreviewAC19(t *testing.T) {
	out := runOK(t, "rename", "build_release", "ship", "--root", corpus)
	want := []string{
		"edit  dyn.sh:4:1  build_release -> ship",
		"edit  lib/util.sh:4:1  build_release -> ship",
		"edit  main.sh:3:10  build_release -> ship",
		"edit  main.sh:6:3  build_release -> ship",
		"skip  main.sh:8:13  candidate/variable-constant  ",
		"skip  main.sh:9:3  candidate/variable-constant  ",
		"skip  main.sh:10:9  candidate/string-eval  ",
		"other orphan.sh:2:1  different symbol named build_release, not renamed",
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d\n got %q\nwant %q...", i, lines[i], w)
		}
	}
	if !strings.HasSuffix(lines[6], "  build_release") || !strings.HasSuffix(lines[4], "  build_release") || !strings.HasSuffix(lines[5], `  "$cmd"`) {
		t.Errorf("skip lines lack their text:\n%s", out)
	}
	js := runOK(t, "rename", "build_release", "ship", "--json", "--root", corpus)
	var doc struct {
		Generation       int
		Symbol, Old, New string
		Edits            []struct {
			Path      string
			Line, Col int
		}
		Excluded, Others []struct {
			Path      string
			Line, Col int
		}
		Truncated bool
	}
	mustJSON(t, js, &doc)
	if doc.Generation != 1 || doc.Old != "build_release" || doc.New != "ship" || len(doc.Edits) != 4 || len(doc.Excluded) != 3 || len(doc.Others) != 1 {
		t.Errorf("%s", js)
	}
	// preview is the default and writes nothing
	if out2 := runOK(t, "rename", "build_release", "ship", "--preview", "--root", corpus); out2 != out {
		t.Errorf("--preview differs from the default")
	}
}

func TestCLIRenameByPosition(t *testing.T) {
	out := runOK(t, "rename", "main.sh:5:9", "nm", "--root", corpus)
	wantLines(t, out,
		"edit  main.sh:5:9  name -> nm",
		"edit  main.sh:6:19  name -> nm",
		"edit  main.sh:7:8  name -> nm",
	)
	if strings.Contains(out, "skip") {
		t.Errorf("%s", out)
	}
}

func TestCLIRenameApply(t *testing.T) {
	root := corpusCopy(t)
	before := treeBytes(t, root)
	out := runOK(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	wantLines(t, out, "edit  main.sh:6:3  build_release -> ship", "skip  main.sh:10:9  candidate/string-eval")
	after := treeBytes(t, root)
	for _, f := range []string{"dyn.sh", "lib/util.sh", "main.sh"} {
		if after[f] == before[f] || strings.Contains(after[f], "build_release \"") {
			t.Errorf("%s not rewritten:\n%s", f, after[f])
		}
	}
	for _, f := range []string{"app.sh", "orphan.sh", "broken_exp.sh", "lib/hooks.sh", "plugins/a.sh"} {
		if after[f] != before[f] {
			t.Errorf("%s changed", f)
		}
	}
	m := after["main.sh"]
	for _, keep := range []string{"cmd=build_release\n", `"$cmd" x`, `eval "build_release y"`, `br "$name"`, "alias br=ship"} {
		if !strings.Contains(m, keep) {
			t.Errorf("main.sh lost %q:\n%s", keep, m)
		}
	}
	if !strings.Contains(m, `  ship "$name"`) || !strings.Contains(after["lib/util.sh"], "ship() {") {
		t.Errorf("edits missing:\n%s", m)
	}
	refs := runOK(t, "refs", "ship", "--json", "--root", root)
	var d struct {
		Results []struct {
			Path string
			Line int
		}
	}
	mustJSON(t, refs, &d)
	var got []string
	for _, r := range d.Results {
		got = append(got, r.Path)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "dyn.sh main.sh main.sh main.sh" {
		t.Errorf("refs ship: %v\n%s", got, refs)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*", ".bash-lsp-fmt-*")); len(left) != 0 {
		t.Errorf("temp files %v", left)
	}
	if fi, _ := os.Stat(filepath.Join(root, "main.sh")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestCLIRenameApplyKeepsMode(t *testing.T) {
	root := corpusCopy(t)
	if err := os.Chmod(filepath.Join(root, "main.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	runOK(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if fi, _ := os.Stat(filepath.Join(root, "main.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestCLIRenameApplyRefusesStale(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		p := filepath.Join(root, "lib", "util.sh")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, append(b, "# edited elsewhere\n"...), 0o644)
	}
	before := treeBytes(t, root)
	// the plan is built after Open, so edit between plan and checks
	code, out, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: stale-file: ") || !strings.Contains(errs, "lib/util.sh") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	before["lib/util.sh"] += "# edited elsewhere\n"
	sameTree(t, treeBytes(t, root), before)
	noTemps(t, root)
}

func TestCLIRenameApplyRefusesChangedMtimeOnly(t *testing.T) {
	// the same bytes are not stale
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		p := filepath.Join(root, "main.sh")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, b, 0o644)
	}
	if code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root); code != 0 {
		t.Fatalf("exit %d %s", code, errs)
	}
}

func TestCLIRenameApplyRefusesSymlink(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		p := filepath.Join(root, "dyn.sh")
		b, _ := os.ReadFile(p)
		os.Remove(p)
		os.WriteFile(filepath.Join(root, "real.txt"), b, 0o644)
		if err := os.Symlink("real.txt", p); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: not-regular-file: ") || !strings.Contains(errs, "dyn.sh") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	got := treeBytes(t, root)
	for _, f := range []string{"main.sh", "lib/util.sh"} {
		if !strings.Contains(got[f], "build_release") {
			t.Errorf("%s was written", f)
		}
	}
	if strings.Contains(got["real.txt"], "ship") {
		t.Error("the link target was written")
	}
	noTemps(t, root)
}

func TestCLIRenameApplyRefusesLinkOutsideRoot(t *testing.T) {
	root := corpusCopy(t)
	outside := t.TempDir()
	h := hooks(t)
	var target string
	h.afterPlan = func() {
		p := filepath.Join(root, "dyn.sh")
		b, _ := os.ReadFile(p)
		os.Remove(p)
		target = filepath.Join(outside, "elsewhere.sh")
		os.WriteFile(target, b, 0o644)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: outside-root: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	if b, _ := os.ReadFile(target); strings.Contains(string(b), "ship") {
		t.Error("the file outside the root was written")
	}
	noTemps(t, outside)
	noTemps(t, root)
	if b, _ := os.ReadFile(filepath.Join(root, "main.sh")); strings.Contains(string(b), "ship") {
		t.Error("main.sh was written")
	}
}

func TestCLIRenameApplyRefusesDirectoryLinkOutsideRoot(t *testing.T) {
	root := corpusCopy(t)
	outside := t.TempDir()
	h := hooks(t)
	h.afterPlan = func() {
		os.Rename(filepath.Join(root, "lib"), filepath.Join(outside, "lib"))
		if err := os.Symlink(filepath.Join(outside, "lib"), filepath.Join(root, "lib")); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: outside-root: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "lib", "util.sh")); strings.Contains(string(b), "ship") {
		t.Error("a file outside the root was written")
	}
}

func TestCLIRenameApplyRefusesNUL(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		p := filepath.Join(root, "main.sh")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, append(b, 0, 'x', '\n'), 0o644)
	}
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: nul-byte: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dyn.sh")); strings.Contains(string(b), "ship") {
		t.Error("dyn.sh was written")
	}
}

func TestCLIRenameApplyRefusesInvalidUTF8(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		p := filepath.Join(root, "main.sh")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, append(b, 0xff, 0xfe, '\n'), 0o644)
	}
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: invalid-utf8: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
}

func TestCLIRenameApplyRefusesReadOnly(t *testing.T) {
	root := corpusCopy(t)
	if err := os.Chmod(filepath.Join(root, "dyn.sh"), 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	before := treeBytes(t, root)
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: read-only: ") || !strings.Contains(errs, "dyn.sh") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	sameTree(t, treeBytes(t, root), before)
}

// A directory that cannot take the temporary file stops the apply before any
// file is replaced.
func TestCLIRenameStagingFailureWritesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	root := corpusCopy(t)
	h := hooks(t)
	h.afterChecks = func() {
		os.Chmod(filepath.Join(root, "lib"), 0o555)
		t.Cleanup(func() { os.Chmod(filepath.Join(root, "lib"), 0o755) })
	}
	before := treeBytes(t, root)
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 1 || !strings.Contains(errs, "not written: ") || !strings.Contains(errs, "lib/util.sh") || strings.Contains(errs, "\nwritten: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	sameTree(t, treeBytes(t, root), before)
	left, _ := filepath.Glob(filepath.Join(root, "*", ".bash-lsp-fmt-*"))
	left2, _ := filepath.Glob(filepath.Join(root, ".bash-lsp-fmt-*"))
	if len(left)+len(left2) != 0 {
		t.Errorf("temp files left: %v %v", left, left2)
	}
}

// A file edited while the temporary files were being written: nothing is
// replaced and no temporary file is left.
func TestCLIRenameApplyChecksAgainBeforeReplacing(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterStaging = func() {
		p := filepath.Join(root, "main.sh")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, append(b, "# late edit\n"...), 0o644)
	}
	before := treeBytes(t, root)
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: stale-file: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	before["main.sh"] += "# late edit\n"
	sameTree(t, treeBytes(t, root), before)
}

// A failure while replacing: the files replaced so far are put back, byte
// for byte, and no temporary file is left.
func TestCLIRenamePartialFailureRollsBack(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.beforeRename = func() {
		p := filepath.Join(root, "main.sh")
		os.Remove(p)
		os.Mkdir(p, 0o755) // a rename onto a directory fails
	}
	before := treeBytes(t, root)
	delete(before, "main.sh")
	code, out, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 1 {
		t.Fatalf("exit %d err %q", code, errs)
	}
	wantLines(t, errs, "rolled back: dyn.sh, lib/util.sh", "not written: dyn.sh, lib/util.sh, main.sh")
	if strings.Contains(errs, "\nwritten:") {
		t.Errorf("files reported as still written: %s", errs)
	}
	// the plan is shown, with the sites of the file that was not written
	wantLines(t, out, "edit  main.sh:6:3  build_release -> ship", "edit  dyn.sh:4:1  build_release -> ship")
	got := treeBytes(t, root)
	sameTree(t, got, before)
	noTemps(t, root)
}

// The Nth rename fails (a hook stands for a file system that refuses it): the
// files before it are restored and the rest was never touched.
func TestCLIRenameNthRenameFailsRestoresEverything(t *testing.T) {
	for _, n := range []int{1, 2, 3} {
		root := corpusCopy(t)
		before := treeBytes(t, root)
		h := hooks(t)
		calls := 0
		h.rename = func(from, to string) error {
			calls++
			if calls == n {
				return errors.New("refused by the test")
			}
			return os.Rename(from, to)
		}
		code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
		if code != 1 || !strings.Contains(errs, "refused by the test") {
			t.Fatalf("n=%d: exit %d err %q", n, code, errs)
		}
		sameTree(t, treeBytes(t, root), before)
		noTemps(t, root)
	}
}

// A roll back step that fails: the files still holding the new name are
// listed with the command that undoes them.
func TestCLIRenameRollBackFailureIsReported(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	calls := 0
	h.rename = func(from, to string) error {
		calls++
		switch {
		case calls == 3: // the third file cannot be replaced
			return errors.New("refused by the test")
		case strings.HasSuffix(to, "dyn.sh") && calls > 3: // dyn.sh cannot be put back
			return errors.New("cannot restore")
		}
		return os.Rename(from, to)
	}
	code, out, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	wantLines(t, errs, "rolled back: lib/util.sh", "written: dyn.sh", "not written: lib/util.sh, main.sh",
		"bash-lsp rename ship build_release --apply --root '"+root+"'")
	if b, _ := os.ReadFile(filepath.Join(root, "dyn.sh")); !strings.Contains(string(b), "ship dyn") {
		t.Errorf("dyn.sh was expected to keep the new name: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "lib", "util.sh")); strings.Contains(string(b), "ship") {
		t.Errorf("lib/util.sh was not put back")
	}
	wantLines(t, out, "edit  dyn.sh:4:1")
	noTemps(t, root)
}

// A file edited after the others were replaced is caught by the check before
// its own rename; what was replaced is put back.
func TestCLIRenameChecksEachFileBeforeItsRename(t *testing.T) {
	root := corpusCopy(t)
	before := treeBytes(t, root)
	h := hooks(t)
	calls := 0
	h.rename = func(from, to string) error {
		if calls++; calls == 2 {
			p := filepath.Join(root, "main.sh")
			os.WriteFile(p, append([]byte(before["main.sh"]), "# late\n"...), 0o644)
		}
		return os.Rename(from, to)
	}
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 1 || !strings.Contains(errs, "refused: stale-file") || !strings.Contains(errs, "rolled back: dyn.sh, lib/util.sh") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	before["main.sh"] += "# late\n"
	sameTree(t, treeBytes(t, root), before)
	noTemps(t, root)
}

func TestCLIRenameFailureUnderJSONPrintsThePlan(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.rename = func(from, to string) error { return errors.New("refused by the test") }
	code, out, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--json", "--root", root)
	var doc struct {
		Applied    bool
		Written    []string
		RolledBack []string `json:"rolled_back"`
		NotWritten []string `json:"not_written"`
		Error      string
		Edits      []struct{ Path string }
	}
	mustJSON(t, out, &doc)
	if code != 1 || doc.Applied || len(doc.Edits) != 4 || len(doc.NotWritten) != 3 || doc.Error == "" || !strings.Contains(errs, "refused by the test") {
		t.Errorf("exit %d\n%s\nstderr %q", code, out, errs)
	}
}

func TestCLIRenameApplyJSON(t *testing.T) {
	root := corpusCopy(t)
	out := runOK(t, "rename", corpusDef, "ship", "--apply", "--json", "--root", root)
	var doc struct {
		Applied bool
		Written []string
		Edits   []struct{ Path string }
	}
	mustJSON(t, out, &doc)
	if !doc.Applied || strings.Join(doc.Written, " ") != "dyn.sh lib/util.sh main.sh" || len(doc.Edits) != 4 {
		t.Errorf("%s", out)
	}
}

// Re-running by name must not touch the other symbol of the same name.
func TestCLIRenameApplyByNameRefusesWhenOthersExist(t *testing.T) {
	root := corpusCopy(t)
	before := treeBytes(t, root)
	code, out, errs := runCode(t, "rename", "build_release", "ship", "--apply", "--root", root)
	if code != 4 || !strings.HasPrefix(errs, "refused: ambiguous-apply: ") || !strings.Contains(errs, "path:line:col") || !strings.Contains(errs, "\nsite  orphan.sh:2:1") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	wantLines(t, out, "edit  lib/util.sh:4:1", "other orphan.sh:2:1") // the plan is shown
	sameTree(t, treeBytes(t, root), before)
	// preview by name still works and shows the others
	if o := runOK(t, "rename", "build_release", "ship", "--root", root); !strings.Contains(o, "other orphan.sh:2:1") {
		t.Errorf("%s", o)
	}
	// by position it applies, and a second run (the old name is now only
	// orphan.sh's) refuses nothing it did not say
	runOK(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if b, _ := os.ReadFile(filepath.Join(root, "orphan.sh")); !strings.Contains(string(b), "build_release") {
		t.Errorf("orphan.sh changed:\n%s", b)
	}
}

func TestCLIRenameApplyRefusesHardLink(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		if err := os.Link(filepath.Join(root, "dyn.sh"), filepath.Join(root, "dyn-copy.sh")); err != nil {
			t.Skip("no hard links:", err)
		}
	}
	before := treeBytes(t, root)
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: hard-link: ") || !strings.Contains(errs, "dyn.sh") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	before["dyn-copy.sh"] = before["dyn.sh"]
	sameTree(t, treeBytes(t, root), before)
}

func TestCLIRenameApplyRefusesSpecialMode(t *testing.T) {
	root := corpusCopy(t)
	h := hooks(t)
	h.afterPlan = func() {
		if err := os.Chmod(filepath.Join(root, "dyn.sh"), 0o755|os.ModeSetuid); err != nil {
			t.Skip("no setuid:", err)
		}
	}
	before := treeBytes(t, root)
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: special-mode: ") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	sameTree(t, treeBytes(t, root), before)
}

// A read-only directory is found before anything is staged: exit 4, nothing
// written, no temporary file.
func TestCLIRenameApplyRefusesReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	root := corpusCopy(t)
	lib := filepath.Join(root, "lib")
	if err := os.Chmod(lib, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lib, 0o755) })
	before := treeBytes(t, root)
	code, _, errs := runCode(t, "rename", corpusDef, "ship", "--apply", "--root", root)
	if code != 4 || !strings.Contains(errs, "refused: read-only: ") || !strings.Contains(errs, "lib/util.sh") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	sameTree(t, treeBytes(t, root), before)
	noTemps(t, root)
}

func TestCLIRenamePreviewWarnsOfFilesApplyWouldRefuse(t *testing.T) {
	root := corpusCopy(t)
	if err := os.Chmod(filepath.Join(root, "dyn.sh"), 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	out := runOK(t, "rename", "build_release", "ship", "--root", root)
	wantLines(t, out, "warn  apply-would-refuse: read-only: dyn.sh")
	js := runOK(t, "rename", "build_release", "ship", "--json", "--root", root)
	wantLines(t, js, "apply-would-refuse: read-only: dyn.sh")
}

func TestCLIRenamePreviewWarnsOfNULAndInvalidUTF8(t *testing.T) {
	root := corpusCopy(t)
	os.WriteFile(filepath.Join(root, "dyn.sh"), []byte("source ./lib/util.sh\nbuild_release dyn\n\x00\n"), 0o644)
	os.WriteFile(filepath.Join(root, "main.sh"), []byte("source ./lib/util.sh\nbuild_release \"$1\"\n# caf\xe9\n"), 0o644)
	out := runOK(t, "rename", corpusDef, "ship", "--root", root)
	wantLines(t, out, "warn  apply-would-refuse: nul-byte: dyn.sh", "warn  apply-would-refuse: invalid-utf8: main.sh")
}

func TestCLIRenameBuildChecksTheEditCoversTheName(t *testing.T) {
	f := &renameFile{rel: "a.sh", src: []byte("abc def\n"), edits: []analysis.RenameEdit{{Path: "a.sh", Range: tree.Range{Start: tree.Pos{Offset: 0}, End: tree.Pos{Offset: 3}}, NewText: "x"}}}
	if err := f.build("abc"); err != nil || string(f.out) != "x def\n" {
		t.Fatalf("%v %q", err, f.out)
	}
	if err := f.build("abd"); err == nil {
		t.Error("an edit that does not cover the old name is accepted")
	}
	f.edits[0].Range.End.Offset = 99
	if err := f.build("abc"); err == nil {
		t.Error("an edit past the end is accepted")
	}
}

func TestCLIRenameOutsideRootSpecIsARefusal(t *testing.T) {
	code, _, errs := runCode(t, "rename", "../x.sh:1:1", "h", "--root", corpus)
	if code != 4 || !strings.HasPrefix(errs, "refused: outside-root: ") {
		t.Errorf("exit %d %q", code, errs)
	}
}

func TestCLIRenameRefusalUnderJSON(t *testing.T) {
	code, out, errs := runCode(t, "rename", "build_release", "log", "--json", "--root", corpus)
	var doc struct {
		Applied bool
		Refused struct {
			Code, Reason string
			Sites        []struct{ Path string }
		}
	}
	mustJSON(t, out, &doc)
	if code != 4 || doc.Refused.Code != "name-conflict" || len(doc.Refused.Sites) == 0 || !strings.HasPrefix(errs, "refused: name-conflict: ") {
		t.Errorf("exit %d\n%s\nstderr %q", code, out, errs)
	}
}

func TestCLIRenameRedactsEveryPath(t *testing.T) {
	const tok = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	root := corpusCopy(t)
	for _, args := range [][]string{
		{"rename", corpusDef, tok},                             // invalid name echoed
		{"rename", tok, "h"},                                   // unknown symbol echoed
		{"rename", "../" + tok + ".sh:1:1", "h"},               // outside spec echoed
		{"rename", corpusDef, "x", "--apply", "--bogus" + tok}, // usage echoes the flag
		{"callers", tok + "/x.sh:1:1"},                         // graph command error
	} {
		_, out, errs := runCode(t, append(args, "--root", root)...)
		if strings.Contains(out+errs, "ghp_") || strings.Contains(out+errs, tok[:20]) {
			t.Errorf("%v: secret printed\n%s%s", args, out, errs)
		}
	}
}

func TestCLIRenameRefusalTextIsOneLineAndWithoutDuplicatePrefix(t *testing.T) {
	_, _, errs := runCode(t, "rename", "a.sh:1:1", "y", "--root", dynRoot(t))
	if !strings.HasPrefix(errs, "refused: dynamic-scope: a function that declares") || strings.Contains(errs, "dynamic scope: dynamic scope") {
		t.Errorf("%q", errs)
	}
}

func dynRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	src := "x=1\nshow() { echo \"$x\"; }\nrun() { local x=2; show; }\nrun\n"
	if err := os.WriteFile(filepath.Join(root, "a.sh"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCLIRenameSkipLinesStayOneLine(t *testing.T) {
	d := &query.RenameDoc{Old: "f", New: "h", Excluded: []query.RenameSkip{{
		Site: query.Site{Path: "a.sh", Line: 1, Col: 1}, Resolution: "candidate", Reason: "string-eval",
		Text: strings.Repeat("a\n", 100)}}}
	for _, l := range renameLines(d) {
		if strings.ContainsAny(l, "\r\n") || len(l) > 200+len("skip  a.sh:1:1  candidate/string-eval  ") {
			t.Errorf("%d bytes: %q", len(l), l)
		}
	}
}

func TestCLIRenameRefusedExit4(t *testing.T) {
	root := corpusCopy(t)
	before := treeBytes(t, root)
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"rename", "build_release", "log"}, "name-conflict"},
		{[]string{"rename", "build_release", "build_release"}, "no-op"},
		{[]string{"rename", "build_release", "a b"}, "invalid-name"},
		{[]string{"rename", "build_release", "if"}, "reserved-name"},
		{[]string{"rename", "nosuch", "x"}, "not-a-symbol"},
		{[]string{"rename", "main.sh:8:13", "x"}, "candidate-site"},
	} {
		for _, mode := range []string{"--preview", "--apply"} {
			args := append(append([]string(nil), c.args...), mode, "--root", root)
			code, out, errs := runCode(t, args...)
			if code != 4 || out != "" || !strings.HasPrefix(errs, "refused: "+c.code+": ") {
				t.Errorf("%v: exit %d out %q err %q", args, code, out, errs)
			}
		}
	}
	sameTree(t, treeBytes(t, root), before)
	// with --json the refusal is still on stderr, and stdout carries it as JSON
	code, out, errs := runCode(t, "rename", "build_release", "log", "--json", "--root", root)
	if code != 4 || !strings.Contains(out, `"code": "name-conflict"`) || !strings.HasPrefix(errs, "refused: name-conflict: ") {
		t.Errorf("--json: exit %d out %q err %q", code, out, errs)
	}
	// the sites of a refusal are listed
	_, _, errs = runCode(t, "rename", "build_release", "log", "--root", root)
	if !strings.Contains(errs, "\nsite  ") {
		t.Errorf("no site lines: %q", errs)
	}
}

func TestCLIRenameUsage(t *testing.T) {
	for _, args := range [][]string{
		{"rename"},
		{"rename", "a"},
		{"rename", "a", "b", "c"},
		{"rename", "a", "b", "--preview", "--apply"},
		{"rename", "a", "b", "--bogus"},
		{"rename", "a", "b", "--root"},
	} {
		code, _, errs := runCode(t, args...)
		if code != 2 || !strings.Contains(errs, "usage: bash-lsp rename") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	// -- ends the flags: a name that starts with a dash is a name
	code, _, errs := runCode(t, "rename", "--root", corpus, "--", "-x", "y")
	if code != 4 || !strings.Contains(errs, "refused: not-a-symbol") {
		t.Errorf("exit %d %q", code, errs)
	}
	var b bytes.Buffer
	if code := run([]string{"rename", "--help"}, strings.NewReader(""), &b, &b); code != 0 || !strings.Contains(b.String(), "bash-lsp rename") {
		t.Errorf("help: exit %d %q", code, b.String())
	}
}

func TestCLIRenameRedactsExcludedText(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	const tok = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	// a function named like a credential, called through a quoted word: the
	// excluded site's text is the word
	src := tok + "() { :; }\n\"" + tok + "\"\n" + tok + "\n"
	if err := os.WriteFile(filepath.Join(root, "a.sh"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"rename", tok, "h", "--root", root}, {"rename", tok, "h", "--json", "--root", root}} {
		out := runOK(t, args...)
		if strings.Contains(out, "ghp_") || strings.Contains(out, tok[:24]) {
			t.Errorf("%v: secret in output:\n%s", args, out)
		}
		if !strings.Contains(out, "inexact-name") {
			t.Errorf("%v: no excluded site:\n%s", args, out)
		}
	}
}
