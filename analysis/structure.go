package analysis

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// Fold is a foldable span of lines, 0-based and inclusive. Kind is "",
// "comment" or "region" (the LSP FoldingRangeKind strings, "" for none). The
// end line of a "" fold is the line of the closing keyword or bracket.
type Fold struct {
	StartLine, EndLine int
	Kind               string
}

// Folds lists the foldable spans of path, outermost first within a start
// line, in start order: function bodies, if/case/loop commands, subshells,
// brace groups and heredocs that span two or more lines, runs of two or
// more whole-line comments, and `# region` / `# endregion` pairs. Nodes the
// parser recovered are left out.
func (s *Snapshot) Folds(path string) []Fold {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	f := s.facts[file].File
	if f == nil || f.Root == nil {
		return nil
	}
	seen := map[Fold]bool{}
	var out []Fold
	add := func(fo Fold) {
		if fo.EndLine > fo.StartLine && !seen[fo] {
			seen[fo] = true
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
			start := n.Range.Start.Line
			if n.Parent != nil && n.Parent.Kind == tree.KRedirect {
				start = n.Parent.Range.Start.Line // the line with the `<<TAG`
			}
			add(Fold{StartLine: start - 1, EndLine: lastLine(n.Range) - 1})
			return true
		default:
			return true
		}
		add(Fold{StartLine: n.Range.Start.Line - 1, EndLine: lastLine(n.Range) - 1})
		return true
	})
	for _, fo := range commentFolds(f) {
		add(fo)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].StartLine != out[b].StartLine {
			return out[a].StartLine < out[b].StartLine
		}
		return out[a].EndLine > out[b].EndLine
	})
	return out
}

// lastLine is the 1-based line holding the last byte of r (an end at column 1
// is exclusive: the line before).
func lastLine(r tree.Range) int {
	if r.End.Col == 1 && r.End.Offset > r.Start.Offset && r.End.Line > r.Start.Line {
		return r.End.Line - 1
	}
	return r.End.Line
}

// marker reports whether text (a comment's text without the '#', trimmed) is
// the marker word, alone or followed by a space.
func marker(text, word string) bool {
	return text == word || strings.HasPrefix(text, word+" ") || strings.HasPrefix(text, word+"\t")
}

// commentFolds finds the runs of whole-line comments and the region pairs.
func commentFolds(f *tree.File) []Fold {
	var out []Fold
	runStart, runEnd := -1, -1 // 0-based lines of the current run
	flush := func() {
		if runStart >= 0 && runEnd > runStart {
			out = append(out, Fold{StartLine: runStart, EndLine: runEnd, Kind: "comment"})
		}
		runStart, runEnd = -1, -1
	}
	var regions []int
	for _, c := range f.Comments {
		if c.Recovered || !wholeLine(f.Src, c.Range.Start.Offset) {
			flush()
			continue
		}
		line := c.Range.Start.Line - 1
		switch {
		case marker(c.Value, "region"):
			flush()
			regions = append(regions, line)
			continue
		case marker(c.Value, "endregion"):
			flush()
			if n := len(regions); n > 0 {
				out = append(out, Fold{StartLine: regions[n-1], EndLine: line, Kind: "region"})
				regions = regions[:n-1]
			}
			continue
		case line == 0 && strings.HasPrefix(c.Value, "!"):
			flush() // the shebang
			continue
		}
		if runStart >= 0 && line == runEnd+1 {
			runEnd = line
		} else {
			flush()
			runStart, runEnd = line, line
		}
	}
	flush()
	return out
}

// wholeLine reports whether only blanks precede offset on its line.
func wholeLine(src []byte, offset int) bool {
	for i := offset - 1; i >= 0; i-- {
		switch src[i] {
		case '\n':
			return true
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return true
}

// maxSelectionDepth is the most levels SelectionChain returns.
const maxSelectionDepth = 128

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
// as a candidate is left out, as are uses whose text is not the name).
func (s *Snapshot) Highlights(path string, offset int) []Highlight {
	h, file, ok := s.at(path, offset)
	if !ok {
		return nil
	}
	byStart := map[int]Highlight{}
	put := func(r tree.Range, write bool) {
		if old, seen := byStart[r.Start.Offset]; seen {
			write = write || old.Write
			if old.Range.End.Offset > r.End.Offset {
				r = old.Range
			}
		}
		byStart[r.Start.Offset] = Highlight{Range: r, Write: write}
	}
	for _, sym := range s.hitSyms(h) {
		for _, d := range sym.Decls {
			if s.declFile[d] == file {
				put(d.NameRange, true)
			}
		}
		for _, b := range s.bySym[sym] {
			if b.File != path || b.Res != Resolved || !b.Use.NamesTarget() {
				continue
			}
			put(b.Use.Range, b.Use.Kind == UseVarWrite)
		}
	}
	out := make([]Highlight, 0, len(byStart))
	for _, hl := range byStart {
		out = append(out, hl)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Range.Start.Offset < out[b].Range.Start.Offset })
	return out
}

// Link is a clickable span of a file. Target is an absolute path.
type Link struct {
	Range   tree.Range
	Target  string
	Tooltip string
}

// Links returns the links of path in source order: each `source` or `.` word
// with exactly one target, each executed workspace file, and the shebang
// interpreter when it is an absolute path to an existing file. A `source` with
// several targets (a glob loader) or none gives no link.
func (s *Snapshot) Links(path string) []Link {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	f := s.facts[file].File
	var out []Link
	if f != nil {
		if l, ok := shebangLink(f); ok {
			out = append(out, l)
		}
	}
	for _, se := range s.edges[file] {
		if se.Status != SourceResolved || len(se.Targets) != 1 {
			continue
		}
		r := se.Ref.Range
		if se.Ref.Word != nil {
			r = se.Ref.Word.Range
		}
		out = append(out, Link{Range: r, Target: se.Targets[0], Tooltip: "source → " + s.rel(se.Targets[0])})
	}
	for k := s.recAt[file]; k < s.recAt[file+1]; k++ {
		if s.recs[k].rel != RelInvokes {
			continue
		}
		e := s.edgeOf(file, &s.recs[k])
		out = append(out, Link{Range: e.Site.Range, Target: e.ToFile, Tooltip: "invokes → " + s.rel(e.ToFile)})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Range.Start.Offset < out[b].Range.Start.Offset })
	return out
}

// shebangLink links the interpreter of a `#!` first line.
func shebangLink(f *tree.File) (Link, bool) {
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
	if st, err := os.Stat(interp); err != nil || st.IsDir() {
		return Link{}, false
	}
	return Link{Range: tree.Range{Start: f.PosAt(start), End: f.PosAt(start + n)}, Target: interp,
		Tooltip: "interpreter → " + filepath.ToSlash(interp)}, true
}
