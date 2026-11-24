package analysis

import (
	"slices"

	"github.com/wamra/bash-lsp/tree"
)

// shebangFor is the shebang line a file of dialect d is given.
func shebangFor(d tree.Dialect) (string, bool) {
	switch d {
	case tree.Bash:
		return "#!/usr/bin/env bash\n", true
	case tree.POSIX:
		return "#!/bin/sh\n", true
	case tree.Mksh:
		return "#!/usr/bin/env mksh\n", true
	case tree.Bats:
		return "#!/usr/bin/env bats\n", true
	}
	return "", false
}

// AddShebang is the fix that inserts a shebang line for the dialect of path
// at its start. It is not offered when the first line already starts with
// `#!`, when a workspace file sources path (a library has no interpreter of
// its own), or for a dialect with no shebang text. It is not Safe: it changes
// how the file runs when it is executed.
func (s *Snapshot) AddShebang(path string) (Fix, bool) {
	file, ok := s.idx[path]
	if !ok || s.facts[file] == nil || s.facts[file].File == nil {
		return Fix{}, false
	}
	f := s.facts[file].File
	if len(f.Src) >= 2 && f.Src[0] == '#' && f.Src[1] == '!' {
		return Fix{}, false
	}
	text, ok := shebangFor(f.Dialect)
	if !ok {
		return Fix{}, false
	}
	g := s.adjacency()
	for _, i := range g.in[Node{Path: path}] {
		if g.steps[i].Edge.Rel == RelSources {
			return Fix{}, false
		}
	}
	at := f.PosAt(0)
	return Fix{
		Title: "Add shebang " + text[:len(text)-1],
		Edits: []TextEdit{{Range: tree.Range{Start: at, End: at}, NewText: text}},
	}, true
}

// QuoteExpansion is the fix that wraps in double quotes the parameter
// expansion at offset, when it is a whole argument word of a command: not the
// command word, not part of a larger word, not in `[[ ]]`, arithmetic, an
// assignment value, a `case` word or a redirect target, and not one whose
// quoting changes the meaning (`${#x}`, `${x:+y}`). It is not Safe: a script
// may rely on the word being split.
func (s *Snapshot) QuoteExpansion(path string, offset int) (Fix, bool) {
	file, ok := s.idx[path]
	if !ok || s.facts[file] == nil || s.facts[file].File == nil {
		return Fix{}, false
	}
	f := s.facts[file].File
	n := f.NodeAt(offset)
	if n.Kind == tree.KFile || n.Kind == tree.KStmt || n.Kind == tree.KCall {
		n = f.NodeAt(offset - 1) // the cursor is just past the word
	}
	for n != nil && n.Kind != tree.KParamExp && n.Kind != tree.KWord && n.Kind != tree.KFile {
		n = n.Parent
	}
	if n == nil {
		return Fix{}, false
	}
	var w *tree.Node
	switch n.Kind {
	case tree.KParamExp:
		w = n.Parent
	case tree.KWord: // the cursor is on the word, not inside one of its parts
		w = n
	}
	if w == nil || w.Kind != tree.KWord || len(w.Children) != 1 || w.Children[0].Kind != tree.KParamExp ||
		w.Parent == nil || w.Parent.Kind != tree.KCall {
		return Fix{}, false
	}
	p := w.Children[0]
	words := callWords(w.Parent)
	if len(words) < 2 || words[0] == w || !slices.Contains(words, w) || !quoteCandidate(p, f.Src) {
		return Fix{}, false
	}
	a, b := w.Range.Start.Offset, w.Range.End.Offset
	if a != p.Range.Start.Offset || b != p.Range.End.Offset || offset < a || offset > b {
		return Fix{}, false
	}
	return Fix{
		Title: "Quote the expansion",
		Edits: []TextEdit{{Range: w.Range, NewText: "\"" + string(f.Src[a:b]) + "\""}},
	}, true
}

// FixAll is the edits of the Safe fixes of fixes, in position order. A fix
// whose edits overlap the edits of an earlier fix (two insertions at one point
// count) is dropped whole, so the first of two competing fixes wins and no fix
// is applied in part. nil when no Safe fix is left.
func FixAll(fixes []Fix) []TextEdit {
	var out []TextEdit
	for _, fx := range fixes {
		if !fx.Safe || len(fx.Edits) == 0 {
			continue
		}
		if !disjointFrom(out, fx.Edits) {
			continue
		}
		for _, e := range fx.Edits {
			i, _ := slices.BinarySearchFunc(out, e, byPosition)
			out = slices.Insert(out, i, e)
		}
	}
	return out
}

// disjointFrom reports whether edits overlap neither each other nor any of
// taken, which is sorted by start and has no overlaps.
func disjointFrom(taken, edits []TextEdit) bool {
	for i, e := range edits {
		if overlapsAny(taken, e) {
			return false
		}
		for _, o := range edits[:i] {
			if editsOverlap(o, e) {
				return false
			}
		}
	}
	return true
}

func editsOverlap(a, b TextEdit) bool {
	as, ae, bs, be := a.Range.Start.Offset, a.Range.End.Offset, b.Range.Start.Offset, b.Range.End.Offset
	if as == ae && bs == be { // two insertions: the same point conflicts
		return as == bs
	}
	return as < be && bs < ae
}

func overlapsAny(taken []TextEdit, e TextEdit) bool {
	// taken is disjoint and sorted by start, so of the edits that start before
	// e does only the last can reach into it, and of the others only those at
	// e's own start and the first one after it can begin inside it.
	i, _ := slices.BinarySearchFunc(taken, e, byPosition)
	for j := max(i-1, 0); j < len(taken); j++ {
		if editsOverlap(taken[j], e) {
			return true
		}
		if j >= i && taken[j].Range.Start.Offset > e.Range.Start.Offset {
			break
		}
	}
	return false
}

// byPosition orders edits by start, then by end, so an insertion comes before
// a replacement that starts at the same point.
func byPosition(a, b TextEdit) int {
	if d := a.Range.Start.Offset - b.Range.Start.Offset; d != 0 {
		return d
	}
	return a.Range.End.Offset - b.Range.End.Offset
}
