package analysis

import (
	"fmt"
	"testing"
)

// edgeStr is "site rel res/reason from -> to": from is "file" or "file:func",
// to is "name@file:line:col" of the target definition, a file, or "-".
func (q *qworld) edgeStr(e Edge) string {
	from := q.rel(e.From.Path)
	if e.From.Func != nil {
		from += ":" + e.From.Func.Name
	}
	to := "-"
	switch {
	case e.ToDecl != nil:
		name := "?"
		if e.To != nil {
			name = e.To.Name
		}
		to = fmt.Sprintf("%s@%s:%d:%d", name, q.rel(q.s.paths[q.s.declFile[e.ToDecl]]), e.ToDecl.NameRange.Start.Line, e.ToDecl.NameRange.Start.Col)
	case e.ToFile != "":
		to = q.rel(e.ToFile)
	}
	return fmt.Sprintf("%s:%d %s %s/%s %s -> %s", q.rel(e.Site.Path), e.Site.Range.Start.Line, e.Rel, e.Res, e.Reason, from, to)
}

func (q *qworld) edgeStrs(es []Edge) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, q.edgeStr(e))
	}
	return out
}

// sym returns the symbol at the occ-th needle plus off.
func (q *qworld) sym(file, needle string, occ, off int) *Symbol {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	s, ok := q.s.SymbolAt(p, o)
	if !ok {
		q.t.Fatalf("%s: no symbol at %q", file, needle)
	}
	return s
}

func wantSame(t *testing.T, got []string, want ...string) {
	t.Helper()
	g := append([]string(nil), got...)
	wantEq(t, sortedCopy(g), want...)
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestEdgesToBuildReleaseCorpus(t *testing.T) {
	q := buildCorpus(t)
	br := q.sym("lib/util.sh", "build_release()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(br)),
		"main.sh:3 aliases resolved/direct main.sh:br -> build_release@lib/util.sh:4:1",
		"main.sh:6 calls resolved/direct main.sh:deploy -> build_release@lib/util.sh:4:1",
		"main.sh:7 calls resolved/alias main.sh:deploy -> build_release@lib/util.sh:4:1",
		"dyn.sh:4 calls resolved/direct dyn.sh -> build_release@lib/util.sh:4:1",
	)
	// orphan.sh's own function: only its own call.
	ob := q.sym("orphan.sh", "build_release()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(ob)),
		"orphan.sh:3 calls resolved/direct orphan.sh -> build_release@orphan.sh:2:1",
	)
}

func TestEdgesOutsideClosureCandidates(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": ". \"$X\"\nhelper\n",
		"b.sh": "helper() { :; }\n",
		"c.sh": "helper() { echo; }\n. ./b.sh\n",
	})
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("a.sh")})),
		"a.sh:1 sources unresolved/not-found a.sh -> -",
		"a.sh:2 calls candidate/name-match-outside-closure a.sh -> helper@b.sh:1:1",
		"a.sh:2 calls candidate/name-match-outside-closure a.sh -> helper@c.sh:1:1",
	)
	es := q.s.EdgesFrom(Caller{Path: q.abs("a.sh")})
	if es[0].Text != `"$X"` || es[0].To != nil || es[0].ToFile != "" {
		t.Fatalf("unresolved source edge %+v", es[0])
	}
}

func TestEdgesTopLevelCallerAndSources(t *testing.T) {
	q := buildCorpus(t)
	top := Caller{Path: q.abs("main.sh")}
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(top)),
		"main.sh:2 sources resolved/direct main.sh -> lib/util.sh",
		"main.sh:21 calls resolved/direct main.sh -> deploy@main.sh:4:1",
	)
	deploy := q.sym("main.sh", "deploy()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("main.sh"), Func: deploy})),
		"main.sh:6 calls resolved/direct main.sh:deploy -> build_release@lib/util.sh:4:1",
		"main.sh:7 calls resolved/alias main.sh:deploy -> build_release@lib/util.sh:4:1",
	)
	for _, e := range q.s.EdgesTo(deploy) {
		if e.From.Func != nil {
			t.Fatalf("top-level call has caller function %s", e.From.Func.Name)
		}
	}
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("app.sh")})),
		"app.sh:2 sources resolved/direct app.sh -> lib/hooks.sh",
		"app.sh:5 sources resolved/direct app.sh -> plugins/a.sh",
		"app.sh:7 calls resolved/direct app.sh -> run_hooks@lib/hooks.sh:3:1",
		"app.sh:8 calls resolved/direct app.sh -> plugin_a@plugins/a.sh:2:1",
	)
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("dyn.sh")})),
		"dyn.sh:3 sources resolved/direct dyn.sh -> lib/util.sh",
		"dyn.sh:4 calls resolved/direct dyn.sh -> build_release@lib/util.sh:4:1",
	)
}

func TestEdgesConditionalDefinition(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "if x; then f() { :; }; else f() { :; }; fi; f\n",
	})
	f := q.sym("m.sh", "f()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(f)),
		"m.sh:1 calls candidate/conditional-definition m.sh -> f@m.sh:1:12",
		"m.sh:1 calls candidate/conditional-definition m.sh -> f@m.sh:1:29",
	)
}

func TestEdgesRedefinitionAcrossVisibleFiles(t *testing.T) {
	// Both definitions are one symbol (a.sh sees b.sh) and both are in view from a.sh.
	q := buildMem(t, map[string]string{
		"a.sh": ". ./b.sh\nf() { :; }\nf\n",
		"b.sh": "f() { :; }\n",
	})
	f := q.sym("a.sh", "f()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(f)),
		"a.sh:3 calls candidate/conditional-definition a.sh -> f@a.sh:2:1",
		"a.sh:3 calls candidate/conditional-definition a.sh -> f@b.sh:1:1",
	)
}

func TestEdgesDiamondStaysResolved(t *testing.T) {
	q := buildMem(t, map[string]string{
		"app1.sh":      ". ./lib/hooks.sh\non_start() { :; }\n",
		"app2.sh":      ". ./lib/hooks.sh\non_start() { :; }\n",
		"lib/hooks.sh": "run() {\n  on_start\n}\n",
	})
	run := q.sym("lib/hooks.sh", "run()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("lib/hooks.sh"), Func: run})),
		"lib/hooks.sh:2 calls resolved/direct lib/hooks.sh:run -> on_start@app1.sh:2:1",
		"lib/hooks.sh:2 calls resolved/direct lib/hooks.sh:run -> on_start@app2.sh:2:1",
	)
}

func aliasChain(n int) string {
	src := "f() { :; }\n"
	for i := 1; i < n; i++ {
		src += fmt.Sprintf("alias a%d=a%d\n", i, i+1)
	}
	src += fmt.Sprintf("alias a%d=f\na1\n", n)
	return src
}

func TestEdgesAliasChainDepth(t *testing.T) {
	q8 := buildMem(t, map[string]string{"m.sh": aliasChain(8)})
	f8 := q8.sym("m.sh", "f()", 0, 0)
	var calls []Edge
	for _, e := range q8.s.EdgesTo(f8) {
		if e.Rel == RelCalls {
			calls = append(calls, e)
		}
	}
	wantSame(t, q8.edgeStrs(calls), "m.sh:10 calls resolved/alias m.sh -> f@m.sh:1:1")

	q9 := buildMem(t, map[string]string{"m.sh": aliasChain(9)})
	for _, e := range q9.s.Edges() {
		if e.Rel == RelCalls {
			t.Fatalf("alias chain of 9 gave a call edge: %s", q9.edgeStr(e))
		}
	}

	qc := buildMem(t, map[string]string{"m.sh": "alias x=y\nalias y=x\nx\n"})
	if es := qc.s.Edges(); len(es) != 0 {
		t.Fatalf("alias cycle gave edges %v", qc.edgeStrs(es))
	}
}

func TestEdgesAliasRelation(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\nalias a=b\nalias b=f\nalias ll='ls -l'\na\nll\n",
	})
	f := q.sym("m.sh", "f()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(f)),
		"m.sh:2 aliases resolved/alias m.sh:a -> f@m.sh:1:1",
		"m.sh:3 aliases resolved/direct m.sh:b -> f@m.sh:1:1",
		"m.sh:5 calls resolved/alias m.sh -> f@m.sh:1:1",
	)
	// ll's target is an external command: no edge at all for the alias or its call.
	for _, e := range q.s.Edges() {
		if e.Site.Range.Start.Line >= 4 && e.Site.Range.Start.Line != 5 {
			t.Fatalf("unexpected edge %s", q.edgeStr(e))
		}
	}
}

func TestEdgesAliasToConditionalFunction(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "if x; then f() { :; }; else f() { :; }; fi\nalias a=f\na\n",
	})
	f := q.sym("m.sh", "f()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(f)),
		"m.sh:2 aliases candidate/conditional-definition m.sh:a -> f@m.sh:1:12",
		"m.sh:2 aliases candidate/conditional-definition m.sh:a -> f@m.sh:1:29",
		"m.sh:3 calls candidate/conditional-definition m.sh -> f@m.sh:1:12",
		"m.sh:3 calls candidate/conditional-definition m.sh -> f@m.sh:1:29",
	)
}

func TestEdgesSubshellDefinition(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { echo a; }\n( f() { echo b; }; f )\nf\n",
		"n.sh": "( g() { :; }; g )\ng\necho $(h() { :; }; h)\nh\n",
	})
	f := q.sym("m.sh", "f()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(f)),
		"m.sh:2 calls resolved/direct m.sh -> f@m.sh:2:3",
		"m.sh:3 calls candidate/conditional-definition m.sh -> f@m.sh:1:1",
		"m.sh:3 calls candidate/conditional-definition m.sh -> f@m.sh:2:3",
	)
	g := q.sym("n.sh", "g()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(g)),
		"n.sh:1 calls resolved/direct n.sh -> g@n.sh:1:3",
		"n.sh:2 calls candidate/conditional-definition n.sh -> g@n.sh:1:3",
	)
	h := q.sym("n.sh", "h()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.EdgesTo(h)),
		"n.sh:3 calls resolved/direct n.sh -> h@n.sh:3:8",
		"n.sh:4 calls candidate/conditional-definition n.sh -> h@n.sh:3:8",
	)
}

func TestEdgesCallerAt(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "outer() {\n  inner() {\n    x\n  }\n  y\n}\nz\n",
	})
	outer := q.sym("m.sh", "outer()", 0, 0)
	inner := q.sym("m.sh", "inner()", 0, 0)
	p, o := q.at("m.sh", "x\n", 0, 0)
	if c := q.s.CallerAt(p, o); c.Path != p || c.Func != inner {
		t.Fatalf("CallerAt x = %+v", c)
	}
	p, o = q.at("m.sh", "y\n", 0, 0)
	if c := q.s.CallerAt(p, o); c.Path != p || c.Func != outer {
		t.Fatalf("CallerAt y = %+v", c)
	}
	p, o = q.at("m.sh", "z\n", 0, 0)
	if c := q.s.CallerAt(p, o); c.Path != p || c.Func != nil {
		t.Fatalf("CallerAt z = %+v", c)
	}
	if c := q.s.CallerAt(q.abs("nope.sh"), 0); c != (Caller{}) {
		t.Fatalf("CallerAt unknown file = %+v", c)
	}
}

func TestEdgesRecursionAndNoEdgeCommands(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() {\n  f\n  echo hi\n  rm -rf x\n}\nf\n",
	})
	f := q.sym("m.sh", "f()", 0, 0)
	wantSame(t, q.edgeStrs(q.s.Edges()),
		"m.sh:2 calls resolved/direct m.sh:f -> f@m.sh:1:1",
		"m.sh:6 calls resolved/direct m.sh -> f@m.sh:1:1",
	)
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("m.sh"), Func: f})),
		"m.sh:2 calls resolved/direct m.sh:f -> f@m.sh:1:1",
	)
}

func TestEdgesSortedCopiedAndSameWithPrev(t *testing.T) {
	q := buildCorpus(t)
	es := q.s.Edges()
	if len(es) == 0 {
		t.Fatal("no edges")
	}
	for i := 1; i < len(es); i++ {
		a, b := es[i-1].Site, es[i].Site
		if a.Path > b.Path || (a.Path == b.Path && a.Range.Start.Offset > b.Range.Start.Offset) {
			t.Fatalf("edges not sorted at %d: %s then %s", i, q.edgeStr(es[i-1]), q.edgeStr(es[i]))
		}
	}
	want := q.edgeStrs(es)
	es[0].Reason = "changed"
	if got := q.edgeStrs(q.s.Edges()); got[0] != want[0] {
		t.Fatal("Edges returned the snapshot's own slice")
	}
	files := map[string]*FileFacts{}
	for _, p := range q.s.Paths() {
		files[p] = q.s.Facts(p)
	}
	again := &qworld{t: t, root: q.root, s: Build(files, Options{Root: q.root, Prev: q.s}, 8)}
	if got := again.edgeStrs(again.s.Edges()); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("edges with Prev differ:\n%v\n%v", got, want)
	}
}

func TestIsBuiltin(t *testing.T) {
	for _, w := range []string{"echo", "cd", "source", ".", ":", "[", "local", "declare", "trap", "eval", "mapfile", "readarray", "compopt",
		"if", "then", "fi", "[[", "]]", "{", "}", "!", "coproc", "function", "time"} {
		if !IsBuiltin(w) {
			t.Errorf("IsBuiltin(%q) = false", w)
		}
	}
	for _, w := range []string{"ls", "rm", "grep", "newgrp", "bash", "build_release", "", "wait -n"} {
		if IsBuiltin(w) {
			t.Errorf("IsBuiltin(%q) = true", w)
		}
	}
}

// The From/To index is built on first use; concurrent first queries are safe (run with -race).
func TestEdgesConcurrentQueries(t *testing.T) {
	q := buildCorpus(t)
	br := q.sym("lib/util.sh", "build_release()", 0, 0)
	done := make(chan int)
	for i := 0; i < 8; i++ {
		go func() {
			done <- len(q.s.EdgesTo(br)) + len(q.s.EdgesFrom(Caller{Path: q.abs("main.sh")}))
		}()
	}
	for i := 0; i < 8; i++ {
		if n := <-done; n != 6 {
			t.Errorf("got %d edges, want 6", n)
		}
	}
}

// A symbol with two definitions of which only one is visible from the caller
// (x.sh and y.sh see each other, w.sh sees only y.sh): resolved, to that one.
func TestEdgesOneDefinitionInViewOfTwo(t *testing.T) {
	q := buildMem(t, map[string]string{
		"w.sh": ". ./y.sh\nf\n",
		"x.sh": ". ./y.sh\nf() { :; }\n",
		"y.sh": "f() { :; }\n",
	})
	f := q.sym("y.sh", "f()", 0, 0)
	if len(f.Decls) != 2 {
		t.Fatalf("want one symbol with two definitions, got %d", len(f.Decls))
	}
	wantSame(t, q.edgeStrs(q.s.EdgesFrom(Caller{Path: q.abs("w.sh")})),
		"w.sh:1 sources resolved/direct w.sh -> y.sh",
		"w.sh:2 calls resolved/direct w.sh -> f@y.sh:1:1",
	)
}
