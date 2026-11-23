package analysis

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// qworld is a built snapshot plus helpers to address files by relative path.
type qworld struct {
	t    *testing.T
	root string
	s    *Snapshot
}

func (q *qworld) abs(file string) string { return filepath.Join(q.root, filepath.FromSlash(file)) }

func (q *qworld) rel(p string) string {
	r, err := filepath.Rel(q.root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func (q *qworld) src(file string) []byte {
	ff := q.s.Facts(q.abs(file))
	if ff == nil {
		q.t.Fatalf("no facts for %s", file)
	}
	return ff.File.Src
}

// at returns the absolute path and the byte offset of the occ-th needle plus off.
func (q *qworld) at(file, needle string, occ, off int) (string, int) {
	q.t.Helper()
	src := q.src(file)
	pos := -1
	from := 0
	for i := 0; i <= occ; i++ {
		j := bytes.Index(src[from:], []byte(needle))
		if j < 0 {
			q.t.Fatalf("%s: needle %q occurrence %d not found", file, needle, occ)
		}
		pos = from + j
		from = pos + 1
	}
	return q.abs(file), pos + off
}

// loc is "file:line" of the first needle.
func (q *qworld) loc(file, needle string) string {
	q.t.Helper()
	p, off := q.at(file, needle, 0, 0)
	return fmt.Sprintf("%s:%d", file, q.s.Facts(p).File.PosAt(off).Line)
}

func (q *qworld) strs(ls []Location) []string {
	out := []string{}
	for _, l := range ls {
		out = append(out, fmt.Sprintf("%s:%d", q.rel(l.Path), l.Range.Start.Line))
	}
	sort.Strings(out)
	return out
}

func (q *qworld) def(file, needle string, occ, off int) []string {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	return q.strs(q.s.Definition(p, o))
}

func (q *qworld) refs(file, needle string, occ, off int) []string {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	return q.strs(q.s.References(p, o, true))
}

func wantEq(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func wantContains(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("%v does not contain %s", got, w)
		}
	}
}

func buildCorpus(t *testing.T) *qworld {
	t.Helper()
	w := loadDir(t, "../testdata/corpus")
	files := map[string]*FileFacts{}
	for _, ff := range w.facts {
		files[ff.Path] = ff
	}
	return &qworld{t: t, root: w.root, s: Build(files, Options{Root: w.root}, 7)}
}

// buildMem binds in-memory files (names relative to a fake root) and builds a snapshot.
func buildMem(t *testing.T, srcs map[string]string) *qworld {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	files := map[string]*FileFacts{}
	for name, s := range srcs {
		p := filepath.Join(root, filepath.FromSlash(name))
		b := []byte(s)
		files[p] = Bind(front.Parse(p, b, front.Detect(p, b)))
	}
	return &qworld{t: t, root: root, s: Build(files, Options{Root: root}, 1)}
}

func TestCorpusQueries(t *testing.T) {
	q := buildCorpus(t)
	def, refs, loc := q.def, q.refs, q.loc

	// C01, C01d, C29
	wantEq(t, def("main.sh", `  build_release "$name"`, 0, 3), loc("lib/util.sh", "build_release() {"))
	wantEq(t, def("dyn.sh", "build_release dyn", 0, 3), loc("lib/util.sh", "build_release() {"))
	wantEq(t, def("main.sh", "./lib/util.sh", 0, 7), "lib/util.sh:1")
	// C02 (alias) and C25 (orphan keeps its own)
	wantEq(t, def("main.sh", `  br "$name"`, 0, 2), loc("lib/util.sh", "build_release() {"))
	wantEq(t, def("orphan.sh", "build_release", 1, 2), "orphan.sh:2")
	// C04, C04b, C05
	r := refs("main.sh", `  build_release "$name"`, 0, 3)
	for _, x := range r {
		if strings.HasPrefix(x, "orphan.sh") {
			t.Fatalf("orphan leaked: %v", r)
		}
	}
	wantContains(t, r, loc("lib/util.sh", "build_release() {"), loc("main.sh", `  build_release "$name"`),
		loc("main.sh", "alias br="), loc("main.sh", `  br "$name"`), loc("dyn.sh", "build_release dyn"))
	// C08 / AC-04: exactly the three occurrences inside deploy()
	wantEq(t, refs("main.sh", `local name="$1"`, 0, 6), loc("main.sh", `local name="$1"`), loc("main.sh", `  build_release "$name"`), loc("main.sh", `  br "$name"`))
	// C15, C16: library calls a function its sourcing script defines
	wantEq(t, def("lib/hooks.sh", "  on_start", 0, 3), loc("app.sh", "on_start() {"))
	wantContains(t, refs("app.sh", "on_start() {", 0, 2), loc("lib/hooks.sh", "  on_start"))
	// C17: glob loader
	wantEq(t, def("app.sh", "plugin_a", 0, 2), loc("plugins/a.sh", "plugin_a() {"))
}

func TestVisibleAndGraphIncomplete(t *testing.T) {
	q := buildCorpus(t)
	vis := func(f string) []string {
		var out []string
		for _, p := range q.s.Visible(q.abs(f)) {
			out = append(out, q.rel(p))
		}
		return out
	}
	wantEq(t, vis("main.sh"), "main.sh", "lib/util.sh")
	wantEq(t, vis("lib/util.sh"), "lib/util.sh", "main.sh", "dyn.sh")
	wantEq(t, vis("orphan.sh"), "orphan.sh")
	wantEq(t, vis("lib/hooks.sh"), "lib/hooks.sh", "app.sh", "plugins/a.sh")
	if q.s.GraphIncomplete(q.abs("main.sh")) {
		t.Fatal("main.sh graph is complete")
	}
	if q.s.Generation() != 7 {
		t.Fatalf("generation %d", q.s.Generation())
	}
}

func TestCandidateOutsideClosure(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": ". \"$X\"\nhelper\n",
		"b.sh": "helper() { :; }\n",
		"c.sh": "helper\n",
		"d.sh": ". ./e.sh\nhelper\n",
		"e.sh": "",
	})
	for _, f := range []string{"a.sh", "c.sh"} {
		wantEq(t, q.def(f, "helper", 0, 2), "b.sh:1")
		p, off := q.at(f, "helper", 0, 2)
		sym, ok := q.s.SymbolAt(p, off)
		if !ok {
			t.Fatalf("%s: no symbol", f)
		}
		bs := q.s.BindingsOf(sym)
		found := false
		for _, b := range bs {
			if b.File == p && b.Res == Candidate && b.Reason == "name-match-outside-closure" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no candidate binding in %+v", f, bs)
		}
	}
	if !q.s.GraphIncomplete(q.abs("a.sh")) {
		t.Fatal("a.sh graph should be incomplete")
	}
	wantEq(t, q.def("d.sh", "helper", 0, 2))
	p, off := q.at("d.sh", "helper", 0, 2)
	if _, ok := q.s.SymbolAt(p, off); ok {
		t.Fatal("d.sh helper must have no binding")
	}
	if q.s.GraphIncomplete(q.abs("d.sh")) {
		t.Fatal("d.sh graph is complete")
	}
}

func TestCyclicSources(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": ". ./b.sh\nfa() { fb; }\n",
		"b.sh": ". ./a.sh\nfb() { fa; }\n",
	})
	for _, f := range []string{"a.sh", "b.sh"} {
		var got []string
		for _, p := range q.s.Visible(q.abs(f)) {
			got = append(got, q.rel(p))
		}
		wantEq(t, got, "a.sh", "b.sh")
	}
	wantEq(t, q.def("a.sh", "fb; }", 0, 1), "b.sh:2")
	wantEq(t, q.def("b.sh", "fa; }", 0, 1), "a.sh:2")
}

func TestRedefinitionAndImplementation(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "if x; then f() { :; }; else f() { :; }; fi; f\n",
	})
	p, off := q.at("m.sh", "fi; f", 0, 4)
	if got := q.strs(q.s.Implementation(p, off)); len(got) != 2 {
		t.Fatalf("implementation %v", got)
	}
	if got := q.s.Definition(p, off); len(got) != 2 || got[0].Range.Start.Offset >= got[1].Range.Start.Offset {
		t.Fatalf("definition %v", got)
	}
	// Cursor on one declaration name: Definition is that declaration, Implementation both.
	p, off = q.at("m.sh", "f()", 1, 0)
	if got := q.s.Definition(p, off); len(got) != 1 || got[0].Range.Start.Offset != off {
		t.Fatalf("definition on decl %v", got)
	}
	if got := q.s.Implementation(p, off); len(got) != 2 {
		t.Fatalf("implementation on decl %v", got)
	}
}

func TestGlobalVariableAcrossSource(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": ". ./b.sh\necho \"$VERSION\"\n",
		"b.sh": "VERSION=1\n",
	})
	wantEq(t, q.def("a.sh", "VERSION", 0, 1), "b.sh:1")
	wantEq(t, q.refs("a.sh", "VERSION", 0, 1), "a.sh:2", "b.sh:1")
	// Without the declaration: only the reads.
	p, off := q.at("a.sh", "VERSION", 0, 1)
	var noDecl []string
	for _, l := range q.s.References(p, off, false) {
		noDecl = append(noDecl, fmt.Sprintf("%s:%d", q.rel(l.Path), l.Range.Start.Line))
	}
	wantEq(t, noDecl, "a.sh:2")
}

func TestEnvVariableHasNoDefinition(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "echo \"$HOME\"\n"})
	wantEq(t, q.def("m.sh", "HOME", 0, 1))
	p, off := q.at("m.sh", "HOME", 0, 1)
	sym, ok := q.s.SymbolAt(p, off)
	if !ok || sym.Name != "HOME" || len(sym.Decls) != 0 {
		t.Fatalf("symbol %+v ok=%v", sym, ok)
	}
	wantEq(t, q.refs("m.sh", "HOME", 0, 1), "m.sh:1")
}

func TestTempAssignmentNeverBinds(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "X=1 cmd\necho \"$X\"\nY=1\nY=2 cmd\necho \"$Y\"\n",
	})
	wantEq(t, q.def("m.sh", "$X", 0, 1))
	wantEq(t, q.def("m.sh", "$Y", 0, 1), "m.sh:3")
	// The prefix assignment is its own declaration.
	wantEq(t, q.def("m.sh", "X=1", 0, 0), "m.sh:1")
	wantEq(t, q.refs("m.sh", "$X", 0, 1), "m.sh:2")
}

func TestLocalVersusGlobal(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "v=g\nf() { local v=1; v=2; echo \"$v\"; }\ng() { echo \"$v\"; v=3; }\n",
	})
	// Inside f both the write and the read bind to the local.
	wantEq(t, q.refs("m.sh", "$v", 0, 1), "m.sh:2", "m.sh:2", "m.sh:2")
	// Inside g, the global.
	wantEq(t, q.def("m.sh", "$v", 1, 1), "m.sh:1", "m.sh:3")
}

func TestAliasChainAndUnknownAlias(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\nalias a=b\nalias b=f\nalias ll='ls -l'\na\nll\n",
	})
	wantEq(t, q.def("m.sh", "a\nll", 0, 0), "m.sh:1")
	wantEq(t, q.def("m.sh", "ll\n", 0, 0), "m.sh:4")
}

func TestBuiltinHasNoCandidate(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": "echo hi\n",
		"b.sh": "echo() { :; }\n",
	})
	wantEq(t, q.def("a.sh", "echo", 0, 1))
}

func TestSymbolAtOnDeclarationAndSourcePath(t *testing.T) {
	q := buildCorpus(t)
	p, off := q.at("main.sh", "deploy() {", 0, 1)
	sym, ok := q.s.SymbolAt(p, off)
	if !ok || sym.Kind != DeclFunction || sym.Name != "deploy" || len(sym.Decls) != 1 {
		t.Fatalf("%+v %v", sym, ok)
	}
	if q.s.Facts(q.abs("main.sh")) == nil || len(q.s.SourceEdges(q.abs("main.sh"))) != 1 {
		t.Fatal("facts or edges missing")
	}
	if n := len(q.s.Paths()); n != 9 {
		t.Fatalf("paths %d", n)
	}
}
