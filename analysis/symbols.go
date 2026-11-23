package analysis

import (
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// DocSymbol is one entry of a file outline. A function's locals are its Children.
type DocSymbol struct {
	Name      string
	Kind      DeclKind
	Detail    string
	Range     tree.Range
	Selection tree.Range
	Children  []DocSymbol
}

// SymbolInfo is one workspace-wide symbol match. Workspace symbols never list
// locals, so there is no container.
type SymbolInfo struct {
	Name  string
	Kind  DeclKind
	Path  string
	Range tree.Range
}

type symKey struct {
	kind DeclKind
	name string
}

// sortedDecls returns the non-Temp declarations of file in source order.
func (s *Snapshot) sortedDecls(file int) []*Decl {
	var ds []*Decl
	for _, d := range s.facts[file].Decls {
		if !d.Temp {
			ds = append(ds, d)
		}
	}
	sort.SliceStable(ds, func(a, b int) bool { return ds[a].NameRange.Start.Offset < ds[b].NameRange.Start.Offset })
	return ds
}

// atFileLevel reports whether d is declared at file level: outside every
// function and outside every private scope (a subshell variable is not a global).
func atFileLevel(d *Decl) bool {
	return d.Scope == nil || (d.Scope.Func == "" && privateScope(d.Scope) == nil)
}

// DocumentSymbols lists the functions, aliases and file-level variables of
// path in source order; each function definition carries its own locals as
// children. Every function definition is listed (one file may define a
// function once per branch of an `if`); any other name appears once per
// scope, at its first declaration.
func (s *Snapshot) DocumentSymbols(path string) []DocSymbol {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	var out []DocSymbol
	seen := map[symKey]bool{}
	fnAt := map[*tree.Node]int{} // function node -> index in out
	locals := map[*tree.Node]map[string]bool{}
	for _, d := range s.sortedDecls(file) {
		switch {
		case d.Kind == DeclFunction:
			fnAt[d.Node] = len(out)
			out = append(out, DocSymbol{Name: d.Name, Kind: d.Kind, Detail: s.signature(d),
				Range: d.Node.Range, Selection: d.NameRange})
		case atFileLevel(d):
			if seen[symKey{d.Kind, d.Name}] {
				continue
			}
			seen[symKey{d.Kind, d.Name}] = true
			out = append(out, DocSymbol{Name: d.Name, Kind: d.Kind, Detail: declDetail(d),
				Range: d.Node.Range, Selection: d.NameRange})
		}
	}
	for _, d := range s.sortedDecls(file) {
		if d.Kind != DeclVariable || atFileLevel(d) || s.declSym[d] == nil || s.declSym[d].Func == "" {
			continue
		}
		fn := d.Scope.body // the function Bind recorded: no walk up the tree per local (O(declarations x depth))
		if !d.Scope.closed {
			fn = d.Node.Enclosing(tree.KFuncDecl)
		}
		i, ok := fnAt[fn]
		if !ok {
			continue
		}
		if locals[fn] == nil {
			locals[fn] = map[string]bool{}
		}
		if locals[fn][d.Name] {
			continue
		}
		locals[fn][d.Name] = true
		out[i].Children = append(out[i].Children, DocSymbol{Name: d.Name, Kind: d.Kind, Detail: declDetail(d),
			Range: d.Node.Range, Selection: d.NameRange})
	}
	return out
}

func declDetail(d *Decl) string {
	switch d.Kind {
	case DeclAlias:
		if d.AliasTo != "" {
			return "alias of " + d.AliasTo
		}
		return "alias"
	case DeclVariable:
		return strings.Join(attrNames(d.Attrs), ", ")
	}
	return ""
}

// matchRank scores name against query: 0 exact, 1 prefix, 2 substring, 3
// subsequence, -1 no match. Matching is case-insensitive.
func matchRank(name, query string) int {
	n, q := strings.ToLower(name), strings.ToLower(query)
	switch {
	case n == q:
		return 0
	case strings.HasPrefix(n, q):
		return 1
	case strings.Contains(n, q):
		return 2
	}
	qi := 0
	qr := []rune(q)
	for _, r := range n {
		if qi < len(qr) && r == qr[qi] {
			qi++
		}
	}
	if qi == len(qr) {
		return 3
	}
	return -1
}

// WorkspaceSymbols finds functions, aliases and file-level variables by name.
// An empty query lists functions only. limit <= 0 means 200.
func (s *Snapshot) WorkspaceSymbols(query string, limit int) []SymbolInfo {
	if limit <= 0 {
		limit = 200
	}
	type scored struct {
		rank int
		info SymbolInfo
	}
	var all []scored
	for file, p := range s.paths {
		seen := map[symKey]bool{}
		for _, d := range s.sortedDecls(file) {
			if d.Kind != DeclFunction && !atFileLevel(d) {
				continue
			}
			if query == "" && d.Kind != DeclFunction {
				continue
			}
			if seen[symKey{d.Kind, d.Name}] {
				continue
			}
			seen[symKey{d.Kind, d.Name}] = true
			rank := 0
			if query != "" {
				if rank = matchRank(d.Name, query); rank < 0 {
					continue
				}
			}
			all = append(all, scored{rank, SymbolInfo{Name: d.Name, Kind: d.Kind, Path: p, Range: d.NameRange}})
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if x.rank != y.rank {
			return x.rank < y.rank
		}
		if x.info.Path != y.info.Path {
			return x.info.Path < y.info.Path
		}
		return x.info.Range.Start.Offset < y.info.Range.Start.Offset
	})
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]SymbolInfo, len(all))
	for i, a := range all {
		out[i] = a.info
	}
	return out
}
