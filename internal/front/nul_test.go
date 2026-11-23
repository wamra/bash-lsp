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
	for _, s := range []string{"declare A\x00B=1\n", "typeset \x00A\x00=1\n", "local A\x00B\n", "declare -i A=1\x002 B=3\n", "local A=\x001\x00\n", "declare A+=\x00x\n"} {
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
			// declArg value literals (POSIX serves declarations as calls) start at their first byte, not at a NUL the lexer dropped; other literals keep the parser's range
			if d == POSIX && n.Kind == tree.KLit && n.Parent != nil && n.Parent.Kind == tree.KWord && n.Parent.Parent != nil && n.Parent.Parent.Kind == tree.KAssign {
				raw := bytes.ReplaceAll(src[n.Range.Start.Offset:n.Range.End.Offset], []byte{0}, nil)
				if string(raw) != n.Value || (len(n.Value) > 0 && src[n.Range.Start.Offset] == 0) {
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
