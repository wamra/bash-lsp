package analysis

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// siteStr is "rel res/reason «site text» -> target" with the site text read
// from the file through the edge's offsets. It also checks that the site's
// line and column agree with its offset in the outer file.
func (q *qworld) siteStr(e Edge) string {
	q.t.Helper()
	f := q.s.Facts(e.Site.Path).File
	a, b := e.Site.Range.Start.Offset, e.Site.Range.End.Offset
	if a < 0 || b > len(f.Src) || a > b {
		q.t.Fatalf("site [%d,%d) outside %s", a, b, e.Site.Path)
	}
	if e.Site.Range.Start != f.PosAt(a) || e.Site.Range.End != f.PosAt(b) {
		q.t.Fatalf("site %+v: line/col disagree with offsets (want %+v %+v)", e.Site.Range, f.PosAt(a), f.PosAt(b))
	}
	to := "-"
	switch {
	case e.ToDecl != nil:
		to = fmt.Sprintf("%s@%s:%d", e.To.Name, q.rel(q.s.paths[q.s.declFile[e.ToDecl]]), e.ToDecl.NameRange.Start.Line)
	case e.ToFile != "":
		to = q.rel(e.ToFile)
	}
	s := fmt.Sprintf("%s:%d %s %s/%s «%s» -> %s", q.rel(e.Site.Path), e.Site.Range.Start.Line, e.Rel, e.Res, e.Reason, f.Src[a:b], to)
	if e.Text != "" {
		s += " text=" + e.Text
	}
	return s
}

// fileEdges lists, as siteStr, the edges whose site is in file, except
// source edges.
func (q *qworld) fileEdges(file string) []string {
	q.t.Helper()
	out := []string{}
	for _, e := range q.s.Edges() {
		if e.Site.Path == q.abs(file) && e.Rel != RelSources {
			out = append(out, q.siteStr(e))
		}
	}
	return out
}

func TestStringEdgesPinnedPositions(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "log() { :; }\nbuild_release() { :; }\n" +
			"trap 'log \"a b\"' EXIT\n" +
			"eval \"build_release \\\"$x\\\"\"\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:3 calls candidate/string-trap «log» -> log@m.sh:1",
		"m.sh:4 calls candidate/string-eval «build_release» -> build_release@m.sh:2",
	)
}

func TestStringEdgesTrapForms(t *testing.T) {
	// From `help trap` (bash 3.2): trap [-lp] [arg signal_spec ...]; ARG
	// absent with one SIGNAL_SPEC, or `-`, resets; ARG null ignores; -l and
	// -p list. A numeric first argument is a signal (checked: `trap 0 INT`
	// on /bin/bash 3.2 resets EXIT).
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\n" +
			"trap - EXIT\ntrap '' INT\ntrap -l\ntrap -p EXIT\ntrap EXIT\ntrap 0 INT\ntrap\n" +
			"trap f EXIT\n" +
			"trap -- 'f; f' INT TERM\n" +
			"trap \"$h\" EXIT\n" +
			"trap 'echo start\n  f' HUP\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:9 calls candidate/string-trap «f» -> f@m.sh:1",
		"m.sh:10 calls candidate/string-trap «f» -> f@m.sh:1",
		"m.sh:10 calls candidate/string-trap «f» -> f@m.sh:1",
		"m.sh:11 calls dynamic/expansion «$h» -> - text=$h",
		"m.sh:13 calls candidate/string-trap «f» -> f@m.sh:1",
	)
}

func TestStringEdgesShellC(t *testing.T) {
	// From `man bash` OPTIONS: with -c, commands are read from the first
	// non-option argument; checked on /bin/bash 3.2: -ec, -ce, `-c -e`,
	// `-o pipefail -c`, `-O extglob -c`, `-c --` and --norc all run the string.
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\n" +
			"bash -c 'f'\nsh -ec 'f'\nbash -o pipefail -c 'f x' name arg\ndash -c -e f\nksh -c \"f\"\n" +
			"bash --norc -O extglob -c -- 'f'\n" +
			"bash -c \"$x\"\n" +
			"bash -c\nbash -x f\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:2 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:3 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:4 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:5 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:6 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:7 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:8 calls dynamic/expansion «$x» -> - text=$x",
	)
}

func TestStringEdgesEvalJoinAndExpansionTime(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\ng() { :; }\n" +
			"eval \"$a\" f\n" +
			"eval f y\n" +
			"eval -- f\n" +
			"eval \"f $(g)\"\n" +
			"eval \"f \\$(g)\"\n" +
			"eval 'f'\"; g\"\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:3 calls dynamic/expansion «$a» -> - text=$a",
		"m.sh:4 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:5 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:6 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:6 calls resolved/direct «g» -> g@m.sh:2",
		"m.sh:7 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:7 calls candidate/string-eval «g» -> g@m.sh:2",
		"m.sh:8 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:8 calls candidate/string-eval «g» -> g@m.sh:2",
	)
}

func TestStringEdgesRecursionBound(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\n" +
			"eval \"eval 'f'\"\n" +
			"eval \"eval 'eval \\\"f\\\"'\"\n" +
			"eval \"eval 'eval \\\"eval f\\\"'\"\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:2 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:3 calls candidate/string-eval «f» -> f@m.sh:1",
		"m.sh:4 calls dynamic/expansion «f» -> - text=f",
	)
}

func TestVariableEdges(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\ng() { :; }\n" +
			"cmd=f\n\"$cmd\" x\n$cmd\n\"${cmd}\"\n${cmd:-g}\n" + // 3-7
			"$(pick) x\n${a}_${b}\n" + // 8-9
			"h() {\n  local c=g\n  \"$c\"\n}\n" + // 10-13
			"k() {\n  if x; then local d=f; else local d=g; fi\n  \"$d\"\n}\n", // 14-17
		"u.sh": "c1=$1\n\"$c1\"\nfor c2 in f g; do \"$c2\"; done\nread c3\n\"$c3\"\nc4=echo\n\"$c4\"\n" +
			"c5=f\nc5+=g\n\"$c5\"\nlocal -n c6=f\n\"$c6\"\n\"$1\"\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:4 calls candidate/variable-constant «\"$cmd\"» -> f@m.sh:1",
		"m.sh:5 calls candidate/variable-constant «$cmd» -> f@m.sh:1",
		"m.sh:6 calls candidate/variable-constant «\"${cmd}\"» -> f@m.sh:1",
		"m.sh:7 calls dynamic/expansion «${cmd:-g}» -> - text=${cmd:-g}",
		"m.sh:8 calls dynamic/expansion «$(pick)» -> - text=$(pick)",
		"m.sh:9 calls dynamic/expansion «${a}_${b}» -> - text=${a}_${b}",
		"m.sh:12 calls candidate/variable-constant «\"$c\"» -> g@m.sh:2",
		"m.sh:16 calls candidate/variable-constant «\"$d\"» -> f@m.sh:1",
		"m.sh:16 calls candidate/variable-constant «\"$d\"» -> g@m.sh:2",
	)
	wantSame(t, q.fileEdges("u.sh"),
		"u.sh:2 calls dynamic/variable-unknown «\"$c1\"» -> - text=\"$c1\"",
		"u.sh:3 calls dynamic/variable-unknown «\"$c2\"» -> - text=\"$c2\"",
		"u.sh:5 calls dynamic/variable-unknown «\"$c3\"» -> - text=\"$c3\"",
		"u.sh:7 calls dynamic/variable-unknown «\"$c4\"» -> - text=\"$c4\"",
		"u.sh:10 calls dynamic/variable-unknown «\"$c5\"» -> - text=\"$c5\"",
		"u.sh:12 calls dynamic/variable-unknown «\"$c6\"» -> - text=\"$c6\"",
		"u.sh:13 calls dynamic/variable-unknown «\"$1\"» -> - text=\"$1\"",
	)
}

func TestVariableEdgeQueries(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\ncmd=f\n\"$cmd\" x\nother=$1\n\"$other\"\n",
	})
	// Cursor on the "$cmd" word, on `$` and on `cmd`: the call wins.
	for _, off := range []int{0, 1, 2} {
		wantEq(t, q.def("m.sh", "\"$cmd\" x", 0, off), "m.sh:1")
		if h, ok := q.hover("m.sh", "\"$cmd\" x", 0, off); !ok || !strings.Contains(h, "Defined in `m.sh:1`") {
			t.Fatalf("hover at %d = %q", off, h)
		}
	}
	// References of f: definition, the literal `f` of cmd=f, the "$cmd" site.
	wantEq(t, q.refs("m.sh", "f()", 0, 0), "m.sh:1", "m.sh:2", "m.sh:3")
	// References of the variable still list the read inside "$cmd".
	wantEq(t, q.refs("m.sh", "cmd=f", 0, 0), "m.sh:2", "m.sh:3")
	// Unknown value: the cursor on "$other" finds the variable.
	wantEq(t, q.def("m.sh", "\"$other\"", 0, 2), "m.sh:4")
}

func TestStringSiteQueries(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "build_release() { :; }\neval \"build_release y\"\ntrap 'build_release' EXIT\n",
	})
	wantEq(t, q.def("m.sh", "build_release y", 0, 3), "m.sh:1")
	wantEq(t, q.refs("m.sh", "build_release y", 0, 3), "m.sh:1", "m.sh:2", "m.sh:3")
	if h, ok := q.hover("m.sh", "build_release y", 0, 3); !ok || !strings.Contains(h, "Called from 2 sites") {
		t.Fatalf("hover = %q", h)
	}
}

func TestDynamicEdgesCorpus(t *testing.T) {
	q := buildCorpus(t)
	// C03 shape: definition of log inside the trap string.
	wantEq(t, q.def("main.sh", "trap 'log bye'", 0, 6), "lib/util.sh:3")
	refs := q.refs("main.sh", "  build_release \"$name\"", 0, 3)
	wantContains(t, refs, "main.sh:8", "main.sh:9", "main.sh:10", "lib/util.sh:4", "main.sh:6", "main.sh:7")
	for _, r := range refs {
		if strings.HasPrefix(r, "orphan.sh") {
			t.Fatalf("references reach orphan.sh: %v", refs)
		}
	}
	br := q.sym("lib/util.sh", "build_release() {", 0, 0)
	var got []string
	for _, e := range q.s.EdgesTo(br) {
		got = append(got, q.siteStr(e))
	}
	wantContains(t, got,
		"main.sh:9 calls candidate/variable-constant «\"$cmd\"» -> build_release@lib/util.sh:4",
		"main.sh:10 calls candidate/string-eval «build_release» -> build_release@lib/util.sh:4",
	)
	log := q.sym("lib/util.sh", "log() {", 0, 0)
	got = nil
	for _, e := range q.s.EdgesTo(log) {
		got = append(got, q.siteStr(e))
	}
	wantContains(t, got, "main.sh:11 calls candidate/string-trap «log» -> log@lib/util.sh:3")
	for _, e := range q.s.EdgesTo(log) {
		if e.Site.Range.Start.Line == 11 && (e.From.Func == nil || e.From.Func.Name != "deploy") {
			t.Fatalf("trap edge caller = %+v, want deploy", e.From)
		}
	}
}

// The C04/C25 guard: a file that cannot see lib/util.sh gets no edge to its
// build_release from a "$cmd" or an eval string.
func TestDynamicEdgesStayInClosure(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib/util.sh": "build_release() { :; }\n",
		"main.sh":     "source ./lib/util.sh\n",
		"other.sh":    "x() { :; }\n",
		"t.sh":        "source ./other.sh\ncmd=build_release\n\"$cmd\"\neval \"build_release\"\nbuild_release\n",
		"lone.sh":     "cmd=build_release\n\"$cmd\"\neval \"build_release\"\nbuild_release\n",
	})
	wantSame(t, q.fileEdges("t.sh"),
		"t.sh:3 calls dynamic/variable-unknown «\"$cmd\"» -> - text=\"$cmd\"",
	)
	// lone.sh has no source edges and nothing sources it: the existing
	// name-match-outside-closure rule applies to the string call exactly as to
	// the plain call on line 4; a variable needs a visible function.
	wantSame(t, q.fileEdges("lone.sh"),
		"lone.sh:2 calls dynamic/variable-unknown «\"$cmd\"» -> - text=\"$cmd\"",
		"lone.sh:3 calls candidate/name-match-outside-closure «build_release» -> build_release@lib/util.sh:1",
		"lone.sh:4 calls candidate/name-match-outside-closure «build_release» -> build_release@lib/util.sh:1",
	)
}

func TestInvokesEdges(t *testing.T) {
	q := buildMem(t, map[string]string{
		"bin/x.sh": "echo x\n",
		"lib/y.sh": "echo y\n",
		"run.sh": "./bin/x.sh a\nbash lib/y.sh\nsh -x ./bin/x.sh\n./missing.sh\nbash -c 'lib/y.sh'\nbash\n" +
			"f() {\n  bash -o errexit ./lib/y.sh\n}\n",
	})
	f := q.sym("run.sh", "f()", 0, 0)
	wantSame(t, q.fileEdges("run.sh"),
		"run.sh:1 invokes resolved/direct «./bin/x.sh» -> bin/x.sh",
		"run.sh:2 invokes resolved/direct «lib/y.sh» -> lib/y.sh",
		"run.sh:3 invokes resolved/direct «./bin/x.sh» -> bin/x.sh",
		"run.sh:8 invokes resolved/direct «./lib/y.sh» -> lib/y.sh",
	)
	for _, e := range q.s.EdgesFrom(Caller{Path: q.abs("run.sh"), Func: f}) {
		if e.Rel != RelInvokes {
			t.Fatalf("unexpected edge from f: %s", q.siteStr(e))
		}
	}
	if len(q.s.Visible(q.abs("run.sh"))) != 1 {
		t.Fatalf("invokes changed visibility: %v", q.s.Visible(q.abs("run.sh")))
	}
}

func TestDynamicScopeBinding(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "outer() {\n  local v=1\n  inner\n}\n" + // 1-4
			"inner() {\n  echo \"$v\"\n}\n" + // 5-7
			"a() {\n  local w=1\n  b\n}\nb() {\n  c\n}\nc() {\n  echo \"$w\"\n}\n" + // 8-16
			"top() {\n  echo \"$z\"\n}\nz=1\n" + // 17-20
			"p() {\n  local u=1\n  br\n}\nalias br=r\nr() {\n  echo \"$u\"\n}\n", // 21-28
	})
	if got := q.resReason("m.sh", "$v", 0, 1); got != "candidate/dynamic-scope" {
		t.Fatalf("$v in inner = %s", got)
	}
	wantEq(t, q.refs("m.sh", "local v=1", 0, 6), "m.sh:2", "m.sh:6")
	wantEq(t, q.def("m.sh", "$v", 0, 1), "m.sh:2")
	// Depth 2 is not followed.
	if got := q.resReason("m.sh", "$w", 0, 1); got != "unresolved/environment" {
		t.Fatalf("$w in c = %s", got)
	}
	// A lexically visible global wins.
	if got := q.resReason("m.sh", "$z", 0, 1); got != "resolved/direct" {
		t.Fatalf("$z = %s", got)
	}
	// A resolved call through an alias counts as a resolved call edge.
	if got := q.resReason("m.sh", "$u", 0, 1); got != "candidate/dynamic-scope" {
		t.Fatalf("$u in r = %s", got)
	}
}

func TestDynamicEdgeReasonsRegistered(t *testing.T) {
	for _, r := range []string{ReasonVariableConstant, ReasonStringEval, ReasonStringTrap, ReasonStringShellC, ReasonVariableUnknown, ReasonExpansion,
		ReasonCandidateCap, ReasonStringBudget} {
		if !slices.Contains(reasonNames, r) {
			t.Fatalf("reason %q not registered", r)
		}
		_ = reasonCode(r) // does not panic
	}
	if len(reasonNames) != 17 {
		t.Fatalf("%d reason names, want 17", len(reasonNames))
	}
}

// resReason is "resolution/reason" of the single binding of the use at needle.
func (q *qworld) resReason(file, needle string, occ, off int) string {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	bs := q.s.hitBindings(p, o)
	if len(bs) != 1 {
		q.t.Fatalf("%s %q: %d bindings", file, needle, len(bs))
	}
	return bs[0].Res.String() + "/" + bs[0].Reason
}

// callNames lists "name«range text»" of every call use of m.sh.
func (q *qworld) callNames(file string) []string {
	q.t.Helper()
	ff := q.s.Facts(q.abs(file))
	out := []string{}
	for _, u := range ff.Uses {
		if u.Kind == UseCall {
			out = append(out, fmt.Sprintf("%s«%s»", u.Name, ff.File.Src[u.Range.Start.Offset:u.Range.End.Offset]))
		}
	}
	return out
}

func TestStringLineContinuationInDoubleQuotes(t *testing.T) {
	// bash 3.2: backslash-newline inside double quotes is removed (checked:
	// `eval "echo a; \<nl>  f"` runs f). Backslash-CR-LF is not a
	// continuation: in double quotes `\<CR>` stays, eval then runs a command
	// named <CR> and f on the next line (checked on /bin/bash 3.2:
	// "line 3: \r: command not found", then F).
	q := buildMem(t, map[string]string{
		"a.sh": "eval \"x; \\\n  f\"\n",
		"b.sh": "eval \"x; \\\nf\"\n",
		"c.sh": "eval \"echo \\\n; f\"\n",
		"d.sh": "bash -c \"x; \\\n  f\"\n",
		"e.sh": "eval \"x; \\\r\nf\"\n",
	})
	wantSame(t, q.callNames("a.sh"), "eval«eval»", "x«x»", "f«f»")
	wantSame(t, q.callNames("b.sh"), "eval«eval»", "x«x»", "f«f»")
	wantSame(t, q.callNames("c.sh"), "eval«eval»", "echo«echo»", "f«f»")
	wantSame(t, q.callNames("d.sh"), "bash«bash»", "x«x»", "f«f»")
	wantContains(t, q.callNames("e.sh"), "f«f»")
}

func TestStringBodySizeCap(t *testing.T) {
	// The reviewer's shape: `eval "$a$a…"` filling the file cap. Each `$a`
	// would become a 7-byte placeholder; over MaxFileBytes the string is a
	// dynamic/expansion site and is not parsed.
	n := (MaxFileBytes - 8) / 2
	src := []byte("eval \"" + strings.Repeat("$a", n) + "\"\n")
	base := []byte("echo \"" + strings.Repeat("$a", n) + "\"\n")
	bind := func(b []byte) (*FileFacts, uint64) {
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		ff := Bind(front.Parse("/ws/f.sh", b, front.Bash))
		runtime.ReadMemStats(&m1)
		return ff, m1.TotalAlloc - m0.TotalAlloc
	}
	_, baseAlloc := bind(base)
	ff, alloc := bind(src)
	if len(ff.Dynamic) != 1 {
		t.Fatalf("want one dynamic site, got %d", len(ff.Dynamic))
	}
	for _, u := range ff.Uses {
		if u.Kind == UseCall && u.Via != ViaNone {
			t.Fatalf("string was parsed: %+v", u)
		}
	}
	t.Logf("eval allocated %d bytes, echo %d", alloc, baseAlloc)
	if alloc > 2*baseAlloc {
		t.Fatalf("eval allocated %d bytes, echo %d", alloc, baseAlloc)
	}
}

func TestUnsetInView(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh":  "k() { :; }\nunset -f k\n",
		"main.sh": "source ./lib.sh\nk\nalias kk=k\nkk\neval k\nc=k\n\"$c\"\nh() { :; }\nh\n",
		"v.sh":    "k() { :; }\nunset k\nk\nunset -fv -- j\nj() { :; }\nj\n",
	})
	wantSame(t, q.fileEdges("main.sh"),
		"main.sh:2 calls candidate/unset-in-view «k» -> k@lib.sh:1",
		"main.sh:3 aliases candidate/unset-in-view «k» -> k@lib.sh:1",
		"main.sh:4 calls candidate/unset-in-view «kk» -> k@lib.sh:1",
		"main.sh:5 calls candidate/unset-in-view «k» -> k@lib.sh:1",
		"main.sh:7 calls candidate/unset-in-view «\"$c\"» -> k@lib.sh:1",
		"main.sh:9 calls resolved/direct «h» -> h@main.sh:8",
	)
	// Plain `unset k` (no -f) is not covered; `unset -fv -- j` is.
	wantSame(t, q.fileEdges("v.sh"),
		"v.sh:3 calls resolved/direct «k» -> k@v.sh:1",
		"v.sh:6 calls candidate/unset-in-view «j» -> j@v.sh:5",
	)
	if !slices.Contains(reasonNames, ReasonUnsetInView) {
		t.Fatal("unset-in-view not registered")
	}
}

func TestHoverExitNotFromChildOrTrap(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "g() { bash -c 'exit 1'; }\nh() { trap 'exit 2' INT; }\ne() { eval 'exit 3'; }\n",
	})
	for _, c := range []struct {
		fn   string
		want bool
	}{{"g()", false}, {"h()", false}, {"e()", true}} {
		md, _ := q.hover("m.sh", c.fn, 0, 0)
		if got := strings.Contains(md, "Calls `exit`"); got != c.want {
			t.Errorf("%s: Calls exit = %v, want %v:\n%s", c.fn, got, c.want, md)
		}
	}
}

func TestValueBindingPerSymbol(t *testing.T) {
	// lib.sh's literal is seen from two closures with different build_release
	// symbols: each gets the literal as a reference, whatever the file order.
	q := buildMem(t, map[string]string{
		"lib.sh": "cmd=build_release\n",
		"u1.sh":  "build_release() { :; }\n",
		"u2.sh":  "build_release() { :; }\n",
		"m1.sh":  "source ./lib.sh\nsource ./u1.sh\n\"$cmd\"\n",
		"m2.sh":  "source ./lib.sh\nsource ./u2.sh\n\"$cmd\"\n",
	})
	wantContains(t, q.refs("u1.sh", "build_release", 0, 0), "lib.sh:1", "m1.sh:3")
	wantContains(t, q.refs("u2.sh", "build_release", 0, 0), "lib.sh:1", "m2.sh:3")
}

func TestNamesTarget(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\nfoo() { :; }\neval \\f\neval f\\\noo\neval f\n\"$c\"\nc=f\n",
	})
	ff := q.s.Facts(q.abs("m.sh"))
	got := []string{}
	for _, u := range ff.Uses {
		if u.Kind == UseCall && u.Via != ViaNone {
			got = append(got, fmt.Sprintf("%s«%s»=%v", u.Name, ff.File.Src[u.Range.Start.Offset:u.Range.End.Offset], u.NamesTarget()))
		}
	}
	wantSame(t, got, "f«\\f»=false", "foo«f\\\noo»=false", "f«f»=true", "c«\"$c\"»=false")
}

func TestPathQualifiedShells(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\n/bin/sh -c 'f'\n/bin/bash -ec f\n",
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:2 calls candidate/string-shell-c «f» -> f@m.sh:1",
		"m.sh:3 calls candidate/string-shell-c «f» -> f@m.sh:1",
	)
}

func TestDynamicScopeReasonIsNoEdgeReason(t *testing.T) {
	if slices.Contains(reasonNames, ReasonDynamicScope) {
		t.Fatal("dynamic-scope registered as an edge reason")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("reasonCode(dynamic-scope) did not panic")
		}
	}()
	reasonCode(ReasonDynamicScope)
}

// A `"$cmd"` call reads the variable by the variable rules, so a private
// declaration around the call (rules S1, S2) is the one it reads. Build
// finds it through privateOf (merge of m2's linear private-scope lookup).
func TestVariableCallSeesPrivateVariable(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\ng() { :; }\ncmd=f\n" +
			"( cmd=g; \"$cmd\" )\n" + // 4
			"\"$cmd\"\n" + // 5
			"x=$( ( cmd=g; ( \"$cmd\" ) ) )\n" + // 6
			"a | { cmd=g; \"$cmd\"; }\n" + // 7
			"h() { ( cmd=g; ( \"$cmd\" ) ); \"$cmd\"; }\n", // 8
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:4 calls candidate/variable-constant «\"$cmd\"» -> g@m.sh:2",
		"m.sh:5 calls candidate/variable-constant «\"$cmd\"» -> f@m.sh:1",
		"m.sh:6 calls candidate/variable-constant «\"$cmd\"» -> g@m.sh:2",
		"m.sh:7 calls candidate/variable-constant «\"$cmd\"» -> g@m.sh:2",
		"m.sh:8 calls candidate/variable-constant «\"$cmd\"» -> g@m.sh:2",
		"m.sh:8 calls candidate/variable-constant «\"$cmd\"» -> f@m.sh:1",
	)
}

// A backslash-quoted command name calls the function as the quoted forms do
// (`\f` is f after quote removal); in bash it also suppresses alias
// expansion, so `\g` with `alias g=f` calls the function g, not f.
func TestBackslashQuotedCommandName(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "f() { :; }\n" + // 1
			"\\f\n" + // 2
			"eval \"\\f\"\n" + // 3
			"'f'\n" + // 4
			"\"f\"\n" + // 5
			"alias g=f\n" + // 6
			"g() { :; }\n" + // 7
			"\\g\n" + // 8
			"g\n" + // 9
			"f\\\n\n" + // 10-11: a trailing escaped newline is not part of the name
			"\"\\f\"\n", // 12: inside double quotes the backslash stays: no such function
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:2 calls resolved/direct «\\f» -> f@m.sh:1",
		"m.sh:3 calls candidate/string-eval «\\f» -> f@m.sh:1",
		"m.sh:4 calls resolved/direct «'f'» -> f@m.sh:1",
		"m.sh:5 calls resolved/direct «\"f\"» -> f@m.sh:1",
		"m.sh:6 aliases resolved/direct «f» -> f@m.sh:1",
		"m.sh:8 calls resolved/direct «\\g» -> g@m.sh:7",
		"m.sh:9 calls resolved/alias «g» -> f@m.sh:1",
		"m.sh:10 calls resolved/direct «f\\\n» -> f@m.sh:1",
	)
	wantEq(t, q.def("m.sh", "\\f", 0, 1), "m.sh:1")
}

// A backslash-quoted builtin is the builtin (bash runs `\source x` as
// `source x`): the quote removal reaches source, eval, trap, shell -c,
// unset -f, read, mapfile, getopts and printf -v as for the plain spelling.
func TestBackslashQuotedBuiltins(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh": "lib_fn() { :; }\n",
		"m.sh": "\\source ./lib.sh\n" + // 1
			"lib_fn\n" + // 2
			"\\eval \"lib_fn\"\n" + // 3
			"k() { :; }\n" + // 4
			"\\unset -f k\n" + // 5
			"k\n" + // 6
			"\\read -r v\n" + // 7
			"echo \"$v\"\n", // 8
	})
	wantSame(t, q.fileEdges("m.sh"),
		"m.sh:2 calls resolved/direct «lib_fn» -> lib_fn@lib.sh:1",
		"m.sh:3 calls candidate/string-eval «lib_fn» -> lib_fn@lib.sh:1",
		"m.sh:6 calls candidate/unset-in-view «k» -> k@m.sh:4",
	)
	if got := q.resReason("m.sh", "$v", 0, 1); got != "resolved/direct" {
		t.Fatalf("$v after `\\read -r v` = %s", got)
	}
}
