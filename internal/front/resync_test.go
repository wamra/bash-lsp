package front

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/tree"
)

// count returns the number of nodes of kind k in the tree.
func count(f *tree.File, k tree.Kind) int {
	n := 0
	f.Root.Walk(func(x *tree.Node) bool {
		if x.Kind == k {
			n++
		}
		return true
	})
	return n
}

// checkInvariants fails the test when the tree is malformed: ranges outside
// [0, len(Src)], inverted ranges, broken parent links, or a child outside its
// parent. Comments are not tree children, so only their range is checked.
func checkInvariants(t *testing.T, f *tree.File) {
	t.Helper()
	size := len(f.Src)
	inside := func(r tree.Range) bool {
		return 0 <= r.Start.Offset && r.Start.Offset <= r.End.Offset && r.End.Offset <= size
	}
	if f.Root == nil || f.Root.Kind != tree.KFile || f.Root.Parent != nil {
		t.Fatalf("bad root %+v", f.Root)
	}
	f.Root.Walk(func(n *tree.Node) bool {
		if !inside(n.Range) {
			t.Errorf("%v range %+v outside [0,%d]", n.Kind, n.Range, size)
		}
		for _, c := range n.Children {
			if c.Parent != n {
				t.Errorf("%v child %v has wrong Parent", n.Kind, c.Kind)
			}
			if c.Range.Start.Offset < n.Range.Start.Offset || c.Range.End.Offset > n.Range.End.Offset {
				t.Errorf("%v %+v escapes parent %v %+v", c.Kind, c.Range, n.Kind, n.Range)
			}
		}
		return true
	})
	for _, c := range f.Comments {
		if !inside(c.Range) {
			t.Errorf("comment range %+v outside [0,%d]", c.Range, size)
		}
	}
	for _, e := range f.Errors {
		if !inside(e.Range) {
			t.Errorf("error range %+v outside [0,%d]", e.Range, size)
		}
	}
	// Features are inside the file, in source order, never inside a gap.
	var gaps []tree.Range
	for _, n := range f.Root.Children {
		if n.Kind == tree.KErrorGap {
			gaps = append(gaps, n.Range)
		}
	}
	for i, ft := range f.Features {
		if !inside(ft.Range) || ft.Key == "" {
			t.Errorf("feature %+v outside [0,%d]", ft, size)
			continue
		}
		if i > 0 && f.Features[i-1].Range.Start.Offset > ft.Range.Start.Offset {
			t.Errorf("features out of order at %d: %+v then %+v", i, f.Features[i-1], ft)
		}
		if ft.Range.Start != f.PosAt(ft.Range.Start.Offset) || ft.Range.End != f.PosAt(ft.Range.End.Offset) {
			t.Errorf("feature %+v disagrees with its offsets", ft)
		}
		for _, g := range gaps {
			if g.Start.Offset <= ft.Range.Start.Offset && ft.Range.Start.Offset < g.End.Offset {
				t.Errorf("feature %+v inside the gap %+v", ft, g)
			}
		}
	}
}

func TestResync(t *testing.T) {
	cases := []struct {
		file    string
		funcs   []string
		errLine int
	}{
		{"unclosed_quote.sh", []string{"a", "c"}, 2},
		{"stray_tokens.sh", []string{"a", "c"}, 2},
		{"bad_expansion.sh", []string{"before_exp", "after_exp"}, 3},
	}
	for _, c := range cases {
		f := Parse(c.file, mustRead(t, "../../testdata/broken/"+c.file), Bash)
		if got := funcNames(f); !reflect.DeepEqual(got, c.funcs) {
			t.Errorf("%s funcs %v", c.file, got)
		}
		if len(f.Errors) != 1 || f.Errors[0].Range.Start.Line != c.errLine {
			t.Errorf("%s errors %+v", c.file, f.Errors)
		}
		if n := count(f, tree.KErrorGap); n != 1 {
			t.Errorf("%s gaps %d", c.file, n)
		}
		checkInvariants(t, f)
	}
}

func TestResyncGapShape(t *testing.T) {
	src := mustRead(t, "../../testdata/broken/stray_tokens.sh")
	f := Parse("s.sh", src, Bash)
	var gap *tree.Node
	for _, c := range f.Root.Children {
		if c.Kind == tree.KErrorGap {
			gap = c
		}
	}
	if gap == nil || !gap.Recovered || gap.Parent != f.Root {
		t.Fatalf("gap %+v", gap)
	}
	// The gap covers exactly the broken line 2.
	if gap.Range.Start.Line != 2 || gap.Range.Start.Col != 1 || gap.Range.End.Line != 3 || gap.Range.End.Col != 1 {
		t.Fatalf("gap range %+v", gap.Range)
	}
	if f.Errors[0].Msg == "" {
		t.Fatal("empty message")
	}
}

func TestUnclosedQuoteMessage(t *testing.T) {
	f := Parse("q.sh", mustRead(t, "../../testdata/broken/unclosed_quote.sh"), Bash)
	if len(f.Errors) != 1 || f.Errors[0].Msg != "unterminated quote" {
		t.Fatalf("%+v", f.Errors)
	}
	if f.Errors[0].Range.Start.Line != 2 || f.Errors[0].Range.Start.Col != 6 {
		t.Fatalf("%+v", f.Errors[0].Range)
	}
	// The call `c` after the recovered tail is still lowered.
	if find(f, tree.KFuncDecl, "c") == nil {
		t.Fatal("c missing")
	}
}

func TestResyncPositionsAreFileAbsolute(t *testing.T) {
	f := Parse("b.sh", mustRead(t, "../../testdata/broken/bad_expansion.sh"), Bash)
	d := find(f, tree.KFuncDecl, "after_exp")
	if d == nil {
		t.Fatal("after_exp missing")
	}
	if d.NameRange.Start.Line != 4 || d.NameRange.Start.Col != 1 {
		t.Fatalf("%+v", d.NameRange)
	}
	src := mustRead(t, "../../testdata/broken/bad_expansion.sh")
	if got := string(src[d.NameRange.Start.Offset:d.NameRange.End.Offset]); got != "after_exp" {
		t.Fatalf("name range covers %q", got)
	}
	checkInvariants(t, f)
}

func TestUnclosedBraceKeepsAllFunctions(t *testing.T) { // parser recovers this one itself; must still report it
	f := Parse("u.sh", mustRead(t, "../../testdata/broken/unclosed_brace.sh"), Bash)
	if got := funcNames(f); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatal(got)
	}
	if len(f.Errors) == 0 {
		t.Fatal("no error reported")
	}
	checkInvariants(t, f)
}

func TestNoResyncPointGapsToEOF(t *testing.T) {
	src := "ok() { echo hi; }\nfi\n"
	f := Parse("e.sh", []byte(src), Bash)
	var gap *tree.Node
	for _, c := range f.Root.Children {
		if c.Kind == tree.KErrorGap {
			gap = c
		}
	}
	if gap == nil || gap.Range.Start.Line != 2 || gap.Range.End.Offset != len(src) {
		t.Fatalf("gap %+v", gap)
	}
	checkInvariants(t, f)
}

func TestResyncBudget(t *testing.T) {
	// 80 candidate lines that each fail to parse: only 50 are tried, so a good
	// function after them is not recovered.
	var src []byte
	src = append(src, "a() { echo hi; }\n) ) fi done\n"...)
	for i := 0; i < 80; i++ {
		src = append(src, "echo ${x\n"...)
	}
	src = append(src, "z() { :; }\n"...)
	f := Parse("budget.sh", src, Bash)
	if find(f, tree.KFuncDecl, "z") != nil {
		t.Fatal("budget exceeded: z was recovered")
	}
	if find(f, tree.KFuncDecl, "a") == nil || count(f, tree.KErrorGap) == 0 {
		t.Fatal("a or gap missing")
	}
	checkInvariants(t, f)
}

func TestJunkIsBounded(t *testing.T) {
	start := time.Now()
	f := Parse("junk.sh", mustRead(t, "../../testdata/broken/junk.sh"), Bash)
	if time.Since(start) > time.Second {
		t.Fatal("too slow")
	}
	if len(f.Errors) == 0 || count(f, tree.KErrorGap) == 0 {
		t.Fatal("expected error and gap")
	}
	checkInvariants(t, f)
}

func FuzzParse(f *testing.F) {
	for _, root := range []string{"../../testdata/corpus", "../../testdata/broken"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, rerr := os.ReadFile(p); rerr == nil && len(b) < 1<<16 {
					f.Add(b)
				}
			}
			return nil
		})
	}
	// Small versions of the shapes that once overflowed the stack or ran
	// quadratically (each input must parse within parseBudget) (deep_test.go); the fuzzer mutates them toward the limits.
	for _, seed := range []string{
		"((((((\n", "echo $($($($(\n", "{ { { { \n", "cat <(<(<(\n", `echo "$("$("$(` + "\n",
		"echo $[$[$[\n", "echo $((a?a?a?1:2:3:4))\n", "let a?a?a?1:2:3:4\n", "[[ ! ! ! a ]]\n",
		"a && a || a && a\n", "a | a | a | a\n", "a | # c\na | # c\na\n",
		"if a;then\nif a;then\n:\nfi\nfi\n", "if a; then :\nelif a; then :\nelif a; then :\nfi\n",
		"time time time a\n", "coproc coproc a\n", "f() g() { :; }\n", "f() # c\ng() # c\n{ :; }\n",
		"echo $((1+1+1+1))\n", "echo $((a=b=c=1))\n", "echo $((2**2**2))\n", "echo $((- - !!1))\n",
		"[[ a && a && a ]]\n", "$( '))' $( '))'\n", "$(case x in a) $(case x in a)\n",
		"$( # ))\n$( # ))\n", "$(cat <<'E'\n)))\nE\n$(cat <<E\n$(\nE\n",
		// features: each kind the lowerer records, alone and broken.
		"echo ${v,,} ${v@Q} ${a[-1]} $'x'\ncase x in a) :;& b) :;;& esac\na |& b &>> f &> g <<< h <(c)\n",
		"[[ -n $x ]]\nfunction f { :; }\na=(1 2)\ncoproc cat\nsource x\n", "echo ${v,,\n[[ -n\ncase x in a) :;&\n",
		"a |& b |& c |& d\nf() { echo \"${a[-1]}\"\n) ) fi\n[[ x ]] <<< y\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		var file *tree.File
		parseWithinBudget(t, len(src), func() { file = Parse("f.sh", src, Bash) })
		checkInvariants(t, file)
	})
}

// parseBudget is the time Parse may take on one fuzz input of n bytes.
// Parsing is linear but deep nesting costs about 7 µs a level, and after an
// error resync parses the rest again from several lines (25 µs a byte for 3 KB
// of `(((`, found by FuzzBind); resync's cost is milestone 3's
// (docs/m1-deferred.md). The bound catches what grows faster than that.
func parseBudget(n int) time.Duration {
	b := time.Second + time.Duration(n)*100*time.Microsecond
	if raceBuild {
		b *= 10
	}
	return b
}

// parseWithinBudget runs fn and fails t when the best of three runs takes
// longer than parseBudget (a fuzz worker shares the machine with the others).
func parseWithinBudget(t *testing.T, n int, fn func()) {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		fn()
		if best = min(best, time.Since(start)); best <= parseBudget(n) {
			return
		}
	}
	t.Fatalf("Parse took %v on %d bytes, over its budget of %v", best, n, parseBudget(n))
}
