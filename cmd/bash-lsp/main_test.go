package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "bash-lsp dev" {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"nope"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "usage") {
		t.Fatalf("stderr lacks usage: %q", errb.String())
	}
}

func frame(body string) string { return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body) }

// With no arguments the binary serves LSP on stdio: stdout carries only frames.
func TestNoArgumentsServes(t *testing.T) {
	in := frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`) +
		frame(`{"jsonrpc":"2.0","id":2,"method":"shutdown"}`) + frame(`{"jsonrpc":"2.0","method":"exit"}`)
	for _, args := range [][]string{nil, {"serve"}} {
		var out, errb bytes.Buffer
		if code := run(args, strings.NewReader(in), &out, &errb); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errb.String())
		}
		if !strings.HasPrefix(out.String(), "Content-Length: ") || !strings.Contains(out.String(), `"name":"bash-lsp"`) {
			t.Fatalf("%v: stdout %q", args, out.String())
		}
	}
}

func TestServeEndOfInputExitsNonZero(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if out.Len() != 0 || !strings.Contains(errb.String(), "input ended") {
		t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
	}
}

func runCode(t *testing.T, args ...string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, strings.NewReader(""), &o, &e)
	return code, o.String(), e.String()
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errs := runCode(t, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, errs)
	}
	return out
}

func wantLines(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, s)
	}
}

func TestAstText(t *testing.T) {
	out := runOK(t, "ast", "../../testdata/corpus/lib/util.sh")
	wantLines(t, out, `File 1:1-`, `  FuncDecl 3:1-3:34 "log"`, `Comment 2:1-2:27 "Log a message to stderr."`)
}

func TestAstJSON(t *testing.T) {
	type node struct {
		Kind      string
		Start     [2]int
		End       [2]int
		Name      string
		Value     string
		Recovered bool
		Children  []node
	}
	var r struct {
		Path    string
		Dialect string
		Errors  []struct {
			Line, Col int
			Msg       string
		}
		Root node
	}
	mustJSON(t, runOK(t, "ast", "../../testdata/corpus/lib/util.sh", "--json"), &r)
	if r.Root.Kind != "File" || r.Dialect != "bash" || len(r.Errors) != 0 || r.Path == "" {
		t.Fatalf("%+v", r)
	}
	if r.Root.Start != [2]int{1, 1} {
		t.Fatalf("root start %v", r.Root.Start)
	}
	var find func(n *node) *node
	find = func(n *node) *node {
		if n.Kind == "FuncDecl" && n.Name == "log" {
			return n
		}
		for i := range n.Children {
			if got := find(&n.Children[i]); got != nil {
				return got
			}
		}
		return nil
	}
	fn := find(&r.Root)
	if fn == nil || fn.Start != [2]int{3, 1} || fn.End != [2]int{3, 34} {
		t.Fatalf("FuncDecl log: %+v", fn)
	}
}

func TestAstReportsRecoveredNodesAndErrors(t *testing.T) {
	text := runOK(t, "ast", "../../testdata/corpus/broken_fi.sh")
	wantLines(t, text, " (recovered)")
	var r struct {
		Errors []struct {
			Line, Col int
			Msg       string
		}
	}
	mustJSON(t, runOK(t, "ast", "../../testdata/corpus/broken_fi.sh", "--json"), &r)
	if len(r.Errors) == 0 || r.Errors[0].Msg == "" || r.Errors[0].Line < 1 {
		t.Fatalf("%+v", r)
	}
}

func TestAstUsage(t *testing.T) {
	if code, _, _ := runCode(t, "ast"); code != 2 {
		t.Fatalf("no file: exit %d", code)
	}
	if code, _, _ := runCode(t, "ast", "a", "b"); code != 2 {
		t.Fatalf("two files: exit %d", code)
	}
	if code, _, _ := runCode(t, "ast", "--nope", "x"); code != 2 {
		t.Fatalf("bad flag: exit %d", code)
	}
	if code, _, errs := runCode(t, "ast", "../../testdata/corpus/missing.sh"); code != 1 || errs == "" {
		t.Fatalf("missing file: exit %d, %q", code, errs)
	}
}

type symJSON struct {
	Path, Kind, Name, Container string
	Line, Col                   int
}

func TestSymbolsJSON(t *testing.T) {
	var r struct {
		Generation int
		Symbols    []symJSON
	}
	mustJSON(t, runOK(t, "symbols", "../../testdata/corpus", "--json"), &r)
	if r.Generation < 1 {
		t.Fatalf("generation %d", r.Generation)
	}
	has := func(want symJSON) bool {
		for _, s := range r.Symbols {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(symJSON{"lib/util.sh", "function", "build_release", "", 4, 1}) {
		t.Errorf("no build_release in lib/util.sh: %+v", r.Symbols)
	}
	if !has(symJSON{"main.sh", "variable", "name", "deploy", 5, 9}) {
		t.Errorf("no local name in deploy: %+v", r.Symbols)
	}
	for _, s := range r.Symbols {
		if strings.Contains(s.Path, `\`) || strings.HasPrefix(s.Path, "/") {
			t.Errorf("path not root-relative with forward slashes: %q", s.Path)
		}
	}
}

func TestSymbolsText(t *testing.T) {
	out := runOK(t, "symbols", "../../testdata/corpus")
	wantLines(t, out, "lib/util.sh:4:1  function  build_release", "main.sh:4:1  function  deploy",
		"\n  main.sh:5:9  variable  name")
}

func TestSymbolsSingleFileAndDefault(t *testing.T) {
	var r struct{ Symbols []symJSON }
	mustJSON(t, runOK(t, "symbols", "../../testdata/corpus/main.sh", "--json"), &r)
	if len(r.Symbols) == 0 {
		t.Fatal("no symbols")
	}
	for _, s := range r.Symbols {
		if s.Path != "main.sh" {
			t.Fatalf("symbol outside the named file: %+v", s)
		}
	}
	// No path means ".": the package directory has no shell files, so the
	// list is empty but the command succeeds.
	var empty struct{ Symbols []symJSON }
	mustJSON(t, runOK(t, "symbols", "--json"), &empty)
	if len(empty.Symbols) != 0 {
		t.Fatalf("%+v", empty)
	}
}

func TestSymbolsBadPath(t *testing.T) {
	if code, _, errs := runCode(t, "symbols", "../../testdata/nope"); code != 1 || errs == "" {
		t.Fatalf("exit %d, %q", code, errs)
	}
}

func TestDoctor(t *testing.T) {
	out := runOK(t, "doctor", "../../testdata/corpus")
	wantLines(t, out, "version: dev", "root: ", "files: 9", "bash: 8", "sh: 1", "syntax errors: 2 files",
		"unresolved sources: 0", "shellcheck: not used in this version")
}

func TestDoctorBadRoot(t *testing.T) {
	if code, _, errs := runCode(t, "doctor", "../../testdata/no-such-dir"); code != 1 || errs == "" {
		t.Fatalf("exit %d, %q", code, errs)
	}
}

func TestDoctorListsUnresolvedSources(t *testing.T) {
	dir := t.TempDir()
	body := "#!/bin/bash\nsource ./missing.sh\n"
	if err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runOK(t, "doctor", dir)
	wantLines(t, out, "unresolved sources: 1", "\n  a.sh:2\n")
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"symbols", "--nope"}, {"doctor", "a", "b"}, {"doctor", "--json"}} {
		if code, _, _ := runCode(t, args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// manyComments is a script of n comment lines with a function before, between
// and after them.
func manyComments(t testing.TB, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/bash\nfirst() { :; }\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "# c%d\n", i)
		if i == n/2 {
			b.WriteString("middle() { :; }\n")
		}
	}
	b.WriteString("last() { :; }\n")
	path := filepath.Join(t.TempDir(), "many.sh")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAstManyCommentsKeepsCountAndOrder(t *testing.T) {
	const n = 20000
	out := runOK(t, "ast", manyComments(t, n))
	var got []string
	var order []string // functions and comments in output order, depth 1 only
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  Comment ") {
			got = append(got, l[strings.Index(l, `"`):])
			order = append(order, "c")
		}
		if strings.HasPrefix(l, "    FuncDecl ") {
			order = append(order, l[strings.Index(l, `"`):])
		}
	}
	// The shebang is a comment too.
	if len(got) != n+1 {
		t.Fatalf("%d top-level comments, want %d", len(got), n+1)
	}
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("\"c%d\"", i); got[i+1] != want {
			t.Fatalf("comment %d is %s, want %s", i+1, got[i+1], want)
		}
	}
	if order[1] != `"first"` || order[2+n/2+1] != `"middle"` || order[len(order)-1] != `"last"` {
		t.Fatalf("function order wrong: first=%s middle=%s last=%s", order[1], order[2+n/2+1], order[len(order)-1])
	}
}

func BenchmarkAstManyComments(b *testing.B) {
	path := manyComments(b, 20000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out, errb bytes.Buffer
		if code := run([]string{"ast", path, "--json"}, strings.NewReader(""), &out, &errb); code != 0 {
			b.Fatalf("exit %d: %s", code, errb.String())
		}
	}
}
