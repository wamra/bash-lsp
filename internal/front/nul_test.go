package front

import (
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

// The lexer drops NUL bytes from a literal's value but its positions still
// span them: a name range must hold the name's bytes and nothing else.
func TestNameRangeExcludesNUL(t *testing.T) {
	for _, s := range []string{"((A\x00[0]++", "((\x00A\x00[0]++\n", "echo ${A\x00[0]}\n", "A\x00=1\n", "for \x00i\x00 in a; do :; done\n", "((0[0\x00A[0]++]", "echo ${A\x00B[0]}\n", "for A\x00B in a; do :; done\n"} {
		src := []byte(s)
		f := Parse("/ws/f.sh", src, Bash)
		var walk func(n *tree.Node)
		walk = func(n *tree.Node) {
			if n.Name != "" && n.NameRange != (tree.Range{}) {
				got := string(src[n.NameRange.Start.Offset:n.NameRange.End.Offset])
				if got != n.Name {
					t.Errorf("%q: %v name %q has range text %q", s, n.Kind, n.Name, got)
				}
			}
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(f.Root)
	}
}
