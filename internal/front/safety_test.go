package front

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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

// P2: valid but unusual files are analysed, with no nesting error. The first
// two were refused whole by a pre-scan that counted brackets (an unclosed `[`
// made every later operator count as nesting); the rest are shapes a nesting
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
		// Flat input far longer than the nesting limit: each heredoc and
		// each comment line is read at the same depth as the one before.
		"heredoc-seq-20000": strings.Repeat("cat <<E\n$x (\nE\n", 20000),
		"comments-300000":   strings.Repeat("# (\n", 300000),
		"long-string":       "x='" + strings.Repeat("((((\n", 200000) + "'\n",
		"statements-200000": strings.Repeat("a=$(b)\n", 200000),
		"comments-in-if":    "if a; then\n" + strings.Repeat("# c\n", 100000) + ":\nfi\n",
		"pipeline-9999":     "cat x" + strings.Repeat(" | cat", 9999) + "\n",
		"array-20000":       "a=(\n" + numbered(20000, "x%d\n") + ")\n",
		"case-items-20000":  "case $1 in\n" + numbered(20000, "a%d) : ;;\n") + "esac\n",
		"args-continued":    "cmd" + numbered(20000, " \\\n  --opt%d") + "\n",
		"dquote-substs":     "echo \"" + strings.Repeat("$(a) ${b} $((c)) ", 20000) + "\"\n",
		"func-seq":          numbered(3000, "f%d() { :; }\n"),
		// The stray `)` makes resync start parses inside the heredoc, where
		// each line of its literal body reads as code nested past the limit;
		// that must not cost the file the function after it.
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
// by 64, up to what fits in the file limit (4): at 3,000 levels few
// constructs reach the parser's limit, so without that bit nearly every
// input would be one it accepts.
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

// FuzzNestingGuard is the check behind P1: whatever the input, Parse must
// parse (or stop at the nesting limit), lower and walk inside a small fixed
// stack. Each input runs in a
// child (TestCrashHelper) under a 64 MiB stack limit, an eighth of what
// a stack can reach by default; the child must exit 0. With the race detector a child takes over
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

// costCases hold the parser deep for megabytes, once and through resync.
func costCases() map[string]string {
	return map[string]string{
		// One parse, 2,400 levels deep for 2 MiB.
		"sustained-under-limit": fill("echo "+strings.Repeat("$(", 1200), " a", "\n"),
		// An unterminated quote, then fifty lines that open twenty levels each
		// and never close them: resync parses the tail again from every one
		// of them, deep.
		"resync-sustained": fill("echo '\n"+strings.Repeat("echo "+strings.Repeat("$(", 20)+"\n", 50), " a", "\n"),
	}
}

// A block of comment lines is flat input: the lexer loops over it, whatever
// its length (it called itself once per line before; 2 MiB of them took
// 256 MiB of stack). TestNoCrashSubprocess runs the same under a stack limit.
func TestCommentBlockOfAnyLengthParses(t *testing.T) {
	f := Parse("x.sh", []byte(strings.Repeat("# c\n", 100000)+lastFunc), Bash)
	if f.Skipped != "" || len(f.Errors) != 0 || len(f.Comments) != 100000 || len(f.Root.Children) != 1 {
		t.Fatalf("100,000 lines: skipped %q, errors %v, %d comments", f.Skipped, f.Errors, len(f.Comments))
	}
	f = Parse("x.sh", []byte(strings.Repeat("#\n", 1<<20)), Bash)
	if f.Skipped != "" || len(f.Errors) != 0 || len(f.Comments) != 1<<20 {
		t.Fatalf("2 MiB: skipped %q, errors %v, %d comments", f.Skipped, f.Errors, len(f.Comments))
	}
}

// BenchmarkGuard measures what a parse costs now that the parser counts its
// own nesting: "parser" is the vendored parser alone, "Parse" adds lowering
// and resync. WB_BENCH_FILE names the input (bench/gen_big.py's file for the
// P3 number); the cost cases above are always run. The same benchmark at the
// commit before the parser was vendored gives the cost without the counter.
func BenchmarkGuard(b *testing.B) {
	inputs := costCases()
	if p := os.Getenv("WB_BENCH_FILE"); p != "" {
		src, err := os.ReadFile(p)
		if err != nil {
			b.Fatal(err)
		}
		inputs["file"] = string(src)
	}
	for name, s := range inputs {
		src := []byte(s)
		b.Run(name+"/parser", func(b *testing.B) {
			for b.Loop() {
				parseSafely(src, "x.sh", Bash)
			}
		})
		b.Run(name+"/Parse", func(b *testing.B) {
			for b.Loop() {
				Parse("x.sh", src, Bash)
			}
		})
	}
}

// summary is what a caller can tell two parses of one file apart by.
func summary(f *tree.File) string {
	nodes := 0
	f.Root.Walk(func(*tree.Node) bool { nodes++; return true })
	return fmt.Sprintf("skipped %q, %d statements, %d nodes, %d comments, errors %v", f.Skipped, len(f.Root.Children), nodes, len(f.Comments), f.Errors)
}

// Parses share no state: a file's result is the same whatever else is being
// parsed at the time. Here a file nested far too deep, files at the parser's
// limit and flat files are parsed side by side, and each must come out as it
// does alone, every time.
func TestDeepFileBesideOthers(t *testing.T) {
	inputs := map[string][]byte{
		"too-deep":    []byte(strings.Repeat("(", 200000) + "\n"),
		"deep-lines":  []byte("f() { :; }\n" + strings.Repeat("if a;then\n", 20000)),
		"at-limit":    []byte(atLimit(0, limits[0].ok)),
		"cmdsubst":    []byte(atLimit(1, 3000)),
		"comments":    []byte(strings.Repeat("# a typical comment line of moderate length here\n", 30000)),
		"functions":   []byte(strings.Repeat("f() { if a; then echo \"$(b \"$(c)\")\"; fi; }\n", 2000)),
		"broken-line": []byte("a() { :; }\necho 'x\nb() { :; }\n"),
	}
	alone := map[string]string{}
	for name, src := range inputs {
		alone[name] = summary(Parse(name+".sh", src, Bash))
	}
	if !strings.Contains(alone["too-deep"], nestingError) || strings.Contains(alone["at-limit"], nestingError) {
		t.Fatalf("alone: too-deep: %.200s\nat-limit: %.200s", alone["too-deep"], alone["at-limit"])
	}
	rounds := 10
	if raceBuild || testing.Short() {
		rounds = 2
	}
	var wg sync.WaitGroup
	var changed atomic.Int64
	for name, src := range inputs {
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range rounds {
					if got := summary(Parse(name+".sh", src, Bash)); got != alone[name] {
						changed.Add(1)
						t.Errorf("%s beside the others: %.200s\nalone: %.200s", name, got, alone[name])
						return
					}
				}
			}()
		}
	}
	wg.Wait()
	if n := changed.Load(); n != 0 {
		t.Fatalf("%d files changed their result", n)
	}
}
