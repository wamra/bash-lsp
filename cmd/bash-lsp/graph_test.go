package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/workspace"
)

const corpus = "../../testdata/corpus"

type gSite struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

type gNode struct {
	Path     string  `json:"path"`
	Function *string `json:"function"`
}

type gEdge struct {
	Caller     *gNode `json:"caller"`
	Callee     *gNode `json:"callee"`
	Relation   string `json:"relation"`
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
	Site       gSite  `json:"site"`
	Text       string `json:"text"`
}

type gEdges struct {
	Generation uint64  `json:"generation"`
	Results    []gEdge `json:"results"`
}

func (n *gNode) String() string {
	if n == nil {
		return "-"
	}
	if n.Function == nil {
		return n.Path
	}
	return n.Path + ":" + *n.Function
}

func (e gEdge) String() string {
	return e.Caller.String() + " -> " + e.Callee.String() + " @" + e.Site.Path + ":" + itoa(e.Site.Line) + " " + e.Relation + " " + e.Resolution + "/" + e.Reason
}

func itoa(n int) string { return strconv.Itoa(n) }

func edgeLines(es []gEdge) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.String())
	}
	return out
}

func labels(es []gEdge) []string {
	set := map[string]bool{}
	for _, e := range es {
		set[e.Resolution+"/"+e.Reason] = true
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// writeRoot makes a temp root holding files (name -> text).
func writeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCallersACLabelSets(t *testing.T) {
	var r gEdges
	mustJSON(t, runOK(t, "callers", "lib/util.sh:4:1", "--root", corpus, "--json"), &r)
	if r.Generation == 0 {
		t.Errorf("generation 0")
	}
	var fromMain []gEdge
	for _, e := range r.Results {
		if e.Site.Path == "main.sh" || e.Site.Path == "dyn.sh" {
			fromMain = append(fromMain, e)
		}
		if e.Site.Path == "orphan.sh" {
			t.Errorf("orphan.sh edge in the callers of lib/util.sh build_release: %v", e)
		}
	}
	want := []string{"candidate/string-eval", "candidate/variable-constant", "resolved/alias", "resolved/direct"}
	if got := labels(fromMain); !slices.Equal(got, want) {
		t.Fatalf("labels %v, want %v\n%v", got, want, edgeLines(r.Results))
	}
	// Each label sits on the site AC-03 names.
	at := map[string]int{}
	for _, e := range fromMain {
		if e.Site.Path == "main.sh" {
			at[e.Resolution+"/"+e.Reason] = e.Site.Line
		}
	}
	for l, line := range map[string]int{"resolved/direct": 6, "resolved/alias": 7, "candidate/variable-constant": 9, "candidate/string-eval": 10} {
		if at[l] != line {
			t.Errorf("%s at main.sh line %d, want %d", l, at[l], line)
		}
	}
}

func TestCallersStringTrap(t *testing.T) {
	var r gEdges
	mustJSON(t, runOK(t, "callers", "lib/util.sh:3:1", "--root", corpus, "--json"), &r)
	found := false
	for _, e := range r.Results {
		if e.Resolution == "candidate" && e.Reason == "string-trap" && e.Site.Path == "main.sh" && e.Site.Line == 11 {
			found = true
			if e.Caller.String() != "main.sh:deploy" || e.Callee.String() != "lib/util.sh:log" || e.Relation != "calls" {
				t.Errorf("trap edge %v", e)
			}
		}
	}
	if !found {
		t.Fatalf("no candidate/string-trap edge for log: %v", edgeLines(r.Results))
	}
}

func TestCallersText(t *testing.T) {
	out := runOK(t, "callers", "lib/util.sh:4:1", "--root", corpus)
	wantLines(t, out,
		"main.sh:6:3  main.sh:deploy -> lib/util.sh:build_release  resolved/direct\n",
		"main.sh:7:3  main.sh:deploy -> lib/util.sh:build_release  resolved/alias\n",
		"main.sh:10:9  main.sh:deploy -> lib/util.sh:build_release  candidate/string-eval\n")
	if strings.Contains(out, "orphan.sh") {
		t.Errorf("orphan.sh in output:\n%s", out)
	}
}

func TestAmbiguousSymbolExits2(t *testing.T) {
	for _, cmd := range []string{"callers", "callees", "refs", "impact"} {
		code, out, errs := runCode(t, cmd, "build_release", "--root", corpus)
		if code != 2 || out != "" {
			t.Fatalf("%s: exit %d stdout %q", cmd, code, out)
		}
		wantLines(t, errs, "lib/util.sh:4:1  build_release\n", "orphan.sh:2:1  build_release\n")
	}
}

func TestNotFoundExits1(t *testing.T) {
	for _, cmd := range []string{"callers", "callees", "refs", "impact"} {
		code, out, errs := runCode(t, cmd, "no_such_fn", "--root", corpus)
		if code != 1 || out != "" || !strings.Contains(errs, `"no_such_fn"`) {
			t.Fatalf("%s: exit %d stdout %q stderr %q", cmd, code, out, errs)
		}
	}
	if code, _, errs := runCode(t, "trace", "deploy", "no_such_fn", "--root", corpus); code != 1 || !strings.Contains(errs, `"no_such_fn"`) {
		t.Fatalf("trace: exit %d %q", code, errs)
	}
}

func TestPositionOutOfRangeExits1(t *testing.T) {
	code, _, errs := runCode(t, "callers", "main.sh:99:1", "--root", corpus)
	if code != 1 || !strings.Contains(errs, `"main.sh:99:1"`) {
		t.Fatalf("exit %d %q", code, errs)
	}
}

func TestCalleesTextAndJSON(t *testing.T) {
	out := runOK(t, "callees", "main.sh:4:1", "--root", corpus)
	wantLines(t, out, "main.sh:6:3  main.sh:deploy -> lib/util.sh:build_release  resolved/direct\n")
	var r gEdges
	mustJSON(t, runOK(t, "callees", "deploy", "--root", corpus, "--json"), &r)
	if len(r.Results) == 0 {
		t.Fatal("no callees")
	}
	for _, e := range r.Results {
		if e.Caller.String() != "main.sh:deploy" || e.Caller.Function == nil {
			t.Errorf("caller %v", e.Caller)
		}
	}
	// Top level of a file: caller function is null.
	mustJSON(t, runOK(t, "callees", "main.sh", "--root", corpus, "--json"), &r)
	sawTop := false
	for _, e := range r.Results {
		if e.Caller.Function == nil && e.Caller.Path == "main.sh" {
			sawTop = true
		}
	}
	if !sawTop {
		t.Errorf("no top-level edge: %v", edgeLines(r.Results))
	}
}

func TestDepth(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": "a() { b; }\nb() { c; }\nc() { :; }\na\n"})
	count := func(args ...string) int {
		var r gEdges
		mustJSON(t, runOK(t, append(args, "--root", root, "--json")...), &r)
		return len(r.Results)
	}
	if n := count("callees", "a"); n != 1 {
		t.Errorf("callees default depth: %d edges, want 1", n)
	}
	if n := count("callees", "a", "--depth", "2"); n != 2 {
		t.Errorf("callees depth 2: %d, want 2", n)
	}
	if n := count("callees", "a", "--depth", "0"); n != 2 {
		t.Errorf("callees depth 0 (unlimited): %d, want 2", n)
	}
	var imp struct {
		Results []gNode `json:"results"`
	}
	mustJSON(t, runOK(t, "impact", "c", "--root", root, "--json"), &imp)
	if len(imp.Results) != 3 { // b, a, a.sh
		t.Errorf("impact default (unlimited): %d nodes", len(imp.Results))
	}
	mustJSON(t, runOK(t, "impact", "c", "--depth", "1", "--root", root, "--json"), &imp)
	if len(imp.Results) != 1 || imp.Results[0].String() != "a.sh:b" {
		t.Errorf("impact depth 1: %v", imp.Results)
	}
	for _, bad := range [][]string{{"callers", "a", "--depth", "-1"}, {"impact", "a", "--depth", "x"}, {"callers", "a", "--depth"}} {
		code, _, errs := runCode(t, append(bad, "--root", root)...)
		if code != 2 || !strings.Contains(errs, "usage") {
			t.Errorf("%v: exit %d %q", bad, code, errs)
		}
	}
}

func TestRefs(t *testing.T) {
	var r struct {
		Generation uint64 `json:"generation"`
		Results    []struct {
			gSite
			Resolution string `json:"resolution"`
			Reason     string `json:"reason"`
		} `json:"results"`
	}
	mustJSON(t, runOK(t, "refs", "lib/util.sh:4:1", "--root", corpus, "--json"), &r)
	if r.Generation == 0 || len(r.Results) == 0 {
		t.Fatalf("%+v", r)
	}
	got := map[string]string{}
	for _, x := range r.Results {
		if x.Path == "orphan.sh" {
			t.Errorf("orphan.sh reference %+v", x)
		}
		if x.Path == "main.sh" {
			got[itoa(x.Line)] = x.Resolution + "/" + x.Reason
		}
	}
	for line, label := range map[string]string{"6": "resolved/direct", "7": "resolved/alias", "10": "candidate/string-eval"} {
		if got[line] != label {
			t.Errorf("main.sh line %s: %q, want %q (all %v)", line, got[line], label, got)
		}
	}
	if _, ok := got["3"]; !ok {
		t.Errorf("alias definition (line 3) missing: %v", got)
	}
	text := runOK(t, "refs", "lib/util.sh:4:1", "--root", corpus)
	wantLines(t, text, "main.sh:6:3  build_release  calls  resolved/direct\n")
}

func TestImpactTraceUnresolvedCycles(t *testing.T) {
	var imp struct {
		Generation uint64  `json:"generation"`
		Results    []gNode `json:"results"`
	}
	mustJSON(t, runOK(t, "impact", "lib/util.sh:4:1", "--root", corpus, "--json"), &imp)
	var names []string
	for _, n := range imp.Results {
		names = append(names, n.String())
	}
	if imp.Generation == 0 || !slices.Contains(names, "main.sh") || !slices.Contains(names, "main.sh:deploy") {
		t.Errorf("impact of lib/util.sh: %v", names)
	}
	wantLines(t, runOK(t, "impact", "lib/util.sh:4:1", "--root", corpus), "main.sh:deploy\n", "main.sh\n")
	// A file's impact is what sources or invokes it.
	wantLines(t, runOK(t, "impact", "lib/util.sh", "--root", corpus), "dyn.sh\n", "main.sh\n")

	var tr gEdges
	mustJSON(t, runOK(t, "trace", "main.sh", "lib/util.sh:3:1", "--root", corpus, "--json"), &tr)
	if len(tr.Results) < 2 || tr.Results[0].Caller.String() != "main.sh" || tr.Results[len(tr.Results)-1].Callee.String() != "lib/util.sh:log" {
		t.Errorf("trace: %v", edgeLines(tr.Results))
	}
	wantLines(t, runOK(t, "trace", "main.sh", "lib/util.sh:3:1", "--root", corpus), "main.sh:21:1  main.sh -> main.sh:deploy  resolved/direct\n", "main.sh:11:9  main.sh:deploy -> lib/util.sh:log  candidate/string-trap\n")
	// No path: an empty answer, exit 0.
	mustJSON(t, runOK(t, "trace", "orphan.sh", "main.sh", "--root", corpus, "--json"), &tr)
	if tr.Results == nil || len(tr.Results) != 0 {
		t.Errorf("no-path trace results %#v, want []", tr.Results)
	}

	var un gEdges
	uroot := writeRoot(t, map[string]string{"a.sh": "source ./missing.sh\nrun() { $1; }\n", "b.sh": ":\n"})
	mustJSON(t, runOK(t, "unresolved", "--root", uroot, "--json"), &un)
	if len(un.Results) != 2 {
		t.Fatalf("unresolved: %v", edgeLines(un.Results))
	}
	for _, e := range un.Results {
		if e.Resolution != "dynamic" && e.Resolution != "unresolved" || e.Callee != nil || e.Reason == "" {
			t.Errorf("edge %+v", e)
		}
	}
	mustJSON(t, runOK(t, "unresolved", "b.sh", "--root", uroot, "--json"), &un)
	if un.Results == nil || len(un.Results) != 0 {
		t.Errorf("unresolved b.sh: %#v, want []", un.Results)
	}
	wantLines(t, runOK(t, "unresolved", "a.sh", "--root", uroot), "a.sh:1:8  a.sh -> ./missing.sh  unresolved/", "a.sh:2:", "  dynamic/")

	var cy struct {
		Generation uint64 `json:"generation"`
		Results    []struct {
			Files []string `json:"files"`
		} `json:"results"`
	}
	mustJSON(t, runOK(t, "cycles", "--root", corpus, "--json"), &cy)
	if cy.Results == nil || len(cy.Results) != 0 {
		t.Errorf("corpus cycles %#v, want []", cy.Results)
	}
	root := writeRoot(t, map[string]string{"a.sh": "source ./b.sh\n", "b.sh": "source ./a.sh\n"})
	mustJSON(t, runOK(t, "cycles", "--root", root, "--json"), &cy)
	if len(cy.Results) != 1 || !slices.Equal(cy.Results[0].Files, []string{"a.sh", "b.sh"}) {
		t.Errorf("cycles %+v", cy.Results)
	}
	wantLines(t, runOK(t, "cycles", "--root", root), "a.sh, b.sh\n")
}

func TestStatus(t *testing.T) {
	var st struct {
		Generation uint64         `json:"generation"`
		Root       string         `json:"root"`
		Files      int            `json:"files"`
		Dialects   map[string]int `json:"dialects"`
		Skipped    int            `json:"skipped"`
		Limits     []string       `json:"limits"`
		Nesting    int            `json:"nesting_errors"`
		Cache      string         `json:"cache"`
	}
	mustJSON(t, runOK(t, "status", "--root", corpus, "--json"), &st)
	if st.Generation == 0 || st.Files == 0 || st.Cache != "none" || st.Dialects["bash"] == 0 || st.Limits == nil {
		t.Errorf("%+v", st)
	}
	out := runOK(t, "status", "--root", corpus)
	wantLines(t, out, "generation: ", "cache: none\n", "bash: ")
}

// `unresolved` lists the sites past the per-site candidate cap. (`status`
// prints Snapshot.LimitsHit, which has the `candidate cap` line, from task 10 on.)
func TestUnresolvedListsCandidateCap(t *testing.T) {
	root := writeRoot(t, map[string]string{"a.sh": strings.Repeat("f(){ :; }\n", 17) + "f\nf\n"})
	var un gEdges
	mustJSON(t, runOK(t, "unresolved", "--root", root, "--json"), &un)
	if len(un.Results) != 2 {
		t.Fatalf("unresolved: %v", edgeLines(un.Results))
	}
	for _, e := range un.Results {
		if e.Resolution != "dynamic" || e.Reason != "candidate-cap" || e.Text != "f" || e.Callee != nil {
			t.Errorf("edge %+v", e)
		}
	}
}

func TestStatusReportsNestingErrors(t *testing.T) {
	deep := strings.Repeat("(", 10005) + ":" + strings.Repeat(")", 10005) + "\n"
	root := writeRoot(t, map[string]string{"deep.sh": deep, "ok.sh": ":\n"})
	var st struct {
		Nesting int `json:"nesting_errors"`
	}
	mustJSON(t, runOK(t, "status", "--root", root, "--json"), &st)
	if st.Nesting != 1 {
		t.Errorf("nesting_errors %d, want 1", st.Nesting)
	}
	wantLines(t, runOK(t, "status", "--root", root), "nesting errors: 1\n")
}

func TestPathOutsideRootRefused(t *testing.T) {
	outside := writeRoot(t, map[string]string{"x.sh": "x() { :; }\n"})
	root := writeRoot(t, map[string]string{"a.sh": "a() { :; }\n"})
	cases := [][]string{
		{"callers", filepath.Join(outside, "x.sh")},
		{"callers", filepath.Join(outside, "x.sh") + ":1:1"},
		{"impact", filepath.Join(outside, "x.sh")},
		{"callees", "../" + filepath.Base(outside) + "/x.sh"},
		{"trace", "a.sh", filepath.Join(outside, "x.sh")},
		{"unresolved", filepath.Join(outside, "x.sh")},
	}
	for _, c := range cases {
		code, out, errs := runCode(t, append(c, "--root", root)...)
		if code != 1 || out != "" || !strings.Contains(errs, "outside the root") {
			t.Errorf("%v: exit %d stdout %q stderr %q", c, code, out, errs)
		}
	}
}

func TestNoCommandExecutesOrSources(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "MARKER")
	root := writeRoot(t, map[string]string{
		"a.sh": "source ./b.sh\nrun() { ./b.sh; eval \"touch " + marker + "\"; }\nrun\ntouch " + marker + "\n",
		"b.sh": "#!/bin/bash\ntouch " + marker + "\nsource ./a.sh\n",
	})
	for _, args := range [][]string{
		{"status"}, {"callers", "run"}, {"callees", "run"}, {"refs", "run"}, {"impact", "b.sh"},
		{"trace", "a.sh", "b.sh"}, {"unresolved"}, {"cycles"},
	} {
		runOK(t, append(args, "--root", root, "--json")...)
		runOK(t, append(args, "--root", root)...)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an analysed command ran: marker exists")
	}
}

func TestGraphUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"callers"}, {"callers", "a", "b"}, {"trace", "a"}, {"cycles", "x"}, {"status", "x"},
		{"callers", "a", "--nope"}, {"callers", "a", "--root"}, {"cycles", "--depth", "1"},
	} {
		code, _, errs := runCode(t, append(args, "--root", corpus)...)
		if code != 2 || !strings.Contains(errs, "usage") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	if code, _, _ := runCode(t, "callers", "a", "--root", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Errorf("missing root: exit %d, want 1", code)
	}
}

func writeUnicodeRoot(t *testing.T) string {
	return writeRoot(t, map[string]string{
		// f is called at byte column 24, UTF-16 column 19 of line 3; g is at
		// byte column 7, UTF-16 column 6 of line 4; line 6 holds a dynamic call.
		"uni.sh":  "#!/bin/bash\nf() { :; }\necho \"héllo ☃😀\"; f\né=1; g() { :; }\ng\necho \"😀\"; $1\n",
		"crlf.sh": "f() { :; }\r\necho \"é😀\"; f\r\n",
	})
}

var posRe = regexp.MustCompile(`^(.+):(\d+):(\d+)$`)

// printedPositions runs each command and returns every "path:line:col" it
// printed, from the JSON form.
func printedPositions(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	add := func(cmd string, sites ...gSite) {
		for _, s := range sites {
			out[cmd] = append(out[cmd], s.Path+":"+itoa(s.Line)+":"+itoa(s.Col))
		}
	}
	edgeSites := func(args ...string) []gSite {
		var r gEdges
		mustJSON(t, runOK(t, append(args, "--root", root, "--json")...), &r)
		var ss []gSite
		for _, e := range r.Results {
			ss = append(ss, e.Site)
		}
		return ss
	}
	add("callers", edgeSites("callers", "uni.sh:2:1")...)
	add("callers", edgeSites("callers", "crlf.sh:1:1")...)
	add("callees", edgeSites("callees", "uni.sh")...)
	add("callees", edgeSites("callees", "crlf.sh")...)
	add("trace", edgeSites("trace", "uni.sh", "uni.sh:2:1")...)
	add("unresolved", edgeSites("unresolved")...)
	var refs struct {
		Results []gSite `json:"results"`
	}
	mustJSON(t, runOK(t, "refs", "uni.sh:2:1", "--root", root, "--json"), &refs)
	add("refs", refs.Results...)
	var syms struct {
		Results []gSite `json:"results"`
		Symbols []struct {
			gSite
		} `json:"symbols"`
	}
	mustJSON(t, runOK(t, "symbols", root, "--json"), &syms)
	for _, s := range syms.Symbols {
		add("symbols", s.gSite)
	}
	return out
}

func TestPrintedPositionsRoundTripAsSpecs(t *testing.T) {
	root := writeUnicodeRoot(t)
	ps := printedPositions(t, root)
	for _, cmd := range []string{"callers", "callees", "trace", "unresolved", "refs", "symbols"} {
		if len(ps[cmd]) == 0 {
			t.Fatalf("%s printed no position", cmd)
		}
	}
	// The call of f after the surrogate pair is printed at UTF-16 column 19.
	if !slices.Contains(ps["callers"], "uni.sh:3:19") || !slices.Contains(ps["callers"], "crlf.sh:2:13") {
		t.Fatalf("callers positions %v", ps["callers"])
	}
	if !slices.Contains(ps["symbols"], "uni.sh:4:6") {
		t.Fatalf("symbols positions %v", ps["symbols"])
	}
	// Every printed position is accepted as a spec.
	for cmd, list := range ps {
		for _, pos := range list {
			if code, _, errs := runCode(t, "callees", pos, "--root", root); code != 0 {
				t.Errorf("%s printed %s, which callees rejects: %s", cmd, pos, errs)
			}
		}
	}
	// And resolves to the same place: the call site of f is f; g's symbol is g.
	same := func(a, b string) {
		t.Helper()
		if x, y := runOK(t, "callers", a, "--root", root), runOK(t, "callers", b, "--root", root); x != y || x == "" {
			t.Errorf("callers %s and callers %s differ:\n%s\n%s", a, b, x, y)
		}
	}
	same("uni.sh:3:19", "uni.sh:2:1")
	same("crlf.sh:2:13", "crlf.sh:1:1")
	var refs struct {
		Results []gSite `json:"results"`
	}
	mustJSON(t, runOK(t, "refs", "uni.sh:4:6", "--root", root, "--json"), &refs)
	if len(refs.Results) != 1 || refs.Results[0].Line != 5 {
		t.Errorf("refs of the symbol printed at uni.sh:4:6: %v", refs.Results)
	}
	// Text output prints the same positions.
	for _, line := range strings.Split(strings.TrimSpace(runOK(t, "callers", "uni.sh:2:1", "--root", root)), "\n") {
		if !strings.HasPrefix(line, "uni.sh:3:19  ") {
			t.Errorf("text line %q", line)
		}
	}
	wantLines(t, runOK(t, "symbols", root), "uni.sh:4:6  function  g")
	// ast prints UTF-16 columns too.
	wantLines(t, runOK(t, "ast", filepath.Join(root, "uni.sh")), "FuncDecl 4:6-")
	var a struct {
		Root struct {
			Children []struct {
				Kind  string `json:"kind"`
				Start [2]int `json:"start"`
			} `json:"children"`
		} `json:"root"`
	}
	mustJSON(t, runOK(t, "ast", filepath.Join(root, "uni.sh"), "--json"), &a)
	found := false
	for _, k := range a.Root.Children {
		if k.Start == [2]int{4, 6} { // `g() { :; }` after "é=1; "
			found = true
		}
	}
	if !found {
		t.Errorf("ast JSON: %+v", a.Root.Children)
	}
}

func TestRefsRejectsDepth(t *testing.T) {
	code, _, errs := runCode(t, "refs", "lib/util.sh:4:1", "--depth", "1", "--root", corpus)
	if code != 2 || !strings.Contains(errs, "usage") || strings.Contains(strings.SplitN(errs, "usage", 2)[1], "refs SYMBOL [--depth") {
		t.Errorf("exit %d %q", code, errs)
	}
}

func TestRefsTextShowsRelation(t *testing.T) {
	out := runOK(t, "refs", "lib/util.sh:4:1", "--root", corpus)
	wantLines(t, out,
		"main.sh:3:10  build_release  aliases  resolved/",
		"main.sh:6:3  build_release  calls  resolved/direct\n",
		"main.sh:10:9  build_release  calls  candidate/string-eval\n")
}

func TestTraceNoPath(t *testing.T) {
	code, out, errs := runCode(t, "trace", "orphan.sh", "main.sh", "--root", corpus)
	if code != 0 || out != "no path\n" || errs != "" {
		t.Errorf("text: exit %d stdout %q stderr %q", code, out, errs)
	}
	var tr struct {
		Found   *bool   `json:"found"`
		Results []gEdge `json:"results"`
	}
	mustJSON(t, runOK(t, "trace", "orphan.sh", "main.sh", "--root", corpus, "--json"), &tr)
	if tr.Found == nil || *tr.Found || tr.Results == nil || len(tr.Results) != 0 {
		t.Errorf("no path JSON: %+v", tr)
	}
	mustJSON(t, runOK(t, "trace", "main.sh", "lib/util.sh:3:1", "--root", corpus, "--json"), &tr)
	if tr.Found == nil || !*tr.Found || len(tr.Results) == 0 {
		t.Errorf("path JSON: %+v", tr)
	}
}

type statusJSON struct {
	Skipped      int `json:"skipped"`
	SkippedFiles []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"skipped_files"`
	Limits []string `json:"limits"`
}

func TestStatusListsSkippedFiles(t *testing.T) {
	root := writeRoot(t, map[string]string{"z/a.zsh": "#!/bin/zsh\n:\n", "b.sh": ":\n"})
	var st statusJSON
	mustJSON(t, runOK(t, "status", "--root", root, "--json"), &st)
	if st.Skipped != 1 || len(st.SkippedFiles) != 1 || st.SkippedFiles[0].Path != "z/a.zsh" || st.SkippedFiles[0].Reason != "zsh is not supported yet" {
		t.Errorf("%+v", st)
	}
	wantLines(t, runOK(t, "status", "--root", root), "skipped: 1\n", "  z/a.zsh: zsh is not supported yet\n")
}

func TestStatusSizeAndFileCountLimits(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"a.sh": ":\n", "b.sh": ":\n", "big.sh": "# " + strings.Repeat("x", 200) + "\n",
	})
	ws, err := workspace.Open(root, workspace.Limits{MaxFiles: 1, MaxFileBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	c := &graphCtx{ws: ws, snap: ws.Snapshot(), root: ws.Root()}
	var out, errb bytes.Buffer
	if code := c.status(graphOpts{json: true}, &out, &errb); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	var st statusJSON
	mustJSON(t, out.String(), &st)
	var got []string
	for _, s := range st.SkippedFiles {
		got = append(got, s.Path+": "+s.Reason)
	}
	want := []string{"b.sh: file limit of 1 reached", "big.sh: 203 bytes exceeds the 100 byte file limit"}
	if !slices.Equal(got, want) || st.Skipped != len(want) {
		t.Errorf("skipped %d %q, want %q", st.Skipped, got, want)
	}
	if len(st.Limits) != 2 || !strings.Contains(strings.Join(st.Limits, "\n"), "big.sh: 203 bytes exceeds") || !strings.Contains(strings.Join(st.Limits, "\n"), "file limit of 1 reached") {
		t.Errorf("limits %q", st.Limits)
	}
	out.Reset()
	c.status(graphOpts{}, &out, &errb)
	wantLines(t, out.String(), "skipped: 2\n", "  b.sh: file limit of 1 reached\n", "  big.sh: 203 bytes exceeds the 100 byte file limit\n", "limit hit: big.sh: 203 bytes exceeds")
}

// refs labels each site with the edge to the queried function, not with
// whichever edge at that site came first (here `unset -f f` relabels the
// edge to f, and the site also calls g).
func TestRefsLabelOfQueriedTarget(t *testing.T) {
	root := writeRoot(t, map[string]string{"v.sh": "f(){ :; }\ng(){ :; }\nunset -f f\nif x; then cmd=f; else cmd=g; fi\n\"$cmd\"\n"})
	out := runOK(t, "refs", "--root", root, "g")
	if !strings.Contains(out, "v.sh:5:1  g  calls  candidate/variable-constant") || strings.Contains(out, "unset-in-view") {
		t.Errorf("refs g:\n%s", out)
	}
	callers := runOK(t, "callers", "--root", root, "g")
	if !strings.Contains(callers, "candidate/variable-constant") {
		t.Fatalf("callers g:\n%s", callers)
	}
	var doc struct {
		Results []jsonRef `json:"results"`
	}
	mustJSON(t, runOK(t, "refs", "--root", root, "--json", "g"), &doc)
	found := false
	for _, r := range doc.Results {
		if r.Line == 5 {
			found = true
			if r.Reason != "variable-constant" || r.Resolution != "candidate" || r.Relation != "calls" {
				t.Errorf("refs g --json at line 5: %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("refs g --json has no line 5: %+v", doc.Results)
	}
	if out := runOK(t, "refs", "--root", root, "f"); !strings.Contains(out, "v.sh:5:1  f  calls  candidate/unset-in-view") {
		t.Errorf("refs f:\n%s", out)
	}
}

// A function redefined in one file: one record per (site, target
// definition), each naming its definition, so the two are told apart.
func TestCallersOfRedefinedFunction(t *testing.T) {
	root := writeRoot(t, map[string]string{"main.sh": "redef() { echo 1; }\nredef() { echo 2; }\nredef\n"})
	for _, cmd := range []string{"callers", "callees"} {
		spec := "redef"
		if cmd == "callees" {
			spec = "main.sh"
		}
		out := strings.Split(strings.TrimSpace(runOK(t, cmd, "--root", root, spec)), "\n")
		if len(out) != 2 || out[0] == out[1] || !strings.Contains(out[0], "main.sh:1:1") || !strings.Contains(out[1], "main.sh:2:1") {
			t.Errorf("%s %s: want two lines naming definitions main.sh:1:1 and main.sh:2:1:\n%s", cmd, spec, strings.Join(out, "\n"))
		}
		var doc struct {
			Results []jsonEdge `json:"results"`
		}
		mustJSON(t, runOK(t, cmd, "--root", root, "--json", spec), &doc)
		var defs []string
		for _, e := range doc.Results {
			if e.Definition == nil {
				t.Fatalf("%s --json: record without definition: %+v", cmd, e)
			}
			defs = append(defs, e.Definition.Path+":"+strconv.Itoa(e.Definition.Line)+":"+strconv.Itoa(e.Definition.Col))
		}
		if strings.Join(defs, ",") != "main.sh:1:1,main.sh:2:1" {
			t.Errorf("%s --json definitions %v", cmd, defs)
		}
	}
}
