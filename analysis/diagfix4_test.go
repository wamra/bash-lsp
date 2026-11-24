package analysis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/parse"
)

// hasFinding reports whether rule reports "line:text" in the one file of src.
func hasFinding(t *testing.T, src, rule, want string) bool {
	t.Helper()
	files := sh(src)
	for _, d := range ofRule(diagsIn(files, "x.sh"), rule) {
		if fmt.Sprintf("%d:%s", d.Range.Start.Line, rangeText(src, d.Range)) == want {
			return true
		}
	}
	return false
}

// An arithmetic shift is no heredoc operator: the text after it stays code
// for the mention scan, so a later single-quoted `$x` is still text.
func TestArithShiftIsNoHeredoc(t *testing.T) {
	for _, line := range []string{
		"echo $((1<<2))",
		"(( y = 1 << n ))",
		"echo $(( a<<b ))",
		`let "y<<=2"`,
		"echo ${arr[1<<2]}",
		"for ((i=1<<2;;)); do break; done",
	} {
		src := "#!/bin/bash\nx=1\n" + line + "\necho '$x'\n"
		if !hasFinding(t, src, "WB1202", "2:x") {
			t.Errorf("%s: no WB1202 for x:\n%s", line, describe(src, diagsIn(sh(src), "x.sh")))
		}
	}
	// a real heredoc on a line that also shifts: its body is still read apart
	expectRule(t, sh("#!/bin/bash\ny=1\ncat <<EOF $((1<<2))\n$y\nEOF\n"), "x.sh", "WB1202")
	if !hasFinding(t, "#!/bin/bash\nx=1\ncat <<'EOF' $((1<<2))\n$x\nEOF\n", "WB1202", "2:x") {
		t.Error("quoted heredoc after a shift on its line: no WB1202 for x")
	}
	if !hasFinding(t, "#!/bin/bash\nx=1\ncat <<EOF $((1<<2))\nbody\nEOF\necho '$x'\n", "WB1202", "2:x") {
		t.Error("heredoc and shift on one line: the heredoc did not close")
	}
}

// IFS assigned by a builtin (no `=` after the name) changes splitting for
// the shell, so the one-word proof is dropped.
func TestIFSAssignedByBuiltin(t *testing.T) {
	for _, line := range []string{
		"read IFS",
		"read -r IFS",
		"for IFS in , ; do :; done",
		"printf -v IFS %s ,",
		"mapfile IFS",
		"readarray -t IFS",
		"getopts ab IFS",
	} {
		src := "mode=755\n" + line + "\nchmod $mode a\n"
		if !hasFinding(t, src, "WB1401", "3:$mode") {
			t.Errorf("%s: proof kept", line)
		}
	}
	expectRule(t, sh("mode=755; read IFS; chmod $mode a\n"), "x.sh", "WB1401", "1:$mode")
	// still kept: a function's own `read IFS` after `local IFS`
	expectRule(t, sh("split(){ local IFS; read IFS; }\nmode=755\nchmod $mode a\nsplit\n"), "x.sh", "WB1401")
}

// `set "-a"` and `set -a>/dev/null` turn on allexport like `set -a`.
func TestAllexportQuotedOrRedirected(t *testing.T) {
	for _, line := range []string{`set "-a"`, `set -a>/dev/null`, `set '-a'`} {
		if !mentionWalkNeeded([]byte(line + "\n")) {
			t.Errorf("%s: no walk", line)
		}
		expectRule(t, sh("#!/bin/bash\n"+line+"\nx=1\necho hi\n"), "x.sh", "WB1202")
	}
	// still reported: allexport in a subshell does not reach the shell
	expectRule(t, sh("#!/bin/bash\n(set -a)\nx=1\necho hi\n"), "x.sh", "WB1202", "3:x")
}

// The caller-local check is not one search per variable name: thousands of
// distinct names, each with a global, a reader and a function declaring it
// local, along a call chain or with ten calls per function, give their
// findings in bounded time. A name whose only local is in a function that
// calls nothing is the shape where the search from the reader is long.
func TestCallerLocalsDistinctNamesInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	build := func(unit func(i int) string) string {
		var sb strings.Builder
		sb.WriteString("#!/bin/bash\n")
		for i := 0; sb.Len() < size-200; i++ {
			sb.WriteString(unit(i))
		}
		return sb.String()
	}
	prev := func(i int) string {
		if i == 0 {
			return ":"
		}
		return fmt.Sprintf("f%d", i-1)
	}
	cases := map[string]string{
		"call chain": build(func(i int) string {
			return fmt.Sprintf("v%[1]d=755\ng%[1]d() { chmod $v%[1]d a; }\nf%[1]d() { local v%[1]d=$1; g%[1]d; %[2]s; }\n", i, prev(i))
		}),
		"ten calls each": build(func(i int) string {
			var calls []string
			for k := 0; k < 10; k++ {
				if j := i - 1 - (k*7)%40; j >= 0 {
					calls = append(calls, fmt.Sprintf("f%d", j))
				}
			}
			calls = append(calls, ":")
			return fmt.Sprintf("v%[1]d=755\ng%[1]d() { chmod $v%[1]d a; }\nf%[1]d() { local v%[1]d=$1; g%[1]d; %[2]s; }\n", i, strings.Join(calls, "; "))
		}),
		"local in a leaf": build(func(i int) string {
			return fmt.Sprintf("v%[1]d=755\ng%[1]d() { chmod $v%[1]d a; }\nh%[1]d() { local v%[1]d=$1; }\nf%[1]d() { g%[1]d; %[2]s; }\n", i, prev(i))
		}),
	}
	for name, src := range cases {
		f := parse.File("/ws/a.sh", []byte(src))
		if f.Skipped != "" {
			t.Fatalf("%s: skipped: %s", name, f.Skipped)
		}
		s := Build(map[string]*FileFacts{"/ws/a.sh": Bind(f)}, Options{Root: "/ws"}, 1)
		var n int
		withinBound(t, name, bound, func() { n = len(ofRule(s.Diagnostics("/ws/a.sh", DiagOptions{}), "WB1401")) })
		t.Logf("%s: %d WB1401", name, n)
	}
}

// Past the search budget in both directions the caller-local check answers
// that the local may reach the reader: the proof is dropped. Here n
// functions call the reader and the declaring function calls n others, none
// of them the reader.
func TestCallerLocalSearchBudget(t *testing.T) {
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
	expectRule(t, sh(shape(600)), "x.sh", "WB1401", "2:$v")
	// within the budget the same shape keeps the proof
	expectRule(t, sh(shape(400)), "x.sh", "WB1401")
}
