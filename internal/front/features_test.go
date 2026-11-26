package front

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

// featureTexts parses src as bash (a bash shebang) and returns "key text"
// for every recorded feature, in order.
func featureTexts(t *testing.T, path, src string) []string {
	t.Helper()
	f := Parse(path, []byte(src), Detect(path, []byte(src)))
	var out []string
	for _, ft := range f.Features {
		a, b := ft.Range.Start.Offset, ft.Range.End.Offset
		if a < 0 || a > b || b > len(src) {
			t.Fatalf("%q: bad range %+v", src, ft)
		}
		out = append(out, fmt.Sprintf("%s %s", ft.Key, src[a:b]))
	}
	return out
}

const bashHead = "#!/bin/bash\n"

// Each syntactic key of the version table and of the bash-only list is
// recorded with the exact text of its token.
func TestFeaturesRecorded(t *testing.T) {
	cases := []struct{ src, want string }{
		{`echo "${v,,}"`, `case-modification ${v,,}`},
		{`echo "${v^^}"`, `case-modification ${v^^}`},
		{`echo "${v,}"`, `case-modification ${v,}`},
		{`echo "${v^}"`, `case-modification ${v^}`},
		{"case x in a) :;& b) :;; esac", `case-fallthrough ;&`},
		{"case x in a) :;;& b) :;; esac", `case-fallthrough ;;&`},
		{"a |& b", `pipe-all |&`},
		{"a &>> f", `append-all &>>`},
		{`echo "${v@Q}"`, `transform-op ${v@Q}`},
		{`echo "${a[-1]}"`, `negative-subscript ${a[-1]}`},
		{`echo "$((a[-2] + 1))"`, `negative-subscript a[-2]`},
		{`a[-1]=x`, `negative-subscript a[-1]=x`},
		{"coproc cat", `coproc coproc`},
		{"[[ -n $x ]]", `double-bracket [[`},
		{"function f { :; }", `function-keyword function f`},
		{"function f() { :; }", `function-keyword function f`},
		{"a=(1)", `array-assign a=(1)`},
		{"declare -a a=(1 2)", `array-assign a=(1 2)`},
		{"echo $'x\\ty'", `ansi-c-quote $'x\ty'`},
		{"cat <<< hi", `herestring <<<`},
		{"a &> f", `redirect-all &>`},
		{"cat <(echo hi)", `process-subst <(`},
		{"tee >(cat)", `process-subst >(`},
		{"source ./x", `source-builtin source`},
		{"FOO=1 source ./x", `source-builtin source`},
	}
	for _, c := range cases {
		got := featureTexts(t, "x.sh", bashHead+c.src+"\n")
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%q: features %q, want [%q]", c.src, got, c.want)
		}
	}
}

// A chain of |& stages records every operator, and the features come back in
// source order although the lowerer reaches some of them out of order.
func TestFeaturesChainAndOrder(t *testing.T) {
	got := featureTexts(t, "x.sh", bashHead+"a |& b |& c\n[[ -n $x ]] && echo ${v,,} <<< hi\n")
	want := []string{"pipe-all |&", "pipe-all |&", "double-bracket [[", "case-modification ${v,,}", "herestring <<<"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("features %q, want %q", got, want)
	}
	f := Parse("x.sh", []byte(bashHead+"a |& b |& c\n[[ -n $x ]] && echo ${v,,} <<< hi\n"), Bash)
	for i := 1; i < len(f.Features); i++ {
		if f.Features[i].Range.Start.Offset < f.Features[i-1].Range.Start.Offset {
			t.Fatalf("not in source order: %+v", f.Features)
		}
	}
}

// Text that only looks like a feature, and forms that need none, record
// nothing: strings, quoted heredocs, comments, `.` for source, the POSIX
// forms.
func TestFeaturesNotRecorded(t *testing.T) {
	for _, src := range []string{
		"echo '${v,,} [[ x ]] <<< a |& b'",
		"echo \"[[ x ]] source <(a) \\${v,,}\"" + "\n" + "echo \"no \\$'x' here\"",
		"cat <<'EOF'\n${v,,} [[ x ]] <(a) $'x' source\nEOF",
		"# ${v,,} [[ x ]] <<< a source function f\n:",
		". ./x",
		"echo source",
		"echo ${v:-x} ${v#a} ${v%b} ${v//a/b} ${#v} ${v:1:2} ${v:-${w}}",
		"echo ${a[1]} ${a[i]} ${a[$i]} ${a[@]} ${a[*]}",
		"f() { [ -n \"$x\" ] && echo $((1 + 2)); }",
		"a=1; b=\"$a\"; c+=2",
		"echo a > f; echo b >> f; echo c 2>&1; echo d >&2; cat < f",
		"case x in a) :;; esac",
		"a | b && c || d",
	} {
		if got := featureTexts(t, "x.sh", bashHead+src+"\n"); len(got) != 0 {
			t.Errorf("%q: features %q, want none", src, got)
		}
	}
}

// A heredoc that expands is parsed, so an expansion in it is evidence.
func TestFeaturesInUnquotedHeredoc(t *testing.T) {
	got := featureTexts(t, "x.sh", bashHead+"cat <<EOF\n${v,,}\nEOF\n")
	if len(got) != 1 || got[0] != "case-modification ${v,,}" {
		t.Fatalf("features %q", got)
	}
}

// Features of a region the parser could not read are not recorded: nothing
// inside an error gap counts, and a candidate parse that is not used leaves
// none.
func TestFeaturesSkipErrorGaps(t *testing.T) {
	src := bashHead + "echo ${a,,}\necho \"unclosed ${b,,}\n) ) fi done\n[[ -n $x ]]\n"
	f := Parse("x.sh", []byte(src), Bash)
	var gap *tree.Node
	for _, n := range f.Root.Children {
		if n.Kind == tree.KErrorGap {
			gap = n
		}
	}
	if gap == nil {
		t.Fatalf("no gap: %v", f.Errors)
	}
	for _, ft := range f.Features {
		if gap.Range.Contains(ft.Range.Start.Offset) {
			t.Errorf("feature %+v inside the gap %+v", ft, gap.Range)
		}
	}
	seen := map[string]int{}
	for _, ft := range f.Features {
		seen[ft.Key+" "+src[ft.Range.Start.Offset:ft.Range.End.Offset]]++
	}
	if seen["case-modification ${a,,}"] != 1 {
		t.Fatalf("the statement before the gap: %v", seen)
	}
	if seen["double-bracket [["] != 1 {
		t.Fatalf("the statement after the gap, recovered by resync, must be recorded once: %v", seen)
	}
}

// An sh file is parsed with the bash variant (its dialect stays sh): the
// forms that stop a POSIX parse parse, with their evidence recorded.
func TestShParsedAsBash(t *testing.T) {
	src := "#!/bin/sh\nf(){ :; }\n[[ -n $x ]]\ng(){ :; }\n"
	f := Parse("t.sh", []byte(src), Detect("t.sh", []byte(src)))
	if f.Dialect != tree.POSIX {
		t.Fatalf("Dialect %v, want sh", f.Dialect)
	}
	if len(f.Errors) != 0 {
		t.Fatalf("errors: %+v", f.Errors)
	}
	if got := funcNames(f); strings.Join(got, ",") != "f,g" {
		t.Fatalf("functions %v", got)
	}
	var tests int
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KTest {
			tests++
		}
		return true
	})
	if tests != 0 { // the tree is the POSIX grammar's: `[[` is a command there
		t.Fatalf("%d [[ ]] test nodes, want 0", tests)
	}
	if got := featureTexts(t, "t.sh", src); len(got) != 1 || got[0] != "double-bracket [[" {
		t.Fatalf("features %q", got)
	}
}

// Each bash form that a POSIX parse refuses (observed with the POSIX variant
// before it was dropped) parses in an sh file without an error.
func TestShFormsParse(t *testing.T) {
	for _, src := range []string{
		`echo "${v,,}"`, "case x in a) :;& b) :;; esac", "case x in a) :;;& b) :;; esac", "a |& b", "a &>> f",
		`echo "${v@Q}"`, `echo "${a[-1]}"`, "coproc cat", "function f { :; }", "a=(1 2)",
		"cat <<< hi", "a &> f", "cat <(echo hi)",
	} {
		f := Parse("t.sh", []byte("#!/bin/sh\n"+src+"\n"), POSIX)
		if len(f.Errors) != 0 || f.Dialect != POSIX {
			t.Errorf("%q: errors %+v dialect %v", src, f.Errors, f.Dialect)
		}
	}
}

// The forms dash itself accepts parse the same under the bash variant: the
// same node kinds, no errors, no features.
func TestShDashFormsUnchanged(t *testing.T) {
	src := "#!/bin/sh\nset -eu\nf() {\n  case \"$1\" in\n    a|b) echo \"${x#pre}\" ;;\n    *) echo $((1 + 2)) ;;\n  esac\n}\n" +
		"if [ -n \"$x\" ] && [ -f /etc/passwd ]; then f a; fi\nwhile read -r l; do echo \"$l\"; done < /dev/null\n" +
		"x=$(echo hi | tr a b)\n. ./lib.sh\nexport A=1\nlocal_var=`echo old`\ncat <<EOF\n$x\nEOF\n"
	f := Parse("t.sh", []byte(src), POSIX)
	if len(f.Errors) != 0 || len(f.Features) != 0 {
		t.Fatalf("errors %+v features %+v", f.Errors, f.Features)
	}
	kinds := map[tree.Kind]int{}
	f.Root.Walk(func(n *tree.Node) bool { kinds[n.Kind]++; return true })
	for _, k := range []tree.Kind{tree.KFuncDecl, tree.KCase, tree.KIf, tree.KWhile, tree.KCmdSubst, tree.KArithExp, tree.KHeredoc, tree.KDecl} {
		if kinds[k] == 0 {
			t.Errorf("no %v node", k)
		}
	}
}

// Only `@Q` has a citable first version; the other `@` operators
// are not recorded at all, and `@Q` is.
func TestTransformOpsRecorded(t *testing.T) {
	for _, op := range []string{"U", "u", "L", "K", "k", "a", "A", "E", "P"} {
		src := bashHead + `echo "${v@` + op + `}"` + "\n"
		if got := featureTexts(t, "x.sh", src); len(got) != 0 {
			t.Errorf("@%s: features %q, want none", op, got)
		}
	}
	for _, src := range []string{`echo "${v@Q}"`, `echo ${v@Q}`, `echo "${a[0]@Q}"`} {
		got := featureTexts(t, "x.sh", bashHead+src+"\n")
		if len(got) != 1 || !strings.HasPrefix(got[0], "transform-op ") {
			t.Errorf("%q: features %q, want one transform-op", src, got)
		}
	}
}

// Shebang lines that name a shell through env or busybox.
func TestDetectShebangForms(t *testing.T) {
	cases := []struct {
		line string
		want Dialect
	}{
		{"#!/bin/busybox sh", POSIX},
		{"#!/usr/bin/env busybox sh", POSIX},
		{"#!/bin/busybox ash", POSIX},
		{"#!/usr/bin/env busybox ash", POSIX},
		{"#!/bin/busybox sh -e", POSIX},
		{"#!/usr/bin/env -S sh -e", POSIX},
		{"#!/usr/bin/env -S bash -e", Bash},
		{"#!/usr/bin/env -S busybox sh", POSIX},
		{"#!/usr/bin/env -i sh", POSIX},
		{"#!/usr/bin/env -i FOO=1 sh", POSIX},
		{"#!/usr/bin/env -u FOO sh", POSIX},
		{"#!/usr/bin/env -C /tmp sh", POSIX},
		{"#!/usr/bin/env -P /bin:/usr/bin sh", POSIX},
		{"#!/usr/bin/env -iS sh", POSIX},
		{"#!/usr/bin/env sh", POSIX},
		{"#!/usr/bin/env bash", Bash},
		{"#!/bin/sh", POSIX},
		{"#!/bin/bash", Bash},
		{"#!/bin/busybox", Bash},
		{"#!/bin/busybox bash", Bash},
		{"#!/usr/bin/env -u", Bash},
	}
	for _, c := range cases {
		if got := Detect("x.sh", []byte(c.line+"\necho\n")); got != c.want {
			t.Errorf("%q: %v, want %v", c.line, got, c.want)
		}
	}
}
