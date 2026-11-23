package front

import (
	"runtime/debug"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"

	"github.com/wamra/bash-lsp/tree"
)

// treeDepth is the depth of f's node tree, computed without recursion.
func treeDepth(f *tree.File) int {
	type item struct {
		n *tree.Node
		d int
	}
	max := 0
	stack := []item{{f.Root, 1}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if it.d > max {
			max = it.d
		}
		for _, c := range it.n.Children {
			stack = append(stack, item{c, it.d + 1})
		}
	}
	return max
}

func hasErr(f *tree.File, sub string) bool {
	for _, e := range f.Errors {
		if strings.Contains(e.Msg, sub) {
			return true
		}
	}
	return false
}

// fill repeats unit between prefix and suffix up to the 2 MiB file limit.
func fill(prefix, unit, suffix string) string {
	n := (2<<20 - len(prefix) - len(suffix) - 1) / len(unit)
	return prefix + strings.Repeat(unit, n) + suffix
}

// Parse must refuse these: a Skipped file with an error that says why. Each
// drove mvdan's recursive-descent parser past Go's stack limit (a fatal error
// no recover() catches; review finding C1), or used a quarter of it or more,
// measured with debug.SetMaxStack. TestNoCrashSubprocess runs the same
// shapes, and more, under a 64 MiB stack limit.
func TestDeepNestingIsSkipped(t *testing.T) {
	cases := map[string]string{
		"paren":       strings.Repeat("(", 200000) + "\n",
		"cmdsubst":    "echo " + strings.Repeat("$(", 500000) + "\n",
		"brace":       strings.Repeat("{ ", 500000) + "\n",
		"procsubst":   "cat " + strings.Repeat("<(", 500000) + "\n",
		"dq-cmdsubst": "echo " + strings.Repeat(`"$(`, 400000) + "\n",
		"dollar-brk":  "echo " + strings.Repeat("$[", 500000) + "\n",
		// Operator chains the parser recurses on.
		"ternary":      fill("echo $((", "a?", "1))\n"),
		"ternary-sp":   fill("(( ", "a ? ", "1 ))\n"),
		"let-ternary":  fill("let ", "a?", "1\n"),
		"for-ternary":  fill("for ((", "a ? ", "1;;)); do :; done\n"),
		"subscript":    fill("x[", "a ? ", "1]=1\n"),
		"param-index":  fill("echo ${x[", "a ? ", "1]}\n"),
		"param-slice":  fill("echo ${x:", "a ? ", "1}\n"),
		"arith-assign": fill("echo $((", "a=", "1))\n"),
		"arith-power":  fill("echo $((", "2**", "2))\n"),
		"arith-minus":  fill("echo $((", "- ", "1))\n"),
		"arith-not":    fill("echo $((", "!", "1))\n"),
		"test-not":     fill("[[ ", "! ", "a ]]\n"),
		"test-and":     fill("[[ ", "a && ", "a ]]\n"),
		// Compound commands, prefixes and function headers.
		"if":              fill("", "if a;then\n", ""),
		"while":           fill("", "while a;do\n", ""),
		"for":             fill("", "for a in b;do\n", ""),
		"for-in-done":     fill("", "for x in done; do ", ":\n"),
		"case":            fill("", "case a in b)\n", ""),
		"time":            fill("", "time ", "a\n"),
		"coproc":          fill("", "coproc ", "a\n"),
		"funcs":           fill("", "f() ", "{ :; }\n"),
		"funcs-commented": fill("", "f() # c\n", "{ :; }\n"),
		"func-dash":       fill("", "a-b() ", "{ :; }\n"),
		"function-kw":     fill("", "function f\n", "{ :; }\n"),
		// Closers the parser does not see as closers must not hide the depth.
		"quoted-closers":  strings.Repeat("$( '))' ", 260000) + "\n",
		"case-patterns":   strings.Repeat("$(case x in a) ", 130000) + "\n",
		"esac-argument":   fill("", "$(case x in a) echo esac;; b) ", "\n"),
		"comment-closers": strings.Repeat("$( # ))\n", 260000),
		"heredoc-closers": strings.Repeat("$(cat <<'E'\n)))\nE\n", 100000),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := Parse("x.sh", []byte(src), Bash)
			if f.Skipped == "" {
				t.Fatalf("not skipped (%d errors, depth %d)", len(f.Errors), treeDepth(f))
			}
			if f.Skipped != refusal || len(f.Errors) != 1 || f.Errors[0].Msg != refusal {
				t.Fatalf("skipped %q, errors %v", f.Skipped, f.Errors)
			}
			if len(f.Root.Children) != 0 {
				t.Fatal("a skipped file has no statements")
			}
			checkInvariants(t, f)
		})
	}
}

// Shapes the parser handles without recursing, so the stack guard never sees
// them, but that recursed (or ran quadratically) in the lowerer, mvdan's Walk
// or End: at the 2 MiB limit they lower whole, to a shallow tree.
func TestLongChainsLowerSafely(t *testing.T) {
	cases := map[string]string{
		"andor":       fill("a ", "&& a ", "\n"),
		"pipes":       fill("a ", "| a ", "\n"),
		"arith-plus":  fill("echo $((", "1+", "1))\n"),
		"arith-comma": fill("echo $((", "1,", "1))\n"),
		// Comments inside a chain made mvdan's Walk call Stmt.End at every level.
		"pipes-commented": fill("a ", "| # c\na ", "\n"),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := Parse("x.sh", []byte(src), Bash)
			if f.Skipped != "" || len(f.Errors) != 0 {
				t.Fatalf("skipped %q, errors %v", f.Skipped, f.Errors)
			}
			if len(f.Root.Children) != 1 {
				t.Fatal("statement dropped")
			}
			if d := treeDepth(f); d > 10 {
				t.Fatalf("tree depth %d", d)
			}
		})
	}
}

// The lowerer's own bound, tested without the stack guard (the parser is
// called directly): a statement nested deeper than maxDepth lowering levels
// is dropped and reported, so analysis never sees a deep tree. Of these only
// the elif chain reaches the lowerer through Parse: the parser builds it in a
// loop, so the guard has nothing to stop.
func TestLowererDepthBound(t *testing.T) {
	cases := map[string]string{
		"if":              strings.Repeat("if a;then\n", 2100) + ":\n" + strings.Repeat("fi\n", 2100),
		"while":           strings.Repeat("while a;do\n", 4100) + ":\n" + strings.Repeat("done\n", 4100),
		"elif":            "if a; then :\n" + strings.Repeat("elif a; then :\n", 4100) + "fi\n",
		"time":            strings.Repeat("time ", 4100) + "a\n",
		"coproc":          strings.Repeat("coproc ", 4100) + "a\n",
		"funcs":           strings.Repeat("f() ", 4100) + "{ :; }\n",
		"funcs-commented": strings.Repeat("f() # c\n", 4100) + "{ :; }\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := tree.NewFile("x.sh", []byte(src))
			af, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "x.sh")
			if err != nil {
				t.Fatal(err)
			}
			stmts, errs := lowerStmts(f, f.PosAt(0), af.Stmts)
			f.Root.Children = stmts
			f.Errors = errs
			if !hasErr(f, "nested deeper") {
				t.Fatalf("no depth error: %v", errs)
			}
			if d := treeDepth(f); d > 3*maxDepth+10 {
				t.Fatalf("tree depth %d over the bound", d)
			}
		})
	}
}

// Nesting far deeper than any real script, but under the stack guard's
// limit, parses, and within 64 MB of stack: a 16th of the 1 GB limit.
func TestNestingAtTheBoundParses(t *testing.T) {
	defer debug.SetMaxStack(debug.SetMaxStack(64 << 20))
	const n = 990
	cases := map[string]string{
		"cmdsubst": "echo " + strings.Repeat("$(", n) + "x" + strings.Repeat(")", n) + "\n",
		"dq":       "echo " + strings.Repeat(`"$(`, n/2) + "x" + strings.Repeat(`)"`, n/2) + "\n",
		"if":       strings.Repeat("if a; then\n", n) + ":\n" + strings.Repeat("fi\n", n),
		"while":    strings.Repeat("while a; do\n", n) + ":\n" + strings.Repeat("done\n", n),
		"braces":   strings.Repeat("{ ", n) + ":" + strings.Repeat("; }", n) + "\n",
		"ternary":  "echo $((" + strings.Repeat("a?", n) + "1" + strings.Repeat(":1", n) + "))\n",
		"funcs":    strings.Repeat("f() ", n) + "{ :; }\n",
		"unary":    "echo $((" + strings.Repeat("- ", n) + "1))\n",
		"test-not": "[[ " + strings.Repeat("! ", n) + "a ]]\n",
		"assign":   "echo $((" + strings.Repeat("a=", n) + "1))\n",
		"elif":     "if a; then :\n" + strings.Repeat("elif a; then :\n", n) + "fi\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := Parse("x.sh", []byte(src), Bash)
			if f.Skipped != "" || len(f.Errors) != 0 {
				t.Fatalf("skipped %q, errors %v", f.Skipped, f.Errors)
			}
		})
	}
}

// A long && / || chain is one KAndOr holding every statement in source order.
func TestAndOrChainIsFlat(t *testing.T) {
	f := Parse("x.sh", []byte("a && b || c && d\n"), Bash)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	ao := f.Root.Children[0].Children[0]
	if ao.Kind != tree.KAndOr || len(ao.Children) != 4 {
		t.Fatalf("%v with %d children", ao.Kind, len(ao.Children))
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if got := ao.Children[i].Children[0].Children[0].Children[0].Value; got != want {
			t.Errorf("stage %d = %q", i, got)
		}
	}
}

// Ordinary nesting, far below the bounds, parses without any depth error.
func TestModerateNestingParses(t *testing.T) {
	cases := map[string]string{
		"cmdsubst": "echo " + strings.Repeat("$(", 300) + "x" + strings.Repeat(")", 300) + "\n",
		"if":       strings.Repeat("if a; then\n", 500) + ":\n" + strings.Repeat("fi\n", 500),
		"braces":   strings.Repeat("{ ", 300) + ":" + strings.Repeat("; }", 300) + "\n",
		"arith":    "echo $((" + strings.Repeat("(", 300) + "1" + strings.Repeat(")", 300) + "))\n",
		// Many globs, URLs and comment parens in a long file are not nesting.
		"globs":    strings.Repeat("ls ?? a?b http://x/?q=1\n", 20000),
		"brackets": strings.Repeat(": [[{][0-9]*[]}][[{][[:digit:]]*[]}] [!{]\n", 5000),
		"comments": strings.Repeat("# note (see below) :(\n", 5000),
		"case":     "case $x in\n" + strings.Repeat("a) echo case esac ;;\n", 5000) + "esac\n",
		"heredocs": strings.Repeat("cat <<E\n(((\nE\n", 5000),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := Parse("x.sh", []byte(src), Bash)
			if f.Skipped != "" || hasErr(f, "nest") {
				t.Fatalf("skipped %q, errors %v", f.Skipped, f.Errors)
			}
		})
	}
}

// The parser panics on some broken input (found by FuzzParse). Parse reports
// it as an error over the region and resyncs on the next line.
func TestParserPanicIsReported(t *testing.T) {
	src := "A=$((0 A\nf() { :; }\n"
	f := Parse("x.sh", []byte(src), Bash)
	if !hasErr(f, "parser failure") {
		t.Fatalf("errors %v", f.Errors)
	}
	found := false
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KFuncDecl && n.Name == "f" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("the line after the failure was not recovered")
	}
	checkInvariants(t, f)
}
