package front

import (
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

// A valid sh file is read with the POSIX grammar: names the bash grammar
// reads as keywords (coproc, function, select) stay function names, and a
// subshell of a subshell is not a bash arithmetic command.
func TestValidShKeepsPOSIXGrammar(t *testing.T) {
	cases := []struct {
		src   string
		funcs string
	}{
		{"((echo a; echo b) | cat)\nafter() { :; }\n", "after"},
		{"coproc() { echo hi; }\nafter() { :; }\n", "coproc,after"},
		{"function() { :; }\nafter() { :; }\n", "function,after"},
		{"select() { :; }\nafter() { :; }\n", "select,after"},
	}
	for _, c := range cases {
		f := Parse("t.sh", []byte("#!/bin/sh\n"+c.src), POSIX)
		if len(f.Errors) != 0 || count(f, tree.KErrorGap) != 0 {
			t.Errorf("%q: errors %+v, %d gaps", c.src, f.Errors, count(f, tree.KErrorGap))
		}
		if got := strings.Join(funcNames(f), ","); got != c.funcs {
			t.Errorf("%q: functions %q, want %q", c.src, got, c.funcs)
		}
	}
}

// Bash-only syntax in an sh file that the POSIX grammar refuses is read with
// the bash grammar when that gives fewer errors: one finding for the
// feature, not a cascade of syntax errors.
func TestShBashFormsFallBackToBashGrammar(t *testing.T) {
	src := "#!/bin/sh\na=(1 2)\ncat <<< hi\nf() { :; }\n"
	f := Parse("t.sh", []byte(src), POSIX)
	if len(f.Errors) != 0 || f.Dialect != POSIX {
		t.Fatalf("errors %+v dialect %v", f.Errors, f.Dialect)
	}
	if got := featureTexts(t, "t.sh", src); strings.Join(got, "|") != "array-assign a=(1 2)|herestring <<<" {
		t.Fatalf("features %q", got)
	}
	// Syntax that neither grammar reads keeps the POSIX result.
	f = Parse("t.sh", []byte("#!/bin/sh\necho \"abc\nf() { :; }\n"), POSIX)
	if len(f.Errors) == 0 {
		t.Fatal("no error for an unclosed quote")
	}
}

// Forms that the POSIX grammar reads as plain commands still show as bash
// evidence in an sh file.
func TestShCleanPOSIXKeepsBashEvidence(t *testing.T) {
	src := "#!/bin/sh\n[[ -n $x ]]\necho $'x'\ncoproc cat\nsource ./y\n"
	f := Parse("t.sh", []byte(src), POSIX)
	if len(f.Errors) != 0 {
		t.Fatalf("errors %+v", f.Errors)
	}
	want := "double-bracket [[|ansi-c-quote $'x'|coproc coproc|source-builtin source"
	if got := featureTexts(t, "t.sh", src); strings.Join(got, "|") != want {
		t.Fatalf("features %q, want %q", got, want)
	}
	// And the tree is the POSIX one: `[[` is an ordinary command.
	if count(f, tree.KTest) != 0 {
		t.Fatal("a [[ ]] node in an sh file's tree")
	}
}
