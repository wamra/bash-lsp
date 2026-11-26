package analysis

import (
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// capsSnap builds one file of src into a snapshot.
func capsSnap(src string) *Snapshot {
	p := "/ws/a.sh"
	ff := Bind(front.Parse(p, []byte(src), front.Bash))
	return Build(map[string]*FileFacts{p: ff}, Options{Root: "/ws"}, 1)
}

// siteEdges groups the call edges (not command edges) of s by site offset.
func siteEdges(s *Snapshot) (map[int][]Edge, []int) {
	by := map[int][]Edge{}
	var order []int
	for _, e := range s.Edges() {
		if e.Rel != RelCalls || e.IsCommand() {
			continue
		}
		o := e.Site.Range.Start.Offset
		if by[o] == nil {
			order = append(order, o)
		}
		by[o] = append(by[o], e)
	}
	return by, order
}

func defsCalls(d, c int) string {
	return strings.Repeat("f(){ :; }\n", d) + strings.Repeat("f\n", c)
}

func TestSiteCandidateCap(t *testing.T) {
	_, bound := shapeSize()
	src := defsCalls(2000, 2000)
	var s *Snapshot
	if !withinBound(t, "build", bound, func() { s = capsSnap(src) }) {
		return
	}
	by, order := siteEdges(s)
	if len(order) != 2000 {
		t.Fatalf("%d call sites, want 2000", len(order))
	}
	total := 0
	var sym *Symbol
	for _, o := range order {
		es := by[o]
		total += len(es)
		var cands []Edge
		var caps []Edge
		for _, e := range es {
			switch {
			case e.Res == Candidate && e.Reason == ReasonConditional:
				cands = append(cands, e)
			case e.Res == Dynamic && e.Reason == ReasonCandidateCap:
				caps = append(caps, e)
			default:
				t.Fatalf("site at %d: unexpected edge %+v", o, e)
			}
		}
		if len(cands) != 16 || len(caps) != 1 || caps[0].Text != "f" {
			t.Fatalf("site at %d: %d candidates, %d caps (%v)", o, len(cands), len(caps), caps)
		}
		sym = cands[0].To
		for i, e := range cands {
			if e.ToDecl != sym.Decls[i] {
				t.Fatalf("site at %d: candidate %d is not declaration %d", o, i, i)
			}
		}
	}
	if total != 34000 {
		t.Fatalf("%d call edges, want 34000", total)
	}
	node := Node{Path: "/ws/a.sh", Func: sym}
	var steps []Step
	if !withinBound(t, "callers", bound, func() { steps = s.Callers(node, 1) }) {
		return
	}
	// Every candidate edge of a site ends at this one node (one function, one
	// file): 16 per site.
	if len(steps) != 2000*16 {
		t.Fatalf("%d callers steps, want %d", len(steps), 2000*16)
	}
	n := 0
	for _, e := range s.Unresolved(nil) {
		if e.Reason == ReasonCandidateCap {
			n++
		}
	}
	if n != 2000 {
		t.Fatalf("Unresolved lists %d candidate-cap edges, want 2000", n)
	}
	ls := limitLines(s, "candidate cap")
	if len(ls) != 1 || !strings.Contains(ls[0], "2000 call sites") {
		t.Fatalf("LimitsHit candidate cap lines: %q", ls)
	}
	want := "a.sh:2001: candidate cap: 2000 call sites of names or variables with more than 16 candidates list only their first 16"
	if !slices.Contains(s.LimitsHit(), want) {
		t.Fatalf("LimitsHit %q lacks %q", s.LimitsHit(), want)
	}
}

func TestSiteCandidateCapBoundary(t *testing.T) {
	for _, c := range []struct{ defs, edges, caps int }{{16, 16, 0}, {17, 16, 1}} {
		s := capsSnap(defsCalls(c.defs, 1))
		by, _ := siteEdges(s)
		var cand, caps int
		for _, es := range by {
			for _, e := range es {
				switch e.Reason {
				case ReasonConditional:
					cand++
				case ReasonCandidateCap:
					caps++
				}
			}
		}
		if cand != c.edges || caps != c.caps {
			t.Errorf("%d definitions: %d candidates, %d caps; want %d, %d", c.defs, cand, caps, c.edges, c.caps)
		}
		if got := len(limitLines(s, "candidate cap")); got != c.caps {
			t.Errorf("%d definitions: %d limit lines", c.defs, got)
		}
	}
}

func TestSiteCandidateCapOutsideClosure(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh":  strings.Repeat("g() { :; }\n", 17),
		"main.sh": "g\n",
	})
	var cand, caps int
	for _, e := range q.s.Edges() {
		if e.Site.Path != q.abs("main.sh") || e.Rel != RelCalls {
			continue
		}
		switch {
		case e.Res == Candidate && e.Reason == ReasonOutsideClosure:
			cand++
		case e.Res == Dynamic && e.Reason == ReasonCandidateCap:
			caps++
		}
	}
	if cand != 16 || caps != 1 {
		t.Fatalf("%d name-match-outside-closure edges, %d caps; want 16 and 1", cand, caps)
	}
}

// cmdShape is the `cmd` kind of bench/gen_product_shapes.py.
func cmdShape(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "f%d() { :; }\n", i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "cmd=f%d\n", i)
	}
	sb.WriteString(strings.Repeat("\"$cmd\" x\n", n))
	return sb.String()
}

func TestVariableCallCandidateCap(t *testing.T) {
	n := 4000
	if raceBuild {
		n = 1000
	}
	src := cmdShape(n)
	var s *Snapshot
	if !withinBound(t, "build", 2*time.Second, func() { s = capsSnap(src) }) {
		return
	}
	var by map[int][]Edge
	var order []int
	if !withinBound(t, "edges", 2*time.Second, func() { by, order = siteEdges(s) }) {
		return
	}
	if len(order) != n {
		t.Fatalf("%d call sites, want %d", len(order), n)
	}
	for _, o := range order {
		var names []string
		caps := 0
		for _, e := range by[o] {
			switch {
			case e.Res == Candidate && e.Reason == ReasonVariableConstant:
				names = append(names, e.To.Name)
			case e.Res == Dynamic && e.Reason == ReasonCandidateCap && e.Text == `"$cmd"`:
				caps++
			default:
				t.Fatalf("unexpected edge %+v", e)
			}
		}
		if len(names) != 16 || caps != 1 {
			t.Fatalf("site at %d: %d targets, %d caps", o, len(names), caps)
		}
		for i, nm := range names {
			if nm != fmt.Sprintf("f%d", i) {
				t.Fatalf("site at %d: target %d is %s, want the functions in order of first appearance", o, i, nm)
			}
		}
	}
	cut := 0
	for _, e := range s.Unresolved(nil) {
		if e.Reason == ReasonCandidateCap {
			cut++
		}
	}
	if cut != n {
		t.Fatalf("Unresolved lists %d candidate-cap edges, want %d", cut, n)
	}
	if ls := limitLines(s, "candidate cap"); len(ls) != 1 || !strings.Contains(ls[0], fmt.Sprintf("%d call sites", n)) {
		t.Fatalf("LimitsHit %q", ls)
	}
}

func TestVariableCallCandidateCapBoundary(t *testing.T) {
	for _, c := range []struct{ n, edges, caps int }{{16, 16, 0}, {17, 16, 1}} {
		var sb strings.Builder
		for i := 0; i < c.n; i++ {
			fmt.Fprintf(&sb, "f%d() { :; }\ncmd=f%d\n", i, i)
		}
		sb.WriteString("\"$cmd\" x\n")
		s := capsSnap(sb.String())
		by, _ := siteEdges(s)
		var cand, caps int
		for _, es := range by {
			for _, e := range es {
				switch e.Reason {
				case ReasonVariableConstant:
					cand++
				case ReasonCandidateCap:
					caps++
				}
			}
		}
		if cand != c.edges || caps != c.caps {
			t.Errorf("%d values: %d edges, %d caps; want %d, %d", c.n, cand, caps, c.edges, c.caps)
		}
	}
}

func TestVariableCallCapHoverAndDefinition(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "f%d() { :; }\ncmd=f%d\n", i, i)
	}
	sb.WriteString("\"$cmd\" x\n")
	q := buildMem(t, map[string]string{"m.sh": sb.String()})
	p, off := q.at("m.sh", `"$cmd"`, 0, 2)
	if got := q.s.Definition(p, off); len(got) != 16 {
		t.Fatalf("definition returned %d targets, want 16", len(got))
	}
	if md, _, ok := q.s.Hover(p, off); !ok || md == "" {
		t.Fatalf("no hover on the capped site")
	}
}

func dynScopeShape(r, c int) string {
	var sb strings.Builder
	sb.WriteString("f() { " + strings.Repeat("echo $y;", r) + " }\n")
	for i := 0; i < c; i++ {
		fmt.Fprintf(&sb, "h%d() { local y; f; }\n", i)
	}
	return sb.String()
}

func dynScopeBindings(s *Snapshot) map[*Use]int {
	m := map[*Use]int{}
	for _, bs := range s.bindings {
		for _, b := range bs {
			if b.Reason == ReasonDynamicScope && b.Res == Candidate {
				m[b.Use]++
			}
		}
	}
	return m
}

func TestDynamicScopeBindingCap(t *testing.T) {
	bound := 300 * time.Millisecond
	if raceBuild {
		bound *= 10
	}
	src := dynScopeShape(2000, 2000)
	var s *Snapshot
	if !withinBound(t, "build", bound, func() { s = capsSnap(src) }) {
		return
	}
	m := dynScopeBindings(s)
	if len(m) != 2000 {
		t.Fatalf("%d reads bound by dynamic scope, want 2000", len(m))
	}
	for u, n := range m {
		if n > maxDynScopeLocals {
			t.Fatalf("read at %d has %d dynamic-scope bindings", u.Range.Start.Offset, n)
		}
	}
	ls := limitLines(s, "dynamic-scope cap")
	want := "a.sh:1: dynamic-scope cap: 2000 variable reads bind to the first 16 of more caller locals"
	if len(ls) != 1 || ls[0] != want {
		t.Fatalf("LimitsHit %q, want %q", ls, want)
	}
}

func TestDynamicScopeBindingCapBoundary(t *testing.T) {
	for _, c := range []struct{ callers, bound, lines int }{{16, 16, 0}, {17, 16, 1}} {
		s := capsSnap(dynScopeShape(1, c.callers))
		m := dynScopeBindings(s)
		for u, n := range m {
			if n != c.bound {
				t.Errorf("%d callers: read at %d has %d bindings, want %d", c.callers, u.Range.Start.Offset, n, c.bound)
			}
		}
		if len(m) != 1 {
			t.Errorf("%d callers: %d reads bound", c.callers, len(m))
		}
		if got := len(limitLines(s, "dynamic-scope cap")); got != c.lines {
			t.Errorf("%d callers: %d limit lines, want %d", c.callers, got, c.lines)
		}
	}
}

func TestReferencesAllocsPerBinding(t *testing.T) {
	per := map[int]float64{}
	for _, n := range []int{10000, 40000} {
		src := "f() { " + strings.Repeat("y=1;", n) + " }\n"
		s := capsSnap(src)
		off := strings.Index(src, "y=1")
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		locs := s.References("/ws/a.sh", off, true)
		runtime.ReadMemStats(&m1)
		if len(locs) != n {
			t.Fatalf("n=%d: %d locations", n, len(locs))
		}
		per[n] = float64(m1.Mallocs-m0.Mallocs) / float64(len(locs))
		t.Logf("n=%d: %.2f mallocs per location", n, per[n])
		if per[n] > 0.5 { // 6.0 before the typed key
			t.Errorf("n=%d: %.2f mallocs per location, want at most 0.5", n, per[n])
		}
	}
	if per[40000] > 1.1*per[10000] {
		t.Errorf("per-location mallocs grow: %.2f at 10,000, %.2f at 40,000", per[10000], per[40000])
	}
}

func TestUnsetShapeCapped(t *testing.T) {
	_, bound := shapeSize()
	src := strings.Repeat("f(){ :; }; unset -f f; f\n", 2000)
	var s *Snapshot
	if !withinBound(t, "build", bound, func() { s = capsSnap(src) }) {
		return
	}
	by, order := siteEdges(s)
	if len(order) == 0 {
		t.Fatal("no call sites")
	}
	for _, o := range order {
		if len(by[o]) > maxSiteTargets+1 {
			t.Fatalf("site at %d has %d edges", o, len(by[o]))
		}
		for _, e := range by[o] {
			if e.Reason != ReasonUnsetInView && e.Reason != ReasonCandidateCap {
				t.Fatalf("site at %d: edge %s/%s", o, e.Res, e.Reason)
			}
		}
	}
}

func TestCallersAllocLinear(t *testing.T) {
	s := capsSnap(defsCalls(2000, 2000))
	by, order := siteEdges(s)
	sym := by[order[0]][0].To
	node := Node{Path: "/ws/a.sh", Func: sym}
	steps := s.Callers(node, 0) // builds the adjacency
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	steps = s.Callers(node, 0)
	runtime.ReadMemStats(&m1)
	limit := uint64(3*len(steps))*uint64(unsafe.Sizeof(Step{})) + 1<<20
	t.Logf("%d steps, %d bytes allocated, limit %d", len(steps), m1.TotalAlloc-m0.TotalAlloc, limit)
	if got := m1.TotalAlloc - m0.TotalAlloc; got > limit {
		t.Fatalf("Callers allocated %d bytes for %d steps, limit %d", got, len(steps), limit)
	}
}

func evalBudgetSrc() string {
	n := (MaxFileBytes - 8) / 2
	return "eval \"" + strings.Repeat("$a", n) + "\"\n"
}

func TestStringBudgetReason(t *testing.T) {
	src := evalBudgetSrc()
	p := "/ws/a.sh"
	s := Build(map[string]*FileFacts{p: Bind(front.Parse(p, []byte(src), front.Bash))}, Options{Root: "/ws"}, 1)
	var dyn []Edge
	for _, e := range s.Edges() {
		if e.Res == Dynamic {
			dyn = append(dyn, e)
		}
	}
	if len(dyn) != 1 || dyn[0].Reason != ReasonStringBudget {
		t.Fatalf("dynamic edges %+v, want one string-budget", dyn)
	}
	if un := s.Unresolved(nil); len(un) != 1 || un[0].Reason != ReasonStringBudget {
		t.Fatalf("Unresolved %+v", un)
	}
	if ls := limitLines(s, "string budget"); len(ls) != 1 || !strings.HasPrefix(ls[0], "a.sh:1: string budget: 1 string bodies past 2 MiB per file were not parsed") {
		t.Fatalf("LimitsHit %q", ls)
	}
	ds := ofRule(s.Diagnostics(p, DiagOptions{}), "WB1006")
	if len(ds) != 1 || ds[0].Severity != SevInfo || ds[0].Range.Start.Line != 1 {
		t.Fatalf("WB1006 findings %+v", ds)
	}
}

func TestStringDepthStaysExpansion(t *testing.T) {
	// maxStringDepth levels parse; the next is a dynamic/expansion site.
	src := "eval \"eval 'eval \\\"eval f\\\"'\"\n"
	s := capsSnap("f() { :; }\n" + src)
	for _, e := range s.Edges() {
		if e.Res == Dynamic && e.Reason != ReasonExpansion {
			t.Fatalf("depth-bound site has reason %s", e.Reason)
		}
	}
	if got := ofRule(s.Diagnostics("/ws/a.sh", DiagOptions{}), "WB1006"); len(got) != 0 {
		t.Fatalf("WB1006 on a depth-bound string: %+v", got)
	}
	var depth int
	for _, e := range s.Edges() {
		if e.Res == Dynamic && e.Reason == ReasonExpansion {
			depth++
		}
	}
	if depth != 1 {
		t.Fatalf("%d dynamic/expansion edges, want 1", depth)
	}
}

func TestBuiltinUnsetForms(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh":  "k() { :; }\nj() { :; }\nm() { :; }\nbuiltin unset -f k\ncommand unset -f j\ncommand -p unset -fv -- m\n",
		"main.sh": "source ./lib.sh\nk\nj\nm\n",
		"no.sh":   "a() { :; }\nb() { :; }\nc() { :; }\nd() { :; }\nbuiltin unset a\ncommand -v unset\ncommand unset\nbuiltin unset -v b\na\nb\nc\n",
		"esc.sh":  "e() { :; }\nbuiltin \\unset -f e\ne\n",
	})
	wantSame(t, q.fileEdges("main.sh"),
		"main.sh:2 calls candidate/unset-in-view «k» -> k@lib.sh:1",
		"main.sh:3 calls candidate/unset-in-view «j» -> j@lib.sh:2",
		"main.sh:4 calls candidate/unset-in-view «m» -> m@lib.sh:3",
	)
	wantSame(t, q.fileEdges("no.sh"),
		"no.sh:9 calls resolved/direct «a» -> a@no.sh:1",
		"no.sh:10 calls resolved/direct «b» -> b@no.sh:2",
		"no.sh:11 calls resolved/direct «c» -> c@no.sh:3",
	)
	wantSame(t, q.fileEdges("esc.sh"), "esc.sh:3 calls candidate/unset-in-view «e» -> e@esc.sh:1")
}

func TestLimitReasonsRegistered(t *testing.T) {
	if len(limitReasons) != 5 {
		t.Fatalf("%d limit reasons: %+v", len(limitReasons), limitReasons)
	}
	for _, lr := range limitReasons {
		if lr.name == "" || lr.format == "" || !strings.Contains(lr.format, "%d") {
			t.Errorf("limit reason %+v has no format", lr)
		}
		inNames := slices.Contains(reasonNames, lr.name)
		edge := lr.name == ReasonCandidateCap || lr.name == ReasonStringBudget
		if inNames != edge {
			t.Errorf("%s: in reasonNames %v, an edge reason %v", lr.name, inNames, edge)
		}
	}
	if slices.Contains(reasonNames, ReasonDynamicScopeCap) {
		t.Error("dynamic-scope-cap is in reasonNames: it is a binding reason, never an edge")
	}
}

// The cap sites are dynamic sites in s.dyn, which Build's edge table makes
// again on every run without doubling them.
func TestCandidateCapSiteIsADynamicSite(t *testing.T) {
	src := strings.Repeat("g() { :; }\n", 17) + "h() { g; }\n"
	s := capsSnap(src)
	if len(s.dyn[0]) != 1 || s.dyn[0][0].reason != codeCandidateCap {
		t.Fatalf("dynamic sites %+v", s.dyn[0])
	}
	s.buildEdges(len(s.recs))
	if len(s.dyn[0]) != 1 {
		t.Fatalf("buildEdges twice: %d dynamic sites", len(s.dyn[0]))
	}
	if by, _ := siteEdges(s); len(by) != 1 {
		t.Fatalf("edges after a second buildEdges: %d sites", len(by))
	}
}

// A function that calls a name with more candidates than the cap lists may
// call any of them: for the caller-local check of WB1401 it is a function
// with calls that are not all known (rules.go reads the dynamic sites), so the
// proof that a local cannot reach the reader is dropped. With 16 definitions
// the calls are all known and the proof stands.
func TestCandidateCapDropsCallerLocalProof(t *testing.T) {
	shape := func(defs int) string {
		return "v=755\nk() { chmod $v a; }\nh() { local v=$1; g; }\n" + strings.Repeat("g() { :; }\n", defs)
	}
	expectRule(t, sh(shape(16)), "x.sh", "WB1401")
	expectRule(t, sh(shape(17)), "x.sh", "WB1401", "2:$v")
}

// --- heredoc scan, limit reporting, `set -a` ---

// The heredoc scan does one pass over the tree, not one tree query
// per heredoc; three doublings of a file that has a heredoc every three lines,
// and of one with each heredoc inside deeply nested `$( )`, grow by at most 2.5
// per doubling on average: (t4/t1)^(1/3), each time the best of 11 runs. A
// linear scan gives 2, a quadratic one 4; single doublings scatter under load
// and are logged, not asserted.
func TestHeredocScanGrowth(t *testing.T) {
	size := 256 << 10
	if raceBuild {
		size = 128 << 10
	}
	for name, unit := range map[string]string{
		"every three lines": "cat <<EOF\nx\nEOF\n",
		"nested":            strings.Repeat("$(", 40) + "cat <<EOF\nx\nEOF\n" + strings.Repeat(")", 40) + "\n",
	} {
		var times []time.Duration
		for i := 0; i < 4; i++ {
			sz := size << i
			f := front.Parse("/ws/a.sh", []byte(strings.Repeat(unit, sz/len(unit))), front.Bash)
			if f.Root == nil || len(f.Errors) > 0 {
				t.Fatalf("%s: not parsed cleanly", name)
			}
			best := time.Hour
			for k := 0; k < 11; k++ {
				start := time.Now()
				codeDollarNames(map[string]bool{}, f, nil)
				best = min(best, time.Since(start))
			}
			times = append(times, best)
		}
		t.Logf("%s: %v", name, times)
		overall := math.Cbrt(float64(times[3]) / float64(max(times[0], time.Millisecond)))
		if overall > 2.5 {
			t.Errorf("%s: grew x%.2f per doubling over three doublings (%v)", name, overall, times)
		}
	}
}

// The scan reads the same names as it did with a query per heredoc.
func TestHeredocScanSameNames(t *testing.T) {
	for _, src := range []string{
		"cat <<EOF\n$a ${b}\nEOF\necho '$c'\n",
		"cat <<'EOF'\n$a\nEOF\necho \"$d\" '$e'\n",
		"x=$((1<<2)); (( y = 1 << n )); echo '$f'\n",
		"cat 3<<E1 4<<-E2\n$g\nE1\n\t$h\n\tE2\necho '$i'\n",
		"exec {fd}<<EOF\n$j\nEOF\ncat <<A; cat <<B\n$k\nA\n$l\nB\n",
		"echo $(cat <<EOF\n$m\nEOF\n) '$n'\n",
		"cat <<EOF\n$o\nEOF\n(( ; <<\n",
		"f() { cat <<-EOF\n\t$p\n\tEOF\n}\necho '$q' $(( 2<<3 ))\n",
	} {
		f := front.Parse("/ws/a.sh", []byte(src), front.Bash)
		got := map[string]bool{}
		codeDollarNames(got, f, nil)
		want := map[string]bool{}
		codeDollarNamesWith(want, f, nil, queryHeredocOp(f))
		if !mapsEqual(got, want) {
			t.Errorf("%q: got %v, want %v", src, got, want)
		}
	}
}

// queryHeredocOp is the heredoc test the scan made before the index: one
// syntax-tree query per `<<`.
func queryHeredocOp(f *tree.File) func(int) bool {
	var loc *tree.Locator
	return func(i int) bool {
		if f.Root == nil {
			return true
		}
		if loc == nil {
			loc = f.NewLocator()
		}
		n := loc.At(i)
		if n == nil || n.Kind == tree.KErrorGap || n.Enclosing(tree.KErrorGap) != nil {
			return true
		}
		if a := n.Range.Start.Offset; n.Kind != tree.KRedirect || a > i || !fdPrefix(f.Src[a:i]) {
			return false
		}
		for _, k := range n.Children {
			if k.Kind == tree.KHeredoc {
				return true
			}
		}
		return false
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// The WB1401 caller-local search budget is in LimitsHit, per file.
func TestCallerLocalBudgetInLimitsHit(t *testing.T) {
	shape := func(n int) string {
		var sb strings.Builder
		sb.WriteString("v=755\ng() { chmod $v a; }\nh() { local v=$1;")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, " k%d;", i)
		}
		sb.WriteString(" }\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "k%[1]d() { :; }\nm%[1]d() { g; }\n", i)
		}
		return sb.String()
	}
	over := diagWorld(sh(shape(600)), Options{})
	p := filepath.Join(diagRoot, "x.sh")
	if got := limitLines(over, "caller-local search"); len(got) != 0 {
		t.Fatalf("before Diagnostics: %q", got)
	}
	over.Diagnostics(p, DiagOptions{})
	first := limitLines(over, "caller-local search")
	if len(first) != 1 || !strings.Contains(first[0], "x.sh:2: caller-local search: ") {
		t.Fatalf("LimitsHit %q", first)
	}
	over.Diagnostics(p, DiagOptions{}) // a second run gives the same line
	if again := limitLines(over, "caller-local search"); !slices.Equal(first, again) {
		t.Fatalf("second run: %q, first %q", again, first)
	}
	within := diagWorld(sh(shape(400)), Options{})
	within.Diagnostics(p, DiagOptions{})
	if got := limitLines(within, "caller-local search"); len(got) != 0 {
		t.Fatalf("within budget: %q", got)
	}
}

// The version-guard search step limit is in LimitsHit.
func versionGuardShape(chain, users int) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("chk() { [ \"${BASH_VERSINFO[0]}\" -ge 4 ] || exit 1; }\n")
	for i := 0; i < chain; i++ {
		next := "chk"
		if i+1 < chain {
			next = fmt.Sprintf("w%d", i+1)
		}
		fmt.Fprintf(&sb, "w%d() { %s; }\n", i, next)
	}
	sb.WriteString("w0\n")
	for i := 0; i < users; i++ {
		fmt.Fprintf(&sb, "u%d() { declare -A m%d; }\n", i, i)
	}
	return sb.String()
}

func TestVersionGuardBudgetInLimitsHit(t *testing.T) {
	over := diagWorld(sh(versionGuardShape(300, 600)), Options{})
	p := filepath.Join(diagRoot, "x.sh")
	over.Diagnostics(p, DiagOptions{Target: VersionRange{Min: BashVersion{Major: 3, Minor: 2}}})
	first := limitLines(over, "version-guard search")
	if len(first) != 1 || !strings.Contains(first[0], "x.sh:") {
		t.Fatalf("LimitsHit %q", first)
	}
	over.Diagnostics(p, DiagOptions{Target: VersionRange{Min: BashVersion{Major: 3, Minor: 2}}})
	if again := limitLines(over, "version-guard search"); !slices.Equal(first, again) {
		t.Fatalf("second run: %q, first %q", again, first)
	}
	small := diagWorld(sh(versionGuardShape(3, 3)), Options{})
	small.Diagnostics(p, DiagOptions{Target: VersionRange{Min: BashVersion{Major: 3, Minor: 2}}})
	if got := limitLines(small, "version-guard search"); len(got) != 0 {
		t.Fatalf("small shape: %q", got)
	}
}

// `set -a` in a pipeline stage or a background job runs in a
// subshell: it does not export the file's assignments.
func TestSetAllexportInSubshellBoundary(t *testing.T) {
	for _, line := range []string{"set -a | cat", "set -a &", "set -a|cat", "set -a&", "set -a |& cat"} {
		if mentionWalkNeeded([]byte(line + "\n")) {
			t.Errorf("%q opens the walk gate", line)
		}
		expectRule(t, sh("#!/bin/bash\n"+line+"\nx=1\necho hi\n"), "x.sh", "WB1202", "3:x")
	}
	// the cluster still ends at `||`, `&&`, `;` and a redirect
	for _, line := range []string{"set -a || exit", "set -a && x=2", "set -a; y=2", "set -a>/dev/null", "set -a &>/dev/null"} {
		if !mentionWalkNeeded([]byte(line + "\n")) {
			t.Errorf("%q does not open the walk gate", line)
		}
	}
	// the walk itself, started by another trigger, agrees
	expectRule(t, sh("#!/bin/bash\ndeclare -p z\nset -a | cat\nx=1\necho hi\n"), "x.sh", "WB1202", "4:x")
	expectRule(t, sh("#!/bin/bash\ndeclare -p z\nset -a\nx=1\necho hi\n"), "x.sh", "WB1202")
}

// Several top-level guard callees spend one step budget; the order they are
// searched in must not depend on map order: fresh snapshots of one source give
// one LimitsHit line and one set of findings.
func multiGuardShape(chain, users int) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("chk() { [ \"${BASH_VERSINFO[0]}\" -ge 4 ] || exit 1; }\n")
	for _, p := range []string{"w", "x", "y", "z"} {
		for i := 0; i < chain; i++ {
			next := "chk"
			if i+1 < chain {
				next = fmt.Sprintf("%s%d", p, i+1)
			}
			fmt.Fprintf(&sb, "%s%d() { %s; }\n", p, i, next)
		}
	}
	sb.WriteString("zq() { chk; }\nw0\nx0\nzq\ny0\nz0\n")
	for i := 0; i < users; i++ {
		fmt.Fprintf(&sb, "u%d() { declare -A m%d; }\n", i, i)
	}
	return sb.String()
}

func TestVersionGuardSearchIsDeterministic(t *testing.T) {
	src := multiGuardShape(300, 600)
	lines, finds := map[string]bool{}, map[string]bool{}
	for run := 0; run < 40; run++ {
		s := diagWorld(sh(src), Options{})
		ds := s.Diagnostics(filepath.Join(diagRoot, "x.sh"), DiagOptions{Target: VersionRange{Min: BashVersion{Major: 3, Minor: 2}}})
		lines[strings.Join(limitLines(s, "version-guard search"), "|")] = true
		var keys []string
		for _, d := range ds {
			keys = append(keys, fmt.Sprintf("%s:%d", d.Rule, d.Range.Start.Line))
		}
		finds[strings.Join(keys, ",")] = true
	}
	for l := range lines {
		if l == "" {
			t.Fatal("the shape does not exhaust the search budget: no LimitsHit line")
		}
	}
	if len(lines) != 1 || len(finds) != 1 {
		t.Fatalf("%d distinct LimitsHit lines, %d distinct finding sets over 40 fresh snapshots", len(lines), len(finds))
	}
}

// Names called from a string (eval, trap, bash -c) list exactly the first 16
// definitions plus one cap edge once there are more.
func TestStringSiteCandidateCap(t *testing.T) {
	for _, form := range []string{"eval \"k\"\n", "trap 'k' EXIT\n", "bash -c 'k'\n"} {
		for _, c := range []struct{ defs, cands, caps int }{{16, 16, 0}, {17, 16, 1}, {20, 16, 1}} {
			s := capsSnap(strings.Repeat("k() { :; }\n", c.defs) + form)
			var cands []Edge
			caps := 0
			for _, e := range s.Edges() {
				switch {
				case e.Rel != RelCalls:
				case e.Res == Candidate:
					cands = append(cands, e)
				case e.Res == Dynamic && e.Reason == ReasonCandidateCap:
					caps++
				default:
					t.Errorf("%q, %d definitions: unexpected edge %+v", form, c.defs, e)
				}
			}
			if len(cands) != c.cands || caps != c.caps {
				t.Errorf("%q, %d definitions: %d candidates and %d caps, want %d and %d", form, c.defs, len(cands), caps, c.cands, c.caps)
				continue
			}
			for i, e := range cands {
				if e.ToDecl != e.To.Decls[i] {
					t.Errorf("%q, %d definitions: candidate %d is not definition %d", form, c.defs, i, i)
				}
			}
		}
	}
}

// The candidates of a name defined outside the closure are the first 16
// definitions, in order.
func TestSiteCandidateCapOutsideClosureKeepsFirst(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh":  strings.Repeat("g() { :; }\n", 20),
		"main.sh": "g\n",
	})
	n := 0
	for _, e := range q.s.Edges() {
		if e.Site.Path != q.abs("main.sh") || e.Rel != RelCalls || e.Res != Candidate {
			continue
		}
		if e.ToDecl != e.To.Decls[n] {
			t.Fatalf("candidate %d is not definition %d", n, n)
		}
		n++
	}
	if n != 16 {
		t.Fatalf("%d candidates, want 16", n)
	}
}

// A read with more caller locals than the cap binds to those of the first 16
// callers, in caller order.
func TestDynamicScopeCapKeepsFirstCallers(t *testing.T) {
	s := capsSnap(dynScopeShape(1, 20))
	lines := map[int]bool{}
	for _, bs := range s.bindings {
		for _, b := range bs {
			if b.Reason == ReasonDynamicScope && b.Res == Candidate {
				lines[b.Sym.Decls[0].NameRange.Start.Line] = true
			}
		}
	}
	if len(lines) != 16 {
		t.Fatalf("bound to locals on %d lines, want 16", len(lines))
	}
	for l := 2; l < 18; l++ { // h0 is on line 2
		if !lines[l] {
			t.Errorf("the local of the caller on line %d is not bound", l)
		}
	}
}

// n guard functions, each called at the top level, and n users of a bash 4
// feature: the version-guard path must not cost more than linear in n.
func TestVersionGuardManyTopCallsGrowth(t *testing.T) {
	n := 500
	if raceBuild {
		n = 250
	}
	var times []time.Duration
	for i := 0; i < 4; i++ {
		k := n << i
		var sb strings.Builder
		sb.WriteString("#!/bin/bash\n")
		for j := 0; j < k; j++ {
			fmt.Fprintf(&sb, "g%d() { [ \"${BASH_VERSINFO[0]}\" -ge 4 ] || exit 1; }\n", j)
		}
		for j := 0; j < k; j++ {
			fmt.Fprintf(&sb, "g%d\n", j)
		}
		for j := 0; j < k; j++ {
			fmt.Fprintf(&sb, "u%d() { declare -A m%d; }\n", j, j)
		}
		src := sb.String()
		best := time.Hour
		for r := 0; r < 5; r++ {
			s := diagWorld(sh(src), Options{})
			start := time.Now()
			var done bool
			if !withinBound(t, fmt.Sprintf("diagnostics n=%d", k), 20*time.Second, func() {
				s.Diagnostics(filepath.Join(diagRoot, "x.sh"), DiagOptions{Target: VersionRange{Min: BashVersion{Major: 3, Minor: 2}}})
				done = true
			}) || !done {
				return
			}
			best = min(best, time.Since(start))
		}
		times = append(times, best)
	}
	t.Logf("best of 5 at n, 2n, 4n, 8n: %v", times)
	if onCI() {
		t.Logf("CI: growth assertion left to development machines")
		return
	}
	if overall := math.Cbrt(float64(times[3]) / float64(max(times[0], time.Millisecond))); overall > 2.5 {
		t.Errorf("grew x%.2f per doubling over three doublings (%v)", overall, times)
	}
}
