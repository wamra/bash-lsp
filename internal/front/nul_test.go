package front

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/tree"
)

// The lexer drops NUL bytes from a literal's value but its positions still
// span them: a name range must hold the name's bytes and nothing else.
func TestNameRangeExcludesNUL(t *testing.T) {
	for _, s := range []string{"((A\x00[0]++", "((\x00A\x00[0]++\n", "echo ${A\x00[0]}\n", "A\x00=1\n", "for \x00i\x00 in a; do :; done\n", "((0[0\x00A[0]++]", "echo ${A\x00B[0]}\n", "for A\x00B in a; do :; done\n"} {
		checkNameRanges(t, s, Bash)
	}
	// declCall/declArg serve the dialects whose parser gives declarations as calls
	for _, s := range []string{"declare A\x00B=1\n", "typeset \x00A\x00=1\n", "local A\x00B\n", "declare -i A=1\x002 B=3\n", "local A=\x001\x00\n", "declare A+=\x00x\n", "declare A\x00B=\x001\x002\x00\n"} {
		for _, d := range []Dialect{POSIX, Mksh} {
			checkNameRanges(t, s, d)
		}
	}
}

func checkNameRanges(t *testing.T, s string, d Dialect) {
	t.Helper()
	{
		src := []byte(s)
		f := Parse("/ws/f.sh", src, d)
		var walk func(n *tree.Node)
		walk = func(n *tree.Node) {
			if n.Name != "" && n.NameRange != (tree.Range{}) {
				got := src[n.NameRange.Start.Offset:n.NameRange.End.Offset]
				// a name holding a NUL and a line continuation is the name the
				// shell reads: its range holds it with both removed
				want := got
				if bytes.IndexByte(got, 0) >= 0 && bytes.Contains(got, []byte("\\\n")) {
					want = bytes.ReplaceAll(bytes.ReplaceAll(got, []byte{0}, nil), []byte("\\\n"), nil)
				}
				if string(want) != n.Name {
					t.Errorf("%q (%v): %v name %q has range text %q", s, d, n.Kind, n.Name, got)
				}
			}
			// a value literal starts and ends at a byte of its own, never at a NUL the lexer dropped
			if n.Kind == tree.KLit && n.Parent != nil && n.Parent.Kind == tree.KWord && n.Parent.Parent != nil && n.Parent.Parent.Kind == tree.KAssign {
				raw := bytes.ReplaceAll(src[n.Range.Start.Offset:n.Range.End.Offset], []byte{0}, nil)
				first, last := src[n.Range.Start.Offset], src[n.Range.End.Offset-1]
				if string(raw) != n.Value || (len(n.Value) > 0 && (first == 0 || last == 0)) {
					t.Errorf("%q (%v): literal %q has range text %q", s, d, n.Value, src[n.Range.Start.Offset:n.Range.End.Offset])
				}
			}
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(f.Root)
	}
}

// A NUL in a declaration's name must not cost the value its text: the value
// offset counts the lexer's value, which has no NUL.
func TestDeclValueSurvivesNULInName(t *testing.T) {
	for s, want := range map[string]string{
		"declare A\x00B=1\n":     "1",
		"declare A\x00B=12\n":    "12",
		"local \x00A\x00B+=12\n": "12",
		"typeset A\x00=xyz\n":    "xyz",
	} {
		f := Parse("/ws/f.sh", []byte(s), POSIX)
		var got []string
		var walk func(n *tree.Node)
		walk = func(n *tree.Node) {
			if n.Kind == tree.KLit && n.Parent != nil && n.Parent.Parent != nil && n.Parent.Parent.Kind == tree.KAssign {
				got = append(got, n.Value)
			}
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(f.Root)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%q: value literals %q, want [%q]", s, got, want)
		}
	}
}

// Without NUL bytes in the source a literal's range is the parser's, whatever
// the lexer dropped from its value (here a line continuation).
func TestLiteralRangeWithoutNULIsTheParsers(t *testing.T) {
	for _, c := range []struct {
		src        string
		start, end int
	}{
		{"x=\\\nfoo\n", 2, 7},
		{"local A=\\\n1\n", 8, 11},
		{"declare A=\\\n12 B=3\n", 10, 14},
	} {
		for _, d := range []Dialect{Bash, POSIX, Mksh} {
			f := Parse("/ws/f.sh", []byte(c.src), d)
			var lit *tree.Node
			var walk func(n *tree.Node)
			walk = func(n *tree.Node) {
				if lit == nil && n.Kind == tree.KLit && n.Parent != nil && n.Parent.Kind == tree.KWord && n.Parent.Parent != nil && n.Parent.Parent.Kind == tree.KAssign {
					lit = n
				}
				for _, ch := range n.Children {
					walk(ch)
				}
			}
			walk(f.Root)
			if lit == nil {
				t.Errorf("%q (%v): no value literal", c.src, d)
				continue
			}
			if lit.Range.Start.Offset != c.start || lit.Range.End.Offset != c.end {
				t.Errorf("%q (%v): literal range %d-%d, want %d-%d", c.src, d, lit.Range.Start.Offset, lit.Range.End.Offset, c.start, c.end)
			}
		}
	}
}

// A NUL earlier in an assignment, before a line continuation or between `+`
// and `=` after an index, still moves the value's start: the parser computed
// it from lexer lengths, which count neither (a re-review asked whether a
// boundary byte between the NUL and the value stopped the correction; the
// newline of `\<newline>` did).
func TestValueAfterNULAndContinuationInName(t *testing.T) {
	for _, d := range []Dialect{Bash, POSIX, Mksh} {
		for _, s := range []string{"A\x00=1\n", "A\x00\\\nBCDE=1\n", "A\x00\\\nBCDE=12 B=3\n", "x=1 A\x00\\\nBCDE=1 echo\n", "a[\x001]=x\n", "a[1]\x00=x\n"} {
			checkNameRanges(t, s, d)
		}
	}
	// the parser also counts `+` and `=` after an index with the lexer's lengths
	for _, d := range []Dialect{Bash, Mksh} {
		for _, s := range []string{"A\x00\\\nBCDE+=1\n", "a[1]+\x00=x\n", "a[1]+\x00=\\\nx\n", "a[1]\x00+=x\n"} {
			checkNameRanges(t, s, d)
		}
	}
	for _, s := range []string{"declare A\x00\\\nBCDE=1\n", "local -i A\x00\\\nBCDE+=1\n", "declare a[1]+\x00=x\n"} {
		checkNameRanges(t, s, Bash)
	}
	checkNameRanges(t, "typeset A\x00\\\nBCDE=1\n", Mksh)
	checkNameRanges(t, "typeset a[1]+\x00=x\n", Mksh)
}

// Lowering costs O(1) per node on a NUL-free source and O(log n) with NULs: a
// re-review measured the word-start scan of each literal at 22.6 s for 256 KB
// of `a${b}` and over 400 s for 2 MiB (about 0.2 s before that scan existed).
// Each input is about 2 MiB, the file limit; the bound is generous. Under the
// race detector a quarter of the size and no wall-clock bound.
func TestLongWordWithNULsLowersInBoundedTime(t *testing.T) {
	const bound = 5 * time.Second
	size := 2 << 20
	if raceBuild {
		size /= 4
	}
	unit := strings.Repeat("a${b}", 12) + "aa\x00a" // 64 bytes, one NUL
	for name, src := range map[string]string{
		"no NUL":                 strings.Repeat("a${b}", size/5),
		"NUL at word start":      "\x00" + strings.Repeat("a${b}", size/5-1),
		"NUL on an earlier line": "x\x00y\n" + strings.Repeat("a${b}", size/5-1),
		"NUL every 64 bytes":     strings.Repeat(unit, size/64),
		"a NUL in every part":    strings.Repeat("a\x00${b}", size/6),
		"NUL in a long name":     "A\x00" + strings.Repeat("B", size/2-2) + "=" + strings.Repeat("a${b}", size/10),
	} {
		done := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			Parse("t.sh", []byte(src), Bash)
			done <- time.Since(start)
		}()
		if raceBuild {
			<-done
			continue
		}
		select {
		case d := <-done:
			t.Logf("%s: %d bytes in %v", name, len(src), d.Round(time.Millisecond))
		case <-time.After(bound):
			t.Errorf("%s: %d bytes not lowered within %v", name, len(src), bound)
		}
	}
}

// The index answers what a byte-by-byte scan would.
func TestNULIndexMatchesScan(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	for range 300 {
		src := make([]byte, rnd.IntN(40))
		for i := range src {
			if rnd.IntN(3) == 0 {
				src[i] = 'a'
			}
		}
		z := newNULIndex(src)
		for a := 0; a <= len(src); a++ {
			s := a
			for s < len(src) && src[s] == 0 {
				s++
			}
			e := a
			for e > 0 && src[e-1] == 0 {
				e--
			}
			if z.skip(a) != s || z.back(a) != e {
				t.Fatalf("%q at %d: skip %d back %d, want %d %d", src, a, z.skip(a), z.back(a), s, e)
			}
			for n := 0; n <= len(src)-a+1; n++ {
				i, k := a, n
				for ; k > 0 && i < len(src); i++ {
					if src[i] != 0 {
						k--
					}
				}
				if got := min(z.nth(a, n), len(src)); got != i {
					t.Fatalf("%q: nth(%d, %d) = %d, want %d", src, a, n, got, i)
				}
			}
			for b := a; b <= len(src); b++ {
				if got, want := z.count(a, b), bytes.Count(src[a:b], []byte{0}); got != want {
					t.Fatalf("%q: count(%d, %d) = %d, want %d", src, a, b, got, want)
				}
			}
		}
	}
}

// The per-kind NUL name rules of docs/m2a-acceptance.md, pinned: in an
// assignment, Bash `declare` and mksh `typeset` the parser ends the name at
// the NUL (`A\0B=1` names `A`, range text `A`); a heredoc tag is the shell's
// word, NUL removed (`<<E\0F` names `EF`), with the source bytes as range.
func TestNULNameRules(t *testing.T) {
	for _, c := range []struct {
		d         Dialect
		src       string
		kind      tree.Kind
		name      string
		rangeText string
	}{
		{Bash, "A\x00B=1\n", tree.KAssign, "A", "A"},
		{Bash, "declare A\x00B=1\n", tree.KAssign, "A", "A"},
		{Mksh, "typeset A\x00B=1\n", tree.KAssign, "A", "A"},
		{Bash, "cat <<E\x00F\nx\nEF\n", tree.KHeredoc, "EF", "E\x00F"},
	} {
		f := Parse("/ws/f.sh", []byte(c.src), c.d)
		var found *tree.Node
		f.Root.Walk(func(n *tree.Node) bool {
			if found == nil && n.Kind == c.kind {
				found = n
			}
			return true
		})
		if found == nil {
			t.Errorf("%q (%v): no %v", c.src, c.d, c.kind)
			continue
		}
		got := c.src[found.NameRange.Start.Offset:found.NameRange.End.Offset]
		if found.Name != c.name || got != c.rangeText {
			t.Errorf("%q (%v): %v name %q range text %q, want %q and %q", c.src, c.d, c.kind, found.Name, got, c.name, c.rangeText)
		}
	}
}
