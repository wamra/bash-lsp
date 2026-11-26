package analysis

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// allocated reports the bytes fn allocates. Unlike time, it does not depend
// on the machine or the race detector, so two sizes can be compared tightly.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// docAbove prepended one line at a time, so a declaration under a
// block of n comment lines cost O(n^2) (400 KB: 40 s). Binding four times the
// lines must cost about four times the bytes, not sixteen.
func TestDocAboveIsLinear(t *testing.T) {
	src := func(n int) *tree.File {
		b := []byte(strings.Repeat("# a comment line\n", n) + "f() { :; }\n")
		return front.Parse("t.sh", b, front.Bash)
	}
	small, large := src(5000), src(20000)
	a := allocated(func() { Bind(small) })
	b := allocated(func() { Bind(large) })
	if b > 8*a {
		t.Fatalf("Bind allocated %d bytes for 5,000 comment lines and %d for 20,000 (%.1fx)", a, b, float64(b)/float64(a))
	}
}

// The doc of a declaration is the comment block above it, cut to the
// maxDocLines lines and maxDocBytes bytes nearest the declaration.
func TestDocAboveIsCapped(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&sb, "# line %d\n", i)
	}
	sb.WriteString("f() { :; }\n")
	d := decl(bindSrc(t, sb.String()), DeclFunction, "f")
	lines := strings.Split(d.Doc, "\n")
	if len(lines) != maxDocLines || lines[0] != fmt.Sprintf("line %d", 100000-maxDocLines) || lines[len(lines)-1] != "line 99999" {
		t.Fatalf("%d lines: first %q, last %q", len(lines), lines[0], lines[len(lines)-1])
	}
	long := "# " + strings.Repeat("é", 300000) + "\ng() { :; }\n"
	d = decl(bindSrc(t, long), DeclFunction, "g")
	if len(d.Doc) > maxDocBytes || len(d.Doc) < maxDocBytes-4 || !utf8.ValidString(d.Doc) {
		t.Fatalf("doc of a 600 KB comment line: %d bytes, valid UTF-8 %v", len(d.Doc), utf8.ValidString(d.Doc))
	}
}
