package front

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

// A Go stack overflow is fatal: no recover() catches it and no test in the
// same process can observe it. The tests here therefore run Parse in a child:
// the test binary itself, re-executed to run TestCrashHelper alone.

const (
	crashCaseEnv = "WB_CRASH_CASE" // case name, or "stdin" to read the source from standard input
	maxStackEnv  = "WB_MAX_STACK"  // when "1", the child lowers the Go stack limit to safetyStack
)

// safetyStack is the stack limit the children run under: an eighth of the
// 512 MiB a stack can reach under the runtime's default limit. The parser
// stops at 10,000 levels, which is 42 MiB of stack for the costliest
// construct (docs/m2a-nesting-design.md).
const safetyStack = 64 << 20

// crashCases are inputs under the 2 MiB file limit that overflowed the stack
// at some point, or come as close to it as the parser now lets them: the first four in review finding C1, the rest in the
// re-review of its first fix (bench/repro-c1) or in deep_test.go. Each is
// built on demand: a child needs one, not a hundred megabytes of them.
func crashCases() map[string]func() string {
	const ternary = 900000
	cases := map[string]func() string{
		// The four review triggers, and `if`.
		"paren":    func() string { return strings.Repeat("(", 200000) + "\n" },
		"cmdsubst": func() string { return "echo " + strings.Repeat("$(", 500000) + "\n" },
		"brace":    func() string { return strings.Repeat("{ ", 500000) + "\n" },
		"andor":    func() string { return strings.Repeat("a && ", 400000) + "a\n" },
		"if":       func() string { return strings.Repeat("if a;then\n", 200000) },
		// bench/repro-c1/gen.py: an unterminated quote on line 1 hides the
		// rest from the first parse; resync re-parses from line 2.
		"resync_ternary": func() string { return "echo '\n" + "echo $(( " + strings.Repeat("a?", ternary) + "1 ))\n" },
		"resync_not":     func() string { return "echo '\n" + "[[ " + strings.Repeat("! ", ternary) + "x ]]\n" },
		"resync_if":      func() string { return "echo '\n" + strings.Repeat("if x; then ", 150000) + "\n" },
		// A comment or a heredoc inside $(( $(...) )) holds an apostrophe the
		// old pre-scan read as an open quote (m1-deferred.md, gen2.py).
		"comment_in_arith": func() string {
			return "x=$(( $(echo 1 # it's\n) ))\necho $(( " + strings.Repeat("a?", ternary) + "1 ))\n"
		},
		"heredoc_in_arith": func() string {
			return "x=$(( $(cat <<EOF\n'\nEOF\n) ))\necho $(( " + strings.Repeat("a?", ternary) + "1 ))\n"
		},
		"control_ternary": func() string { return "echo $(( " + strings.Repeat("a?", ternary) + "1 ))\n" },
		// gen3.py: megabytes of blanks or operators inside moderate nesting
		// made the old pre-scan quadratic.
		"scan_slow_blanks": func() string {
			return "echo $(( " + strings.Repeat("a=", 990) + strings.Repeat(" ", 2000000) + "1 ))\n"
		},
		"scan_slow_plus": func() string {
			return "echo $(( " + strings.Repeat("a=", 990) + strings.Repeat("+", 2000000) + "1 ))\n"
		},
		"scan_slow_ctrl": func() string {
			return "echo $(( " + strings.Repeat("a=", 5) + strings.Repeat(" ", 2000000) + "1 ))\n"
		},
		// gen2.py's small controls.
		"heredoc_in_arith_small": func() string {
			return "x=$(( $(cat <<EOF\n'\nEOF\n) ))\necho $(( a?1:2 ))\nf() { :; }\n"
		},
		"resync_small": func() string { return "echo '\nf() { :; }\n" },
	}
	// Every shape deep_test.go once listed as refused before parsing.
	for name, src := range map[string]func() string{
		"procsubst":   func() string { return "cat " + strings.Repeat("<(", 500000) + "\n" },
		"dq-cmdsubst": func() string { return "echo " + strings.Repeat(`"$(`, 400000) + "\n" },
		"dollar-brk":  func() string { return "echo " + strings.Repeat("$[", 500000) + "\n" },
		// Operator chains the parser recurses on.
		"ternary-sp":   func() string { return fill("(( ", "a ? ", "1 ))\n") },
		"let-ternary":  func() string { return fill("let ", "a?", "1\n") },
		"for-ternary":  func() string { return fill("for ((", "a ? ", "1;;)); do :; done\n") },
		"subscript":    func() string { return fill("x[", "a ? ", "1]=1\n") },
		"param-index":  func() string { return fill("echo ${x[", "a ? ", "1]}\n") },
		"param-slice":  func() string { return fill("echo ${x:", "a ? ", "1}\n") },
		"param-nested": func() string { return fill("echo ", "${x[", "\n") },
		"arith-paren":  func() string { return fill("echo $((", "(", "\n") },
		"arith-assign": func() string { return fill("echo $((", "a=", "1))\n") },
		"arith-power":  func() string { return fill("echo $((", "2**", "2))\n") },
		"arith-minus":  func() string { return fill("echo $((", "- ", "1))\n") },
		"arith-not":    func() string { return fill("echo $((", "!", "1))\n") },
		"arith-plus":   func() string { return fill("echo $((", "1+", "1))\n") },
		"test-not":     func() string { return fill("[[ ", "! ", "a ]]\n") },
		"test-and":     func() string { return fill("[[ ", "a && ", "a ]]\n") },
		"test-paren":   func() string { return fill("[[ ", "( ", "a ]]\n") },
		// Compound commands, prefixes and function headers.
		"while":           func() string { return fill("", "while a;do\n", "") },
		"for":             func() string { return fill("", "for a in b;do\n", "") },
		"for-in-done":     func() string { return fill("", "for x in done; do ", ":\n") },
		"case":            func() string { return fill("", "case a in b)\n", "") },
		"elif":            func() string { return fill("if a; then :\n", "elif a; then :\n", "fi\n") },
		"time":            func() string { return fill("", "time ", "a\n") },
		"coproc":          func() string { return fill("", "coproc ", "a\n") },
		"bang":            func() string { return fill("", "! ", "a\n") },
		"funcs":           func() string { return fill("", "f() ", "{ :; }\n") },
		"funcs-commented": func() string { return fill("", "f() # c\n", "{ :; }\n") },
		"func-dash":       func() string { return fill("", "a-b() ", "{ :; }\n") },
		"function-kw":     func() string { return fill("", "function f\n", "{ :; }\n") },
		"pipes":           func() string { return fill("a ", "| a ", "\n") },
		// Closers the parser does not see as closers.
		"quoted-closers":  func() string { return strings.Repeat("$( '))' ", 260000) + "\n" },
		"case-patterns":   func() string { return strings.Repeat("$(case x in a) ", 130000) + "\n" },
		"esac-argument":   func() string { return fill("", "$(case x in a) echo esac;; b) ", "\n") },
		"comment-closers": func() string { return strings.Repeat("$( # ))\n", 260000) },
		"heredoc-closers": func() string { return strings.Repeat("$(cat <<'E'\n)))\nE\n", 100000) },
		// Nesting held deep for megabytes, then taken past the limit.
		"sustained": func() string {
			return "echo " + strings.Repeat("$(", 1200) + strings.Repeat(" a", 900000) + strings.Repeat("$(", 60000) + "\n"
		},
		"sustained-under-limit": func() string { return costCases()["sustained-under-limit"] },
		"resync-sustained":      func() string { return costCases()["resync-sustained"] },
		"resync-at-limit":       func() string { return costCases()["resync-at-limit"] },
		"resync-under-limit":    func() string { return costCases()["resync-under-limit"] },
		// One comment line was one parser frame until the lexer's self-call
		// became a loop: 2 MiB of them took 256 MiB of stack.
		"comments-2MiB":      func() string { return strings.Repeat("#\n", 1<<20) },
		"comments-then-code": func() string { return strings.Repeat("# c\n", 500000) + lastFunc },
		"comments-in-pipes":  func() string { return fill("a ", "|\n#\n", "a\n") },
		"comments-in-subst":  func() string { return "echo $(\n" + strings.Repeat("#\n", 1000000) + "a)\n" },
		// `++` and `--` recurse; an error ends it after two levels, and the
		// parser now counts it anyway.
		"arith-incr": func() string { return fill("echo $((", "++", "a))\n") },
		"arith-decr": func() string { return fill("echo $((", "-- ", "a))\n") },
		// A heredoc body is read by a nested call of the lexer, counted now.
		"heredoc-lines":  func() string { return fill("", "cat <<E\n$(\nE\n", "") },
		"heredoc-nested": func() string { return fill("", "cat <<E\n$(cat <<E\n", "") },
	} {
		cases[name] = src
	}
	// Every construct at the deepest the parser accepts, closed properly:
	// the most stack a parse can use, and the deepest tree the lowerer and
	// the walks can be handed.
	for i, l := range limits {
		cases["limit-"+l.name] = func() string { return atLimit(i, l.ok) }
	}
	return cases
}

// nested is a case that wraps mid in n levels of open and close.
func nested(pre, open, mid, close, post string, n int) func() string {
	return func() string {
		return pre + strings.Repeat(open, n) + mid + strings.Repeat(close, n) + post
	}
}

// TestCrashHelper is the child side: it parses one source and walks the
// result, as analysis does. It does nothing unless crashCaseEnv is set.
func TestCrashHelper(t *testing.T) {
	name := os.Getenv(crashCaseEnv)
	if name == "" {
		t.Skip("child side of TestNoCrashSubprocess and FuzzNestingGuard")
	}
	var src []byte
	if name == "stdin" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		src = b
	} else {
		build, ok := crashCases()[name]
		if !ok {
			t.Fatalf("unknown case %q", name)
		}
		src = []byte(build())
	}
	if os.Getenv(maxStackEnv) == "1" {
		debug.SetMaxStack(safetyStack)
	}
	fmt.Println(parseAndWalk(src))
}

// parseAndWalk parses src and walks the tree recursively, the way every
// consumer does. It panics when the tree breaks the depth the lowerer promises.
func parseAndWalk(src []byte) string {
	f := Parse("x.sh", src, Bash)
	nodes := 0
	f.Root.Walk(func(*tree.Node) bool { nodes++; return true })
	if d := treeDepth(f); d > 3*maxDepth+10 {
		panic(fmt.Sprintf("tree depth %d over the lowerer's bound", d))
	}
	if f.Skipped != "" {
		if f.Skipped != nestingError || len(f.Root.Children) != 0 || !errorWith(f.Errors, nestingError) {
			panic(fmt.Sprintf("skipped %q with %d statements, errors %v", f.Skipped, len(f.Root.Children), f.Errors))
		}
		return "result: skipped: " + f.Skipped
	}
	return fmt.Sprintf("result: parsed: %d statements, %d nodes, %d errors, nesting error: %v", len(f.Root.Children), nodes, len(f.Errors), hasErr(f, nestingError))
}

// runChild runs TestCrashHelper in a child process and returns its output.
func runChild(name string, stdin []byte) (string, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), crashCaseEnv+"="+name, maxStackEnv+"=1")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// firstLines keeps a crash dump (hundreds of thousands of frames) readable.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// P1: for every input up to the file limit Parse returns, and the tree it
// returns can be walked.
func TestNoCrashSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("runs one child process per case")
	}
	for name, build := range crashCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if n := len(build()); n > 2<<20 {
				t.Fatalf("%d bytes, over the 2 MiB file limit", n)
			}
			out, err := runChild(name, nil)
			if err != nil {
				t.Fatalf("child failed: %v\n%s", err, firstLines(out, 12))
			}
			if !strings.Contains(out, "result: parsed: ") && !strings.Contains(out, "result: skipped: "+nestingError) {
				t.Fatalf("child printed no result:\n%s", firstLines(out, 12))
			}
			// The cases named for it sit at the parser's limit, not past it.
			if strings.HasPrefix(name, "limit-") && !strings.Contains(out, "nesting error: false") {
				t.Fatalf("nesting error at the limit:\n%s", firstLines(out, 12))
			}
			// Flat input is never a nesting error, whatever its length.
			if strings.HasPrefix(name, "comments-") && !strings.Contains(out, "nesting error: false") {
				t.Fatalf("nesting error in comments:\n%s", firstLines(out, 12))
			}
		})
	}
}
