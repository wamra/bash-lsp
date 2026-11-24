package analysis

import "testing"

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
		{"right of && after [", "[ -n \"$x\" ] && shopt -s globstar\n"},
		{"right of && after [[", "[[ -n $x ]] && declare -A m\n"},
		{"right of && after test", "test -n \"$x\" && declare -A m\n"},
		{"right of && after ((", "(( n > 1 )) && declare -A m\n"},
		{"right of && after test and &&", "[ -n \"$x\" ] && [ -n \"$y\" ] && declare -A m\n"},
		{"if body", "if [ -n \"$x\" ]; then declare -A m; fi\n"},
		{"if [[ body", "if [[ -n $x ]]; then shopt -s globstar; fi\n"},
		{"elif body", "if false; then :; elif [ -n \"$x\" ]; then declare -A m; fi\n"},
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
	reported := []struct{ name, src string }{
		{"plain", "declare -A m\n"},
		{"after ;", "true; declare -A m\n"},
		{"after && of a command", "true && declare -A m\n"},
		{"after && of a command, then a test", "true && [ -n x ] || declare -A m\n"},
		{"right of ||", "[ -n x ] || declare -A m\n"},
		{"if on a command", "if grep -q x f; then declare -A m; fi\n"},
		{"else of a test", "if [ -n x ]; then :; else declare -A m; fi\n"},
		{"nested deeper than the if body", "if [ -n x ]; then for i in 1; do declare -A m; done; fi\n"},
		{"stderr to a file", "shopt -s globstar 2>err.log\n"},
		{"stdout to null", "shopt -s globstar >/dev/null\n"},
		{"function body", "f() { shopt -s globstar; }\n"},
		{"loop body", "while read l; do declare -A m; done\n"},
		{"EPOCHSECONDS printed", "echo \"$EPOCHSECONDS\"\n"},
		{"EPOCHSECONDS assigned from", "now=$EPOCHSECONDS\n"},
		{"test on another name", "[ -n \"$x\" ]; echo $EPOCHSECONDS\n"},
	}
	for _, c := range reported {
		t.Run("reported "+c.name, func(t *testing.T) {
			if n := wb1301(t, c.src); n != 1 {
				t.Fatalf("%d findings for %q, want 1", n, c.src)
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
		{"assigned then used", "v=${BASH_VERSION%%.*}\ndeclare -A m\n", 0},
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
