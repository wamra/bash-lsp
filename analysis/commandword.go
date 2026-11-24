package analysis

import "github.com/wamra/bash-lsp/tree"

// CommandWordAt returns the literal command name of the simple command whose
// first word is under offset (or ends at it, as for the other position
// queries), with the range of that word. It says nothing about what the name
// refers to: a function, an alias, a builtin and a program on the PATH all
// answer, and a word that is not a command name (an argument, an expansion, a
// name with a dynamic part) does not.
func (s *Snapshot) CommandWordAt(path string, offset int) (name string, rng tree.Range, ok bool) {
	file, found := s.idx[path]
	if !found || s.facts[file].File == nil || s.facts[file].File.Root == nil {
		return "", tree.Range{}, false
	}
	root := s.facts[file].File.Root
	for _, o := range []int{offset, offset - 1} {
		if o < 0 {
			continue
		}
		if call := callAt(root, o); call != nil {
			for _, c := range call.Children {
				if c.Kind != tree.KWord {
					continue
				}
				if n, lit, _ := commandName(c); lit && n != "" && c.Range.Contains(o) {
					return n, c.Range, true
				}
				break // only the first word names the command
			}
		}
	}
	return "", tree.Range{}, false
}

// callAt finds the innermost KCall that holds offset, descending only into
// nodes whose range holds it.
func callAt(n *tree.Node, offset int) *tree.Node {
	if n == nil || n.Kind != tree.KFile && !n.Range.Contains(offset) {
		return nil
	}
	for _, c := range n.Children {
		if r := callAt(c, offset); r != nil {
			return r
		}
	}
	if n.Kind == tree.KCall {
		return n
	}
	return nil
}
