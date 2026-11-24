package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/parse"
)

// emptyPath makes the PATH hold one empty directory: no ShellCheck.
func emptyPath(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("PATH", d)
	return d
}

// fakeShellCheck writes a program that answers --version and reports one
// finding on standard input, and returns its directory.
func fakeShellCheck(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo 'ShellCheck - shell script analysis tool'; echo 'version: 0.10.0'; exit 0; fi\n" +
		"while IFS= read -r l; do :; done\n" +
		"echo '{\"comments\":[{\"file\":\"-\",\"line\":1,\"endLine\":1,\"column\":1,\"endColumn\":3,\"level\":\"warning\",\"code\":2086,\"message\":\"fake finding\"}]}'\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "shellcheck"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type checkJSON struct {
	Generation  uint64 `json:"generation"`
	Diagnostics []struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Col      int    `json:"col"`
		EndLine  int    `json:"endLine"`
		EndCol   int    `json:"endCol"`
		Severity string `json:"severity"`
		Rule     string `json:"rule"`
		Origin   string `json:"origin"`
		Message  string `json:"message"`
		Fixes    []any  `json:"fixes"`
	} `json:"diagnostics"`
}

func checkJSONOf(t *testing.T, out string) checkJSON {
	t.Helper()
	var d checkJSON
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return d
}

func TestAC12Doctor(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, "a.sh", "#!/bin/bash\nlocal x=1\n")
	wantLines(t, runOK(t, "doctor", root), "shellcheck: absent", "shellcheck memory guard: none (FDS-017 unmet, see docs/m3-acceptance.md)",
		"target: 3.2-5.3", "path profile: 1 dirs (0 skipped)", "code-index: absent", "config: none")
	code, out, errs := runCode(t, "check", root)
	if code != 1 || !strings.Contains(out, "a.sh:2:1: error WB1105 ") {
		t.Fatalf("check: exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
}

func TestAC18(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable scripts")
	}
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	evil := "#!/bin/sh\necho ran > '" + marker + "'\nexit 0\n"
	putFile(t, root, ".bash-lsp.toml", "[shellcheck]\npath = \"./evil\"\n")
	putFile(t, root, "evil", evil)
	putFile(t, root, "shellcheck", evil)
	putFile(t, root, "a.sh", "#!/bin/bash\nlocal x=1\n")
	for _, f := range []string{"evil", "shellcheck"} {
		if err := os.Chmod(filepath.Join(root, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	code, out, errs := runCode(t, "check", root)
	if code != 1 || !strings.Contains(out, "WB1105") {
		t.Fatalf("check: exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "shellcheck: refused ") || !strings.Contains(errs, "inside the workspace folder") {
		t.Errorf("check does not say why ShellCheck was not run:\n%s", errs)
	}
	doc := runOK(t, "doctor", root)
	wantLines(t, doc, "shellcheck: refused ", "inside the workspace folder",
		"refused setting: .bash-lsp.toml shellcheck.path: executables and hooks are accepted only from user settings")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the program named by the repository ran")
	}
}

func TestCheckFileDoesNotWalkParent(t *testing.T) {
	emptyPath(t)
	tmp := t.TempDir()
	for i := 0; i < 300; i++ {
		putFile(t, tmp, fmt.Sprintf("s%d.sh", i), "#!/bin/bash\nlocal y=1\n")
	}
	putFile(t, tmp, "sib.sh", "sib() { :; }\n")
	// The file sources a sibling two levels up. Analysed alone, the sibling is
	// not in view and the source is unresolved; had the parents been walked,
	// it would resolve, and the findings of the 300 siblings would not be
	// the only difference.
	file := filepath.Join(tmp, "x", "y", "file.sh")
	putFile(t, file[:len(file)-len("file.sh")], "file.sh", "#!/bin/bash\nsource ../../sib.sh\nlocal z=1\n")
	code, out, errs := runCode(t, "check", "--format", "json", file)
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	d := checkJSONOf(t, out)
	rules := map[string]bool{}
	for _, g := range d.Diagnostics {
		rules[g.Rule] = true
		if filepath.Base(g.Path) != "file.sh" {
			t.Errorf("a finding of %q: only the named file is analysed", g.Path)
		}
	}
	if !rules["WB1105"] || !rules["WB1101"] {
		t.Fatalf("rules %v; want WB1105 and the unresolved source WB1101 (the parent was walked?)", rules)
	}
}

// Findings are printed, and the run still ends with 3 when a path could not be
// read: the result is incomplete, so a failure is not hidden behind a 1.
func TestCheckExitThreeBeatsOne(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, "e.sh", "#!/bin/bash\nlocal x=1\n")
	code, out, errs := runCode(t, "check", "--format", "json", root, filepath.Join(root, "missing.sh"))
	if code != 3 {
		t.Fatalf("exit %d, want 3\n%s", code, errs)
	}
	if d := checkJSONOf(t, out); len(d.Diagnostics) == 0 || d.Diagnostics[0].Rule != "WB1105" {
		t.Errorf("the findings of the readable path are not printed: %s", out)
	}
	if !strings.Contains(errs, "missing.sh") {
		t.Errorf("stderr does not name the failed path: %s", errs)
	}
}

func TestCheckDashDash(t *testing.T) {
	emptyPath(t)
	dir := t.TempDir()
	putFile(t, dir, "-x", "#!/bin/bash\nlocal x=1\n")
	t.Chdir(dir)
	if code, _, errs := runCode(t, "check", "-x"); code != 2 {
		t.Errorf("-x as a flag: exit %d, %s", code, errs)
	}
	code, out, errs := runCode(t, "check", "--format", "json", "--", "-x")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if d := checkJSONOf(t, out); len(d.Diagnostics) == 0 || d.Diagnostics[0].Path != "-x" {
		t.Errorf("findings of -x: %s", out)
	}
}

func TestCheckPrintsWorkspaceNotices(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, ".git/HEAD", "x")
	putFile(t, root, ".bash-lsp.toml", "[sources]\npaths = [\"../x\"]\n")
	putFile(t, root, "a.sh", "echo\n")
	_, _, errs := runCode(t, "check", root)
	if !strings.Contains(errs, `notice: sources.paths "../x"`) {
		t.Errorf("stderr lacks the workspace notice that doctor shows:\n%s", errs)
	}
}

func TestCheckSaysWhenNothingWasChecked(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, ".git/HEAD", "x")
	putFile(t, root, ".gitignore", "ign/\n")
	putFile(t, root, "ign/a.sh", "#!/bin/bash\nlocal x=1\n")
	putFile(t, root, "docs/readme.txt", "text\n")
	t.Chdir(root)
	for _, d := range []string{"ign", "docs"} {
		code, out, errs := runCode(t, "check", d)
		if code != 0 || out != "" || !strings.Contains(errs, "no shell files checked: "+d) {
			t.Errorf("check %s: exit %d, stdout %q, stderr %q", d, code, out, errs)
		}
	}
	if _, _, errs := runCode(t, "check", "ign"); strings.Count(errs, "no shell files checked") != 1 {
		t.Errorf("want one line: %q", errs)
	}
}

func TestCheckSaysWhatWasNotAnalysed(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, ".git/HEAD", "x")
	putFile(t, root, "big.sh", "#!/bin/bash\n# "+strings.Repeat("x", 2<<20)+"\n")
	putFile(t, root, "ok.sh", "echo ok\n")
	t.Chdir(root)
	for _, args := range [][]string{{"check", "big.sh"}, {"check", "."}} {
		code, _, errs := runCode(t, args...)
		if code != 0 || !strings.Contains(errs, "not analysed: big.sh: ") {
			t.Errorf("%v: exit %d, stderr %q (a file over the size limit is not covered by the exit code and says so)", args, code, errs)
		}
	}
	// A limit about another file is not this run's business.
	if _, _, errs := runCode(t, "check", "ok.sh"); strings.Contains(errs, "big.sh") {
		t.Errorf("check ok.sh mentions big.sh: %q", errs)
	}
	// Workspace-wide limits are.
	putFile(t, root, "two.sh", "echo two\n")
	putFile(t, root, ".bash-lsp.toml", "[limits]\nmax_files = 1\n")
	if _, _, errs := runCode(t, "check", "ok.sh"); !strings.Contains(errs, "file limit of 1 reached") {
		t.Errorf("the count limit is not shown: %q", errs)
	}
}

func TestCheckDisplayPaths(t *testing.T) {
	emptyPath(t)
	tmp := evalDir(t, t.TempDir())
	repo := filepath.Join(tmp, "repo")
	putFile(t, repo, ".git/HEAD", "x")
	putFile(t, repo, "d/t.sh", "#!/bin/bash\nlocal x=1\n")
	putFile(t, repo, "o.sh", "#!/bin/bash\nlocal x=1\n")
	loose := filepath.Join(tmp, "loose")
	putFile(t, loose, "x.sh", "#!/bin/bash\nlocal x=1\n")
	first := func(t *testing.T, args ...string) string {
		t.Helper()
		_, out, errs := runCode(t, args...)
		if out == "" {
			t.Fatalf("%v: no output, stderr %s", args, errs)
		}
		return strings.SplitN(out, ":", 2)[0]
	}
	want := func(t *testing.T, got, w string) {
		t.Helper()
		if got != w {
			t.Errorf("path %q, want %q", got, w)
		}
	}
	t.Chdir(repo)
	want(t, first(t, "check", "d"), "d/t.sh")
	want(t, first(t, "check", "d/t.sh"), "d/t.sh")
	want(t, first(t, "check", filepath.Join(repo, "d", "t.sh")), "d/t.sh")
	t.Chdir(filepath.Join(repo, "d"))
	want(t, first(t, "check", "t.sh"), "t.sh")
	want(t, first(t, "check", "."), "t.sh")
	want(t, first(t, "check", "../d/t.sh"), "t.sh")
	_, out, _ := runCode(t, "check", "..")
	if !strings.HasPrefix(out, "t.sh:") && !strings.Contains(out, "\nt.sh:") || !strings.Contains(out, filepath.ToSlash(filepath.Join(repo, "o.sh"))+":") {
		t.Errorf("walked from a subdirectory: a file under the cwd is relative, another is absolute:\n%s", out)
	}
	_, js, _ := runCode(t, "check", "--format", "json", "..")
	for _, g := range checkJSONOf(t, js).Diagnostics {
		if g.Path != "t.sh" && g.Path != filepath.ToSlash(filepath.Join(repo, "o.sh")) {
			t.Errorf("JSON path %q", g.Path)
		}
	}
	// Not walked.
	t.Chdir(loose)
	want(t, first(t, "check", "x.sh"), "x.sh")
	t.Chdir(tmp)
	want(t, first(t, "check", "loose/x.sh"), "loose/x.sh")
	t.Chdir(repo)
	want(t, first(t, "check", filepath.Join(loose, "x.sh")), filepath.ToSlash(filepath.Join(loose, "x.sh")))
}

func TestDoctorShowsResyncLimit(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	src := "a() { echo hi; }\n) ) fi done\n" + strings.Repeat("echo ${x\n", 80) + "z() { :; }\n"
	putFile(t, root, "budget.sh", src)
	f := parse.File(filepath.Join(root, "budget.sh"), []byte(src))
	if len(f.Limits) == 0 {
		t.Fatal("the parser recorded no limit for the input")
	}
	line := f.Limits[0].Range.Start.Line
	out := runOK(t, "doctor", root)
	want := fmt.Sprintf("limit hit: budget.sh:%d: %s: ", line, f.Limits[0].Name)
	if !strings.Contains(out, want) || !strings.Contains(out, "resync-attempts") {
		t.Fatalf("doctor lacks %q:\n%s", want, out)
	}
	st := runOK(t, "status", "--root", root)
	if !strings.Contains(st, "limit hit: budget.sh:") {
		t.Errorf("status lacks the snapshot limit lines:\n%s", st)
	}
	code, _, errs := runCode(t, "check", root)
	if !strings.Contains(errs, "limit hit: ") || !strings.Contains(errs, "budget.sh:") {
		t.Errorf("check (exit %d) does not report the limit:\n%s", code, errs)
	}
}

func TestCheckExitCodes(t *testing.T) {
	emptyPath(t)
	clean := t.TempDir()
	putFile(t, clean, "ok.sh", "echo ok\n")
	warn := t.TempDir()
	putFile(t, warn, "w.sh", "source ./missing.sh\n") // WB1101, a warning
	bad := t.TempDir()
	putFile(t, bad, "e.sh", "#!/bin/bash\nlocal x=1\n") // WB1105, an error
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"clean", []string{"check", clean}, 0},
		{"warning is a finding by default", []string{"check", warn}, 1},
		{"fail-on error ignores a warning", []string{"check", "--fail-on", "error", warn}, 0},
		{"fail-on=info", []string{"check", "--fail-on=info", warn}, 1},
		{"error", []string{"check", bad}, 1},
		{"fail-on hint", []string{"check", "--fail-on", "hint", clean}, 0},
		{"sarif", []string{"check", "--format", "sarif", clean}, 2},
		{"unknown format", []string{"check", "--format", "xml", clean}, 2},
		{"unknown flag", []string{"check", "--hook", clean}, 2},
		{"bad fail-on", []string{"check", "--fail-on", "fatal", clean}, 2},
		{"bad shellcheck mode", []string{"check", "--shellcheck", "on", clean}, 2},
		{"flag without value", []string{"check", clean, "--format"}, 2},
		{"missing path", []string{"check", filepath.Join(clean, "nope.sh")}, 3},
	}
	for _, c := range cases {
		if code, out, errs := runCode(t, c.args...); code != c.want {
			t.Errorf("%s: exit %d, want %d\nstdout: %s\nstderr: %s", c.name, code, c.want, out, errs)
		}
	}
	if _, _, errs := runCode(t, "check", "--format", "sarif", clean); !strings.Contains(errs, "sarif: not available in this build") {
		t.Errorf("sarif message: %q", errs)
	}
}

func TestCheckOutputFormats(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, "sub/a.sh", "#!/bin/bash\necho é; local x=1\n")
	t.Chdir(root)
	_, out, _ := runCode(t, "check", ".")
	if !strings.HasPrefix(out, "sub/a.sh:2:") || !strings.Contains(out, ": error WB1105 ") {
		t.Fatalf("text: %q", out)
	}
	_, out, _ = runCode(t, "check", "--format", "json", ".")
	d := checkJSONOf(t, out)
	if d.Generation == 0 || len(d.Diagnostics) < 1 {
		t.Fatalf("json: %s", out)
	}
	g := d.Diagnostics[0]
	if g.Path != "sub/a.sh" || g.Line != 2 || g.Col != 9 || g.EndLine != 2 || g.EndCol <= g.Col || g.Severity != "error" ||
		g.Rule != "WB1105" || g.Origin != "bash-lsp" || g.Message == "" || g.Fixes == nil {
		t.Errorf("diagnostic %+v (columns are UTF-16, 1-based; fixes is a list)", g)
	}
	// A directory with no finding prints an empty list, not null.
	clean := t.TempDir()
	putFile(t, clean, "ok.sh", "echo ok\n")
	if _, out, _ = runCode(t, "check", "--format", "json", clean); !strings.Contains(out, `"diagnostics": []`) {
		t.Errorf("no findings: %s", out)
	}
}

func TestCheckWithShellCheck(t *testing.T) {
	sc := fakeShellCheck(t)
	t.Setenv("PATH", sc)
	root := t.TempDir()
	putFile(t, root, "a.sh", "echo $x\n")
	putFile(t, root, ".shellcheckrc", "disable=notacode\n")
	doc := runOK(t, "doctor", root)
	wantLines(t, doc, "shellcheck: "+filepath.Join(evalDir(t, sc), "shellcheck")+" 0.10.0")
	code, out, errs := runCode(t, "check", "--format", "json", root)
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var origins []string
	for _, g := range checkJSONOf(t, out).Diagnostics {
		origins = append(origins, g.Origin+":"+g.Rule)
	}
	if !strings.Contains(strings.Join(origins, " "), "shellcheck:SC2086") {
		t.Errorf("no ShellCheck finding: %v", origins)
	}
	if !strings.Contains(errs, ".shellcheckrc:1: not valid rc syntax") {
		t.Errorf("the .shellcheckrc notice is not shown:\n%s", errs)
	}
	_, out, _ = runCode(t, "check", "--format", "json", "--shellcheck", "off", root)
	if strings.Contains(out, "SC2086") {
		t.Errorf("--shellcheck off still ran it:\n%s", out)
	}
	// shellcheck.enabled = false in user settings also turns it off.
	userConfig(t, "[shellcheck]\nenabled = false\n")
	_, out, _ = runCode(t, "check", "--format", "json", root)
	if strings.Contains(out, "SC2086") {
		t.Errorf("shellcheck.enabled = false still ran it:\n%s", out)
	}
	wantLines(t, runOK(t, "doctor", root), "shellcheck: disabled")
}

func evalDir(t *testing.T, d string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCheckNamedFileInWalkedRepo(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, ".git/HEAD", "ref: refs/heads/main\n")
	putFile(t, root, "lib/util.sh", "util() { :; }\n")
	putFile(t, root, "bin/run.sh", "#!/bin/bash\nsource ../lib/util.sh\nutil\nlocal x=1\n")
	putFile(t, root, "bin/other.sh", "#!/bin/bash\nlocal y=1\n")
	code, out, errs := runCode(t, "check", filepath.Join(root, "bin", "run.sh"))
	if code != 1 || strings.Contains(out, "other.sh") || strings.Contains(out, "WB1101") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "WB1105") {
		t.Errorf("no WB1105 for the named file:\n%s", out)
	}
}

func TestFindRoot(t *testing.T) {
	tmp := evalDir(t, t.TempDir())
	putFile(t, tmp, "repo/.git/HEAD", "x")
	putFile(t, tmp, "repo/a/b/f.sh", "echo\n")
	putFile(t, tmp, "proj/.bash-lsp.toml", "")
	putFile(t, tmp, "proj/s/g.sh", "echo\n")
	putFile(t, tmp, "loose/h.sh", "echo\n")
	if r, walk := findRoot([]string{filepath.Join(tmp, "repo", "a", "b", "f.sh")}, ""); r != filepath.Join(tmp, "repo") || !walk {
		t.Errorf("repo file: %s %v", r, walk)
	}
	if r, walk := findRoot([]string{filepath.Join(tmp, "proj", "s")}, ""); r != filepath.Join(tmp, "proj") || !walk {
		t.Errorf("proj dir: %s %v", r, walk)
	}
	if r, walk := findRoot([]string{filepath.Join(tmp, "loose", "h.sh")}, ""); r != filepath.Join(tmp, "loose") || walk {
		t.Errorf("loose file: %s %v (no marker: no walk)", r, walk)
	}
	if r, walk := findRoot([]string{filepath.Join(tmp, "loose")}, ""); r != filepath.Join(tmp, "loose") || !walk {
		t.Errorf("loose dir: %s %v (a directory is walked)", r, walk)
	}
	if r, walk := findRoot([]string{filepath.Join(tmp, "loose", "h.sh")}, filepath.Join(tmp, "repo")); r != filepath.Join(tmp, "repo") || !walk {
		t.Errorf("--root: %s %v", r, walk)
	}
}

// doctor reads the PATH directories once.
func TestDoctorBuildsOneProfile(t *testing.T) {
	emptyPath(t)
	root := t.TempDir()
	putFile(t, root, "a.sh", "echo\n")
	n := 0
	old := profileFor
	profileFor = func(st config.Settings) *extool.Profile { n++; return old(st) }
	defer func() { profileFor = old }()
	runOK(t, "doctor", root)
	if n != 1 {
		t.Errorf("doctor built %d PATH profiles, want 1", n)
	}
}
