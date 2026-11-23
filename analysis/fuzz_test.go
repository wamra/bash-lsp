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
		// review C1 at the 64 KiB this fuzzer takes: a bracket run, and names
		// deep in subshells (TestBracketRunsBindInBoundedTime, deepCases).
		"let \"" + strings.Repeat("a[", 32000) + "\"\n",
	} {
		f.Add([]byte(s))
	}
	for _, s := range deepCases(1 << 16) {
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
