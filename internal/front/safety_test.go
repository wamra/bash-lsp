package front

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"

	"github.com/wamra/bash-lsp/tree"
)

// lastFunc is appended to every case of TestNoFalseRefusal: a file is
// analysed only if the function after the unusual part reaches the tree.
const lastFunc = "wb_last_function() { :; }\n"

func numbered(n int, format string) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, format, i)
	}
	return b.String()
}

// P2: valid but unusual files are analysed, not refused. The first two were
// refused whole by the pre-scan this guard replaced (an unclosed `[` made
// every later operator count as nesting); the rest are shapes a nesting
// count is tempted to refuse. bench/repro-c1/fp.py holds the same list.
func TestNoFalseRefusal(t *testing.T) {
	cases := map[string]string{
		"param-unclosed-bracket": "x=${y//[/_}\n" + numbered(1200, "export A%d=b\n"),
		"word-unclosed-bracket":  "echo \"usage:\" cmd [\n" + numbered(1200, "A%d=1\n"),
		"elif-999":               "if [ \"$1\" = a ]; then :\n" + numbered(999, "elif [ \"$1\" = a%d ]; then :\n") + "fi\n",
		"pipeline-3000":          "cat x" + strings.Repeat(" | cat", 3000) + "\n",
		"sequential-if-3000":     strings.Repeat("if x; then y; fi\n", 3000),
		"heredoc-parens-500k":    "cat <<EOF\n" + strings.Repeat("(((((\n", 500000/6) + "EOF\n",
		// The rest of fp.py.
		"bracket-then-assigns": "echo [\n" + numbered(1200, "A%d=1\n"),
		"bracket-then-pipes":   "echo Press [Enter\n" + strings.Repeat("ls | grep x | wc -l\n", 400),
		"elif-1500":            "if [ \"$1\" = a0 ]; then :\n" + numbered(1500, "elif [ \"$1\" = a%d ]; then :\n") + "fi\n",
		"case-3000":            "case $1 in\n" + numbered(3000, "  a%[1]d) echo %[1]d ;;\n") + "esac\n",
		"andor-3000":           "true" + strings.Repeat(" && true", 3000) + "\n",
		"heredoc-brackets":     "cat <<EOF\n" + strings.Repeat("((((([[[[{{{{ $x\n", 5000) + "EOF\n",
		"dquote-brackets":      strings.Repeat("echo \"((((( [[[[ {{{{\"\n", 5000),
		"comment-smileys":      strings.Repeat("# :( :( :( :(\n", 6000),
		"sequential-if-5000":   strings.Repeat("if x; then y; fi\n", 5000),
		"time-seq":             strings.Repeat("time ls\n", 3000),
		"func-seq":             numbered(3000, "f%d() { :; }\n"),
		// The stray `)` makes resync start parses inside the heredoc, where
		// each line of its literal body reads as code deep enough to be
		// stopped; that must not refuse the file.
		"broken-heredoc-line": "cat <<'EOF' )\n" + strings.Repeat(strings.Repeat("$(", 40000)+"\n", 5) + "EOF\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f := Parse("x.sh", []byte(src+lastFunc), Bash)
			if f.Skipped != "" {
				t.Fatalf("refused: %s (errors %v)", f.Skipped, f.Errors)
			}
			if hasErr(f, "nest") {
				t.Fatalf("depth error: %v", f.Errors)
			}
			found := false
			f.Root.Walk(func(n *tree.Node) bool {
				if n.Kind == tree.KFuncDecl && n.Name == "wb_last_function" {
					found = true
				}
				return true
			})
			if !found {
				t.Fatalf("the function after the unusual part is not in the tree (errors %v)", f.Errors)
			}
		})
	}
}

// A refusal says why and nothing else: no level count, one error.
func TestRefusalMessage(t *testing.T) {
	f := Parse("x.sh", []byte(strings.Repeat("$(", 100000)+"\n"), Bash)
	const want = "nesting too deep to parse safely"
	if f.Skipped != want {
		t.Fatalf("Skipped = %q, want %q", f.Skipped, want)
	}
	if len(f.Errors) != 1 || f.Errors[0].Msg != want {
		t.Fatalf("errors %v, want one %q", f.Errors, want)
	}
	if len(f.Root.Children) != 0 {
		t.Fatal("a refused file has no statements")
	}
	checkInvariants(t, f)
}

// wraps are the constructs the parser recurses on (docs/m2a-nesting-design.md),
// as text to repeat before and after a seed.
var wraps = []struct{ pre, open, close, post string }{
	{"", "(", ")", "\n"},
	{"echo ", "$(", ")", "\n"},
	{"", "{ ", "; }", "\n"},
	{"cat ", "<(", ")", "\n"},
	{"echo ", `"$(`, `)"`, "\n"},
	{"echo ", "$[", "]", "\n"},
	{"echo ", "${a:-", "}", "\n"},
	{"echo ", "${x[", "]}", "\n"},
	{"echo ", "`\\", "", "\n"},
	{"echo $((", "(", ")", "))\n"},
	{"echo $((", "a?", ":1", "))\n"},
	{"echo $((", "a=", "", "))\n"},
	{"echo $((", "2**", "", "))\n"},
	{"echo $((", "- ", "", "))\n"},
	{"echo $((", "!", "", "))\n"},
	{"echo $((", "1+", "", "))\n"},
	{"let ", "a?", ":1", "\n"},
	{"[[ ", "! ", "", " ]]\n"},
	{"[[ ", "( ", " )", " ]]\n"},
	{"[[ ", "a && ", "", " ]]\n"},
	{"", "if a;then\n", "\nfi", "\n"},
	{"", "while a;do\n", "\ndone", "\n"},
	{"", "for a in b;do\n", "\ndone", "\n"},
	{"", "case a in b)\n", "\nesac", "\n"},
	{"if a; then :\n", "elif a; then :\n", "", "\nfi\n"},
	{"", "time ", "", "\n"},
	{"", "coproc ", "", "\n"},
	{"", "! ", "", "\n"},
	{"", "f() ", "", "\n"},
	{"", "function f\n", "", "\n"},
	{"", "a && ", "", "\n"},
	{"", "a | ", "", "\n"},
	{"x=$(( $(echo 1 # it's\n) ))\necho $((", "a?", ":1", "))\n"},
	{"x=$(( $(cat <<EOF\n'\nEOF\n) ))\necho $((", "a?", ":1", "))\n"},
}

// wrap nests seed n levels deep in one construct. Bits of shape leave the
// nesting unclosed (1), put a line with an unterminated quote in front so
// that the deep part is first parsed by resync (2), and multiply the depth
// by 64, up to what fits in the file limit (4): at 3,000 levels no
// construct uses half the stack the guard allows, so without that bit every
// input would be one the guard accepts.
func wrap(seed []byte, construct, shape uint8, n uint16) []byte {
	w := wraps[int(construct)%len(wraps)]
	levels := int(n) % 3001
	if shape&4 != 0 {
		per := len(w.open)
		if shape&1 == 0 {
			per += len(w.close)
		}
		levels = min(levels*64, (2<<20-len(seed)-len(w.pre)-len(w.post)-8)/per)
	}
	var b strings.Builder
	if shape&2 != 0 {
		b.WriteString("echo '\n")
	}
	b.WriteString(w.pre)
	b.WriteString(strings.Repeat(w.open, levels))
	b.Write(seed)
	if shape&1 == 0 {
		b.WriteString(strings.Repeat(w.close, levels))
	}
	b.WriteString(w.post)
	return []byte(b.String())
}

// FuzzNestingGuard is the differential check behind P1: whatever Parse
// accepts must parse, lower and walk inside a small fixed stack, and whatever
// it refuses must be refused before the stack is gone. Each input runs in a
// child (TestCrashHelper) under a 64 MiB stack limit, a sixteenth of the
// default; the child must exit 0. With the race detector a child takes over
// a second to start, so the inputs run in this process instead, under the
// same limit: an overflow then ends the test binary, which still fails.
func FuzzNestingGuard(f *testing.F) {
	add := func(seed []byte) {
		f.Add(seed, uint8(0), uint8(0), uint16(0))
	}
	for _, build := range crashCases() {
		// Small versions: the first few units and the tail of each case.
		src := build()
		if len(src) > 400 {
			src = src[:200] + src[len(src)-100:]
		}
		add([]byte(src))
	}
	for _, root := range []string{"../../testdata/corpus", "../../testdata/broken"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, rerr := os.ReadFile(p); rerr == nil && len(b) < 1<<16 {
					add(b)
				}
			}
			return nil
		})
	}
	// The corpus FuzzParse has collected.
	entries, _ := filepath.Glob("testdata/fuzz/FuzzParse/*")
	for _, p := range entries {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[1], "[]byte(") {
			continue
		}
		if s, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(lines[1], "[]byte("), ")")); err == nil {
			add([]byte(s))
		}
	}
	// Every construct at depth, closed and unclosed, directly and through resync.
	for c := range wraps {
		f.Add([]byte("a"), uint8(c), uint8(0), uint16(3))
		for shape := range uint8(4) {
			f.Add([]byte("a"), uint8(c), shape, uint16(3000))
		}
		f.Add([]byte("a"), uint8(c), uint8(4), uint16(400))
		f.Add([]byte("a"), uint8(c), uint8(7), uint16(3000))
	}
	if raceBuild {
		defer debug.SetMaxStack(debug.SetMaxStack(safetyStack))
	}
	f.Fuzz(func(t *testing.T, seed []byte, construct, shape uint8, n uint16) {
		src := wrap(seed, construct, shape, n)
		if len(src) > 2<<20 {
			t.Skip("over the file limit")
		}
		if raceBuild {
			parseAndWalk(src)
			return
		}
		out, err := runChild("stdin", src)
		if err != nil {
			t.Fatalf("child failed on %d bytes (construct %d, shape %d, n %d): %v\n%s", len(src), int(construct)%len(wraps), shape, int(n)%3001, err, firstLines(out, 12))
		}
		if !strings.Contains(out, "result: ") {
			t.Fatalf("child printed no result:\n%s", firstLines(out, 12))
		}
	})
}

// costCases hold the parser deep for megabytes: the most work the guard can
// be made to do.
func costCases() map[string]string {
	return map[string]string{
		// One parse, about 9,600 frames deep for 2 MiB.
		"sustained-under-limit": fill("echo "+strings.Repeat("$(", 1200), " a", "\n"),
		// An unterminated quote, then fifty lines that open twenty levels each
		// and never close them: resync parses the tail again from every one
		// of them, deep.
		"resync-sustained": fill("echo '\n"+strings.Repeat("echo "+strings.Repeat("$(", 20)+"\n", 50), " a", "\n"),
	}
}

// holdStack parks a goroutine on a stack of at least n bytes until the
// returned function is called; that function returns once the stack has
// been given back.
func holdStack(n int) (release func()) {
	ready, done, gone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var dive func(left int) int
	dive = func(left int) int {
		var pad [1024]byte
		if left <= 0 {
			close(ready)
			<-done
			return int(pad[0])
		}
		return dive(left-len(pad)) + int(pad[len(pad)-1])
	}
	go func() {
		defer close(gone)
		dive(n)
	}()
	<-ready
	return func() {
		close(done)
		<-gone
		runtime.GC() // a stack freed while the collector runs is released when it ends
		runtime.GC()
	}
}

// P3: the guard's work is bounded per file, whatever the input and whatever
// else the process is doing. Alone in the process, the cost cases do not get
// the parser's frames counted: its stack stays small. Next to a goroutine
// with a large stack they are counted at every read, until the file's budget
// is spent; then resync stops and a parse that still needs counting is
// refused. BenchmarkGuard turns the counts into time.
func TestGuardCostIsBounded(t *testing.T) {
	for name, src := range costCases() {
		run := func(t *testing.T) (*resyncer, attempt) {
			f := tree.NewFile("x.sh", []byte(src))
			r := &resyncer{f: f, d: Bash, budget: maxResyncAttempts}
			a := r.raw(0)
			if !a.tooDeep {
				r.resolve(a)
			}
			r.g.release()
			t.Logf("%d bytes: %d frames counted, %d resync attempts, stopped: %v", len(src), r.g.frames, maxResyncAttempts-r.budget, a.tooDeep)
			return r, a
		}
		t.Run(name+"/alone", func(t *testing.T) {
			// Frames are counted here only while a stack an earlier test
			// grew has not been released yet.
			if _, a := run(t); a.tooDeep {
				t.Fatal("stopped")
			}
		})
		t.Run(name+"/beside-a-large-stack", func(t *testing.T) {
			defer holdStack(maxStackMemory)()
			// The last count may start just under the budget.
			if r, _ := run(t); r.g.frames > maxProbeFrames+maxFrames+1 {
				t.Fatalf("%d frames counted, bound %d", r.g.frames, maxProbeFrames+maxFrames+1)
			}
		})
	}
}

// Another goroutine's large stack makes the guard count frames; it must not
// make it refuse a file whose own nesting is ordinary.
func TestLargeStackElsewhereDoesNotRefuse(t *testing.T) {
	defer holdStack(maxStackMemory)()
	src := strings.Repeat("f() { if a; then echo \"$(b \"$(c)\")\"; fi; }\n", 20000)
	f := Parse("x.sh", []byte(src), Bash)
	if f.Skipped != "" || len(f.Errors) != 0 || len(f.Root.Children) != 20000 {
		t.Fatalf("skipped %q, %d errors, %d statements", f.Skipped, len(f.Errors), len(f.Root.Children))
	}
}

// A comment line costs the parser one small frame (the lexer calls itself
// for the next token) until a line of code ends the block. A hundred
// thousand of them in a row is about 13 MiB of stack, more under the race
// detector: accepted.
func TestLongCommentBlockParses(t *testing.T) {
	runtime.GC() // release the stacks earlier tests grew
	runtime.GC()
	f := Parse("x.sh", []byte(strings.Repeat("# c\n", 100000)+lastFunc), Bash)
	if f.Skipped != "" || len(f.Errors) != 0 || len(f.Comments) != 100000 {
		t.Fatalf("skipped %q, errors %v, %d comments", f.Skipped, f.Errors, len(f.Comments))
	}
}

// The second regime (scan.go): in a process that already holds more than
// maxStackMemory of stack, the guard cannot tell whose it is and falls back
// to counting frames, so the limit is maxFrames frames, whatever their size,
// and the counting itself is rationed. A comment block is the cheapest way
// to get there.
func TestFrameLimitBesideLargeStack(t *testing.T) {
	defer holdStack(maxStackMemory)()
	under := Parse("x.sh", []byte(strings.Repeat("# c\n", 10000)+lastFunc), Bash)
	if under.Skipped != "" || len(under.Errors) != 0 {
		t.Fatalf("10,000 comment lines: skipped %q, errors %v", under.Skipped, under.Errors)
	}
	over := Parse("x.sh", []byte(strings.Repeat("# c\n", maxFrames+5000)+lastFunc), Bash)
	if over.Skipped != refusal {
		t.Fatalf("%d comment lines: skipped %q, errors %v", maxFrames+5000, over.Skipped, over.Errors)
	}
}

// A refused file must not leave the process in the second regime: the stack
// the parser grew is given back before Parse returns.
func TestRefusalGivesTheStackBack(t *testing.T) {
	runtime.GC() // release the stacks earlier tests grew
	runtime.GC()
	g := &stackGuard{}
	if before := g.stackMemory(); before >= maxStackMemory/2 {
		t.Skipf("%d bytes of stack held before the test", before)
	}
	done := make(chan uint64)
	go func() {
		f := Parse("x.sh", []byte(strings.Repeat("(", 200000)+"\n"), Bash)
		if f.Skipped != refusal {
			t.Errorf("not refused: %q", f.Skipped)
		}
		done <- g.stackMemory()
	}()
	if after := <-done; after >= maxStackMemory {
		t.Fatalf("%d bytes of stack held after a refusal", after)
	}
}

// BenchmarkGuard measures what the guard adds. WB_BENCH_FILE names the input
// (bench/gen_big.py's file for the P3 number); the cost cases above are
// always run. "unguarded" is the same parser reading the bytes directly.
// With WB_BENCH_LARGE_STACK=1 a parked goroutine holds a large stack, which
// makes the guard count frames at every read: its worst case.
func BenchmarkGuard(b *testing.B) {
	inputs := costCases()
	if os.Getenv("WB_BENCH_LARGE_STACK") == "1" {
		defer holdStack(maxStackMemory)()
	}
	if p := os.Getenv("WB_BENCH_FILE"); p != "" {
		src, err := os.ReadFile(p)
		if err != nil {
			b.Fatal(err)
		}
		inputs["file"] = string(src)
	}
	for name, s := range inputs {
		src := []byte(s)
		b.Run(name+"/parser-unguarded", func(b *testing.B) {
			for b.Loop() {
				p := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50), syntax.Variant(syntax.LangBash))
				p.Parse(bytes.NewReader(src), "x.sh")
			}
		})
		b.Run(name+"/parser-guarded", func(b *testing.B) {
			for b.Loop() {
				g := &stackGuard{}
				parseSafely(g, src, "x.sh", Bash)
				g.release()
			}
		})
		b.Run(name+"/Parse", func(b *testing.B) {
			for b.Loop() {
				Parse("x.sh", src, Bash)
			}
		})
	}
}
