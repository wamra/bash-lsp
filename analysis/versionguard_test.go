package analysis

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func wb1301(t *testing.T, src string) int {
	t.Helper()
	return len(ofRule(diagsWith(t, oneFile("#!/bin/bash\n"+src), "x.sh", "3.2-5.3"), "WB1301"))
}

// Uses of a newer feature that tolerate its absence or sit behind a test
// stay silent; the same features written plainly are still reported.
func TestWB1301ToleratedAndGuarded(t *testing.T) {
	silent := []struct{ name, src string }{
		{"left of ||", "shopt -s globstar 2>/dev/null || true\n"},
		{"left of || without redirect", "declare -A m || exit 1\n"},
		{"left of || in a chain", "true && shopt -s globstar || true\n"},
		{"stderr to null", "shopt -s globstar 2>/dev/null\n"},
		{"stderr to null with space", "declare -A m 2> /dev/null\n"},
		{"both streams to null", "shopt -s lastpipe &>/dev/null\n"},
		{"EPOCHSECONDS in [[", "[[ -z $EPOCHSECONDS ]] && x=1\n"},
		{"EPOCHSECONDS in [", "[ \"$EPOCHSECONDS\" -gt 5 ]\n"},
		{"EPOCHSECONDS in test", "test -n \"$EPOCHREALTIME\" || x=1\n"},
		{"EPOCHSECONDS in a condition ((", "if ((EPOCHSECONDS > 5)); then :; fi\n"},
	}
	for _, c := range silent {
		t.Run(c.name, func(t *testing.T) {
			if n := wb1301(t, c.src); n != 0 {
				t.Fatalf("%d findings for %q", n, c.src)
			}
		})
	}
	reported := []struct {
		name, src string
		want      int
	}{
		{"unrelated test and &&", "[ -n \"$x\" ] && shopt -s globstar\n", 1},
		{"$# test and &&", "[ $# -gt 0 ] && declare -A m\n", 1},
		{"[[ and &&", "[[ -n $HOME ]] && shopt -s globstar\n", 1},
		{"test -d and &&", "test -d /tmp && declare -A m\n", 1},
		{"(( and &&", "(( n > 1 )) && declare -A m\n", 1},
		{"two tests and &&", "[ -f a ] && [ -f b ] && declare -A m\n", 1},
		{"negated test and &&", "! [ -f a ] && declare -A m\n", 1},
		{"if body on a test", "if [ -f x ]; then declare -A m; fi\n", 1},
		{"if [[ body", "if [[ $x == y ]]; then\n  readarray -t a < f\n  declare -A m\n  echo ${a[-1]} ${x,,}\nfi\n", 3},
		{"elif body", "if false; then :; elif [ -f a ]; then declare -A m; fi\n", 1},
		{"if (( body", "if (( $# > 1 )); then declare -n r=x; fi\n", 1},
		{"case arm with test", "case $1 in a) [ -f x ] && declare -A m;; esac\n", 1},
		{"plain", "declare -A m\n", 1},
		{"after ;", "true; declare -A m\n", 1},
		{"after && of a command", "true && declare -A m\n", 1},
		{"after && of a command, then a test", "true && [ -n x ] || declare -A m\n", 1},
		{"right of ||", "[ -n x ] || declare -A m\n", 1},
		{"if on a command", "if grep -q x f; then declare -A m; fi\n", 1},
		{"else of a test", "if [ -n x ]; then :; else declare -A m; fi\n", 1},
		{"nested deeper than the if body", "if [ -n x ]; then for i in 1; do declare -A m; done; fi\n", 1},
		{"stderr to a file", "shopt -s globstar 2>err.log\n", 1},
		{"stdout to null", "shopt -s globstar >/dev/null\n", 1},
		{"function body", "f() { shopt -s globstar; }\n", 1},
		{"loop body", "while read l; do declare -A m; done\n", 1},
		{"EPOCHSECONDS printed", "echo \"$EPOCHSECONDS\"\n", 1},
		{"EPOCHSECONDS assigned from", "now=$EPOCHSECONDS\n", 1},
		{"test on another name", "[ -n \"$x\" ]; echo $EPOCHSECONDS\n", 1},
	}
	for _, c := range reported {
		t.Run("reported "+c.name, func(t *testing.T) {
			if n := wb1301(t, c.src); n != c.want {
				t.Fatalf("%d findings for %q, want %d", n, c.src, c.want)
			}
		})
	}
}

// Only a read of BASH_VERSION or BASH_VERSINFO in code, that is not just
// printed, guards the features after it.
func TestWB1301VersionReads(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"printed", "echo \"running on $BASH_VERSION\"\ndeclare -A m\n", 1},
		{"printf", "printf '%s\\n' \"$BASH_VERSION\"\ndeclare -A m\n", 1},
		{"comment", "# TODO check BASH_VERSION\ndeclare -A m\n", 1},
		{"single quoted", "echo 'BASH_VERSION'\ndeclare -A m\n", 1},
		{"quoted heredoc", "cat <<'EOF'\n$BASH_VERSION\nEOF\ndeclare -A m\n", 1},
		{"unquoted heredoc print", "cat <<EOF\n$BASH_VERSION\nEOF\ndeclare -A m\n", 1},
		{"before the read", "declare -A m\n[[ ${BASH_VERSION%%.*} -ge 4 ]] || exit 1\n", 1},
		{"if", "if ((BASH_VERSINFO[0] >= 4)); then declare -A m; fi\n", 0},
		{"early exit", "[[ ${BASH_VERSION%%.*} -ge 4 ]] || exit 1\ndeclare -A m\n", 0},
		{"early exit with [", "[ \"${BASH_VERSINFO[0]}\" -ge 4 ] || exit 1\ndeclare -A m\n", 0},
		{"case", "case $BASH_VERSION in 3.*) :;; *) shopt -s globstar;; esac\n", 0},
		{"assigned, never tested", "v=${BASH_VERSION%%.*}\ndeclare -A m\n", 1},
		{"arith expansion", "echo $((BASH_VERSINFO[0]))\ndeclare -A m\n", 0},
		{"two features, guard between", "declare -A a\n[[ ${BASH_VERSION%%.*} -ge 4 ]] || exit 1\ndeclare -A b\n", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := wb1301(t, c.src); n != c.want {
				t.Fatalf("%d findings for %q, want %d", n, c.src, c.want)
			}
		})
	}
}

// A directive after a command on the same line covers nothing, for these
// rules as for the others: ShellCheck 0.11.0 refuses one there (SC1126,
// "Place shellcheck directives before commands, not after"), and bash-lsp
// does not apply it to the next line either.
func TestVersionRulesTrailingDirective(t *testing.T) {
	src := "#!/bin/bash\necho start\ndeclare -A m # bash-lsp disable=WB1301\ndeclare -A n\n"
	got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301")
	if len(got) != 2 {
		t.Fatalf("trailing directive silenced something:\n%s", describe(src, got))
	}
	for dir, line := range map[string]string{
		"bash-lsp":  "[[ -n $x ]] # bash-lsp disable=WB1302",
		"shellcheck": "[[ -n $x ]] # shellcheck disable=SC3010",
	} {
		src := "#!/bin/sh\necho start\n" + line + "\n[[ -n $y ]]\n"
		if got := ofRule(diagsWith(t, oneFile(src), "x.sh", ""), "WB1302"); len(got) != 2 {
			t.Errorf("%s: trailing directive silenced something:\n%s", dir, describe(src, got))
		}
	}
	// Alone on the line before the command, it does silence it.
	src = "#!/bin/bash\necho start\n# bash-lsp disable=WB1301\ndeclare -A m\n"
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301"); len(got) != 0 {
		t.Fatalf("leading directive:\n%s", describe(src, got))
	}
}

// A guard read at the top level of a file covers every function body in it,
// wherever defined, and the top-level code after it; code before it is still
// reported. An assignment from a version variable guards only when the
// variable is later tested; a piped print that decides an `||`, `&&` or `if`
// is a check.
func TestWB1301GuardScope(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"function above the guard", "f() { declare -A m; }\n((BASH_VERSINFO[0]>=4)) || exit 1\nf\n", 0},
		{"function called from an if", "main() {\n  declare -A m\n}\nif ((BASH_VERSINFO[0] >= 4)); then main; fi\n", 0},
		{"function below the guard", "((BASH_VERSINFO[0]>=4)) || exit 1\nf() { declare -A m; }\n", 0},
		{"top level before the guard", "declare -A m\nif ((BASH_VERSINFO[0] < 4)); then exit 1; fi\n", 1},
		{"top level before, function above", "declare -A a\nf() { declare -A b; }\n[[ ${BASH_VERSION%%.*} -ge 4 ]] || exit 1\n", 1},
		{"guard function called at the top covers functions above", "f() { declare -A a; }\ng() { [[ ${BASH_VERSINFO[0]} -ge 4 ]] || exit 1; }\ng\ndeclare -A b\n", 0},
		{"feature before the call of a guard function", "declare -A a\ng() { [[ ${BASH_VERSINFO[0]} -ge 4 ]] || exit 1; }\ng\ndeclare -A b\n", 1},
		{"guard function, call before the feature", "check() { [[ ${BASH_VERSINFO[0]} -ge 4 ]] || { echo old; exit 1; }; }\ncheck\ndeclare -A m\n", 0},
		{"guard function never called", "h() { ((BASH_VERSINFO[0]>=4)) || return; }\ndeclare -A m\n", 1},
		{"guard inside a function covers the rest of that function", "h() { ((BASH_VERSINFO[0]>=4)) || return; declare -A m; }\n", 0},
		{"guard inside a function does not cover its sibling", "h() { ((BASH_VERSINFO[0]>=4)) || return; }\nk() { declare -A m; }\n", 1},
		{"guard function called in another function", "h() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nmain() { h; declare -A m; }\nmain\n", 0},
		{"guard function, feature before the call", "h() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\ndeclare -A a\nh\n", 1},
		{"guard inside a function only covers after it", "g() { declare -A q; ((BASH_VERSINFO[0]>=4)) || exit 1; declare -A r; }; g\n", 1},
		{"guard function's own statements before the guard", "g() { declare -A q; ((BASH_VERSINFO[0]>=4)) || exit 1; }\ng\n", 1},
		{"top-level guard call covers a function called before it", "f() { declare -A m; }\nh() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nf\nh\n", 0},
		{"top-level guard call covers a function with its own later check", "req() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nmaybe() { declare -A x; ((BASH_VERSINFO[0]>=5)) && echo new; }\nreq\nmaybe\n", 0},
		{"wrapper of a guard function", "g() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nhelper() { g; }; helper; declare -A m\n", 0},
		{"wrapper of a wrapper", "g() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nw1() { g; }\nw2() { w1; declare -A m; }\nw2\n", 0},
		{"wrapper body before its guard call", "g() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nmain() { declare -A m; g; declare -A p; }\nmain\ndeclare -A n\n", 1},
		{"conditional call in a wrapper does not make a guard", "g() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nhelper() { if [ -n \"$1\" ]; then g; fi; }\nhelper\ndeclare -A m\n", 1},
		{"wrapper calling the guard function left of ||", "g() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nhelper() { g || return 1; }\nhelper\ndeclare -A m\n", 0},
		{"top-level wrapper covers a sibling it calls after the guard", "req() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\nmaybe() { declare -A x; ((BASH_VERSINFO[0]>=5)) && echo new; }\nmain() { req; maybe; }\nmain\n", 0},
		{"assignment, never tested", "v=$BASH_VERSION\ndeclare -A m\n", 1},
		{"assignment, tested in [", "ver=${BASH_VERSION%%.*}\n[ \"$ver\" -ge 4 ] || exit 1\ndeclare -A m\n", 0},
		{"assignment, tested in [[", "v=$BASH_VERSION\n[[ $v == 4* ]] || exit 1\ndeclare -A m\n", 0},
		{"assignment, tested in case", "v=$BASH_VERSION\ncase $v in 4*) declare -A m;; esac\n", 0},
		{"assignment, tested in arithmetic", "v=${BASH_VERSINFO[0]}\nif ((v >= 4)); then declare -A m; fi\n", 0},
		{"assignment, only printed later", "v=$BASH_VERSION\necho \"$v\"\ndeclare -A m\n", 1},
		{"assignment, tested before it", "[ -n \"$v\" ]\nv=$BASH_VERSION\ndeclare -A m\n", 1},
		{"piped check on ||", "echo \"$BASH_VERSION\" | grep -q ^4 || exit 1\ndeclare -A m\n", 0},
		{"piped check on &&", "echo \"$BASH_VERSION\" | grep -q ^4 && declare -A m\n", 0},
		{"piped check in if", "if echo \"$BASH_VERSION\" | grep -q ^4; then declare -A m; fi\n", 0},
		{"piped print, no check", "printf \"%s\\n\" \"$BASH_VERSION\" | tee log\ndeclare -A m\n", 1},
		{"standalone print left of ||", "echo \"$BASH_VERSION\" || true\ndeclare -A m\n", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := wb1301(t, c.src); n != c.want {
				t.Fatalf("%d findings for %q, want %d", n, c.src, c.want)
			}
		})
	}
}

// A check against a major version guarantees that version and no more, when
// the form is simple enough to decide; any other form silences as before.
func TestWB1301GuardThreshold(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"-ge 3 does not guard 4.0", "[ \"${BASH_VERSINFO[0]}\" -ge 3 ] || exit 1\ndeclare -A m\n", 1},
		{"-ge 4 guards 4.0", "[ \"${BASH_VERSINFO[0]}\" -ge 4 ] || exit 1\ndeclare -A m\n", 0},
		{"-ge 5 guards 4.0", "[ \"${BASH_VERSINFO[0]}\" -ge 5 ] || exit 1\ndeclare -A m\n", 0},
		{">= 3 in arithmetic", "if ((BASH_VERSINFO[0] >= 3)); then declare -A m; fi\n", 1},
		{">= 4 in arithmetic", "if ((BASH_VERSINFO[0] >= 4)); then declare -A m; fi\n", 0},
		{"> 3 guards 4.0", "(( BASH_VERSINFO[0] > 3 )) || exit 1\ndeclare -A m\n", 0},
		{"> 2 does not", "(( BASH_VERSINFO[0] > 2 )) || exit 1\ndeclare -A m\n", 1},
		{"-lt 4 with exit guards", "if [ \"${BASH_VERSINFO[0]}\" -lt 4 ]; then echo old; exit 1; fi\ndeclare -A m\n", 0},
		{"-lt 3 with exit does not", "if [ \"${BASH_VERSINFO[0]}\" -lt 3 ]; then echo old; exit 1; fi\ndeclare -A m\n", 1},
		{"-lt 4 and return", "f() { [ \"${BASH_VERSINFO[0]}\" -lt 4 ] && return 1; declare -A m; }\n", 0},
		{"-le 3 with exit guards 4.0", "[[ ${BASH_VERSINFO[0]} -le 3 ]] && exit 1\ndeclare -A m\n", 0},
		{"-lt 4 without exit stays silent", "if [ \"${BASH_VERSINFO[0]}\" -lt 4 ]; then echo old; fi\ndeclare -A m\n", 0},
		{"major of BASH_VERSION", "[ \"${BASH_VERSION%%.*}\" -ge 3 ] || exit 1\ndeclare -A m\n", 1},
		{"major of BASH_VERSION, 4", "[ \"${BASH_VERSION%%.*}\" -ge 4 ] || exit 1\ndeclare -A m\n", 0},
		{"minor comparison keeps silencing", "(( BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 3) )) || exit 1\ndeclare -A m\n", 0},
		{"string comparison keeps silencing", "[[ $BASH_VERSION < 4 ]] && exit 1\ndeclare -A m\n", 0},
		{"case keeps silencing", "case $BASH_VERSION in 3.*) exit 1;; esac\ndeclare -A m\n", 0},
		{"4.3 feature under a major 4 guard", "(( BASH_VERSINFO[0] >= 4 )) || exit 1\ndeclare -n r=x\n", 0},
		{"5.0 feature under a major 4 guard", "(( BASH_VERSINFO[0] >= 4 )) || exit 1\necho $EPOCHSECONDS\n", 1},
		{"5.0 feature under a major 5 guard", "(( BASH_VERSINFO[0] >= 5 )) || exit 1\necho $EPOCHSECONDS\n", 0},
		{"guard function with a threshold", "check() { (( BASH_VERSINFO[0] >= 3 )) || exit 1; }\ncheck\ndeclare -A m\n", 1},
		{"hex number keeps silencing", "(( BASH_VERSINFO[0] >= 0x4 )) || exit 1; declare -A m\n", 0},
		{"base#n number keeps silencing", "(( BASH_VERSINFO[0] >= 2#100 )) || exit 1\ndeclare -A m\n", 0},
		{"octal number keeps silencing", "(( BASH_VERSINFO[0] >= 010 )) || exit 1\ndeclare -A m\n", 0},
		{"expansion keeps silencing", "(( BASH_VERSINFO[0] >= $min )) || exit 1\ndeclare -A m\n", 0},
		{"== then exit keeps silencing", "[[ ${BASH_VERSINFO[0]} == 3 ]] && exit 1; declare -A m\n", 0},
		{"-eq in an if that exits keeps silencing", "if [ \"${BASH_VERSINFO[0]}\" -eq 3 ]; then echo old; exit 1; fi\ndeclare -A m\n", 0},
		{"== or exit is a bound", "[[ ${BASH_VERSINFO[0]} == 4 ]] || exit 1\necho $EPOCHSECONDS\n", 1},
		{"-eq in an if body is a bound", "if [ \"${BASH_VERSINFO[0]}\" -eq 3 ]; then declare -A m; fi\n", 1},
		{"== in an if with else keeps silencing", "if [[ ${BASH_VERSINFO[0]} == 3 ]]; then :; else declare -A m; fi\n", 0},
		{"negated == or exit keeps silencing", "! [[ ${BASH_VERSINFO[0]} == 3 ]] || exit 1; declare -A m\n", 0},
		{"negated == in an if keeps silencing", "if ! [[ ${BASH_VERSINFO[0]} == 3 ]]; then declare -A m; fi\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := wb1301(t, c.src); n != c.want {
				t.Fatalf("%d findings for %q, want %d", n, c.src, c.want)
			}
		})
	}
}

// The guard function's own statements before its check are reported where
// they are, and the statements after it are not.
func TestWB1301GuardFunctionOwnBody(t *testing.T) {
	src := "#!/bin/bash\ng() { declare -A q; ((BASH_VERSINFO[0]>=4)) || exit 1; declare -A r; }; g\n"
	got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301")
	if len(got) != 1 || got[0].Range.Start.Line != 2 || got[0].Range.Start.Col != 7 {
		t.Fatalf("want one finding at 2:7:\n%s", describe(src, got))
	}
}

// wrapperChain is a guard function g and n wrappers, each calling the one
// below it at its top level; callers are defined before their callees. The
// top level calls the outermost wrapper and then uses a feature, and every
// wrapper uses one after its call.
func wrapperChain(n int) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	for i := n; i >= 1; i-- {
		callee := "g"
		if i > 1 {
			callee = fmt.Sprintf("w%d", i-1)
		}
		fmt.Fprintf(&b, "w%d() { %s; declare -A m%d; }\n", i, callee, i)
	}
	b.WriteString("g() { ((BASH_VERSINFO[0]>=4)) || exit 1; }\n")
	fmt.Fprintf(&b, "w%d\ndeclare -A m\n", n)
	return b.String()
}

// A guard reaches the top level through a chain of wrappers, and the cost
// of finding that grows with the chain, not with its square: three doublings
// of the chain grow the time by at most 2.5 per doubling on average
// ((t4/t1)^(1/3), each time the best of five runs). A linear search gives 2, a
// quadratic one 4; single doublings scatter under load and are logged, not
// asserted.
func TestWB1301WrapperChainGrowth(t *testing.T) {
	if got := ofRule(diagsWith(t, oneFile(wrapperChain(50)), "x.sh", "3.2-5.3"), "WB1301"); len(got) != 0 {
		t.Fatalf("%d findings behind a chain of 50 wrappers", len(got))
	}
	best := func(n int) time.Duration {
		files := oneFile(wrapperChain(n))
		d := time.Duration(1<<63 - 1)
		for k := 0; k < 5; k++ {
			start := time.Now()
			diagsWith(t, files, "x.sh", "3.2-5.3")
			d = min(d, time.Since(start))
		}
		return d
	}
	var times []time.Duration
	for _, n := range []int{2000, 4000, 8000, 16000} {
		times = append(times, best(n))
	}
	t.Logf("2000, 4000, 8000, 16000 wrappers: %v", times)
	if overall := math.Cbrt(float64(times[3]) / float64(max(times[0], time.Millisecond))); overall > 2.5 {
		t.Fatalf("grew x%.2f per doubling over three doublings (%v)", overall, times)
	}
}
