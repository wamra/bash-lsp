package front

import (
	"bytes"
	"testing"

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
				got := string(src[n.NameRange.Start.Offset:n.NameRange.End.Offset])
				if got != n.Name {
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
