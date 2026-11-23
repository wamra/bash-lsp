package front

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// depthShapes wrap a statement in n levels of one construct.
var depthShapes = map[string]func(n int) string{
	"{ }": func(n int) string { return strings.Repeat("{ ", n) + ":" + strings.Repeat("; }", n) + "\n" },
	"( )": func(n int) string { return strings.Repeat("( ", n) + ":" + strings.Repeat(" )", n) + "\n" },
	"if":  func(n int) string { return strings.Repeat("if a; then ", n) + ":" + strings.Repeat("; fi", n) + "\n" },
	"while": func(n int) string {
		return strings.Repeat("while a; do ", n) + ":" + strings.Repeat("; done", n) + "\n"
	},
	"for": func(n int) string {
		return strings.Repeat("for x in a; do ", n) + ":" + strings.Repeat("; done", n) + "\n"
	},
	"case": func(n int) string {
		return strings.Repeat("case a in a) ", n) + ":" + strings.Repeat(";; esac", n) + "\n"
	},
	"f() { }":     func(n int) string { return strings.Repeat("f() { ", n) + ":" + strings.Repeat("; }", n) + "\n" },
	"elif (flat)": func(n int) string { return "if a; then :\n" + strings.Repeat("elif a; then :\n", n) + "fi\n" },
	"$( )":        func(n int) string { return "echo " + strings.Repeat("$(", n) + "a" + strings.Repeat(")", n) + "\n" },
	`"$( )"`:      func(n int) string { return "echo " + strings.Repeat(`"$(`, n) + "a" + strings.Repeat(`)"`, n) + "\n" },
	"<( )":        func(n int) string { return "cat " + strings.Repeat("<(", n) + "a" + strings.Repeat(")", n) + "\n" },
	"${x:-}":      func(n int) string { return "echo " + strings.Repeat("${x:-", n) + "a" + strings.Repeat("}", n) + "\n" },
	"$( ) in ( )": func(n int) string { return strings.Repeat("( echo $(", n) + "a" + strings.Repeat(") )", n) + "\n" },
	"if in f() { }": func(n int) string {
		return strings.Repeat("f() { if a; then ", n) + ":" + strings.Repeat("; fi; }", n) + "\n"
	},
	"pipeline of ( )": func(n int) string { return strings.Repeat("a | ( ", n) + ":" + strings.Repeat(" )", n) + "\n" },
}

// firstRefused is the smallest n at which the lowerer drops the statement;
// 0 with parserFirst when the parser's own limit refuses the input first.
func firstRefused(shape func(int) string) (n int, parserFirst bool) {
	refused := func(n int) (lowerer, parser bool) {
		f := Parse("x.sh", []byte(shape(n)), Bash)
		return hasErr(f, loweringLimitError), f.Skipped != "" || hasErr(f, nestingError)
	}
	lo, hi := 0, 64 // not refused at lo, refused at hi
	for {
		l, p := refused(hi)
		if l {
			break
		}
		if p || hi > 1<<16 {
			return 0, true
		}
		lo, hi = hi, hi*2
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if l, _ := refused(mid); l {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, false
}

// The lowerer's limit counts lowering levels, not what a user sees as nesting:
// every statement, word, word part and if or elif clause inside another is a
// level. These are the first depths each construct is refused at
// (bench/results/lowerer-depth-m2a-final.txt is this test's -v output), the
// figures docs/library.md and docs/m2a-acceptance.md state.
func TestLowererLimitPerConstruct(t *testing.T) {
	want := map[string]int{ // 0: the parser's own limit refuses it first
		"{ }": 3998, "( )": 3998, "while": 3998, "for": 3998, "case": 3998, "elif (flat)": 3996,
		"if": 1999, "f() { }": 1999, "${x:-}": 1999, "pipeline of ( )": 1999,
		"$( )": 1333, "<( )": 1333, `"$( )"`: 1000, "if in f() { }": 1000, "$( ) in ( )": 0,
	}
	names := make([]string, 0, len(depthShapes))
	for name := range depthShapes {
		names = append(names, name)
	}
	sort.Strings(names)
	var sb strings.Builder
	fmt.Fprintf(&sb, "lowerer limit %d levels; first refused depth per construct:\n", maxDepth)
	for _, name := range names {
		n, parserFirst := firstRefused(depthShapes[name])
		if parserFirst {
			fmt.Fprintf(&sb, "  %-16s refused by the parser's limit first\n", name)
		} else {
			fmt.Fprintf(&sb, "  %-16s %5d levels (%.2f lowering levels each)\n", name, n, float64(maxDepth)/float64(n))
		}
		if n != want[name] {
			t.Errorf("%s: first refused at %d levels, want %d", name, n, want[name])
		}
	}
	t.Log("\n" + sb.String())
}

// review I2b: the error said "nested deeper than 4000 levels", which an `if`
// ladder of 2,000 or a `"$(` chain of 1,000 meets: it says what it counts.
func TestLoweringLimitErrorSaysWhatItCounts(t *testing.T) {
	f := Parse("x.sh", []byte(depthShapes["if"](2000)), Bash)
	const want = "statement not analysed: nested more than 4000 levels deep, counting each statement, word, word part (expansion, quote or literal) and if or elif clause inside another"
	if !errorWith(f.Errors, want) {
		t.Fatalf("errors %v, want %q", f.Errors, want)
	}
}
