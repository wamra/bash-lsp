package front

import (
	"strings"
	"testing"

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

// Each of these drove mvdan's recursive-descent parser past Go's 1 GB stack
// limit, a fatal error no recover() catches (review finding C1). Parse must
// refuse them before parsing: a Skipped file with an error that says why.
func TestDeepNestingIsSkipped(t *testing.T) {
	cases := map[string]string{
		"paren":       strings.Repeat("(", 200000) + "\n",
		"cmdsubst":    "echo " + strings.Repeat("$(", 500000) + "\n",
		"brace":       strings.Repeat("{ ", 500000) + "\n",
		"procsubst":   "cat " + strings.Repeat("<(", 500000) + "\n",
		"dq-cmdsubst": "echo " + strings.Repeat(`"$(`, 400000) + "\n",
		"dollar-brk":  "echo " + strings.Repeat("$[", 500000) + "\n",
		"ternary":     "echo $((" + strings.Repeat("a?", 900000) + "\n",
		"let-ternary": "let " + strings.Repeat("a?", 900000) + "1:2\n",
		"test-not":    "[[ " + strings.Repeat("! ", 1000000) + "a ]]\n",
		// Closers the parser does not see as closers must not hide the depth.
		"quoted-closers":  strings.Repeat("$( '))' ", 260000) + "\n",
		"case-patterns":   strings.Repeat("$(case x in a) ", 130000) + "\n",
		"comment-closers": strings.Repeat("$( # ))\n", 260000),
		"heredoc-closers": strings.Repeat("$(cat <<'E'\n)))\nE\n", 100000),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := Parse("x.sh", []byte(src), Bash)
			if f.Skipped == "" {
				t.Fatalf("not skipped (%d errors, depth %d)", len(f.Errors), treeDepth(f))
			}
			if !strings.Contains(f.Skipped, "nesting") || !hasErr(f, "nesting") {
				t.Fatalf("skipped %q, errors %v", f.Skipped, f.Errors)
			}
			if len(f.Root.Children) != 0 {
				t.Fatal("a skipped file has no statements")
			}
			checkInvariants(t, f)
		})
	}
}

// Shapes the parser survives but that recursed (or ran quadratically) in the
// lowerer, mvdan's own Walk or End: they must lower to a shallow tree, either
// whole (chains are flattened) or with the over-deep statement dropped and
// reported.
func TestDeepShapesLowerSafely(t *testing.T) {
	cases := []struct {
		name, src string
		whole     bool // the statement survives (chains), else it is dropped with an error
		unclosed  bool // a hard parse error comes first: any error will do
	}{
		{"andor", "a " + strings.Repeat("&& a ", 400000) + "\n", true, false},
		{"pipes", "a " + strings.Repeat("| a ", 400000) + "\n", true, false},
		{"if", strings.Repeat("if a;then\n", 150000) + ":\n" + strings.Repeat("fi\n", 150000), false, false},
		{"while", strings.Repeat("while a;do\n", 120000) + ":\n" + strings.Repeat("done\n", 120000), false, false},
		{"if-unclosed", strings.Repeat("if a;then\n", 190000), false, true},
		{"while-unclosed", strings.Repeat("while a;do\n", 190000), false, true},
		{"elif", "if a; then :\n" + strings.Repeat("elif a; then :\n", 100000) + "fi\n", false, false},
		{"time", strings.Repeat("time ", 400000) + "a\n", false, false},
		{"coproc", strings.Repeat("coproc ", 290000) + "a\n", false, false},
		{"funcs", strings.Repeat("f() ", 400000) + "{ :; }\n", false, false},
		// Comments inside a chain made mvdan's Walk call Stmt.End at every level.
		{"funcs-commented", strings.Repeat("f() # c\n", 200000) + "{ :; }\n", false, false},
		{"pipes-commented", "a " + strings.Repeat("| # c\na ", 200000) + "\n", true, false},
		{"arith-plus", "echo $((" + strings.Repeat("1+", 900000) + "1))\n", true, false},
		{"arith-comma", "echo $((" + strings.Repeat("1,", 900000) + "1))\n", true, false},
		{"arith-assign", "echo $((" + strings.Repeat("a=", 900000) + "1))\n", true, false},
		{"arith-power", "echo $((" + strings.Repeat("2**", 600000) + "2))\n", true, false},
		{"arith-minus", "echo $((" + strings.Repeat("- ", 1000000) + "1))\n", true, false},
		{"arith-not", "echo $((" + strings.Repeat("!", 2000000) + "1))\n", true, false},
		{"test-and", "[[ " + strings.Repeat("a && ", 400000) + "a ]]\n", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := Parse("x.sh", []byte(c.src), Bash)
			if f.Skipped != "" {
				t.Fatalf("skipped: %s", f.Skipped)
			}
			if hasErr(f, "internal error") {
				t.Fatalf("internal error: %v", f.Errors[:1])
			}
			if d := treeDepth(f); d > 3*maxDepth+10 {
				t.Fatalf("tree depth %d over the bound", d)
			}
			if c.whole && len(f.Root.Children) == 0 {
				t.Fatalf("statement dropped: %v", f.Errors)
			}
			if c.unclosed {
				if len(f.Errors) == 0 {
					t.Fatal("no error")
				}
				return
			}
			if !c.whole && !hasErr(f, "nested deeper") {
				t.Fatalf("no depth error: %v", f.Errors)
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
