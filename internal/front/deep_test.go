package front

import (
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front/syntax"

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

// Each of these drove the recursive-descent parser past Go's stack limit (a
// fatal error no recover() catches; review finding C1) before it counted its
// own nesting. Now it stops at 10,000 levels with a syntax error that
// carries the parser's text. These files hold nothing else, so they are
// skipped, unless resync finds a statement on a later line.
// TestNoCrashSubprocess runs the same shapes, and more, under a 64 MiB stack
// limit.
func TestDeepNestingIsSkipped(t *testing.T) {
	cases := map[string]string{
		"paren":       strings.Repeat("(", 200000) + "\n",
		"cmdsubst":    "echo " + strings.Repeat("$(", 500000) + "\n",
		"brace":       strings.Repeat("{ ", 500000) + "\n",
		"procsubst":   "cat " + strings.Repeat("<(", 500000) + "\n",
		"dq-cmdsubst": "echo " + strings.Repeat(`"$(`, 400000) + "\n",
		"dollar-brk":  "echo " + strings.Repeat("$[", 500000) + "\n",
		// Operator chains: every operator is a level.
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
		"arith-plus":   fill("echo $((", "1+", "1))\n"),
		"arith-comma":  fill("echo $((", "1,", "1))\n"),
		"test-not":     fill("[[ ", "! ", "a ]]\n"),
		"test-and":     fill("[[ ", "a && ", "a ]]\n"),
		"andor":        fill("a ", "&& a ", "\n"),
		"pipes":        fill("a ", "| a ", "\n"),
		// Comments inside a chain made the parser's Walk call Stmt.End at every level.
		"pipes-commented": fill("a ", "| # c\na ", "\n"),
		// Compound commands, prefixes and function headers.
		"if":              fill("", "if a;then\n", ""),
		"while":           fill("", "while a;do\n", ""),
		"for":             fill("", "for a in b;do\n", ""),
		"for-in-done":     fill("", "for x in done; do ", ":\n"),
		"case":            fill("", "case a in b)\n", ""),
		"elif":            fill("if a; then :\n", "elif a; then :\n", "fi\n"),
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
			if len(f.Errors) == 0 || f.Errors[0].Msg != nestingError {
				t.Fatalf("first error is not the nesting error: %v", firstErrors(f, 3))
			}
			if hasErr(f, "parser failure") {
				t.Fatalf("parser failure: %v", firstErrors(f, 3))
			}
			checkSkipped(t, f)
			checkInvariants(t, f)
		})
	}
}

// checkSkipped fails unless f is skipped exactly when a nesting error left
// it without a statement, and then has no statements at all.
func checkSkipped(t *testing.T, f *tree.File) {
	t.Helper()
	if f.Skipped != "" {
		if f.Skipped != nestingError || len(f.Root.Children) != 0 || !errorWith(f.Errors, nestingError) {
			t.Fatalf("skipped %q with %d statements, errors %v", f.Skipped, len(f.Root.Children), firstErrors(f, 3))
		}
		return
	}
	if nothingBut(f.Root.Children, tree.KErrorGap) && errorWith(f.Errors, nestingError) {
		t.Fatalf("not skipped, with no statement and errors %v", firstErrors(f, 3))
	}
}

// A file that is nothing but too much nesting is skipped, with the parser's
// text and nothing else: no level count of our own, one error.
func TestRefusalMessage(t *testing.T) {
	f := Parse("x.sh", []byte(strings.Repeat("$(", 100000)+"\n"), Bash)
	if f.Skipped != nestingError {
		t.Fatalf("Skipped = %q, want %q", f.Skipped, nestingError)
	}
	if len(f.Errors) != 1 || f.Errors[0].Msg != nestingError {
		t.Fatalf("errors %v, want one %q", firstErrors(f, 3), nestingError)
	}
	if len(f.Root.Children) != 0 {
		t.Fatal("a skipped file has no statements")
	}
	checkInvariants(t, f)
}

func firstErrors(f *tree.File, n int) []tree.SyntaxError {
	if len(f.Errors) > n {
		return f.Errors[:n]
	}
	return f.Errors
}

// Beside other code a nesting error is an ordinary syntax error: one
// diagnostic at the place the limit was passed, the statements before it
// analysed, the lines after it recovered. The file is not skipped.
func TestNestingErrorKeepsTheRestOfTheFile(t *testing.T) {
	src := "before() { :; }\necho " + strings.Repeat("$(", 100000) + "\nafter() { :; }\n"
	f := Parse("x.sh", []byte(src), Bash)
	if f.Skipped != "" {
		t.Fatalf("skipped: %q", f.Skipped)
	}
	if len(f.Errors) != 1 || f.Errors[0].Msg != nestingError {
		t.Fatalf("errors %v, want one %q", firstErrors(f, 3), nestingError)
	}
	// 5,000 `$(` are 10,000 levels; the next one is refused where it starts.
	if at := f.Errors[0].Range.Start; at.Line != 2 || at.Col != 6+2*5000 {
		t.Fatalf("error at %d:%d", at.Line, at.Col)
	}
	found := map[string]bool{}
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KFuncDecl {
			found[n.Name] = true
		}
		return true
	})
	if !found["before"] || !found["after"] {
		t.Fatalf("functions found: %v", found)
	}
	checkInvariants(t, f)
}

// limits is where the parser's nesting limit falls for each construct it
// recurses on or counts: ok repetitions of open (closed by as many close)
// parse, one more is the nesting error. The limit is 10,000 levels; a
// construct that takes two levels (a command substitution is a word part and
// a statement) or three reaches it sooner, and what surrounds the repeated
// part takes a few. Every operator of a chain and every elif is a level too:
// a chain or ladder longer than this is a syntax error (ruling for milestone
// 2a; real scripts do not have them). docs/m2a-nesting-design.md has the table.
var limits = []struct {
	name, pre, open, mid, close, post string
	ok                                int
}{
	{"paren", "", "(", "a", ")", "\n", 10000},
	{"subshell", "", "( ", "a", " )", "\n", 9999},
	{"cmdsubst", "echo ", "$(", "a", ")", "\n", 4999},
	{"brace", "", "{ ", "a", "; }", "\n", 9999},
	{"procsubst", "cat ", "<(", "a", ")", "\n", 4999},
	{"dq-cmdsubst", "echo ", `"$(`, "a", `)"`, "\n", 3333},
	{"dollar-brk", "echo ", "$[", "1", "]", "\n", 4999},
	{"param", "echo ", "${a:-", "a", "}", "\n", 9998},
	{"param-index", "echo ", "${x[", "1", "]}", "\n", 4999},
	{"arith-paren", "echo $((", "(", "1", ")", "))\n", 9997},
	{"ternary", "echo $((", "a?", "1", ":1", "))\n", 9997},
	{"arith-assign", "echo $((", "a=", "1", "", "))\n", 9997},
	{"arith-power", "echo $((", "2**", "2", "", "))\n", 9997},
	{"arith-minus", "echo $((", "- ", "1", "", "))\n", 9997},
	{"arith-not", "echo $((", "!", "1", "", "))\n", 9997},
	{"arith-plus", "echo $((", "1+", "1", "", "))\n", 9997},
	{"arith-comma", "echo $((", "1,", "1", "", "))\n", 9997},
	{"arith-subscript", "echo $((", "a[", "1", "]", "))\n", 9997},
	{"arith-in-arith", "echo ", "$((", "1", "))", "\n", 4999},
	{"assign-subscript", "", "a[", "1", "]", "=1\n", 9999},
	{"param-replace", "echo ", "${a/", "a", "}", "\n", 9998},
	{"dq-param", "echo ", `"${a:-`, "a", `}"`, "\n", 4999},
	{"func-braces", "", "function f { ", ":", "; }", "\n", 4999},
	{"if-cond", "", "if ", ":", "; then :; fi", "\n", 9999},
	{"case-word", "case ", "$(case ", "a", " in a) ;; esac)", " in a) ;; esac\n", 4999},
	{"pipe-subshell", "", "a | ( ", "a", " )", "\n", 4999},
	{"let-ternary", "let ", "a?", "1", ":1", "\n", 9998},
	{"test-not", "[[ ", "! ", "a", "", " ]]\n", 9997},
	{"test-paren", "[[ ", "( ", "a", " )", " ]]\n", 9997},
	{"test-and", "[[ ", "a && ", "a", "", " ]]\n", 9997},
	{"if", "", "if a;then\n", ":", "\nfi", "\n", 9999},
	{"while", "", "while a;do\n", ":", "\ndone", "\n", 9999},
	{"for", "", "for a in b;do\n", ":", "\ndone", "\n", 9999},
	{"case", "", "case a in b)\n", ":", "\nesac", "\n", 9999},
	{"elif", "if a; then :\n", "elif a; then :\n", "", "", "fi\n", 9998},
	{"time", "", "time ", "a", "", "\n", 9999},
	{"coproc", "", "coproc ", "a", "", "\n", 9998},
	{"funcs", "", "f() ", "{ :; }", "", "\n", 9998},
	{"function-kw", "", "function f\n", "{ :; }", "", "\n", 9998},
	{"andor", "", "a && ", "a", "", "\n", 9998},
	{"pipes", "", "a | ", "a", "", "\n", 9999},
	{"pipes-commented", "a ", "| # c\na ", "", "", "\n", 9999},
}

// atLimit is the i'th construct of limits nested n deep.
func atLimit(i, n int) string {
	l := limits[i]
	return l.pre + strings.Repeat(l.open, n) + l.mid + strings.Repeat(l.close, n) + l.post
}

// The parser's limit, pinned per construct: an upgrade of the vendored parser
// that moves it, or stops counting something, fails here.
func TestNestingLimit(t *testing.T) {
	for i, l := range limits {
		t.Run(l.name, func(t *testing.T) {
			if f := Parse("x.sh", []byte(atLimit(i, l.ok)), Bash); f.Skipped != "" || hasErr(f, "nesting is deeper") {
				t.Fatalf("%d levels: skipped %q, errors %v", l.ok, f.Skipped, firstErrors(f, 3))
			}
			f := Parse("x.sh", []byte(atLimit(i, l.ok+1)), Bash)
			if len(f.Errors) == 0 || f.Errors[0].Msg != nestingError {
				t.Fatalf("%d levels: skipped %q, errors %v, want %q", l.ok+1, f.Skipped, firstErrors(f, 3), nestingError)
			}
			checkSkipped(t, f)
		})
	}
}

// Chains the parser builds without nesting the tree, and that recursed (or
// ran quadratically) in the lowerer, the parser's Walk or End: at the longest
// the parser accepts they lower whole, to a shallow tree.
func TestLongChainsLowerSafely(t *testing.T) {
	for i, l := range limits {
		switch l.name {
		case "andor", "pipes", "arith-plus", "arith-comma", "pipes-commented":
		default:
			continue
		}
		t.Run(l.name, func(t *testing.T) {
			f := Parse("x.sh", []byte(atLimit(i, l.ok)), Bash)
			if f.Skipped != "" || len(f.Errors) != 0 {
				t.Fatalf("skipped %q, errors %v", f.Skipped, firstErrors(f, 3))
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

// The lowerer's own bound, lower than the parser's: a statement nested deeper
// than maxDepth lowering levels is dropped and reported, so analysis never
// sees a deep tree.
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
			stmts, errs, _ := lowerStmts(f, f.PosAt(0), af.Stmts, newNULIndex(f.Src))
			f.Root.Children = stmts
			f.Errors = errs
			if !hasErr(f, loweringLimitError) {
				t.Fatalf("no depth error: %v", errs)
			}
			if d := treeDepth(f); d > 3*maxDepth+10 {
				t.Fatalf("tree depth %d over the bound", d)
			}
		})
	}
}

// Nesting far deeper than any real script parses without an error, and
// within 64 MiB of stack.
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

// TestStackPerLevel measures, for each construct, the stack one level of it
// costs, and so what a parse at the limit uses. It prints; it asserts
// nothing. Run with WB_MEASURE_STACK=1 (bench/results has the output).
//
// The runtime doubles a goroutine's stack when it is full, so the stack
// memory of the process steps up when the parse passes a power of two. The
// test finds the nesting at which the last step is taken: there the parse
// uses half the new size.
func TestStackPerLevel(t *testing.T) {
	if os.Getenv("WB_MEASURE_STACK") != "1" {
		t.Skip("set WB_MEASURE_STACK=1 to measure")
	}
	stacks := func() uint64 {
		s := []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64()
	}
	grown := func(src string) uint64 {
		runtime.GC() // return the stacks of earlier parses
		runtime.GC()
		before, after := stacks(), make(chan uint64)
		go func() {
			Parse("x.sh", []byte(src), Bash)
			after <- stacks()
		}()
		if a := <-after; a > before {
			return a - before
		}
		return 0
	}
	worst := 0.0
	for i, l := range limits {
		top := grown(atLimit(i, l.ok))
		if top < 2<<20 {
			t.Logf("%-16s %5d levels at the limit: under 2 MiB of stack", l.name, l.ok)
			continue
		}
		step := uint64(1 << 20)
		for step*2 <= top {
			step *= 2
		}
		lo, hi := 1, l.ok // hi: the least nesting whose parse holds a stack of step bytes
		for hi-lo > 1 {
			if m := (lo + hi) / 2; grown(atLimit(i, m)) >= step {
				hi = m
			} else {
				lo = m
			}
		}
		per := float64(step/2) / float64(hi)
		atLim := per * float64(l.ok) / (1 << 20)
		worst = max(worst, atLim)
		t.Logf("%-16s %5d levels at the limit: stack of %2d MiB from %5d levels on, %4.0f bytes a level, %4.1f MiB at the limit", l.name, l.ok, step>>20, hi, per, atLim)
	}
	t.Logf("most stack at the limit: %.1f MiB", worst)
}
