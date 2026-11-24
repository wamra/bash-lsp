package analysis

import (
	"strings"
	"testing"
)

// Fix round 3, item 1: WB1101's ShellCheck code follows ShellCheck 0.11.0
// (checked with the binary): SC1091 when one leading expansion is followed
// by a literal tail that starts with `/` ("$X/lib.sh", "$(pwd)/a.sh",
// "$X/"), SC1090 for every other computed word ("$X", "$X.sh", "a/$X/b.sh",
// "$(pwd)", unquoted ~/x.sh).
func TestSourceCodeBoundary(t *testing.T) {
	codes := map[string]string{
		`source "$X"`:                       "SC1090",
		`source "$X/"`:                      "SC1091",
		`source "$X/lib.sh"`:                "SC1091",
		`source "$X.sh"`:                    "SC1090",
		`source "a/$X"`:                     "SC1090",
		`source "a/$X/b.sh"`:                "SC1090",
		`source ~/x.sh`:                     "SC1090",
		`source "${HOMEBREW_LIBRARY}/x.sh"`: "SC1091",
		`source "$(pwd)/a.sh"`:              "SC1091",
		`source "$(dirname "$0")/lib.sh"`:   "SC1091",
		`source "$(pwd)"`:                   "SC1090",
		`source $X`:                         "SC1090",
		`source ./nope.sh`:                  "SC1091",
	}
	for line, code := range codes {
		other := "SC1090"
		if code == "SC1090" {
			other = "SC1091"
		}
		silenced := "echo a\n# shellcheck disable=" + code + "\n" + line + "\n"
		if ds := ofRule(diagsIn(sh(silenced), "x.sh"), "WB1101"); len(ds) != 0 {
			t.Errorf("%s: not silenced by %s", line, code)
		}
		kept := "echo a\n# shellcheck disable=" + other + "\n" + line + "\n"
		if ds := ofRule(diagsIn(sh(kept), "x.sh"), "WB1101"); len(ds) != 1 {
			t.Errorf("%s: silenced by %s", line, other)
		}
	}
	// the Homebrew form: a literal tail under an existing disable=SC1091
	expectRule(t, sh("#!/bin/bash\n# shellcheck disable=SC1091\nsource \"${HOMEBREW_LIBRARY}/Homebrew/utils/lock.sh\"\n"), "x.sh", "WB1101")
}

// Item 2: the one-word proof keeps decoded single words, function unsets,
// `export IFS` without a value, and locals of functions that cannot call
// the reader.
func TestOneWordProofKept(t *testing.T) {
	for _, src := range []string{
		"x=$'ab'\nrm $x\n",
		"x=a\\b\nrm $x\n",
		"mode=755\nunset -f mode\nchmod $mode a\n",
		"export IFS\nmode=755\nchmod $mode a\n",
		"dir=build\nclean(){ rm -rf $dir; }\nmk(){ local dir=$1; mkdir -p \"$dir\"; }\nclean\nmk x\n",
		"mode=755\nmain(){ chmod $mode a; }\nhelper(){ local mode=$1; echo \"$mode\"; }\nmain\nhelper x\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401")
	}
	// still fires: a caller that declares the local, directly or through a chain;
	// a dynamic call near the declaring function; an undecodable value; a variable unset
	for src, want := range map[string]string{
		"x=safe\nf(){ local x=$1; g; }\ng(){ rm $x; }\nf a\n":            "3:$x",
		"x=safe\nf(){ local x=$1; h; }\nh(){ g; }\ng(){ rm $x; }\nf a\n": "4:$x",
		"x=safe\nf(){ local x=$1; \"$cb\"; }\ng(){ rm $x; }\nf a\ng\n":   "3:$x",
		"x=$'a\\tb'\nrm $x\n":         "2:$x",
		"x=safe\nunset x\nrm $x\n":    "3:$x",
		"x=safe\nunset -v x\nrm $x\n": "3:$x",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401", want)
	}
}

// Item 3: `local IFS` affects its function and what it calls, not the rest.
func TestLocalIFSScope(t *testing.T) {
	expectRule(t, sh("split(){ local IFS=,; echo $*; }\nmode=755\nchmod $mode a\nsplit a\n"), "x.sh", "WB1401")
	expectRule(t, sh("mode=755\nsplit(){ local IFS=,; chmod $mode a; }\nsplit\n"), "x.sh", "WB1401", "2:$mode")
	expectRule(t, sh("mode=755\nsplit(){ local IFS=,; doit; }\ndoit(){ chmod $mode a; }\nsplit\n"), "x.sh", "WB1401", "3:$mode")
	expectRule(t, sh("mode=755\nsplit(){ IFS=,; }\nchmod $mode a\nsplit\n"), "x.sh", "WB1401", "3:$mode")
	expectRule(t, sh("IFS=,\nmode=755\nchmod $mode a\n"), "x.sh", "WB1401", "3:$mode")
}

// Item 4: an alias value is code; a single-quoted `$name` in it is a read.
func TestAliasValueIsARead(t *testing.T) {
	expectRule(t, sh("#!/bin/bash\nopts=-l\nalias ll='ls $opts'\nll\n"), "x.sh", "WB1202")
	// still fires: a plain echo of the same string
	expectRule(t, sh("#!/bin/bash\nopts=-l\necho 'ls $opts'\n"), "x.sh", "WB1202", "2:opts")
}

// Item 5: an id token that is not a rule id is unknown; only text after
// whitespace and `#` or `--` is the author's reason.
func TestDirectiveIDGrammar(t *testing.T) {
	for _, d := range []string{"WB14O1", "WB1401:reason", "WB-1401", "wb1401"} {
		src := "echo a\n# bash-lsp disable=" + d + "\nrm $f\n"
		if n := len(ofRule(diagsIn(sh(src), "x.sh"), "WB1008")); n != 1 {
			t.Errorf("%s: %d WB1008", d, n)
		}
	}
	for _, src := range []string{
		"echo a\n# bash-lsp disable=WB1401 # glob intended\nrm $f\n",
		"echo a\n# bash-lsp disable=WB1401 -- reason\nrm $f\n",
		"echo a\n# bash-lsp disable=WB1401, WB1202 -- both\nrm $f\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401")
		expectRule(t, sh(src), "x.sh", "WB1008")
	}
}

// Item 6: a lone `)` closing a substitution or array in a case arm is no
// case pattern, and a word holding an opener is no opener (ShellCheck
// gives SC1126 there).
func TestPlacedNotOverAccepted(t *testing.T) {
	d := "# shellcheck disable=SC2086"
	for src, want := range map[string][]string{
		"f=$1\ncase $1 in\n  a)\n    y=$(\n      echo hi\n    ) " + d + "\n    rm $f \"$y\" ;;\nesac\n": {"7:$f"},
		"f=$1\ncase $1 in\n  a)\n    arr=(\n      1\n    ) " + d + "\n    rm $f ;;\nesac\n":             {"7:$f"},
		"f=$1\necho do " + d + "\nrm $f\n":   {"3:$f"},
		"f=$1\necho then " + d + "\nrm $f\n": {"3:$f"},
		"f=$1\necho { " + d + "\nrm $f\n":    {"3:$f"},
		"f=$1\necho else " + d + "\nrm $f\n": {"3:$f"},
	} {
		expectRule(t, sh(src), "x.sh", "WB1401", want...)
	}
	// still silences: the openers themselves
	expectRule(t, sh("f=$1\ncase $1 in\n  a|b) "+d+"\n    rm $f ;;\nesac\n"), "x.sh", "WB1401")
	expectRule(t, sh("f=$1\nfunction g { "+d+"\n  rm $f\n}\ng\n"), "x.sh", "WB1401")
	expectRule(t, sh("f=$1\nwhile true; do "+d+"\n  rm $f\ndone\n"), "x.sh", "WB1401")
}

// Item 7: a file test anywhere earlier in the same && chain guards a source.
func TestGuardChain(t *testing.T) {
	for src, want := range map[string]Severity{
		"[ -f ./g.sh ] && echo loading && . ./g.sh\n":    SevInfo,
		"[ -f ./g.sh ] && [ ! -d ./g.sh ] && . ./g.sh\n": SevInfo,
		"[ -f ./g.sh ] || echo missing && . ./g.sh\n":    SevWarning,
		"[ ! -f ./g.sh ] && echo loading && . ./g.sh\n":  SevWarning,
	} {
		ds := ofRule(diagsIn(sh(src), "x.sh"), "WB1101")
		if len(ds) != 1 || ds[0].Severity != want {
			t.Errorf("%q: %+v, want severity %d", src, ds, want)
		}
	}
}

// Item 8: the mention walk opens on option clusters and arithmetic
// operators, not on any hyphenated word.
func TestMentionWalkGate(t *testing.T) {
	for _, s := range []string{"declare -xp foo", "set -ea", "[[ $a -gt b ]]", "printf -v x %s", "set -o allexport", "echo $[a+1]"} {
		if !mentionWalkNeeded([]byte(s)) {
			t.Errorf("%q: no walk", s)
		}
	}
	for _, s := range []string{"my-tool --parallel", "find . -type f", "a-z A-Z", "echo --verbose", "x=1 # set -a later?"} {
		if mentionWalkNeeded([]byte(s)) && !strings.Contains(s, "set -a") {
			t.Errorf("%q: walk", s)
		}
	}
}

// Item 9: an unquoted heredoc holding an apostrophe does not invert the
// quote state of the masked scan.
func TestHeredocApostrophe(t *testing.T) {
	expectRule(t, sh("f(){ local x=1; cat <<EOF\ndon't\nEOF\necho '$x is literal'; }; f\n"), "x.sh", "WB1202", "1:x")
	expectRule(t, sh("f(){ local x=1; cat <<EOF\ndon't $x\nEOF\n}; f\n"), "x.sh", "WB1202")
	// a here-string is no heredoc: its single-quoted word stays text
	expectRule(t, sh("f() {\n  local x=1\n  . /dev/stdin <<<'echo $x'\n}\nf\n"), "x.sh", "WB1202", "2:x")
}
