package analysis

import (
	"slices"
	"sort"

	"github.com/wamra/bash-lsp/tree"
)

// TokenType is a semantic token type, an index into the legend's type list.
type TokenType uint8

const (
	TokFunction TokenType = iota
	TokVariable
	TokParameter
	TokMacro
)

// TokenMod is a set of semantic token modifiers, a bit per index of the
// legend's modifier list.
type TokenMod uint16

const (
	ModDeclaration TokenMod = 1 << iota
	ModReadonly
	ModModification
	ModDefaultLibrary
	ModLocal
	ModExported
	ModArray
	ModAssociative
	ModInteger
	ModNameref
	ModEnvironment
	ModExternal
	ModUnresolved
)

// Token is one highlighted name. Range lies on one line and is not empty.
type Token struct {
	Range tree.Range
	Type  TokenType
	Mods  TokenMod
}

// attrMods are the modifiers a variable's declarations give it, from every
// declaration of its symbol.
func attrMods(sym *Symbol) TokenMod {
	var m TokenMod
	for _, d := range sym.Decls {
		a := d.Attrs
		if a.Local {
			m |= ModLocal
		}
		if a.Exported {
			m |= ModExported
		}
		if a.Readonly {
			m |= ModReadonly
		}
		if a.Integer {
			m |= ModInteger
		}
		if a.Array {
			m |= ModArray
		}
		if a.Assoc {
			m |= ModAssociative
		}
		if a.Nameref {
			m |= ModNameref
		}
	}
	return m
}

// plainAssignment reports whether d is `name=value` written on its own, not
// a declaring command (`local`, `declare`, `read`, `for`, ...).
func plainAssignment(d *Decl) bool {
	n := d.Node
	return d.Implicit == "" && n != nil && n.Kind == tree.KAssign && (n.Parent == nil || n.Parent.Kind != tree.KDecl)
}

// isPositional reports whether a parameter expansion of this name reads a
// positional parameter or one of the argument lists: `$1`..`$9` (and `${10}`
// and up), `$@`, `$*` and `$#`. `$0` is the script's name, not an argument.
func isPositional(name string) bool {
	switch name {
	case "@", "*", "#":
		return true
	case "", "0":
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return name[0] != '0'
}

// Tokens returns the semantic tokens of file path, sorted by position and
// not overlapping, each on one line: declared and called functions, aliases
// (macro), builtins, external and unresolved commands, variables and the
// positional parameters. Within, when not nil, keeps the tokens that touch
// it. Names that sit inside a string (a call in `eval "f"`, a `"$cmd"` call)
// have no exact range and get no token. A path the snapshot does not hold has no tokens.
func (s *Snapshot) Tokens(path string, within *tree.Range) []Token {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	ff := s.facts[file]
	if ff == nil || ff.File == nil {
		return nil
	}
	lo, hi := 0, len(ff.File.Src)
	if within != nil {
		lo, hi = within.Start.Offset, within.End.Offset
	}
	var toks []Token
	add := func(r tree.Range, t TokenType, m TokenMod) {
		if r.Start.Line != r.End.Line || r.End.Offset <= r.Start.Offset || r.End.Offset <= lo || r.Start.Offset >= hi {
			return
		}
		toks = append(toks, Token{r, t, m})
	}
	attrs := map[*Symbol]TokenMod{}
	modsOf := func(sym *Symbol) TokenMod {
		m, ok := attrs[sym]
		if !ok {
			m = attrMods(sym)
			attrs[sym] = m
		}
		return m
	}

	// Declarations come first so that, where a declaration and a use of its
	// name start at the same place, the declaration is the token. The first
	// declaration of a name in a scope is a declaration; a later plain
	// assignment to it in the same scope is a modification.
	type scopeName struct {
		sc   *Scope
		name string
	}
	var seen map[scopeName]bool
	for _, d := range ff.Decls {
		sym := s.declSym[d]
		if sym == nil {
			continue
		}
		switch d.Kind {
		case DeclFunction:
			add(d.NameRange, TokFunction, ModDeclaration)
		case DeclAlias:
			add(d.NameRange, TokMacro, ModDeclaration)
		case DeclVariable:
			m := modsOf(sym)
			if d.Temp { // lives for one command only
				add(d.NameRange, TokVariable, ModDeclaration|m)
				break
			}
			if seen == nil {
				seen = map[scopeName]bool{}
			}
			k := scopeName{d.Scope, d.Name}
			if seen[k] && plainAssignment(d) {
				m |= ModModification
			} else {
				m |= ModDeclaration
			}
			seen[k] = true
			add(d.NameRange, TokVariable, m)
		}
	}

	binds := s.bindings[file]
	var cmds []cmdSite
	if s.cmds != nil {
		cmds = s.cmds[file]
	}
	ci := 0
	for j, u := range ff.Uses {
		for ci < len(cmds) && int(cmds[ci].use) < j {
			ci++
		}
		if u.Range.End.Offset <= lo || u.Range.Start.Offset >= hi || u.Via != ViaNone || !u.NamesTarget() {
			continue
		}
		b := s.bindingOf(binds, u)
		switch u.Kind {
		case UseCall:
			switch {
			case b != nil && (b.Sym.Kind == DeclAlias || b.Reason == ReasonAlias):
				add(u.Range, TokMacro, 0)
			case b != nil:
				add(u.Range, TokFunction, 0)
			case IsBuiltin(u.Name):
				add(u.Range, TokFunction, ModDefaultLibrary)
			case ci < len(cmds) && int(cmds[ci].use) == j:
				if s.cmdPaths[cmds[ci].tab] != "" {
					add(u.Range, TokFunction, ModExternal)
				} else {
					add(u.Range, TokFunction, ModUnresolved)
				}
			default:
				add(u.Range, TokFunction, 0)
			}
		case UseVarRead, UseVarWrite:
			if b == nil {
				continue
			}
			m := modsOf(b.Sym)
			if b.Res == Unresolved && b.Reason == ReasonEnvironment {
				m |= ModEnvironment
			}
			if u.Kind == UseVarWrite {
				m |= ModModification // a write that is a declaration loses this to its declaration token
			}
			add(u.Range, TokVariable, m)
		}
	}

	// Positional parameters have no use of their own.
	ff.File.Root.Walk(func(n *tree.Node) bool {
		if n.Range.End.Offset <= lo && n.Range.End.Offset != n.Range.Start.Offset || n.Range.Start.Offset >= hi {
			return false
		}
		if n.Kind == tree.KParamExp && isPositional(n.Name) {
			if o := n.Range.Start.Offset; o >= 0 && o < len(ff.File.Src) && ff.File.Src[o] == '$' {
				add(n.NameRange, TokParameter, 0)
			}
		}
		return true
	})

	byStart := func(a, b Token) int { return a.Range.Start.Offset - b.Range.Start.Offset }
	if !slices.IsSortedFunc(toks, byStart) {
		slices.SortStableFunc(toks, byStart)
	}
	out := toks[:0]
	end := -1
	for _, t := range toks {
		if t.Range.Start.Offset < end {
			// The same place, or inside the token before. A read and a write
			// of one name (`(( n++ ))`) are one token that is both.
			if k := len(out) - 1; t.Type == TokVariable && t.Range.Start == out[k].Range.Start && out[k].Type == TokVariable && out[k].Mods&ModDeclaration == 0 {
				out[k].Mods |= t.Mods & ModModification
			}
			continue
		}
		out = append(out, t)
		end = t.Range.End.Offset
	}
	return out
}

// bindingOf is the first binding of use u among binds, which are in use
// offset order; nil when the use has none.
func (s *Snapshot) bindingOf(binds []Binding, u *Use) *Binding {
	off := u.Range.Start.Offset
	i := sort.Search(len(binds), func(i int) bool { return binds[i].Use.Range.Start.Offset >= off })
	for ; i < len(binds) && binds[i].Use.Range.Start.Offset == off; i++ {
		if binds[i].Use == u {
			return &binds[i]
		}
	}
	return nil
}
