package analysis

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
)

// shapeSize and shapeBound are the input size and time bound of the
// bounded-time shape tests: about 2 MiB in 3 s (each shape takes well under a
// second once linear), a quarter of that size in 60 s under -race.
func shapeSize() (int, time.Duration) {
	if raceBuild {
		return (2<<20 - 64) / 4, 60 * time.Second
	}
	return 2<<20 - 64, 3 * time.Second
}

// fillTo repeats unit after pre until the text is about size bytes, then post.
func fillTo(size int, pre, unit, post string) string {
	return pre + strings.Repeat(unit, (size-len(pre)-len(post))/len(unit)) + post
}

// withinBound runs fn and fails t if it does not return within bound; it
// reports whether fn returned.
func withinBound(t *testing.T, what string, bound time.Duration, fn func()) bool {
	t.Helper()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		fn()
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		t.Logf("%s: %v", what, d.Round(time.Millisecond))
		return true
	case <-time.After(bound):
		t.Errorf("%s: not done within %v", what, bound)
		return false
	}
}

// withinBoundBest is withinBound for a bound that a loaded machine can miss
// once: setup then fn run up to tries times, and t fails only when every
// attempt overruns bound. It reports whether an attempt finished in time.
func withinBoundBest(t *testing.T, what string, bound time.Duration, tries int, setup func() func()) bool {
	t.Helper()
	for k := 0; k < tries; k++ {
		fn := setup()
		done := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			fn()
			done <- time.Since(start)
		}()
		select {
		case d := <-done:
			t.Logf("%s: %v (attempt %d)", what, d.Round(time.Millisecond), k+1)
			return true
		case <-time.After(bound):
			t.Logf("%s: not done within %v (attempt %d of %d)", what, bound, k+1, tries)
		}
	}
	t.Errorf("%s: not done within %v in %d attempts", what, bound, tries)
	return false
}

// parseClean parses src as Bash and fails t unless it parsed without error.
func parseClean(t *testing.T, name, src string) *FileFacts {
	t.Helper()
	f := front.Parse("/ws/a.sh", []byte(src), front.Bash)
	if f.Skipped != "" || len(f.Errors) > 0 {
		t.Fatalf("%s: not parsed cleanly: %q %v", name, f.Skipped, f.Errors)
	}
	return Bind(f)
}

// m2b shapes found by the merge's shape run: each must bind, build and list
// its edges in bounded time. "$cmd" calls each scanned every declaration of
// the variable's symbol: O(calls x assignments), 9.3 s at 128 KiB. The
// dynamic-scope pass listed the targets of every call of a function once
// per definition: O(definitions^2) for one call, 1.2 s at 176 KB. Resolving
// `. $f` scanned every declaration of the file for f's (varIsScriptDir):
// O(sources x declarations), 63 s at 900 KB. Each alias edge scanned every
// alias declaration of the file (aliasDeclOf): 0.43 s for 20,000 aliases.
// Each source edge walked up the tree to its function (funcDeclAround):
// O(sources x depth), Edges 3.9 s for 2 MiB of `source $f` 3,900 deep.
func TestM2bShapesBuildInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	cases := map[string]string{
		"assign and call cmd":            fillTo(size, "f() { :; }\n", "cmd=f; \"$cmd\";\n", ""),
		"assign and call cmd, private":   fillTo(size, "f() { :; }\ng() { (", " cmd=f; \"$cmd\";", " ); }\n"),
		"many definitions with reads":    fillTo(size, "", "f() { x=1; echo $y; }\n", "f\n"),
		"many definitions, local caller": fillTo(size, "g() { local y; f; }\n", "f() { echo $y; }\n", ""),
		"assignments, then sources":      strings.Repeat("x=1\n", size/8) + strings.Repeat(". $f\n", size/10),
		"aliases of one function":        aliasesOf(size),
		"deep sources":                   fillTo(size, "g() { "+strings.Repeat("( ", 3900), ". $f;", strings.Repeat(" )", 3900)+" }\n"),
	}
	for name, src := range cases {
		ff := parseClean(t, name, src)
		withinBound(t, name, bound, func() {
			s := Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1)
			s.Edges()
		})
	}
}

// aliasesOf is about size bytes of `alias aN=f` lines, after f's definition.
func aliasesOf(size int) string {
	var sb strings.Builder
	sb.WriteString("f() { :; }\n")
	for i := 0; sb.Len() < size-20; i++ {
		fmt.Fprintf(&sb, "alias a%d=f\n", i)
	}
	sb.WriteString("a0\n")
	return sb.String()
}

// Hover on a function scanned every use and binding of the file once per
// definition (bodyEffects): O(definitions x uses), 0.46 s at 20,000
// definitions of one name and quadratic beyond.
func TestHoverOnManyDefinitionsInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	src := fillTo(size, "", "f() { x=1; echo $y; }\n", "f\n")
	ff := parseClean(t, "many definitions", src)
	var s *Snapshot
	if !withinBound(t, "build", bound, func() { s = Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1) }) {
		return
	}
	var md string
	if !withinBound(t, "hover on a call of f", bound, func() { md, _, _ = s.Hover("/ws/a.sh", len(src)-2) }) {
		return
	}
	if !strings.Contains(md, "Reads globals: `y`") || !strings.Contains(md, "Writes globals: `x`") {
		t.Errorf("hover lost the body effects:\n%.300s", md)
	}
}

// Each function definition walked the scope chain to its confining private
// scope, through function scopes (confining): O(definitions x depth), Bind
// 1.2 s on 2 MiB of definitions inside 1,990 nested functions against 0.14 s
// at 500. Binding the same size at 1,990 levels must cost about what it
// costs at 250 (best of three each), not eight times as much.
func TestNestedFunctionDefinitionsBindIndependentOfDepth(t *testing.T) {
	size, _ := shapeSize()
	bind := func(depth int) time.Duration {
		src := fillTo(size, strings.Repeat("f() { ", depth), "h() { :; };", strings.Repeat(" }", depth)+"\n")
		f := front.Parse("/ws/a.sh", []byte(src), front.Bash)
		if f.Skipped != "" || len(f.Errors) > 0 {
			t.Fatalf("depth %d: not parsed cleanly: %q %v", depth, f.Skipped, f.Errors)
		}
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			ff := Bind(f)
			best = min(best, time.Since(start))
			if d := ff.Decls[len(ff.Decls)-1]; d.Kind != DeclFunction || d.Name != "h" {
				t.Fatalf("depth %d: last declaration %+v", depth, d)
			}
		}
		return best
	}
	shallow, deep := bind(250), bind(1990)
	t.Logf("Bind at 250 levels %v, at 1,990 levels %v", shallow, deep)
	if deep > 4*shallow+50*time.Millisecond {
		t.Errorf("Bind at 1,990 levels took %v, %.1fx its %v at 250", deep, float64(deep)/float64(shallow), shallow)
	}
}

// dynamicShapes are the dynamic-scope shapes that once cost O(reads x calls), about size
// bytes each: unresolved reads of y in f and calls of f from one function
// (with and without `local y`) or from many; and many distinct names read
// in f with many callers declaring some other local. dynamicScope looped,
// per unresolved read, over one entry per call of f (O(reads x calls):
// 13.4 s at 512 KiB and 56 s at 1 MiB without locals, 88 s and 351 s with).
func dynamicShapes(size int) map[string]string {
	q := size / 2
	var callers, names, localCallers strings.Builder
	for i := 0; callers.Len() < q-20; i++ {
		fmt.Fprintf(&callers, "h%d() { f; }\n", i)
	}
	for i := 0; names.Len() < q-20; i++ {
		fmt.Fprintf(&names, "echo $v%d;", i)
	}
	for i := 0; localCallers.Len() < q-30; i++ {
		fmt.Fprintf(&localCallers, "h%d() { local w; f; }\n", i)
	}
	reads := "f() { " + strings.Repeat("echo $y;", q/8) + " }\n"
	// the read name is a local of many callers (one symbol
	// each); also with three callees, each called by every caller
	var localY, localY3 strings.Builder
	localY.WriteString("f() { echo $y; }\n")
	for i := 0; localY.Len() < size-30; i++ {
		fmt.Fprintf(&localY, "h%d() { local y; f; }\n", i)
	}
	localY3.WriteString("f() { echo $y; }\ng() { echo $y; }\nk() { echo $y; }\n")
	for i := 0; localY3.Len() < size-40; i++ {
		fmt.Fprintf(&localY3, "h%d() { local y; f; g; k; }\n", i)
	}
	return map[string]string{
		"read name local in callers":            localY.String(),
		"read name local in callers, 3 callees": localY3.String(),
		"reads, calls":                          reads + "h() { " + strings.Repeat("f;", q/2) + " }\n",
		"reads, calls, local":                   reads + "h() { local y; " + strings.Repeat("f;", q/2) + " }\n",
		"reads, callers":                        reads + callers.String(),
		"distinct names, callers":               "f() { " + names.String() + " }\n" + localCallers.String(),
	}
}

func TestDynamicScopeInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	for name, src := range dynamicShapes(size) {
		b := bound
		if strings.HasPrefix(name, "read name local") {
			// O(callers^2) took 1.4 s and 2.6 s at 2 MiB, inside the
			// general bound; once linear these take about 0.1 s
			b = bound / 3
		}
		if !withinBoundBest(t, name, b, 3, func() func() {
			ff := parseClean(t, name, src)
			return func() { Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1) }
		}) {
			continue
		}
	}
}

// "$cmd" with n constant values on n call lines (n functions, cmd=f0 ...
// cmd=f(n-1)): the use has n symbols, each with n call bindings, each site n
// edges. ResolvedReferences listed every edge at a site for every binding of
// every symbol: O(n^3), 9.3 s at n = 1,000 (32 KB) and 76 s at 2,000. It must
// cost no more than the n^2 edges there are.
func TestResolvedReferencesOnVariableProductInBoundedTime(t *testing.T) {
	n, bound := 1000, 2*time.Second
	if raceBuild {
		n, bound = 500, 20*time.Second
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "f%d() { :; }\n", i)
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "cmd=f%d\n", i)
	}
	b.WriteString(strings.Repeat("\"$cmd\" x\n", n))
	src := b.String()
	ff := parseClean(t, "variable product", src)
	s := Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1)
	off := len(src) - len("\"$cmd\" x\n") + 1
	var got []Location
	if !withinBound(t, "ResolvedReferences on the last \"$cmd\"", bound, func() { got = s.ResolvedReferences("/ws/a.sh", off, true) }) {
		return
	}
	// Every site is candidate/variable-constant: only the definitions and
	// the `cmd=fi` literals (literal values are left out) remain.
	// A site lists its first maxSiteTargets functions (candidate-cap), so the
	// n definitions are 16.
	if want := min(n, maxSiteTargets); len(got) != want {
		t.Errorf("%d resolved references, want the %d definitions", len(got), want)
	}
}
