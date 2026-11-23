package analysis

import (
	"path/filepath"
	"sort"

	"github.com/wamra/bash-lsp/tree"
)

// Resolution says how sure a Binding is.
type Resolution uint8

const (
	Resolved Resolution = iota + 1
	Candidate
	Dynamic
	Unresolved
)

// Symbol is one named thing: a function, an alias or a variable.
type Symbol struct {
	Kind  DeclKind
	Name  string
	Decls []*Decl // every declaration that is this symbol, in (path, offset) order
	Func  string  // for a local variable: its function; "" otherwise
	File  string  // for a local variable: its file; "" for globals and functions
}

// Binding ties one Use to the symbol it names.
type Binding struct {
	Use    *Use
	File   string
	Sym    *Symbol
	Res    Resolution
	Reason string
}

// Location is a range in a file.
type Location struct {
	Path  string
	Range tree.Range
}

// Options configure Build. SearchPaths may be nil.
type Options struct {
	Root        string
	SearchPaths func(path string) []string
}

// maxAliasDepth bounds alias chains.
const maxAliasDepth = 8

// bashWords are the bash builtins and reserved words: a call to one of them
// with no function of that name in view is not a missing definition.
var bashWords = map[string]bool{}

func init() {
	for _, w := range []string{
		// builtins
		".", ":", "[", "alias", "bg", "bind", "break", "builtin", "caller", "cd", "command", "compgen",
		"complete", "compopt", "continue", "declare", "dirs", "disown", "echo", "enable", "eval", "exec",
		"exit", "export", "false", "fc", "fg", "getopts", "hash", "help", "history", "jobs", "kill", "let",
		"local", "logout", "mapfile", "popd", "printf", "pushd", "pwd", "read", "readarray", "readonly",
		"return", "set", "shift", "shopt", "source", "suspend", "test", "times", "trap", "true", "type",
		"typeset", "ulimit", "umask", "unalias", "unset", "wait",
		// reserved words
		"!", "case", "coproc", "do", "done", "elif", "else", "esac", "fi", "for", "function", "if", "in",
		"select", "then", "time", "until", "while", "{", "}", "[[", "]]",
	} {
		bashWords[w] = true
	}
}

type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)/64) }
func (b bitset) set(i int)      { b[i>>6] |= 1 << (uint(i) & 63) }
func (b bitset) has(i int) bool { return b[i>>6]&(1<<(uint(i)&63)) != 0 }
func (b bitset) or(o bitset) {
	for i := range b {
		b[i] |= o[i]
	}
}
func (b bitset) each(fn func(i int)) {
	for w, v := range b {
		for v != 0 {
			bit := 0
			for v&(1<<uint(bit)) == 0 {
				bit++
			}
			fn(w<<6 | bit)
			v &^= 1 << uint(bit)
		}
	}
}

type declEntry struct {
	file int
	d    *Decl
}

type localKey struct {
	file     int
	fn, name string
}

type posKey struct{ file, offset int }

// Snapshot is an immutable analysed workspace: facts, the source graph,
// visibility and every name binding.
type Snapshot struct {
	gen        uint64
	paths      []string
	idx        map[string]int
	facts      []*FileFacts
	edges      [][]SourceEdge
	visible    []bitset
	incomplete []bool
	sourced    []bool

	funcSyms, aliasSyms, varSyms map[string][]*Symbol
	localSyms                    map[localKey]*Symbol
	envSyms                      map[string]*Symbol
	tempSyms                     map[posKey]*Symbol
	locals                       []map[string]map[string]bool // file -> function -> names declared local
	declSym                      map[*Decl]*Symbol
	declFile                     map[*Decl]int

	bindings [][]Binding // per file, in use-offset order
	bySym    map[*Symbol][]Binding
}

// Build analyses files (keyed by absolute path) into a Snapshot.
func Build(files map[string]*FileFacts, opt Options, generation uint64) *Snapshot {
	s := &Snapshot{gen: generation, idx: map[string]int{},
		funcSyms: map[string][]*Symbol{}, aliasSyms: map[string][]*Symbol{}, varSyms: map[string][]*Symbol{},
		localSyms: map[localKey]*Symbol{}, envSyms: map[string]*Symbol{}, tempSyms: map[posKey]*Symbol{},
		declSym: map[*Decl]*Symbol{}, declFile: map[*Decl]int{}, bySym: map[*Symbol][]Binding{}}
	for p, ff := range files {
		if ff != nil {
			s.paths = append(s.paths, p)
		}
	}
	sort.Strings(s.paths)
	n := len(s.paths)
	for i, p := range s.paths {
		s.idx[p] = i
		s.facts = append(s.facts, files[p])
	}
	exists := func(p string) bool { _, ok := s.idx[p]; return ok }
	glob := func(pat string) []string {
		var out []string
		for _, p := range s.paths {
			if ok, _ := filepath.Match(pat, p); ok {
				out = append(out, p)
			}
		}
		return out
	}
	s.edges = make([][]SourceEdge, n)
	s.sourced = make([]bool, n)
	adj := make([][]int, n)
	unresolved := make([]bool, n)
	for i, p := range s.paths {
		var search []string
		if opt.SearchPaths != nil {
			search = opt.SearchPaths(p)
		}
		s.edges[i] = ResolveSources(s.facts[i], opt.Root, search, exists, glob)
		for _, e := range s.edges[i] {
			switch e.Status {
			case SourceUnresolved:
				unresolved[i] = true
			case SourceResolved:
				for _, t := range e.Targets {
					if j, ok := s.idx[t]; ok {
						adj[i] = append(adj[i], j)
						s.sourced[j] = true
					}
				}
			}
		}
	}
	s.computeVisibility(adj, unresolved)
	s.collectSymbols()
	s.bindAll()
	return s
}

// computeVisibility fills visible and incomplete. Visible(A) is everything
// reached from any file that reaches A (A included).
func (s *Snapshot) computeVisibility(adj [][]int, unresolved []bool) {
	n := len(s.paths)
	reach := make([]bitset, n)
	for i := 0; i < n; i++ {
		b := newBitset(n)
		stack := []int{i}
		b.set(i)
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, y := range adj[x] {
				if !b.has(y) {
					b.set(y)
					stack = append(stack, y)
				}
			}
		}
		reach[i] = b
	}
	s.visible = make([]bitset, n)
	for i := range s.visible {
		s.visible[i] = newBitset(n)
	}
	for r := 0; r < n; r++ {
		reach[r].each(func(a int) { s.visible[a].or(reach[r]) })
	}
	s.incomplete = make([]bool, n)
	for a := 0; a < n; a++ {
		s.visible[a].each(func(f int) {
			if unresolved[f] {
				s.incomplete[a] = true
			}
		})
	}
}

// collectSymbols groups every declaration into symbols.
func (s *Snapshot) collectSymbols() {
	n := len(s.paths)
	s.locals = make([]map[string]map[string]bool, n)
	sorted := make([][]*Decl, n)
	for i, ff := range s.facts {
		ds := append([]*Decl(nil), ff.Decls...)
		sort.SliceStable(ds, func(a, b int) bool { return ds[a].NameRange.Start.Offset < ds[b].NameRange.Start.Offset })
		sorted[i] = ds
		for _, d := range ds {
			s.declFile[d] = i
			if d.Kind == DeclVariable && d.Attrs.Local && d.Scope != nil && d.Scope.Func != "" {
				if s.locals[i] == nil {
					s.locals[i] = map[string]map[string]bool{}
				}
				if s.locals[i][d.Scope.Func] == nil {
					s.locals[i][d.Scope.Func] = map[string]bool{}
				}
				s.locals[i][d.Scope.Func][d.Name] = true
			}
		}
	}
	funcs, aliases, vars := map[string][]declEntry{}, map[string][]declEntry{}, map[string][]declEntry{}
	for i, ds := range sorted {
		for _, d := range ds {
			e := declEntry{i, d}
			switch d.Kind {
			case DeclFunction:
				funcs[d.Name] = append(funcs[d.Name], e)
			case DeclAlias:
				aliases[d.Name] = append(aliases[d.Name], e)
			case DeclVariable:
				fn := ""
				if d.Scope != nil {
					fn = d.Scope.Func
				}
				switch {
				case d.Temp:
					sym := &Symbol{Kind: DeclVariable, Name: d.Name, Decls: []*Decl{d}}
					s.declSym[d] = sym
					s.tempSyms[posKey{i, d.NameRange.Start.Offset}] = sym
				case fn != "" && s.locals[i][fn][d.Name]:
					k := localKey{i, fn, d.Name}
					sym := s.localSyms[k]
					if sym == nil {
						sym = &Symbol{Kind: DeclVariable, Name: d.Name, Func: fn, File: s.paths[i]}
						s.localSyms[k] = sym
					}
					sym.Decls = append(sym.Decls, d)
					s.declSym[d] = sym
				default:
					vars[d.Name] = append(vars[d.Name], e)
				}
			}
		}
	}
	for name, es := range funcs {
		s.funcSyms[name] = s.group(DeclFunction, name, es)
	}
	for name, es := range aliases {
		s.aliasSyms[name] = s.group(DeclAlias, name, es)
	}
	for name, es := range vars {
		s.varSyms[name] = s.group(DeclVariable, name, es)
	}
}

// group makes one symbol per set of declarations whose files are visible to
// each other.
func (s *Snapshot) group(kind DeclKind, name string, es []declEntry) []*Symbol {
	parent := make([]int, len(es))
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for a := range es {
		for b := a + 1; b < len(es); b++ {
			if s.visible[es[a].file].has(es[b].file) {
				if ra, rb := find(a), find(b); ra != rb {
					if ra > rb {
						ra, rb = rb, ra
					}
					parent[rb] = ra
				}
			}
		}
	}
	var out []*Symbol
	byRoot := map[int]*Symbol{}
	for i, e := range es {
		r := find(i)
		sym := byRoot[r]
		if sym == nil {
			sym = &Symbol{Kind: kind, Name: name}
			byRoot[r] = sym
			out = append(out, sym)
		}
		sym.Decls = append(sym.Decls, e.d)
		s.declSym[e.d] = sym
	}
	return out
}

// pick returns the symbol among syms that has a declaration visible from
// file, preferring one declared in file itself.
func (s *Snapshot) pick(syms []*Symbol, file int) *Symbol {
	var best *Symbol
	for _, sym := range syms {
		for _, d := range sym.Decls {
			f := s.declFile[d]
			if f == file {
				return sym
			}
			if best == nil && s.visible[file].has(f) {
				best = sym
			}
		}
	}
	return best
}

func (s *Snapshot) bindAll() {
	s.bindings = make([][]Binding, len(s.paths))
	for i, ff := range s.facts {
		var out []Binding
		for _, u := range ff.Uses {
			switch u.Kind {
			case UseCall:
				out = s.bindCall(out, i, u)
			case UseVarRead, UseVarWrite:
				out = append(out, s.bindVar(i, u))
			}
		}
		sort.SliceStable(out, func(a, b int) bool { return out[a].Use.Range.Start.Offset < out[b].Use.Range.Start.Offset })
		s.bindings[i] = out
		for _, b := range out {
			s.bySym[b.Sym] = append(s.bySym[b.Sym], b)
		}
	}
}

func (s *Snapshot) bindVar(file int, u *Use) Binding {
	b := Binding{Use: u, File: s.paths[file], Res: Resolved, Reason: "direct"}
	if u.Kind == UseVarWrite {
		if sym := s.tempSyms[posKey{file, u.Range.Start.Offset}]; sym != nil {
			b.Sym = sym
			return b
		}
	}
	fn := ""
	if u.Scope != nil {
		fn = u.Scope.Func
	}
	if fn != "" && s.locals[file][fn][u.Name] {
		b.Sym = s.localSyms[localKey{file, fn, u.Name}]
		return b
	}
	if sym := s.pick(s.varSyms[u.Name], file); sym != nil {
		b.Sym = sym
		return b
	}
	sym := s.envSyms[u.Name]
	if sym == nil {
		sym = &Symbol{Kind: DeclVariable, Name: u.Name}
		s.envSyms[u.Name] = sym
	}
	b.Sym, b.Res, b.Reason = sym, Unresolved, "no-declaration"
	return b
}

// resolveCall finds what name means in file: an alias (followed to its
// target when that resolves) or a function. followed is true when an alias
// led to a different symbol.
func (s *Snapshot) resolveCall(file int, name string, depth int) (sym *Symbol, followed bool) {
	if a := s.pick(s.aliasSyms[name], file); a != nil {
		if to := a.Decls[len(a.Decls)-1].AliasTo; to != "" && depth < maxAliasDepth {
			if t, _ := s.resolveCall(file, to, depth+1); t != nil && t != a {
				return t, true
			}
		}
		return a, false
	}
	return s.pick(s.funcSyms[name], file), false
}

func (s *Snapshot) bindCall(out []Binding, file int, u *Use) []Binding {
	b := Binding{Use: u, File: s.paths[file], Res: Resolved, Reason: "direct"}
	if sym, followed := s.resolveCall(file, u.Name, 0); sym != nil {
		b.Sym = sym
		if followed {
			b.Reason = "alias"
		}
		return append(out, b)
	}
	if bashWords[u.Name] {
		return out
	}
	if s.incomplete[file] || (len(s.edges[file]) == 0 && !s.sourced[file]) {
		b.Res, b.Reason = Candidate, "name-match-outside-closure"
		for _, sym := range s.funcSyms[u.Name] {
			b.Sym = sym
			out = append(out, b)
		}
	}
	return out
}

// Generation returns the number Build was given.
func (s *Snapshot) Generation() uint64 { return s.gen }

// Facts returns the facts of path, nil when unknown.
func (s *Snapshot) Facts(path string) *FileFacts {
	if i, ok := s.idx[path]; ok {
		return s.facts[i]
	}
	return nil
}

// Paths lists every file, sorted.
func (s *Snapshot) Paths() []string { return append([]string(nil), s.paths...) }

// SourceEdges returns the resolved source commands of path.
func (s *Snapshot) SourceEdges(path string) []SourceEdge {
	if i, ok := s.idx[path]; ok {
		return s.edges[i]
	}
	return nil
}

// Visible lists, sorted, the files whose declarations path can see. It always
// includes path; nil for an unknown path.
func (s *Snapshot) Visible(path string) []string {
	i, ok := s.idx[path]
	if !ok {
		return nil
	}
	var out []string
	s.visible[i].each(func(f int) { out = append(out, s.paths[f]) })
	return out
}

// GraphIncomplete reports whether some file in Visible(path) has a source
// command that could not be resolved.
func (s *Snapshot) GraphIncomplete(path string) bool {
	i, ok := s.idx[path]
	return ok && s.incomplete[i]
}

// BindingsOf lists every use bound to sym, in (path, offset) order.
func (s *Snapshot) BindingsOf(sym *Symbol) []Binding { return s.bySym[sym] }
