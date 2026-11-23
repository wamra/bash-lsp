package analysis

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func (q *qworld) nodeStr(n Node) string {
	s := q.rel(n.Path)
	if n.Func != nil {
		s += ":" + n.Func.Name
	}
	return s
}

func (q *qworld) nodeStrs(ns []Node) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, q.nodeStr(n))
	}
	return out
}

// stepStr is "from -> to line rel res/reason": the Step's From is the node the
// edge leaves, To the node it enters, whichever way the query walked.
func (q *qworld) stepStr(s Step) string {
	return fmt.Sprintf("%s -> %s @%d %s %s/%s", q.nodeStr(s.From), q.nodeStr(s.To), s.Edge.Site.Range.Start.Line, s.Edge.Rel, s.Edge.Res, s.Edge.Reason)
}

func (q *qworld) stepStrs(ss []Step) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, q.stepStr(s))
	}
	return out
}

// node looks spec up and requires exactly one node.
func (q *qworld) node(spec string) Node {
	q.t.Helper()
	ns, err := q.s.Lookup(spec)
	if err != nil || len(ns) != 1 {
		q.t.Fatalf("Lookup(%q) = %v, %v; want one node", spec, q.nodeStrs(ns), err)
	}
	return ns[0]
}

func TestGraphMutualRecursion(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { g; }\ng() { f; }\nf\n"})
	f := q.node("f")
	wantSeq(t, q.stepStrs(q.s.Callers(f, 0)),
		"a.sh:g -> a.sh:f @2 calls resolved/direct",
		"a.sh -> a.sh:f @3 calls resolved/direct",
		"a.sh:f -> a.sh:g @1 calls resolved/direct")
	wantSeq(t, q.nodeStrs(q.s.Impact(f, 0)), "a.sh", "a.sh:g")
	wantSeq(t, q.stepStrs(q.s.Callees(f, 0)),
		"a.sh:f -> a.sh:g @1 calls resolved/direct",
		"a.sh:g -> a.sh:f @2 calls resolved/direct")
}

func TestGraphSelfRecursion(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { f; }\n"})
	f := q.node("f")
	wantSeq(t, q.stepStrs(q.s.Callers(f, 0)), "a.sh:f -> a.sh:f @1 calls resolved/direct")
	wantSeq(t, q.stepStrs(q.s.Callees(f, 0)), "a.sh:f -> a.sh:f @1 calls resolved/direct")
	wantSeq(t, q.nodeStrs(q.s.Impact(f, 0)))
	if tr := q.s.Trace(f, f); tr != nil {
		t.Fatalf("Trace(f, f) = %v, want nil", q.stepStrs(tr))
	}
}

func TestGraphDepth(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "a() { b; }\nb() { c; }\nc() { d; }\nd() { :; }\na\n"})
	d, a := q.node("d"), q.node("a")
	for _, c := range []struct{ depth, callers, callees int }{{1, 1, 1}, {2, 2, 1}, {3, 3, 1}, {0, 4, 1}, {-1, 4, 1}, {99, 4, 1}} {
		if got := len(q.s.Callers(d, c.depth)); got != c.callers {
			t.Errorf("Callers(d, %d) = %d steps, want %d", c.depth, got, c.callers)
		}
	}
	wantSeq(t, q.stepStrs(q.s.Callers(d, 2)),
		"a.sh:c -> a.sh:d @3 calls resolved/direct",
		"a.sh:b -> a.sh:c @2 calls resolved/direct")
	wantSeq(t, q.nodeStrs(q.s.Impact(d, 1)), "a.sh:c")
	wantSeq(t, q.nodeStrs(q.s.Impact(d, 2)), "a.sh:c", "a.sh:b")
	wantSeq(t, q.nodeStrs(q.s.Impact(d, 0)), "a.sh:c", "a.sh:b", "a.sh:a", "a.sh")
	wantSeq(t, q.stepStrs(q.s.Callees(a, 2)),
		"a.sh:a -> a.sh:b @1 calls resolved/direct",
		"a.sh:b -> a.sh:c @2 calls resolved/direct")
	if got := len(q.s.Callees(a, 0)); got != 3 {
		t.Errorf("Callees(a, 0) = %d steps, want 3", got)
	}
}

func TestGraphTraceShortest(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "s() { x; d; }\nx() { y; }\ny() { d; }\nd() { :; }\n"})
	wantSeq(t, q.stepStrs(q.s.Trace(q.node("s"), q.node("d"))), "a.sh:s -> a.sh:d @1 calls resolved/direct")
	wantSeq(t, q.stepStrs(q.s.Trace(q.node("x"), q.node("d"))),
		"a.sh:x -> a.sh:y @2 calls resolved/direct",
		"a.sh:y -> a.sh:d @3 calls resolved/direct")
	if tr := q.s.Trace(q.node("d"), q.node("s")); tr != nil {
		t.Fatalf("Trace against the edges = %v, want nil", q.stepStrs(tr))
	}
}

func TestGraphTraceThroughSources(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh": "source ./lib.sh\nrun\n",
		"lib.sh":  "run() { :; }\n",
	})
	wantSeq(t, q.stepStrs(q.s.Trace(q.node("main.sh"), q.node("lib.sh"))), "main.sh -> lib.sh @1 sources resolved/direct")
	wantSeq(t, q.stepStrs(q.s.Trace(q.node("main.sh"), q.node("run"))), "main.sh -> lib.sh:run @2 calls resolved/direct")
}

func TestGraphSourceCycle(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": "source ./b.sh\n", "b.sh": "source ./c.sh\n", "c.sh": "source ./a.sh\n",
		"d.sh": "source ./a.sh\n", "self.sh": "source ./self.sh\n", "e.sh": ":\n",
	})
	var got [][]string
	for _, c := range q.s.SourceCycles() {
		var r []string
		for _, p := range c {
			r = append(r, q.rel(p))
		}
		got = append(got, r)
	}
	if !slices.EqualFunc(got, [][]string{{"a.sh", "b.sh", "c.sh"}, {"self.sh"}}, slices.Equal) {
		t.Fatalf("SourceCycles = %v", got)
	}
	// Traversals over the cycle terminate and report every file once.
	a := q.node("a.sh")
	wantSeq(t, q.nodeStrs(q.s.Impact(a, 0)), "c.sh", "d.sh", "b.sh")
}

func TestGraphLookup(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh":     "f() { :; }\ng() {\n  f\n}\ng\n",
		"sub/b.sh": "f() { :; }\n",
	})
	wantSeq(t, q.nodeStrs(q.look("f")), "a.sh:f", "sub/b.sh:f")
	wantSeq(t, q.nodeStrs(q.look("g")), "a.sh:g")
	wantSeq(t, q.nodeStrs(q.look("sub/b.sh")), "sub/b.sh")
	wantSeq(t, q.nodeStrs(q.look(q.abs("a.sh"))), "a.sh")
	// Position: inside g's body, on the call to f (the named function wins), on
	// g's name, on a top-level call, and on a blank column past the line end.
	wantSeq(t, q.nodeStrs(q.look("a.sh:3:3")), "a.sh:f")
	wantSeq(t, q.nodeStrs(q.look("a.sh:2:1")), "a.sh:g")
	wantSeq(t, q.nodeStrs(q.look("a.sh:5:1")), "a.sh:g")
	wantSeq(t, q.nodeStrs(q.look("a.sh:1:11")), "a.sh:f") // one past the last byte is in range
	wantSeq(t, q.nodeStrs(q.look("sub/b.sh:1:1")), "sub/b.sh:f")
	for _, bad := range []string{"nope", "nope.sh:1:1", "a.sh:1", ""} {
		if ns, err := q.s.Lookup(bad); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
			t.Errorf("Lookup(%q) = %v, %v; want an error naming the spec", bad, ns, err)
		}
	}
}

func TestGraphLookupPositionInBody(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() {\n  echo hi\n}\n"})
	wantSeq(t, q.nodeStrs(q.look("a.sh:2:4")), "a.sh:f")
	wantSeq(t, q.nodeStrs(q.look("a.sh:3:1")), "a.sh:f")
}

func TestGraphUnsetInViewCandidateIsTraversed(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh":  "k() { :; }\nunset -f k\n",
		"main.sh": "source ./lib.sh\nk\n",
	})
	k := q.node("k")
	got := q.s.Callers(k, 0)
	wantSeq(t, q.stepStrs(got),
		"main.sh -> lib.sh:k @2 calls candidate/unset-in-view")
}

func TestGraphDynamicEdgeIsNotTraversed(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { \"$1\"; }\ng() { :; }\n"})
	f, g := q.node("f"), q.node("g")
	if tr := q.s.Trace(f, g); tr != nil {
		t.Fatalf("Trace over a dynamic edge = %v, want nil", q.stepStrs(tr))
	}
	if got := q.s.Callees(f, 0); len(got) != 0 {
		t.Fatalf("Callees(f) = %v, want none", q.stepStrs(got))
	}
	un := q.s.Unresolved(nil)
	if len(un) != 1 || un[0].Res != Dynamic || un[0].From.Func == nil || un[0].From.Func.Name != "f" {
		t.Fatalf("Unresolved = %+v, want the one dynamic edge from f", un)
	}
}

func TestGraphUnresolved(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": "source ./nope.sh\nf() { \"$1\"; }\n",
		"b.sh": "source \"$X/y.sh\"\n",
		"c.sh": "source ./a.sh\n",
	})
	var got []string
	for _, e := range q.s.Unresolved(nil) {
		got = append(got, fmt.Sprintf("%s:%d %s/%s", q.rel(e.Site.Path), e.Site.Range.Start.Line, e.Rel, e.Res))
	}
	wantSeq(t, got, "a.sh:1 sources/unresolved", "a.sh:2 calls/dynamic", "b.sh:1 sources/unresolved")
	only := q.s.Unresolved([]string{"b.sh"})
	if len(only) != 1 || q.rel(only[0].Site.Path) != "b.sh" {
		t.Fatalf("Unresolved([b.sh]) = %+v", only)
	}
	if got := q.s.Unresolved([]string{q.abs("c.sh"), "missing.sh"}); len(got) != 0 {
		t.Fatalf("Unresolved(c.sh) = %+v, want none", got)
	}
}

func TestGraphImpactOfFile(t *testing.T) {
	q := buildMem(t, map[string]string{
		"top.sh": "source ./mid.sh\n", "mid.sh": "source ./leaf.sh\n", "leaf.sh": "f() { :; }\n", "other.sh": ":\n",
	})
	wantSeq(t, q.nodeStrs(q.s.Impact(q.node("leaf.sh"), 0)), "mid.sh", "top.sh")
	wantSeq(t, q.nodeStrs(q.s.Impact(q.node("leaf.sh"), 1)), "mid.sh")
	wantSeq(t, q.stepStrs(q.s.Callees(q.node("top.sh"), 0)),
		"top.sh -> mid.sh @1 sources resolved/direct",
		"mid.sh -> leaf.sh @1 sources resolved/direct")
	// A function's callers do not imply a file-to-file step: sourcing leaf.sh
	// is not calling f, and top.sh is not an impact of f.
	wantSeq(t, q.nodeStrs(q.s.Impact(q.node("f"), 0)))
}

func TestGraphAliasIsNotACaller(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nalias ff=f\nff\n"})
	f := q.node("f")
	for _, s := range q.s.Callers(f, 0) {
		if s.From.Func != nil && s.From.Func.Kind != DeclFunction {
			t.Fatalf("alias reported as a caller: %s", q.stepStr(s))
		}
		if s.Edge.Rel == RelAliases {
			t.Fatalf("alias edge is a step: %s", q.stepStr(s))
		}
	}
	wantSeq(t, q.stepStrs(q.s.Callers(f, 0)), "a.sh -> a.sh:f @3 calls resolved/alias")
}

// graphDump runs every query over a snapshot and returns one comparable string.
func graphDump(q *qworld, ns []Node) string {
	var b strings.Builder
	for _, n := range ns {
		fmt.Fprintln(&b, "N", q.nodeStr(n))
		fmt.Fprintln(&b, q.stepStrs(q.s.Callers(n, 0)))
		fmt.Fprintln(&b, q.stepStrs(q.s.Callees(n, 0)))
		fmt.Fprintln(&b, q.nodeStrs(q.s.Impact(n, 0)))
		for _, m := range ns {
			fmt.Fprintln(&b, q.stepStrs(q.s.Trace(n, m)))
		}
	}
	for _, c := range q.s.SourceCycles() {
		for _, p := range c {
			fmt.Fprint(&b, q.rel(p), " ")
		}
	}
	fmt.Fprintln(&b, len(q.s.Unresolved(nil)))
	return b.String()
}

func TestGraphDeterministic(t *testing.T) {
	srcs := map[string]string{}
	for i := 0; i < 6; i++ {
		srcs[fmt.Sprintf("f%d.sh", i)] = fmt.Sprintf("source ./f%d.sh\nsource ./f%d.sh\nfn%d() { fn%d; fn%d; common; }\ncommon() { :; }\n", (i+1)%6, (i+2)%6, i, (i+1)%6, (i+3)%6)
	}
	var want string
	for run := 0; run < 20; run++ {
		q := buildMem(t, srcs)
		var ns []Node
		for _, p := range q.s.Paths() {
			ns = append(ns, Node{Path: p})
		}
		for _, name := range []string{"common", "fn0", "fn3", "fn5"} {
			ns = append(ns, q.look(name)...)
		}
		got := graphDump(q, ns)
		if run == 0 {
			want = got
			if !strings.Contains(got, "calls") {
				t.Fatalf("dump has no calls:\n%s", got)
			}
		} else if got != want {
			t.Fatalf("run %d differs from run 0:\n%s\n---\n%s", run, got, want)
		}
	}
}

func TestGraphScale(t *testing.T) {
	const n = 5000
	t.Run("chain", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("f0() { :; }\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "f%d() { f%d; }\n", i, i-1)
		}
		fmt.Fprintf(&b, "f%d\n", n-1)
		q := buildMem(t, map[string]string{"a.sh": b.String()})
		start := time.Now()
		f0, last := q.node("f0"), q.node(fmt.Sprintf("f%d", n-1))
		if got := len(q.s.Callers(f0, 0)); got != n { // n-1 callers plus the top-level call
			t.Fatalf("Callers = %d steps, want %d", got, n)
		}
		if got := len(q.s.Callees(last, 0)); got != n-1 {
			t.Fatalf("Callees = %d steps, want %d", got, n-1)
		}
		if got := len(q.s.Impact(f0, 0)); got != n { // n-1 callers plus the file
			t.Fatalf("Impact = %d nodes, want %d", got, n)
		}
		if got := len(q.s.Trace(last, f0)); got != n-1 {
			t.Fatalf("Trace = %d steps, want %d", got, n-1)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("queries took %v", d)
		}
	})
	t.Run("fan-out", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("hub() { :; }\nmain() {\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "  w%d\n", i)
		}
		b.WriteString("}\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "w%d() { hub; }\n", i)
		}
		q := buildMem(t, map[string]string{"a.sh": b.String()})
		start := time.Now()
		hub, main := q.node("hub"), q.node("main")
		if got := len(q.s.Callers(hub, 1)); got != n {
			t.Fatalf("Callers(hub, 1) = %d, want %d", got, n)
		}
		if got := len(q.s.Callers(hub, 0)); got != 2*n {
			t.Fatalf("Callers(hub, 0) = %d, want %d", got, 2*n)
		}
		if got := len(q.s.Impact(hub, 0)); got != n+1 {
			t.Fatalf("Impact(hub) = %d, want %d", got, n+1)
		}
		if got := len(q.s.Callees(main, 0)); got != 2*n {
			t.Fatalf("Callees(main, 0) = %d, want %d", got, 2*n)
		}
		if got := len(q.s.Trace(main, hub)); got != 2 {
			t.Fatalf("Trace(main, hub) = %d steps, want 2", got)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("queries took %v", d)
		}
	})
}

// look is Lookup that fails the test on error.
func (q *qworld) look(spec string) []Node {
	q.t.Helper()
	ns, err := q.s.Lookup(spec)
	if err != nil {
		q.t.Fatal(err)
	}
	return ns
}

// wantSeq compares in order (wantEq sorts the expectation).
func wantSeq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestGraphLookupOutOfRange(t *testing.T) {
	q := buildMem(t, map[string]string{"main.sh": "f() { :; }\nf\n"})
	for _, bad := range []string{"main.sh:99:1", "main.sh:3:1", "main.sh:1:99", "main.sh:2:3"} {
		if ns, err := q.s.Lookup(bad); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
			t.Errorf("Lookup(%q) = %v, %v; want an error naming the spec", bad, q.nodeStrs(ns), err)
		}
	}
	// The end of a line (one past its last byte) is in range.
	wantSeq(t, q.nodeStrs(q.look("main.sh:1:11")), "main.sh:f")
	wantSeq(t, q.nodeStrs(q.look("main.sh:2:2")), "main.sh:f")
}

func TestGraphInvokesEdges(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh": "run() { ./tool.sh; }\nrun\n",
		"tool.sh": "echo hi\n",
	})
	tool := q.node("tool.sh")
	wantSeq(t, q.stepStrs(q.s.Callers(tool, 1)), "main.sh:run -> tool.sh @1 invokes resolved/direct")
	wantSeq(t, q.nodeStrs(q.s.Impact(tool, 0)), "main.sh:run", "main.sh")
	wantSeq(t, q.stepStrs(q.s.Trace(q.node("main.sh"), tool)),
		"main.sh -> main.sh:run @2 calls resolved/direct",
		"main.sh:run -> tool.sh @1 invokes resolved/direct")
	if c := q.s.SourceCycles(); len(c) != 0 {
		t.Fatalf("SourceCycles = %v, invokes must not count", c)
	}
}

func TestGraphLookupFileBeatsFunction(t *testing.T) {
	q := buildMem(t, map[string]string{"util.sh": "echo hi\n", "a.sh": "util.sh() { :; }\n"})
	wantSeq(t, q.nodeStrs(q.look("util.sh")), "util.sh")
}

// Columns of a "path:line:col" spec are UTF-16 units, as every position the
// tools print.
func TestGraphLookupUTF16Columns(t *testing.T) {
	// Line 3: `echo "héllo ☃😀"; f`; f is at byte column 24, UTF-16 column 19.
	q := buildMem(t, map[string]string{
		"uni.sh":  "#!/bin/bash\nf() { :; }\necho \"héllo ☃😀\"; f\n",
		"crlf.sh": "f() { :; }\r\necho \"é😀\"; f\r\n",
	})
	wantSeq(t, q.nodeStrs(q.look("uni.sh:3:19")), "uni.sh:f")
	wantSeq(t, q.nodeStrs(q.look("crlf.sh:2:13")), "crlf.sh:f")
	// The byte column of the same place is past the end of the line in UTF-16.
	for _, bad := range []string{"uni.sh:3:25", "uni.sh:3:21", "crlf.sh:2:15", "crlf.sh:1:12"} {
		if ns, err := q.s.Lookup(bad); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
			t.Errorf("Lookup(%q) = %v, %v; want an error naming the spec", bad, q.nodeStrs(ns), err)
		}
	}
	// A column inside a surrogate pair is not a position.
	if _, err := q.s.Lookup("uni.sh:3:15"); err == nil {
		t.Errorf("Lookup inside a surrogate pair succeeded")
	}
}
