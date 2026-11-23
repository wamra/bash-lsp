package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/workspace"
)

var update = flag.Bool("update", false, "rewrite testdata/extract.golden.jsonl from the current output")

// astRec is a copy of the decode struct code-index uses for the output of its
// PowerShell extractor: ~/dev/code-index/internal/indexer/ast.go:34-52
// at code-index HEAD d6ba2fd (comments on the fields dropped). Copied so a test
// can prove every line `bash-lsp extract` writes decodes into it.
type astRec struct {
	T       string `json:"t"`
	File    string `json:"file"`
	Name    string `json:"name"`
	Alias   string `json:"alias"`
	Target  string `json:"target"`
	Path    string `json:"path"`
	Expr    string `json:"expr"`
	In      string `json:"in"`
	Message string `json:"message"`
	First   string `json:"first"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"endLine"`
	EndCol  int    `json:"endCol"`
	Count   int    `json:"count"`
}

// exRec is astRec plus the extra keys extract writes (code-index ignores them).
type exRec struct {
	astRec
	To         string `json:"to"`
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
}

func (r exRec) String() string {
	return r.T + " " + r.File + ":" + itoa(r.Line) + ":" + itoa(r.Col) + " " + r.Name + r.Alias + r.Path + " " + r.Resolution + "/" + r.Reason
}

// decodeExtract decodes every line of out as code-index's reader would and
// fails the test for a line that is not a compact JSON object with a type.
func decodeExtract(t *testing.T, out string) []exRec {
	t.Helper()
	if out != "" && !strings.HasSuffix(out, "\n") {
		t.Fatalf("output does not end with a newline")
	}
	var recs []exRec
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			if out == "" {
				break
			}
			t.Fatalf("line %d is empty", i+1)
		}
		if line[0] != '{' || strings.ContainsAny(line, "\r") {
			t.Fatalf("line %d is not a compact object: %q", i+1, line)
		}
		var a astRec
		if err := json.Unmarshal([]byte(line), &a); err != nil || a.T == "" {
			t.Fatalf("line %d does not decode into astRec (%v): %s", i+1, err, line)
		}
		var r exRec
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if !filepath.IsAbs(r.File) || r.Line < 1 || r.Col < 1 {
			t.Fatalf("line %d: want an absolute file and 1-based line and col: %s", i+1, line)
		}
		switch r.T {
		case "func":
			if r.Name == "" || r.EndLine < r.Line {
				t.Errorf("func without name or end: %s", line)
			}
		case "alias":
			if r.Alias == "" || r.Target == "" || r.EndLine < r.Line {
				t.Errorf("alias without alias, target or end: %s", line)
			}
		case "call":
			if r.Name == "" || r.To == "" || r.Resolution == "" || r.Reason == "" {
				t.Errorf("call without name, to, resolution or reason: %s", line)
			}
		case "dynamic":
			if r.Expr == "" || r.Resolution != "dynamic" || r.Reason == "" {
				t.Errorf("dynamic without expr, resolution or reason: %s", line)
			}
		case "dotsource", "invoke":
			if r.Expr == "" || r.Resolution == "" || r.Reason == "" {
				t.Errorf("%s without expr, resolution or reason: %s", r.T, line)
			}
			if r.Resolution == "resolved" && !filepath.IsAbs(r.Path) {
				t.Errorf("resolved %s without an absolute path: %s", r.T, line)
			}
		case "parse_error":
			if r.Count < 1 || r.First == "" {
				t.Errorf("parse_error without count or first: %s", line)
			}
		case "error":
			if r.Message == "" {
				t.Errorf("error without message: %s", line)
			}
		default:
			t.Errorf("unknown record type %q", r.T)
		}
		recs = append(recs, r)
	}
	return recs
}

func extractRoot(t *testing.T, root string, extra ...string) []exRec {
	t.Helper()
	return decodeExtract(t, runOK(t, append([]string{"extract", "--root", root}, extra...)...))
}

func find(recs []exRec, typ, file string, line int) []exRec {
	var out []exRec
	for _, r := range recs {
		if r.T == typ && filepath.Base(r.File) == file && r.Line == line {
			out = append(out, r)
		}
	}
	return out
}

func TestExtractCorpusDecodes(t *testing.T) {
	recs := extractRoot(t, corpus)
	counts := map[string]int{}
	for _, r := range recs {
		counts[r.T]++
	}
	for _, typ := range []string{"func", "alias", "call", "dotsource", "parse_error"} {
		if counts[typ] == 0 {
			t.Errorf("no %q record for the corpus; counts %v", typ, counts)
		}
	}
	// Every site AC-03 names is there with its label.
	for _, w := range []struct {
		line          int
		resolution    string
		reason, funcN string
	}{{6, "resolved", "direct", "build_release"}, {7, "resolved", "alias", "build_release"}} {
		got := find(recs, "call", "main.sh", w.line)
		if len(got) != 1 || got[0].Name != w.funcN || got[0].Resolution != w.resolution || got[0].Reason != w.reason ||
			got[0].In != "deploy" || filepath.ToSlash(got[0].To[len(got[0].To)-len("lib/util.sh"):]) != "lib/util.sh" {
			t.Errorf("main.sh:%d calls %v", w.line, got)
		}
	}
	if a := find(recs, "alias", "main.sh", 3); len(a) != 1 || a[0].Alias != "br" || a[0].Target != "build_release" || a[0].Col != 7 {
		t.Errorf("alias br: %v", a)
	}
	if d := find(recs, "dotsource", "main.sh", 2); len(d) != 1 || d[0].Resolution != "resolved" || !strings.HasSuffix(filepath.ToSlash(d[0].Path), "/lib/util.sh") || d[0].Expr != "./lib/util.sh" || d[0].Col != 8 {
		t.Errorf("dotsource: %v", d)
	}
}

func TestExtractNoCallToOrphanFromMain(t *testing.T) {
	recs := extractRoot(t, corpus)
	var fromMain int
	for _, r := range recs {
		if r.T != "call" {
			continue
		}
		if r.Reason == "name-match-outside-closure" {
			t.Errorf("call with the outside-closure reason: %v", r)
		}
		if filepath.Base(r.To) == "orphan.sh" && filepath.Base(r.File) != "orphan.sh" {
			t.Errorf("call into orphan.sh from another file: %v", r)
		}
		if filepath.Base(r.File) == "main.sh" && r.Name == "build_release" {
			fromMain++
		}
	}
	if fromMain == 0 {
		t.Errorf("main.sh has no build_release call records at all")
	}
	if got := find(recs, "call", "orphan.sh", 3); len(got) != 1 || filepath.Base(got[0].To) != "orphan.sh" {
		t.Errorf("orphan.sh's own call: %v", got)
	}
}

// normalise rewrites the absolute root prefix of the path-valued keys to
// "<root>/" with forward slashes, so the golden file holds on every machine,
// and re-encodes each line with sorted keys.
func normaliseExtract(t *testing.T, out, root string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"file", "path", "to"} {
			if s, ok := m[k].(string); ok && strings.HasPrefix(s, root+string(filepath.Separator)) {
				m[k] = "<root>/" + filepath.ToSlash(strings.TrimPrefix(s, root+string(filepath.Separator)))
			}
		}
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func TestExtractGolden(t *testing.T) {
	abs, err := filepath.Abs(corpus)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatal(err)
	}
	got := normaliseExtract(t, runOK(t, "extract", "--root", corpus), root)
	golden := filepath.Join("..", "..", "testdata", "extract.golden.jsonl")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run: go test ./cmd/bash-lsp -run TestExtractGolden -update)", err)
	}
	if string(want) != got {
		t.Errorf("extract output differs from %s (run with -update and review the diff)\n--- want\n%s--- got\n%s", golden, want, got)
	}
}

func TestExtractDeterministicAndOrdered(t *testing.T) {
	first := runOK(t, "extract", "--root", corpus)
	for i := 0; i < 4; i++ {
		if again := runOK(t, "extract", "--root", corpus); again != first {
			t.Fatalf("run %d differs from the first", i+2)
		}
	}
	recs := decodeExtract(t, first)
	var seen []string
	last := ""
	line, col := 0, 0
	for _, r := range recs {
		if r.File != last {
			if slices.Contains(seen, r.File) {
				t.Fatalf("records of %s are not contiguous", r.File)
			}
			if last != "" && r.File < last {
				t.Fatalf("files out of order: %s after %s", r.File, last)
			}
			seen, last, line, col = append(seen, r.File), r.File, 0, 0
		}
		if r.Line < line || (r.Line == line && r.Col < col) {
			t.Errorf("%s: %d:%d after %d:%d", r.File, r.Line, r.Col, line, col)
		}
		line, col = r.Line, r.Col
	}
}

func TestExtractNonASCIIBeforeCallSite(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "f() { :; }\necho \"é😀\"; f\n"})
	recs := extractRoot(t, root)
	got := find(recs, "call", "a.sh", 2)
	// echo "é😀"; f : the e of echo is col 1, f is 12 UTF-16 units later (😀 is 2).
	if len(got) != 1 || got[0].Name != "f" || got[0].Col != 13 {
		t.Fatalf("call f: %v (want col 13, UTF-16 units)", got)
	}
}

func TestExtractCRLF(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"lib.sh":  "g() {\r\n  :\r\n}\r\n",
		"main.sh": "source ./lib.sh\r\nf() {\r\n  g\r\n}\r\nf\r\n",
	})
	recs := extractRoot(t, root)
	if got := find(recs, "call", "main.sh", 3); len(got) != 1 || got[0].Name != "g" || got[0].Col != 3 || got[0].In != "f" {
		t.Errorf("g call: %v", got)
	}
	if got := find(recs, "call", "main.sh", 5); len(got) != 1 || got[0].Name != "f" || got[0].Col != 1 {
		t.Errorf("f call: %v", got)
	}
	if got := find(recs, "dotsource", "main.sh", 1); len(got) != 1 || got[0].Expr != "./lib.sh" || got[0].Col != 8 {
		t.Errorf("dotsource: %v", got)
	}
	if got := find(recs, "func", "main.sh", 2); len(got) != 1 || got[0].EndLine != 4 {
		t.Errorf("func f: %v", got)
	}
}

func TestExtractMapping(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"lib.sh":  "helper() { :; }\n",
		"tool.sh": "#!/bin/bash\n:\n",
		"main.sh": "source ./lib.sh\n" + // 1 resolved
			"source \"$DIR/nope.sh\"\n" + // 2 unresolved, expression
			"source ./missing.sh\n" + // 3 unresolved, literal
			"alias h=helper\n" + // 4
			"run() {\n" + // 5
			"  helper\n" + // 6
			"  h\n" + // 7
			"  \"$unknown\" a\n" + // 8
			"  eval \"$cmd\"\n" + // 9
			"  ls /tmp\n" + // 10 builtin or external: no record
			"  ./tool.sh\n" + // 11
			"}\n" +
			"function other {\n :\n}\n",
	})
	recs := extractRoot(t, root)
	if got := find(recs, "dotsource", "main.sh", 1); len(got) != 1 || got[0].Resolution != "resolved" || filepath.Base(got[0].Path) != "lib.sh" {
		t.Errorf("line 1: %v", got)
	}
	got := find(recs, "dotsource", "main.sh", 2)
	if len(got) != 1 || got[0].Resolution != "unresolved" || got[0].Path != "" || got[0].Expr == "" || !strings.Contains(got[0].Expr, "$DIR/nope.sh") {
		t.Errorf("line 2 (expression): %+v", got)
	}
	got = find(recs, "dotsource", "main.sh", 3)
	if len(got) != 1 || got[0].Resolution != "unresolved" || got[0].Path != "./missing.sh" {
		t.Errorf("line 3 (literal): %+v", got)
	}
	if got := find(recs, "alias", "main.sh", 4); len(got) != 1 || got[0].Alias != "h" || got[0].Target != "helper" || got[0].In != "" {
		t.Errorf("alias: %v", got)
	}
	if got := find(recs, "call", "main.sh", 6); len(got) != 1 || got[0].In != "run" || got[0].Resolution != "resolved" {
		t.Errorf("line 6: %v", got)
	}
	if got := find(recs, "call", "main.sh", 7); len(got) != 1 || got[0].Name != "helper" || got[0].Reason != "alias" {
		t.Errorf("line 7 (through alias, target stays the function): %v", got)
	}
	if got := find(recs, "dynamic", "main.sh", 8); len(got) != 1 || got[0].In != "run" || !strings.Contains(got[0].Expr, "$unknown") {
		t.Errorf("line 8: %v", got)
	}
	if got := find(recs, "dynamic", "main.sh", 9); len(got) != 1 || got[0].Expr != "$cmd" || got[0].Reason != "expansion" {
		t.Errorf("line 9: %v", got)
	}
	if got := find(recs, "call", "main.sh", 10); len(got) != 0 {
		t.Errorf("external command emitted as call: %v", got)
	}
	if got := find(recs, "invoke", "main.sh", 11); len(got) != 1 || filepath.Base(got[0].Path) != "tool.sh" || got[0].In != "run" {
		t.Errorf("invoke: %v", got)
	}
	if got := find(recs, "func", "main.sh", 5); len(got) != 1 || got[0].Name != "run" || got[0].Col != 1 || got[0].EndLine != 12 {
		t.Errorf("func run: %v", got)
	}
	if got := find(recs, "func", "main.sh", 13); len(got) != 1 || got[0].Name != "other" {
		t.Errorf("func other: %v", got)
	}
}

func TestExtractParseErrorAndSkippedFiles(t *testing.T) {
	root := writeRoot(t, map[string]string{"bad.sh": "f() {\n  echo $(\n", "z/a.zsh": "#!/bin/zsh\n:\n", "ok.sh": ":\n"})
	recs := extractRoot(t, root)
	if got := find(recs, "parse_error", "bad.sh", 1); len(got) != 1 || got[0].Count < 1 || got[0].First == "" {
		t.Errorf("parse_error: %v", got)
	}
	if got := find(recs, "error", "a.zsh", 1); len(got) != 1 || got[0].Message != "zsh is not supported yet" {
		t.Errorf("error for the zsh file: %v", got)
	}
	// Over the size cap: through the internal entry point with a small limit.
	big := writeRoot(t, map[string]string{"big.sh": "# " + strings.Repeat("x", 200) + "\n", "a.sh": ":\n"})
	ws, err := workspace.Open(big, workspace.Limits{MaxFileBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := extractWorkspace(ws, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if got := find(decodeExtract(t, out.String()), "error", "big.sh", 1); len(got) != 1 || !strings.Contains(got[0].Message, "exceeds the 100 byte file limit") {
		t.Errorf("error for the big file: %v\n%s", got, out.String())
	}
}

func writeList(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractListFile(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "f() { :; }\nf\n", "b.sh": "g() { :; }\ng\n", "note.txt": "x\n"})
	outside := writeRoot(t, map[string]string{"evil.sh": "h() { :; }\nh\n"})
	ws, err := workspace.Open(root, workspace.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(ws.Root(), "a.sh")
	list := writeList(t, a, "", filepath.Join(outside, "evil.sh")+"\r", filepath.Join(ws.Root(), "note.txt"), filepath.Join(ws.Root(), "gone.sh"), "rel/path.sh")
	code, out, errs := runCode(t, "extract", "--root", root, "--list-file", list)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	recs := decodeExtract(t, out)
	for _, r := range recs {
		if r.File != a {
			t.Errorf("record outside the list: %v", r)
		}
	}
	if len(find(recs, "call", "a.sh", 2)) != 1 {
		t.Errorf("a.sh missing: %v", recs)
	}
	for _, w := range []string{"evil.sh", "outside the root", "note.txt", "gone.sh", "not a file of the workspace", "rel/path.sh", "not an absolute path"} {
		if !strings.Contains(errs, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errs)
		}
	}
	if strings.Contains(out, "evil.sh") {
		t.Errorf("a file outside the root reached stdout")
	}
}

func TestExtractListFileErrors(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": ":\n"})
	if code, _, errs := runCode(t, "extract", "--root", root, "--list-file", filepath.Join(root, "no-such-list")); code != 1 || errs == "" {
		t.Errorf("unreadable list: exit %d %q", code, errs)
	}
	huge := filepath.Join(t.TempDir(), "huge.txt")
	if err := os.WriteFile(huge, bytes.Repeat([]byte("/x\n"), maxListFileBytes/3+10), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := runCode(t, "extract", "--root", root, "--list-file", huge); code != 1 || out != "" || !strings.Contains(errs, "list file") {
		t.Errorf("oversized list: exit %d %q %q", code, out, errs)
	}
	// An empty list restricts the output to nothing.
	if code, out, _ := runCode(t, "extract", "--root", root, "--list-file", writeList(t)); code != 0 || out != "" {
		t.Errorf("empty list: exit %d %q", code, out)
	}
}

func TestExtractUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"extract", "--bogus"}, {"extract", "somefile.sh"}, {"extract", "--root"}, {"extract", "--list-file"}, {"extract", "--json"},
	} {
		if code, _, errs := runCode(t, args...); code != 2 || !strings.Contains(errs, "usage:") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	if code, _, _ := runCode(t, "extract", "--root", filepath.Join(t.TempDir(), "absent")); code != 1 {
		t.Errorf("missing root: exit %d", code)
	}
}

func TestExtractStreamsNotBuilt(t *testing.T) {
	// A writer that fails after the first write: extract must report exit 1.
	code := extractWorkspace(openCorpus(t), nil, failingWriter{}, &bytes.Buffer{})
	if code != 1 {
		t.Errorf("write failure: exit %d", code)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func openCorpus(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.Open(corpus, workspace.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestExtractListFileSymlinkOutsideRoot(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "f() { :; }\nf\n"})
	outside := writeRoot(t, map[string]string{"evil.sh": "h() { :; }\nh\n"})
	link := filepath.Join(root, "link.sh")
	if err := os.Symlink(filepath.Join(outside, "evil.sh"), link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	code, out, errs := runCode(t, "extract", "--root", root, "--list-file", writeList(t, link))
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if !strings.Contains(errs, "link.sh") || !strings.Contains(errs, "outside the root") {
		t.Errorf("stderr lacks the outside-the-root note:\n%s", errs)
	}
}

// keyOrder is the keys of one JSON object line in the order written.
func keyOrder(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", line)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// The key order of a record is documented (docs/code-index-integration.md
// section 3 and the golden-file note): pin it on raw output.
func TestExtractRawKeyOrder(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "alias g=f\nf() { :; }\nrun() {\n  f\n  \"$x\"\n}\nsource ./gone.sh\n"})
	out := runOK(t, "extract", "--root", root)
	want := map[string][]string{
		"func":      {"t", "file", "line", "col", "endLine", "endCol", "name"},
		"alias":     {"t", "file", "line", "col", "endLine", "endCol", "alias", "target"},
		"call":      {"t", "file", "line", "col", "name", "in", "to", "resolution", "reason"},
		"dynamic":   {"t", "file", "line", "col", "expr", "in", "resolution", "reason"},
		"dotsource": {"t", "file", "line", "col", "path", "expr", "resolution", "reason"},
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var r struct{ T string }
		mustJSON(t, line, &r)
		if w, ok := want[r.T]; ok {
			seen[r.T] = true
			if got := keyOrder(t, line); !slices.Equal(got, w) {
				t.Errorf("%s keys %v, want %v\n%s", r.T, got, w, line)
			}
		}
	}
	for typ := range want {
		if !seen[typ] {
			t.Errorf("no %s record in\n%s", typ, out)
		}
	}
}
