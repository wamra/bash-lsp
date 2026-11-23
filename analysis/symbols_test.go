package analysis

import (
	"strings"
	"testing"
)

func topNames(ds []DocSymbol) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

func childNames(ds []DocSymbol, parent string) []string {
	for _, d := range ds {
		if d.Name == parent {
			return topNames(d.Children)
		}
	}
	return []string{"<no such symbol>"}
}

func TestDocumentSymbolsNestLocals(t *testing.T) { // AC-06
	q := buildCorpus(t)
	ds := q.s.DocumentSymbols(q.abs("main.sh"))
	if names := topNames(ds); !eq(names, "br", "deploy", "other", "unquoted") {
		t.Fatal(names)
	}
	if got := childNames(ds, "deploy"); !eq(got, "name", "cmd") {
		t.Fatal(got)
	}
	if got := childNames(ds, "other"); !eq(got, "name") {
		t.Fatal(got)
	}
	for _, d := range ds {
		if d.Name == "deploy" {
			if d.Kind != DeclFunction || d.Selection.Start.Line != 4 || d.Range.Start.Line != 4 || d.Range.End.Line != 12 {
				t.Fatalf("%+v", d)
			}
			if d.Detail != "deploy <name>" {
				t.Fatalf("detail %q", d.Detail)
			}
		}
		if d.Name == "br" && d.Kind != DeclAlias {
			t.Fatalf("%+v", d)
		}
	}
}

func TestDocumentSymbolsRules(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": `X=1
X=2
export Y=3
f() {
  local a=1
  local a=2
  Z=9
  FOO=1 env
}
f() { :; }
FOO=1 env
read line
`})
	ds := q.s.DocumentSymbols(q.abs("a.sh"))
	// Each function definition is listed (review T8: the second one keeps its
	// own locals); other names once.
	if got := topNames(ds); !eq(got, "X", "Y", "f", "f", "line") {
		t.Fatal(got)
	}
	// Temp is never listed, each name once, function-body globals are not file-level.
	if got := childNames(ds, "f"); !eq(got, "a") {
		t.Fatal(got)
	}
	if q.s.DocumentSymbols(q.abs("missing.sh")) != nil {
		t.Fatal("unknown path")
	}
}

func TestWorkspaceSymbols(t *testing.T) { // C22
	q := buildCorpus(t)
	got := q.s.WorkspaceSymbols("plugin_a", 10)
	if len(got) != 1 || !strings.HasSuffix(got[0].Path, "plugins/a.sh") {
		t.Fatalf("%+v", got)
	}
	if r := q.s.WorkspaceSymbols("bldrel", 10); len(r) == 0 || r[0].Name != "build_release" { // subsequence
		t.Fatalf("%+v", r)
	}
	for _, x := range q.s.WorkspaceSymbols("name", 50) {
		if x.Container != "" {
			t.Fatal("local leaked into workspace symbols")
		}
		if x.Name == "name" {
			t.Fatalf("local %+v listed", x)
		}
	}
	// empty query: functions only
	all := q.s.WorkspaceSymbols("", 0)
	if len(all) == 0 {
		t.Fatal("no functions")
	}
	for _, x := range all {
		if x.Kind != DeclFunction {
			t.Fatalf("non-function in empty query: %+v", x)
		}
	}
	// aliases are matched by a non-empty query
	if r := q.s.WorkspaceSymbols("br", 50); len(r) == 0 || r[0].Name != "br" || r[0].Kind != DeclAlias {
		t.Fatalf("%+v", r)
	}
}

func TestWorkspaceSymbolsRanking(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": "xbuild() { :; }\nbuild() { :; }\nbuild_all() { :; }\nb_u_i_l_d() { :; }\nBUILD=1\n",
		"b.sh": "build() { :; }\nfunc_in_b() { local build=1; }\n",
	})
	var got []string
	for _, x := range q.s.WorkspaceSymbols("build", 0) {
		got = append(got, x.Name+"@"+q.rel(x.Path))
	}
	// exact (case-insensitive: BUILD too) by path then offset, prefix, substring, subsequence
	want := []string{"build@a.sh", "BUILD@a.sh", "build@b.sh", "build_all@a.sh", "xbuild@a.sh", "b_u_i_l_d@a.sh"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if r := q.s.WorkspaceSymbols("build", 2); len(r) != 2 {
		t.Fatalf("limit: %+v", r)
	}
	if r := q.s.WorkspaceSymbols("zzz", 5); len(r) != 0 {
		t.Fatal(r)
	}
}
