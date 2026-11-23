package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
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
	wantLines(t, text, "main.sh:6:3  build_release  resolved/direct\n")
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
