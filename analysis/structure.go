package analysis

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// Fold is a foldable span of text: the byte offset of its first byte and of
// its last byte, so the lines it covers follow from whichever line table the
// caller counts with. Kind is "", "comment" or "region" (the LSP
// FoldingRangeKind strings, "" for none). The last byte of a "" fold is on the
// line of the closing keyword or bracket.
type Fold struct {
	StartOffset, EndOffset int
	Kind                   string
}

// Folds lists the foldable spans of path, in start order and outermost first
// among spans that start at the same byte: function bodies, if/case/loop commands, subshells,
// brace groups and heredocs that span two or more lines, runs of two or
// more whole-line comments, and `# region` / `# endregion` pairs. A span that
// stays on one line, counting "\n", "\r\n" and a lone "\r" as line breaks, is
// left out. Nodes the parser recovered are left out.
func (s *Snapshot) Folds(path string) []Fold {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	f := s.facts[file].File
	if f == nil || f.Root == nil {
		return nil
	}
	type span struct {
		start, end int // 0-based lines
		kind       string
	}
	seen := map[span]bool{}
	var out []Fold
	add := func(fo Fold) {
		k := span{f.LSPLine(fo.StartOffset), f.LSPLine(fo.EndOffset), fo.Kind}
		if k.end > k.start && !seen[k] {
			seen[k] = true
			out = append(out, fo)
		}
	}
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Recovered {
			return false
		}
		switch n.Kind {
		case tree.KFuncDecl, tree.KIf, tree.KWhile, tree.KFor, tree.KCStyleFor, tree.KCase, tree.KSubshell:
		case tree.KBlock:
			// a brace group; the bodies inside if, loop, case and function nodes
			// are covered by their parent
			if n.Parent == nil || n.Parent.Kind != tree.KStmt {
				return true
			}
		case tree.KHeredoc:
			start := n.Range.Start.Offset
			if n.Parent != nil && n.Parent.Kind == tree.KRedirect {
				start = n.Parent.Range.Start.Offset // the line with the `<<TAG`
			}
			add(Fold{StartOffset: start, EndOffset: lastByte(n.Range)})
			return true
		default:
			return true
		}
		add(Fold{StartOffset: n.Range.Start.Offset, EndOffset: lastByte(n.Range)})
		return true
	})
	for _, fo := range commentFolds(f) {
		add(fo)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].StartOffset != out[b].StartOffset {
			return out[a].StartOffset < out[b].StartOffset
		}
		return out[a].EndOffset > out[b].EndOffset
	})
	return out
}

// lastByte is the offset of the last byte of r, or its start when r is empty.
func lastByte(r tree.Range) int {
	return max(r.Start.Offset, r.End.Offset-1)
}

// marker reports whether text (a comment's text without the '#', trimmed) is
// the marker word, alone or followed by a space.
func marker(text, word string) bool {
	return text == word || strings.HasPrefix(text, word+" ") || strings.HasPrefix(text, word+"\t")
}

// commentFolds finds the runs of whole-line comments and the region pairs.
func commentFolds(f *tree.File) []Fold {
	var out []Fold
	// The current run: the offset of its first byte, the offset of the last
	// byte of its last comment and that comment's line.
	runStart, runEnd, runLine := -1, -1, -1
	flush := func() {
		if runStart >= 0 && f.LSPLine(runEnd) > f.LSPLine(runStart) {
			out = append(out, Fold{StartOffset: runStart, EndOffset: runEnd, Kind: "comment"})
		}
		runStart, runEnd, runLine = -1, -1, -1
	}
	var regions []int // offsets of the open `# region` comments
	for _, c := range f.Comments {
		if c.Recovered || !wholeLine(f.Src, c.Range.Start.Offset) {
			flush()
			continue
		}
		line := f.LSPLine(c.Range.Start.Offset)
		last := lastByte(c.Range)
		switch {
		case marker(c.Value, "region"):
			flush()
			regions = append(regions, c.Range.Start.Offset)
			continue
		case marker(c.Value, "endregion"):
			flush()
			if n := len(regions); n > 0 {
				out = append(out, Fold{StartOffset: regions[n-1], EndOffset: last, Kind: "region"})
				regions = regions[:n-1]
			}
			continue
		case line == 0 && strings.HasPrefix(c.Value, "!"):
			flush() // the shebang
			continue
		}
		if runStart >= 0 && line == runLine+1 {
			runEnd, runLine = last, f.LSPLine(last)
		} else {
			flush()
			runStart, runEnd, runLine = c.Range.Start.Offset, last, f.LSPLine(last)
		}
	}
	flush()
	return out
}

// wholeLine reports whether only blanks precede offset on its line, where a
// lone "\r" ends a line too.
func wholeLine(src []byte, offset int) bool {
	for i := offset - 1; i >= 0; i-- {
		switch src[i] {
		case '\n', '\r':
			return true
		case ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// maxSelectionDepth is the most levels SelectionChain returns.
const maxSelectionDepth = 128

// maxHighlights is the most occurrences Highlights returns, and maxLinks the
// most links Links returns.
const (
	maxHighlights = 5000
	maxLinks      = 5000
)

// SelectionChain returns the nested ranges around offset of path, innermost
// first, each strictly containing the one before: the name or word part, the
// word, the command, the statement, the enclosing blocks, the function and the
// file. Equal ranges collapse. When the nesting is deeper than
// maxSelectionDepth the outermost levels are dropped.
func (s *Snapshot) SelectionChain(path string, offset int) []tree.Range {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	f := s.facts[file].File
	if f == nil || f.Root == nil || offset < 0 || offset > len(f.Src) {
		return nil
	}
	var out []tree.Range
	add := func(r tree.Range) {
		if r.End.Offset < r.Start.Offset {
			return
		}
		if n := len(out); n > 0 {
			last := out[n-1]
			if r.Start.Offset > last.Start.Offset || r.End.Offset < last.End.Offset ||
				(r.Start.Offset == last.Start.Offset && r.End.Offset == last.End.Offset) {
				return
			}
		}
		out = append(out, r)
	}
	for n := f.NodeAt(offset); n != nil && len(out) < maxSelectionDepth; n = n.Parent {
		if n.Name != "" && n.NameRange.End.Offset > n.NameRange.Start.Offset && n.NameRange.Contains(offset) {
			add(n.NameRange)
		}
		r := n.Range
		if n.Kind == tree.KStmt {
			r = trimTerminator(f, r)
		}
		add(r)
	}
	if len(out) > maxSelectionDepth {
		out = out[:maxSelectionDepth]
	}
	return out
}

// trimTerminator cuts the `;` and blanks a statement's range ends with, so a
// statement and its command are one level.
func trimTerminator(f *tree.File, r tree.Range) tree.Range {
	end := r.End.Offset
	for end > r.Start.Offset && end <= len(f.Src) && strings.IndexByte(";\n \t\r", f.Src[end-1]) >= 0 {
		end--
	}
	if end != r.End.Offset {
		r.End = f.PosAt(end)
	}
	return r
}

// Highlight is one occurrence of the symbol under the cursor in one file.
// Write is true for a declaration and an assignment; a read or a call is not.
type Highlight struct {
	Range tree.Range
	Write bool
}

// Highlights returns, in source order, the declarations of the symbol under
// the cursor in path and the uses in path that resolve to it (a use bound only
// as a candidate is left out, as are uses whose text is not the name). At most
// maxHighlights come back, the one under the cursor among them; cut is true
// when there were more.
func (s *Snapshot) Highlights(path string, offset int) (out []Highlight, cut bool) {
	h, file, ok := s.at(path, offset)
	if !ok {
		return nil, false
	}
	byStart := map[int]Highlight{}
	put := func(r tree.Range, write bool) bool {
		old, seen := byStart[r.Start.Offset]
		if !seen && len(byStart) >= maxHighlights {
			return false
		}
		if seen {
			write = write || old.Write
			if old.Range.End.Offset > r.End.Offset {
				r = old.Range
			}
		}
		byStart[r.Start.Offset] = Highlight{Range: r, Write: write}
		return true
	}
	// the occurrence under the cursor goes in first, so the cap cannot drop it
	if h.decl != nil {
		put(h.decl.NameRange, true)
	}
	for _, b := range h.binds {
		if b.File == path && b.Res == Resolved && b.Use.NamesTarget() {
			put(b.Use.Range, b.Use.Kind == UseVarWrite)
		}
	}
syms:
	for _, sym := range s.hitSyms(h) {
		for _, d := range sym.Decls {
			if s.declFile[d] == file && !put(d.NameRange, true) {
				cut = true
				break syms
			}
		}
		for _, b := range s.bySym[sym] {
			if b.File != path || b.Res != Resolved || !b.Use.NamesTarget() {
				continue
			}
			if !put(b.Use.Range, b.Use.Kind == UseVarWrite) {
				cut = true
				break syms
			}
		}
	}
	out = make([]Highlight, 0, len(byStart))
	for _, hl := range byStart {
		out = append(out, hl)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Range.Start.Offset < out[b].Range.Start.Offset })
	return out, cut
}

// Link is a clickable span of a file. Target is an absolute path.
type Link struct {
	Range   tree.Range
	Target  string
	Tooltip string
}

// Links returns the links of path in source order: each `source` or `.` word
// with exactly one target and each executed workspace file. A `source` with
// several targets (a glob loader) or none gives no link. The shebang is not
// here: Shebang names it. At most maxLinks come back, the first in source
// order; cut is true when there were more.
func (s *Snapshot) Links(path string) (out []Link, cut bool) {
	file, ok := s.idx[path]
	if !ok {
		return nil, false
	}
	for _, se := range s.edges[file] {
		if se.Status != SourceResolved || len(se.Targets) != 1 {
			continue
		}
		if len(out) >= maxLinks {
			cut = true
			break
		}
		r := se.Ref.Range
		if se.Ref.Word != nil {
			r = se.Ref.Word.Range
		}
		out = append(out, Link{Range: r, Target: se.Targets[0], Tooltip: "source → " + s.rel(se.Targets[0])})
	}
	invokes := 0
	for k := s.recAt[file]; k < s.recAt[file+1]; k++ {
		if s.recs[k].rel != RelInvokes {
			continue
		}
		if invokes++; invokes > maxLinks {
			cut = true
			break
		}
		e := s.edgeOf(file, &s.recs[k])
		out = append(out, Link{Range: e.Site.Range, Target: e.ToFile, Tooltip: "invokes → " + s.rel(e.ToFile)})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Range.Start.Offset < out[b].Range.Start.Offset })
	if len(out) > maxLinks {
		out, cut = out[:maxLinks], true
	}
	return out, cut
}

// Shebang returns the interpreter of the `#!` first line of path as a link
// candidate: its range, the absolute path text as Target, and the tooltip.
// ok is false when there is no shebang or the interpreter is not an absolute
// path (`#!/usr/bin/env bash` names `/usr/bin/env` only). It does not look at
// the file system: the caller checks that Target exists.
func (s *Snapshot) Shebang(path string) (Link, bool) {
	file, ok := s.idx[path]
	if !ok {
		return Link{}, false
	}
	f := s.facts[file].File
	if f == nil {
		return Link{}, false
	}
	src := f.Src
	if len(src) < 3 || src[0] != '#' || src[1] != '!' {
		return Link{}, false
	}
	end := len(src)
	for i, c := range src {
		if c == '\n' {
			end = i
			break
		}
	}
	line := string(src[2:end])
	start := 2 + len(line) - len(strings.TrimLeft(line, " \t"))
	rest := string(src[start:end])
	n := strings.IndexAny(rest, " \t\r")
	if n < 0 {
		n = len(rest)
	}
	interp := rest[:n]
	if interp == "" || !filepath.IsAbs(interp) {
		return Link{}, false
	}
	return Link{Range: tree.Range{Start: f.PosAt(start), End: f.PosAt(start + n)}, Target: interp,
		Tooltip: "interpreter → " + filepath.ToSlash(interp)}, true
}
