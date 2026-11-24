package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// fuzzBudget is the time one fuzz input of n bytes may take in Bind or Build:
// generous (a 64 KiB input takes a few milliseconds), but review C1's
// quadratic bracket runs took 0.8 s at that size.
func fuzzBudget(n int) time.Duration {
	b := 100*time.Millisecond + time.Duration(n)*2*time.Microsecond
	if raceBuild {
		b *= 10
	}
	return b
}

// parseBudget is the time Parse may take on n bytes. Parsing is linear but
// deep nesting costs about 7 µs a level, and after an error resync parses the
// rest again from several lines (25 µs a byte for 3 KB of `(((`, found by this
// fuzzer); resync's cost is milestone 3's (docs/m1-deferred.md). The bound
// catches what grows faster than that.
func parseBudget(n int) time.Duration {
	b := time.Second + time.Duration(n)*100*time.Microsecond
	if raceBuild {
		b *= 10
	}
	return b
}

// withinBudget runs fn and fails t when it takes longer than budget, the best
// of three runs (a fuzz worker shares the machine with the others).
func withinBudget(t *testing.T, what string, budget time.Duration, n int, fn func()) {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		fn()
		if best = min(best, time.Since(start)); best <= budget {
			return
		}
	}
	t.Fatalf("%s took %v on %d bytes, over its budget of %v", what, best, n, budget)
}

// FuzzBind parses, binds and builds arbitrary input, Parse within parseBudget, Bind and Build each within fuzzBudget: no panic, every variable use and
// declaration lies inside the file, and a use that came from arithmetic (an
// arithmetic node, or a subscript or slice inside an expansion or assignment)
// covers exactly its name's bytes. The name range of a plain `$x` is the
// parser's, not this package's, and is not asserted.
func FuzzBind(f *testing.F) {
	for _, s := range []string{
		arithProgram,
		"echo $(( $x + y ))\n",
		"(( a[b[c]] = d ? e : f++, --g ))\n",
		"let \"x=x+1\" y++ 'z=1'\n",
		"declare -A m; echo \"${m[key]} ${v:o+1:l*2} ${a[i]:1:n}\" m[k]=1\n",
		"for ((i=0;i<n;i++)); do ((s+=i)); done\n",
		"echo \"héllo ✓\" $(( é + yy )) ${arr[ñ]}\n",
		"x=${arr[$((i+1))]} y=$(( ${z:-3} ** 2 ))\n",
		"( (( n++ )) ); echo $n; f() { (( g-- )); }\n",
		"eval \"f \\\"$x\\\"\"; trap 'log \"a b\"' EXIT\n",
		"bash -c 'g $((i+1))'; sh -ec \"h ${v:-1}\"\n",
		"cmd=f\n\"$cmd\" x; ${cmd} y\n",
		"eval \"x; \\\n  f\"; eval \\f\n",
		// review C1 at the 64 KiB this fuzzer takes: a bracket run, and names
		// deep in subshells (TestBracketRunsBindInBoundedTime, deepCases).
		"let \"" + strings.Repeat("a[", 32000) + "\"\n",
	} {
		f.Add([]byte(s))
	}
	for _, s := range deepCases(1 << 16) {
		f.Add([]byte(s))
	}
	for _, s := range dynamicShapes(60000) { // fix round 1: O(reads x calls) took 1 to 8 s at this size
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<16 {
			return
		}
		var file *tree.File
		var ff *FileFacts
		withinBudget(t, "Parse", parseBudget(len(src)), len(src), func() { file = front.Parse("/ws/f.sh", src, front.Bash) })
		withinBudget(t, "Bind", fuzzBudget(len(src)), len(src), func() { ff = Bind(file) })
		withinBudget(t, "Build", fuzzBudget(len(src)), len(src), func() { Build(map[string]*FileFacts{"/ws/f.sh": ff}, Options{Root: "/ws"}, 1) })
		check := func(what, name string, s, e int, arith bool) {
			if s < 0 || e > len(src) || s > e {
				t.Fatalf("%s %q: range [%d,%d) outside file of %d bytes", what, name, s, e, len(src))
			}
			if arith && string(src[s:e]) != name {
				t.Fatalf("%s %q: range text %q", what, name, src[s:e])
			}
		}
		for _, u := range ff.Uses {
			if u.Kind == UseVarRead || u.Kind == UseVarWrite {
				arith := false
				switch u.Node.Kind {
				case tree.KArith, tree.KArithExp, tree.KLet, tree.KCStyleFor:
					arith = true
				case tree.KParamExp, tree.KAssign:
					arith = u.Range != u.Node.NameRange
				}
				check("use", u.Name, u.Range.Start.Offset, u.Range.End.Offset, arith)
			}
		}
		for _, d := range ff.Decls {
			if d.Kind == DeclVariable {
				check("decl", d.Name, d.NameRange.Start.Offset, d.NameRange.End.Offset, d.Implicit == "arith")
			}
		}
	})
}

// FuzzEdges builds a snapshot of arbitrary input, so every eval, trap and
// shell -c string goes through string-body parsing and the offset map, and
// asks for every edge: no panic; every call use and every edge site lies
// inside the file, with line and column matching its offset. Parse takes at
// most parseBudget, Bind, Build and Edges each at most fuzzBudget.
func FuzzEdges(f *testing.F) {
	for _, s := range []string{
		"f() { :; }\ntrap 'log \"a b\"' EXIT\neval \"f \\\"$x\\\"\"\n",
		"eval \"eval 'eval \\\"eval f\\\"'\"\n",
		"bash -ec 'f; g' n a\nsh -o pipefail -c \"$x\"\ntrap -- 'f\n g' INT\n",
		"cmd=f\n\"$cmd\" x\n${a}_${b}\n$(pick) y\n./run.sh\nbash x.sh\n",
		"o() { local v=1; i; }\ni() { echo $v; }\n",
		"eval 'f'\"; g\" b\\ c $'x' \"$(h)\" \"\\$(k)\"\n",
		"eval \"",
		"trap '",
		"eval \"x; \\\n  f\"\neval \\f\neval f\\\noo\neval \"x; \\\r\nf\"\n",
		"k() { :; }\nunset -fv -- k\nk\n/bin/sh -c 'k'\n",
	} {
		f.Add([]byte(s))
	}
	// The merge's shapes at the 16 KiB this fuzzer takes (shapes_test.go).
	const n = 1 << 14
	for _, s := range []string{
		fillTo(n, "f() { :; }\n", "cmd=f; \"$cmd\";\n", ""),
		fillTo(n, "f() { :; }\ng() { "+strings.Repeat("( ", 1000), "cmd=f; \"$cmd\";", strings.Repeat(" )", 1000)+" }\n"),
		fillTo(n, "", "f() { x=1; echo $y; }\n", "f\n"),
		strings.Repeat("x=1\n", n/8) + strings.Repeat(". $f\n", n/10),
		aliasesOf(n),
		fillTo(n, "g() { "+strings.Repeat("( ", 1000), ". $f;", strings.Repeat(" )", 1000)+" }\n"),
		fillTo(n, "f() { :; }\n", "eval \"f $x\"\n", ""),
		fillTo(n, "f() { :; }\neval \"", "f;", "\"\n"),
		fillTo(n, "f() { :; }\n", "unset -f f; f\n", ""),
	} {
		f.Add([]byte(s))
	}
	for _, s := range dynamicShapes(n) {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<14 {
			return
		}
		p := "/ws/f.sh"
		var file *tree.File
		var ff *FileFacts
		withinBudget(t, "Parse", parseBudget(len(src)), len(src), func() { file = front.Parse(p, src, front.Bash) })
		withinBudget(t, "Bind", fuzzBudget(len(src)), len(src), func() { ff = Bind(file) })
		check := func(what string, r tree.Range) {
			a, b := r.Start.Offset, r.End.Offset
			if a < 0 || b > len(src) || a > b {
				t.Fatalf("%s: range [%d,%d) outside file of %d bytes", what, a, b, len(src))
			}
			if r.Start != file.PosAt(a) || r.End != file.PosAt(b) {
				t.Fatalf("%s: range %+v disagrees with its offsets", what, r)
			}
		}
		for _, u := range ff.Uses {
			if u.Kind == UseCall {
				check("call use "+u.Name, u.Range)
				if u.NamesTarget() && string(src[u.Range.Start.Offset:u.Range.End.Offset]) != u.Name {
					t.Fatalf("call use %q (via %d) claims its range spells it, text %q", u.Name, u.Via, src[u.Range.Start.Offset:u.Range.End.Offset])
				}
			}
		}
		var s *Snapshot
		withinBudget(t, "Build", fuzzBudget(len(src)), len(src), func() { s = Build(map[string]*FileFacts{p: ff}, Options{Root: "/ws"}, 1) })
		var edges []Edge
		withinBudget(t, "Edges", fuzzBudget(len(src)), len(src), func() { edges = s.Edges() })
		for _, bs := range s.bindings {
			for _, b := range bs {
				if u := b.Use; u.Kind == UseCall && u.NamesTarget() {
					check("binding "+u.Name, u.Range)
					if string(src[u.Range.Start.Offset:u.Range.End.Offset]) != u.Name {
						t.Fatalf("binding %q (via %d): range text %q", u.Name, u.Via, src[u.Range.Start.Offset:u.Range.End.Offset])
					}
				}
			}
		}
		for _, e := range edges {
			if e.Site.Path == p {
				check("edge "+e.Reason, e.Site.Range)
			}
		}
	})
}

// FuzzDiagnostics runs the native rules on arbitrary input: no panic, every
// finding's range (and every fix's edit) lies inside the file and agrees
// with its offsets, findings come sorted, a quote fix keeps the text it
// quotes. Diagnostics takes at most fuzzBudget.
func FuzzDiagnostics(f *testing.F) {
	for _, s := range []string{
		"#!/bin/bash\nf(){ local u=1; rm $u; }\nf(){ :; }\nrm $x\nsource ./nope.sh\n",
		"echo hi | read y; echo \"$y\"\n( y=2 ); echo \"$y\"\n",
		"#!/bin/bash\nlocal x=1\ntest() { :; }\ncd() { builtin cd \"$@\"; }\n",
		"# bash-lsp disable=WB1401\nrm $a\n# shellcheck disable=SC2000-SC2100,all\nrm $b\n",
		"f() {\n  # shellcheck disable=SC2086\n  rm $a ${b:+x} ${#c} \"$d\"$e\n}\n",
		"x=$(foo \\\n\"bar\nf() { :; }\n",
		"A=$((0 A\nf() { :; }\n",
		"tmp=$(mktemp); trap 'rm -f \"$tmp\"' EXIT\neval \"echo \\$u\"\n[[ -v z ]]\ndeclare -p w\nset -a\n",
		"a() { echo hi; }\n) ) fi done\n" + strings.Repeat("echo ${x\n", 60) + "z() { :; }\n",
		"declare -A m; echo ${m[-1]} ${a[-1]} ${v,,} $EPOCHSECONDS\nshopt -s globstar lastpipe; wait -n -p v\ndeclare -gn r=x\n",
		"#!/bin/sh\n[[ -n $x ]] && cat <<< hi <(a) $'x'; function f { a=(1); source y; }\ncoproc c\na |& b &> f\n",
		"((BASH_VERSINFO[0] >= 4)) && declare -A m\necho ${EPOCHSECONDS:-1} ${EPOCHSECONDS+x}\nEPOCHREALTIME=1; echo $EPOCHREALTIME\n",
	} {
		f.Add([]byte(s))
	}
	const n = 1 << 14
	for _, s := range []string{
		fillTo(n, "", "f() { local u=1; \"$h\"; }\n", ""),
		fillTo(n, "", "( y=1 ); echo \"$y\"\n", ""),
		fillTo(n, strings.Repeat("{\n# shellcheck disable=SC2086\n", 200), "rm $f\n", strings.Repeat("}\n", 200)),
		fillTo(n, "#!/bin/bash\necho hi\n", "f() { :; }\nx=1\n", ""),
		fillTo(n, "declare -a a\n", "echo \"${a[-1]} ${v,,}\"; wait -n\n", ""),
		fillTo(n, "#!/bin/sh\n", "[[ -n $x ]] && cat <<< hi\n", ""),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<14 {
			return
		}
		p := "/ws/f.sh"
		// Once as a bash file, once as an sh file (the dialect rules).
		for _, dialect := range []front.Dialect{front.Bash, front.POSIX} {
			fuzzDiagnostics(t, p, src, dialect)
		}
	})
}

func fuzzDiagnostics(t *testing.T, p string, src []byte, dialect front.Dialect) {
	t.Helper()
	var file *tree.File
	withinBudget(t, "Parse", parseBudget(len(src)), len(src), func() { file = front.Parse(p, src, dialect) })
	s := Build(map[string]*FileFacts{p: Bind(file)}, Options{Root: "/ws"}, 1)
	var ds []Diagnostic
	withinBudget(t, "Diagnostics", fuzzBudget(len(src)), len(src), func() {
		s.scans, s.mentions = nil, nil
		ds = s.Diagnostics(p, DiagOptions{})
	})
	check := func(what string, r tree.Range) {
		a, b := r.Start.Offset, r.End.Offset
		if a < 0 || b > len(src) || a > b {
			t.Fatalf("%s: range [%d,%d) outside file of %d bytes", what, a, b, len(src))
		}
		if r.Start != file.PosAt(a) || r.End != file.PosAt(b) {
			t.Fatalf("%s: range %+v disagrees with its offsets", what, r)
		}
	}
	for i, d := range ds {
		check(d.Rule, d.Range)
		if i > 0 && (ds[i-1].Range.Start.Offset > d.Range.Start.Offset ||
			ds[i-1].Range.Start.Offset == d.Range.Start.Offset && ds[i-1].Rule > d.Rule) {
			t.Fatalf("not sorted at %d: %+v then %+v", i, ds[i-1], d)
		}
		for _, fx := range d.Fixes {
			for _, e := range fx.Edits {
				check(d.Rule+" fix", e.Range)
				if old := string(src[e.Range.Start.Offset:e.Range.End.Offset]); e.NewText != "\""+old+"\"" {
					t.Fatalf("fix %q for %q", e.NewText, old)
				}
			}
		}
	}
}
