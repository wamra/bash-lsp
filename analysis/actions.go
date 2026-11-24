package analysis

import (
	"bytes"
	"slices"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

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
	at := 0
	if bytes.HasPrefix(f.Src, utf8BOM) {
		at = len(utf8BOM) // the line goes after the byte order mark
	}
	if bytes.HasPrefix(f.Src[at:], []byte("#!")) {
		return Fix{}, false
	}
	text, ok := shebangFor(f.Dialect)
	if !ok {
		return Fix{}, false
	}
	if i := bytes.IndexByte(f.Src, '\n'); i > 0 && f.Src[i-1] == '\r' {
		text = strings.TrimSuffix(text, "\n") + "\r\n" // the file's own line ending
	}
	g := s.adjacency()
	for _, i := range g.in[Node{Path: path}] {
		if g.steps[i].Edge.Rel == RelSources {
			return Fix{}, false
		}
	}
	pos := f.PosAt(at)
	return Fix{
		Title: "Add shebang " + strings.TrimRight(text, "\r\n"),
		Edits: []TextEdit{{Range: tree.Range{Start: pos, End: pos}, NewText: text}},
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
	pe := *w.Children[0]
	words := callWords(w.Parent)
	if len(words) < 2 || words[0] == w || !slices.Contains(words, w) {
		return Fix{}, false
	}
	a, b := w.Range.Start.Offset, w.Range.End.Offset
	if b > a && b <= len(f.Src) && f.Src[b-1] == '\r' { // a CRLF line ending is not part of the word
		b--
		if pe.Range.End.Offset == b+1 {
			pe.Range.End = f.PosAt(b)
		}
	}
	if a != pe.Range.Start.Offset || b != pe.Range.End.Offset || offset < a || offset > b || !quoteCandidate(&pe, f.Src) {
		return Fix{}, false
	}
	return Fix{
		Title: "Quote the expansion",
		Edits: []TextEdit{{Range: tree.Range{Start: w.Range.Start, End: f.PosAt(b)}, NewText: "\"" + string(f.Src[a:b]) + "\""}},
	}, true
}

// FixAll is the edits of the Safe fixes of fixes, in position order. A fix
// whose edits overlap the edits of an earlier fix (two insertions at one point
// count) is dropped whole, so the first of two competing fixes wins and no fix
// is applied in part. An insertion at the start or end of a replaced range
// does not overlap it. nil when no Safe fix is left.
//
// The offsets of all edits are compressed to ranks; the edits taken so far
// live in Fenwick trees, so each
// edit costs O(log n) and a fix that fails is rolled back by the same moves.
func FixAll(fixes []Fix) []TextEdit {
	var coords []int
	for _, fx := range fixes {
		if fx.Safe {
			for _, e := range fx.Edits {
				coords = append(coords, e.Range.Start.Offset, e.Range.End.Offset)
			}
		}
	}
	if len(coords) == 0 {
		return nil
	}
	slices.Sort(coords)
	coords = slices.Compact(coords)
	rank := func(off int) int { i, _ := slices.BinarySearch(coords, off); return i }
	t := &takenSet{starts: newFenwick(len(coords)), ends: newFenwick(len(coords)), point: newFenwick(len(coords)), at: make([]bool, len(coords))}
	var out []TextEdit
	for _, fx := range fixes {
		if !fx.Safe || len(fx.Edits) == 0 {
			continue
		}
		done := 0
		for _, e := range fx.Edits {
			lo, hi := rank(e.Range.Start.Offset), rank(e.Range.End.Offset)
			if hi < lo || !t.add(lo, hi) {
				break
			}
			done++
		}
		if done < len(fx.Edits) {
			for _, e := range fx.Edits[:done] {
				t.remove(rank(e.Range.Start.Offset), rank(e.Range.End.Offset))
			}
			continue
		}
		out = append(out, fx.Edits...)
	}
	slices.SortStableFunc(out, byPosition)
	return out
}

// byPosition orders edits by start, then by end, so an insertion comes before
// a replacement that starts at the same point.
func byPosition(a, b TextEdit) int {
	if d := a.Range.Start.Offset - b.Range.Start.Offset; d != 0 {
		return d
	}
	return a.Range.End.Offset - b.Range.End.Offset
}

// takenSet is the edits accepted so far, over ranks lo..hi of the compressed
// offsets. The replacements taken are disjoint, so those overlapping the range
// x..y are the ones starting before y minus the ones ending at or before x. An
// insertion occupies one point.
type takenSet struct {
	starts, ends fenwick // the replacements taken, at their start and end ranks
	point        fenwick // the insertions taken, at their rank
	at           []bool  // per rank: an insertion is there
}

// replaced is how many replacements taken overlap the range x..y (x <= y; for
// x == y, those that have the point strictly inside).
func (t *takenSet) replaced(x, y int) int { return t.starts.prefix(y) - t.ends.prefix(x+1) }

// add takes the edit lo..hi and reports false, changing nothing, when it
// overlaps one already taken.
func (t *takenSet) add(lo, hi int) bool {
	if lo == hi {
		if t.at[lo] || t.replaced(lo, lo) > 0 {
			return false
		}
		t.at[lo] = true
		t.point.add(lo, 1)
		return true
	}
	if t.replaced(lo, hi) > 0 || t.point.sum(lo+1, hi) > 0 {
		return false
	}
	t.starts.add(lo, 1)
	t.ends.add(hi, 1)
	return true
}

// remove undoes add for an edit that was taken.
func (t *takenSet) remove(lo, hi int) {
	if lo == hi {
		t.at[lo] = false
		t.point.add(lo, -1)
		return
	}
	t.starts.add(lo, -1)
	t.ends.add(hi, -1)
}

// fenwick is a binary indexed tree of counts.
type fenwick []int

func newFenwick(n int) fenwick { return make(fenwick, n+1) }

func (f fenwick) add(i, d int) {
	for i++; i < len(f); i += i & -i {
		f[i] += d
	}
}

// sum is the total of the entries lo..hi-1.
func (f fenwick) sum(lo, hi int) int { return f.prefix(hi) - f.prefix(lo) }

func (f fenwick) prefix(n int) int {
	s := 0
	for ; n > 0; n -= n & -n {
		s += f[n]
	}
	return s
}
