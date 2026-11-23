package analysis

import (
	"sort"

	"github.com/wamra/bash-lsp/tree"
)

// hit is what lies under a cursor.
type hit struct {
	decl  *Decl
	binds []Binding // every binding of the use under the cursor
	edge  *SourceEdge
}

// at finds what is under offset; when nothing is, it retries one byte to the
// left so a cursor placed just after a name still finds it.
func (s *Snapshot) at(path string, offset int) (h hit, file int, ok bool) {
	file, ok = s.idx[path]
	if !ok {
		return hit{}, 0, false
	}
	for _, o := range []int{offset, offset - 1} {
		if o < 0 {
			continue
		}
		if h = s.hitAt(file, o); h.decl != nil || len(h.binds) > 0 || h.edge != nil {
			return h, file, true
		}
	}
	return hit{}, file, true
}

func (s *Snapshot) hitAt(file, offset int) hit {
	for _, d := range s.facts[file].Decls {
		if d.NameRange.Contains(offset) && s.declSym[d] != nil {
			return hit{decl: d}
		}
	}
	var h hit
	for _, b := range s.bindings[file] {
		if b.Use.Range.Contains(offset) && (h.binds == nil || h.binds[0].Use == b.Use) {
			h.binds = append(h.binds, b)
		}
	}
	if h.binds != nil {
		return h
	}
	for i := range s.edges[file] {
		e := &s.edges[file][i]
		if e.Ref.Word != nil && e.Ref.Word.Range.Contains(offset) {
			return hit{edge: e}
		}
	}
	return h
}

// SymbolAt returns the symbol under the cursor: a declaration name or a bound use.
func (s *Snapshot) SymbolAt(path string, offset int) (*Symbol, bool) {
	h, _, _ := s.at(path, offset)
	if syms := s.hitSyms(h); len(syms) > 0 {
		return syms[0], true
	}
	return nil, false
}

func (s *Snapshot) hitSyms(h hit) []*Symbol {
	if h.decl != nil {
		return []*Symbol{s.declSym[h.decl]}
	}
	var out []*Symbol
	for _, b := range h.binds {
		out = append(out, b.Sym)
	}
	return out
}

func (s *Snapshot) declLoc(d *Decl) Location {
	return Location{Path: s.paths[s.declFile[d]], Range: d.NameRange}
}

// locSet collects locations without duplicates and sorts them.
type locSet struct {
	seen map[[2]interface{}]bool
	out  []Location
}

func (l *locSet) add(loc Location) {
	k := [2]interface{}{loc.Path, loc.Range.Start.Offset}
	if l.seen == nil {
		l.seen = map[[2]interface{}]bool{}
	}
	if !l.seen[k] {
		l.seen[k] = true
		l.out = append(l.out, loc)
	}
}

func (l *locSet) sorted() []Location {
	sort.SliceStable(l.out, func(a, b int) bool {
		if l.out[a].Path != l.out[b].Path {
			return l.out[a].Path < l.out[b].Path
		}
		return l.out[a].Range.Start.Offset < l.out[b].Range.Start.Offset
	})
	return l.out
}

// Definition returns where the name under the cursor is declared. On a
// source path it returns the target files at 1:1; on a declaration name, that declaration.
func (s *Snapshot) Definition(path string, offset int) []Location {
	h, _, ok := s.at(path, offset)
	if !ok {
		return nil
	}
	var ls locSet
	switch {
	case h.edge != nil:
		if h.edge.Status == SourceResolved {
			top := tree.Pos{Offset: 0, Line: 1, Col: 1}
			for _, t := range h.edge.Targets {
				ls.add(Location{Path: t, Range: tree.Range{Start: top, End: top}})
			}
		}
	case h.decl != nil:
		ls.add(s.declLoc(h.decl))
	default:
		for _, sym := range s.hitSyms(h) {
			for _, d := range sym.Decls {
				ls.add(s.declLoc(d))
			}
		}
	}
	return ls.sorted()
}

// References returns every use bound to the symbol under the cursor, plus its
// declarations when includeDecl. Without includeDecl, the write that is a
// declaration's own name is left out.
func (s *Snapshot) References(path string, offset int, includeDecl bool) []Location {
	h, _, ok := s.at(path, offset)
	if !ok {
		return nil
	}
	var ls locSet
	for _, sym := range s.hitSyms(h) {
		declAt := map[[2]interface{}]bool{}
		for _, d := range sym.Decls {
			declAt[[2]interface{}{s.paths[s.declFile[d]], d.NameRange.Start.Offset}] = true
			if includeDecl {
				ls.add(s.declLoc(d))
			}
		}
		for _, b := range s.BindingsOf(sym) {
			if !includeDecl && declAt[[2]interface{}{b.File, b.Use.Range.Start.Offset}] {
				continue
			}
			ls.add(Location{Path: b.File, Range: b.Use.Range})
		}
	}
	return ls.sorted()
}

// Implementation returns every declaration of the function under the cursor.
func (s *Snapshot) Implementation(path string, offset int) []Location {
	h, _, ok := s.at(path, offset)
	if !ok {
		return nil
	}
	var ls locSet
	for _, sym := range s.hitSyms(h) {
		if sym.Kind != DeclFunction {
			continue
		}
		for _, d := range sym.Decls {
			ls.add(s.declLoc(d))
		}
	}
	return ls.sorted()
}
