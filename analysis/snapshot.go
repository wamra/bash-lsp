package analysis

import (
	"path/filepath"
	"slices"
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

// Symbol is one named thing: a function, an alias or a variable. Read-only. A
// Symbol belongs to the snapshot that built it (each Build makes new ones from
// the shared, read-only Decls); it is not valid in another snapshot.
type Symbol struct {
	Kind  DeclKind
	Name  string
	Decls []*Decl // every declaration that is this symbol, in (path, offset) order
	Func  string  // for a local variable: its function; "" otherwise
	File  string  // for a local variable: its file; "" for globals and functions
	// Private is the kind of scope a variable declared in a subshell, command
	// substitution, pipeline stage, process substitution or coproc is confined
	// to; 0 otherwise. Such a symbol also has Func (when inside a function) and File set.
	Private ScopeKind
}

// Reason strings of a Binding.
const (
	ReasonDirect         = "direct"
	ReasonAlias          = "alias"
	ReasonOutsideClosure = "name-match-outside-closure"
	ReasonNotFound       = "not-found"
	ReasonEnvironment    = "environment"
	ReasonConditional    = "conditional-definition"
)

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

// Options configure Build. SearchPaths may be nil. Prev, when set, is the
// snapshot of the previous generation: Build reuses its visibility sets when
// the files and the source graph are unchanged, which is the case for most
// edits (a change inside a function body). The result is the same as a build
// without Prev.
type Options struct {
	Root        string
	SearchPaths func(path string) []string
	Prev        *Snapshot
}

// maxAliasDepth bounds alias chains.
const maxAliasDepth = 8

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

// localKey names a local variable: the function body it belongs to (the
// KFuncDecl node, not the name: one file may define a function twice, once
// per branch of an `if`, with different locals) and its name.
type localKey struct {
	file int
	fn   *tree.Node
	name string
}

type posKey struct{ file, offset int }

// privEntry is a private symbol and the scope it belongs to.
type privEntry struct {
	sc  *Scope
	sym *Symbol
}

// privKey names a variable declared in a private scope: the scope itself
// (one object per subshell, command substitution, pipeline stage, ...) and its name.
type privKey struct {
	file int
	sc   *Scope
	name string
}

// Snapshot is an immutable analysed workspace: facts, the source graph,
// visibility and every name binding.
type Snapshot struct {
	gen        uint64
	root       string
	paths      []string
	idx        map[string]int
	facts      []*FileFacts
	edges      [][]SourceEdge
	visible    []bitset
	incomplete []bool
	sourced    []bool
	adj        [][]int // resolved source edges as file indexes; with unresolved, the key for reusing visible
	unresolved []bool

	funcSyms, aliasSyms, varSyms map[string][]*Symbol
	localSyms                    map[localKey]*Symbol
	privSyms                     map[privKey]*Symbol
	privByFile                   [][]privEntry // per file, every private symbol, in the order collectSymbols made them
	envSyms                      map[string]*Symbol
	tempSyms                     map[posKey]*Symbol
	locals                       []map[*tree.Node]map[string]bool // file -> function body -> names declared local
	declSym                      map[*Decl]*Symbol
	declFile                     map[*Decl]int
	funcDecls, aliasDecls        [][]*Decl // per file, function and alias declarations in offset order

	bindings [][]Binding // per file, in use-offset order
	bySym    map[*Symbol][]Binding

	dyn       [][]dynSite                 // per file, dynamic edge sites (dynamic.go)
	invokes   [][]invokeSite              // per file, resolved invocations
	extra     []Binding                   // during Build: literal-value bindings to merge
	valueSeen map[valueKey]bool           // during Build: literal-value bindings made
	valueUse  map[*Decl]*Use              // during Build: the literal-value use of an assignment
	varVals   map[varValuesKey]*varValues // during Build: per variable symbol and file, what "$cmd" calls may run
	unsetBy   map[string][]int            // function name -> files with `unset -f` of it

	recs     []edgeRec // every edge, compact, grouped by file and sorted (see edges.go)
	recAt    []int     // file i's edges are recs[recAt[i]:recAt[i+1]]
	graphIx  graphIndex
	graphAdj graphAdj
}

// Build analyses files (keyed by absolute path) into a Snapshot.
func Build(files map[string]*FileFacts, opt Options, generation uint64) *Snapshot {
	s := &Snapshot{gen: generation, root: opt.Root, idx: map[string]int{},
		funcSyms: map[string][]*Symbol{}, aliasSyms: map[string][]*Symbol{}, varSyms: map[string][]*Symbol{},
		localSyms: map[localKey]*Symbol{}, privSyms: map[privKey]*Symbol{}, envSyms: map[string]*Symbol{}, tempSyms: map[posKey]*Symbol{},
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
	searches := make([][]string, n)
	for i, p := range s.paths {
		var search []string
		if opt.SearchPaths != nil {
			search = opt.SearchPaths(p)
		}
		searches[i] = search
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
	s.adj, s.unresolved = adj, unresolved
	if p := opt.Prev; p != nil && s.sameGraph(p) {
		s.visible, s.incomplete = p.visible, p.incomplete
	} else {
		s.computeVisibility(adj, unresolved)
	}
	s.collectSymbols()
	s.prepareDynamic()
	s.bindAll()
	s.finishDynamic(func(i int) *resolver {
		return &resolver{ff: s.facts[i], root: opt.Root, search: searches[i], exists: exists, glob: glob, dir: filepath.Dir(s.paths[i])}
	})
	hint := 0
	if opt.Prev != nil {
		hint = len(opt.Prev.recs) + len(opt.Prev.recs)/8
	}
	s.buildEdges(hint)
	return s
}

// sameGraph reports whether p has the same files and the same source graph as
// s, so that p's visibility and incomplete flags are s's too.
func (s *Snapshot) sameGraph(p *Snapshot) bool {
	if len(s.paths) != len(p.paths) || len(p.visible) != len(s.paths) {
		return false
	}
	for i, path := range s.paths {
		if path != p.paths[i] || s.unresolved[i] != p.unresolved[i] || !slices.Equal(s.adj[i], p.adj[i]) {
			return false
		}
	}
	return true
}

// computeVisibility fills visible and incomplete. Visible(A) is everything
// reached from any file that reaches A (A included).
//
// It works on the condensation of the source graph (strongly connected
// components, so a cycle is one node): the files a component reaches are the
// union of what its successors reach, and what a component sees is what it
// reaches plus everything seen by components that reach it. Both are computed
// in one pass each over the component DAG, O((files + edges) * files / 64),
// instead of one traversal per file. Files of one component share a bitset;
// visible sets are never modified after Build.
func (s *Snapshot) computeVisibility(adj [][]int, unresolved []bool) {
	n := len(s.paths)
	comp, nc := sccs(adj)
	members := make([][]int, nc)
	for f, c := range comp {
		members[c] = append(members[c], f)
	}
	// Tarjan emits a component after every component it can reach, so an edge
	// between components always runs from a higher index to a lower one.
	vis := make([]bitset, nc)
	for c := 0; c < nc; c++ {
		b := newBitset(n)
		for _, f := range members[c] {
			b.set(f)
		}
		for _, f := range members[c] {
			for _, y := range adj[f] {
				if d := comp[y]; d != c {
					b.or(vis[d]) // vis[d] holds only what d reaches here
				}
			}
		}
		vis[c] = b
	}
	// vis[c] is now reach(c). Add it to every successor, highest component
	// first: every component that reaches c has a higher index, so vis[c] is
	// final (reach(c) plus what its predecessors see) when it is pushed on.
	seen := make([]int, nc)
	for c := range seen {
		seen[c] = -1
	}
	for c := nc - 1; c >= 0; c-- {
		for _, f := range members[c] {
			for _, y := range adj[f] {
				if d := comp[y]; d != c && seen[d] != c {
					seen[d] = c
					vis[d].or(vis[c])
				}
			}
		}
	}
	s.visible = make([]bitset, n)
	for f := range s.visible {
		s.visible[f] = vis[comp[f]]
	}
	mask := newBitset(n)
	hasUnresolved := false
	for f, u := range unresolved {
		if u {
			mask.set(f)
			hasUnresolved = true
		}
	}
	s.incomplete = make([]bool, n)
	if !hasUnresolved {
		return
	}
	inc := make([]bool, nc)
	for c, b := range vis {
		for i := range b {
			if b[i]&mask[i] != 0 {
				inc[c] = true
				break
			}
		}
	}
	for f := range s.incomplete {
		s.incomplete[f] = inc[comp[f]]
	}
}

// sccs returns the strongly connected component of every node of adj and the
// number of components. Components are numbered in the order Tarjan's
// algorithm completes them: reverse topological order of the condensation.
func sccs(adj [][]int) (comp []int, nc int) {
	n := len(adj)
	comp = make([]int, n)
	index := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i], comp[i] = -1, -1
	}
	var stack []int
	type frame struct{ v, next int }
	next := 0
	for root := 0; root < n; root++ {
		if index[root] != -1 {
			continue
		}
		call := []frame{{root, 0}}
		index[root], low[root] = next, next
		next++
		stack = append(stack, root)
		onStack[root] = true
		for len(call) > 0 {
			fr := &call[len(call)-1]
			v := fr.v
			if fr.next < len(adj[v]) {
				w := adj[v][fr.next]
				fr.next++
				if index[w] == -1 {
					index[w], low[w] = next, next
					next++
					stack = append(stack, w)
					onStack[w] = true
					call = append(call, frame{w, 0})
				} else if onStack[w] && index[w] < low[v] {
					low[v] = index[w]
				}
				continue
			}
			if low[v] == index[v] {
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp[w] = nc
					if w == v {
						break
					}
				}
				nc++
			}
			call = call[:len(call)-1]
			if len(call) > 0 {
				if p := call[len(call)-1].v; low[v] < low[p] {
					low[p] = low[v]
				}
			}
		}
	}
	return comp, nc
}

// funcBody returns the KFuncDecl node of the function sc lies in (the nearest
// function scope), nil at file level or in a function without a name.
func funcBody(sc *Scope) *tree.Node {
	if sc == nil || sc.Func == "" {
		return nil
	}
	if sc.body != nil {
		return sc.body
	}
	for ; sc != nil; sc = sc.Parent {
		if sc.Kind == ScopeFunction {
			return sc.Node
		}
	}
	return nil
}

// collectSymbols groups every declaration into symbols.
func (s *Snapshot) collectSymbols() {
	n := len(s.paths)
	s.locals = make([]map[*tree.Node]map[string]bool, n)
	s.funcDecls, s.aliasDecls = make([][]*Decl, n), make([][]*Decl, n)
	s.privByFile = make([][]privEntry, n)
	sorted := make([][]*Decl, n)
	total := 0
	for _, ff := range s.facts {
		total += len(ff.Decls)
	}
	s.declFile = make(map[*Decl]int, total)
	s.declSym = make(map[*Decl]*Symbol, total)
	byOffset := func(a, b *Decl) int { return a.NameRange.Start.Offset - b.NameRange.Start.Offset }
	nfunc, totalFuncs := make([]int, n), 0
	for i, ff := range s.facts {
		ds := append([]*Decl(nil), ff.Decls...)
		if !slices.IsSortedFunc(ds, byOffset) {
			slices.SortStableFunc(ds, byOffset)
		}
		sorted[i] = ds
		for _, d := range ds {
			s.declFile[d] = i
			if d.Kind == DeclFunction {
				nfunc[i]++
				totalFuncs++
			}
			if fn := funcBody(d.Scope); d.Kind == DeclVariable && d.Attrs.Local && fn != nil && privateScope(d.Scope) == nil {
				if s.locals[i] == nil {
					s.locals[i] = map[*tree.Node]map[string]bool{}
				}
				if s.locals[i][fn] == nil {
					s.locals[i][fn] = map[string]bool{}
				}
				s.locals[i][fn][d.Name] = true
			}
		}
	}
	funcs, aliases, vars := map[string][]declEntry{}, map[string][]declEntry{}, map[string][]declEntry{}
	funcSlab := make([]*Decl, totalFuncs) // one allocation for every file's function list
	for i, ds := range sorted {
		s.funcDecls[i] = funcSlab[:0:nfunc[i]]
		funcSlab = funcSlab[nfunc[i]:]
		for _, d := range ds {
			e := declEntry{i, d}
			switch d.Kind {
			case DeclFunction:
				funcs[d.Name] = append(funcs[d.Name], e)
				s.funcDecls[i] = append(s.funcDecls[i], d)
			case DeclAlias:
				aliases[d.Name] = append(aliases[d.Name], e)
				s.aliasDecls[i] = append(s.aliasDecls[i], d)
			case DeclVariable:
				fn := funcBody(d.Scope)
				switch {
				case d.Temp:
					sym := &Symbol{Kind: DeclVariable, Name: d.Name, Decls: []*Decl{d}}
					s.declSym[d] = sym
					s.tempSyms[posKey{i, d.NameRange.Start.Offset}] = sym
				case privateScope(d.Scope) != nil:
					ps := privateScope(d.Scope)
					k := privKey{i, ps, d.Name}
					sym := s.privSyms[k]
					if sym == nil {
						sym = &Symbol{Kind: DeclVariable, Name: d.Name, Func: d.Scope.Func, File: s.paths[i], Private: ps.Kind}
						s.privSyms[k] = sym
						s.privByFile[i] = append(s.privByFile[i], privEntry{ps, sym})
					}
					sym.Decls = append(sym.Decls, d)
					s.declSym[d] = sym
				case fn != nil && s.locals[i][fn][d.Name]:
					k := localKey{i, fn, d.Name}
					sym := s.localSyms[k]
					if sym == nil {
						sym = &Symbol{Kind: DeclVariable, Name: d.Name, Func: d.Scope.Func, File: s.paths[i]}
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
	// Declarations in one file are always one symbol (a file sees itself), so
	// link them first and compare files, not declarations, below: a variable
	// name declared 2,000 times in one file is not 2 million visibility tests.
	// Every link goes to the lowest index, so a component's root is its lowest
	// index whatever order the links are made in.
	firstOf := map[int]int{}
	var reps []int
	for i, e := range es {
		if f, ok := firstOf[e.file]; ok {
			parent[i] = f
		} else {
			firstOf[e.file] = i
			reps = append(reps, i)
		}
	}
	for x, a := range reps {
		for _, b := range reps[x+1:] {
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

// pickAll returns every distinct symbol among syms with a declaration visible from file.
func (s *Snapshot) pickAll(syms []*Symbol, file int) []*Symbol {
	var out []*Symbol
	for _, sym := range syms {
		for _, d := range sym.Decls {
			if s.visible[file].has(s.declFile[d]) {
				out = append(out, sym)
				break
			}
		}
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
		out := make([]Binding, 0, len(ff.Uses))
		priv := s.privateOf(i, ff)
		for j, u := range ff.Uses {
			var ps *Symbol
			if priv != nil {
				ps = priv[j]
			}
			switch u.Kind {
			case UseCall:
				out = s.bindCall(out, i, u, ps)
			case UseVarRead, UseVarWrite:
				out = s.bindVar(out, i, u, ps)
			}
		}
		byUse := func(a, b Binding) int { return a.Use.Range.Start.Offset - b.Use.Range.Start.Offset }
		if !slices.IsSortedFunc(out, byUse) {
			slices.SortStableFunc(out, byUse)
		}
		s.bindings[i] = out
		for _, b := range out {
			s.bySym[b.Sym] = append(s.bySym[b.Sym], b)
		}
	}
}

// privateOf returns, for each use of ff, the symbol of the innermost private
// scope around it that declares its name (rules S1, S2), nil when none; nil
// for a file without private symbols. Bind numbered each scope with the uses
// recorded while it was open, so a scope encloses use j exactly when
// first <= j < last, and the enclosing scopes of a name form a stack swept once
// in use order: linear, where a walk up the scope chain per use cost
// O(depth) each (review C1: 45 s for 2 MiB of reads 3,900 subshells deep).
// The walk passes through function scopes, looking only at private scopes,
// so a function defined inside a subshell reads that subshell's variables.
func (s *Snapshot) privateOf(file int, ff *FileFacts) []*Symbol {
	es := s.privByFile[file]
	if len(es) == 0 {
		return nil
	}
	if !ff.Root.closed { // not numbered by Bind: walk the chain per use
		out := make([]*Symbol, len(ff.Uses))
		for j, u := range ff.Uses {
			if !namesVariable(u) {
				continue
			}
			for sc := u.Scope; sc != nil && sc.Kind != ScopeFile; sc = sc.Parent {
				if sym := s.privSyms[privKey{file, sc, u.Name}]; sym != nil && sc.Kind.Private() {
					out[j] = sym
					break
				}
			}
		}
		return out
	}
	es = slices.Clone(es)
	// Outer scopes first where two open together.
	slices.SortStableFunc(es, func(a, b privEntry) int {
		if a.sc.first != b.sc.first {
			return a.sc.first - b.sc.first
		}
		return b.sc.last - a.sc.last
	})
	out := make([]*Symbol, len(ff.Uses))
	open := map[string][]privEntry{}
	k := 0
	for j, u := range ff.Uses {
		for ; k < len(es) && es[k].sc.first <= j; k++ {
			e := es[k]
			st := open[e.sym.Name]
			for len(st) > 0 && st[len(st)-1].sc.last <= e.sc.first {
				st = st[:len(st)-1]
			}
			open[e.sym.Name] = append(st, e)
		}
		if !namesVariable(u) {
			continue
		}
		st, ok := open[u.Name]
		if !ok {
			continue
		}
		for len(st) > 0 && st[len(st)-1].sc.last <= j {
			st = st[:len(st)-1]
		}
		open[u.Name] = st
		if len(st) > 0 {
			out[j] = st[len(st)-1].sym
		}
	}
	return out
}

// namesVariable reports whether u's Name is a variable resolved by the
// variable rules: a read, a write, or a `"$cmd"` call (ViaVariable), whose
// variable bindVariableCall resolves at the call's position.
func namesVariable(u *Use) bool {
	return u.Kind == UseVarRead || u.Kind == UseVarWrite || (u.Kind == UseCall && u.Via == ViaVariable)
}

func (s *Snapshot) bindVar(out []Binding, file int, u *Use, priv *Symbol) []Binding {
	b := Binding{Use: u, File: s.paths[file], Res: Resolved, Reason: ReasonDirect}
	if u.Kind == UseVarWrite {
		if sym := s.tempSyms[posKey{file, u.Range.Start.Offset}]; sym != nil {
			b.Sym = sym
			return append(out, b)
		}
	}
	// Innermost private scope with a declaration of the name wins (rules S1,
	// S2; privateOf). The enclosing function's locals are not visible to a
	// nested function.
	if priv != nil {
		b.Sym = priv
		return append(out, b)
	}
	if fn := funcBody(u.Scope); fn != nil && s.locals[file][fn][u.Name] {
		b.Sym = s.localSyms[localKey{file, fn, u.Name}]
		return append(out, b)
	}
	if syms := s.pickAll(s.varSyms[u.Name], file); len(syms) > 0 {
		for _, sym := range syms {
			b.Sym = sym
			out = append(out, b)
		}
		return out
	}
	sym := s.envSyms[u.Name]
	if sym == nil {
		sym = &Symbol{Kind: DeclVariable, Name: u.Name}
		s.envSyms[u.Name] = sym
	}
	b.Sym, b.Res, b.Reason = sym, Unresolved, ReasonEnvironment
	return append(out, b)
}

// resolveCall finds what name means in file: an alias (followed to its
// target when that resolves) or every function symbol in view. followed is
// true when an alias led to a different symbol.
func (s *Snapshot) resolveCall(file int, name string, depth int) (syms []*Symbol, followed bool) {
	if a := s.pick(s.aliasSyms[name], file); a != nil {
		if to := a.Decls[len(a.Decls)-1].AliasTo; to != "" && depth < maxAliasDepth {
			if ts, _ := s.resolveCall(file, to, depth+1); len(ts) > 0 && ts[0] != a {
				return ts, true
			}
		}
		return []*Symbol{a}, false
	}
	return s.pickAll(s.funcSyms[name], file), false
}

// bindCall binds a call use; priv is privateOf's symbol for a ViaVariable
// call (the private variable its name reads), nil otherwise.
func (s *Snapshot) bindCall(out []Binding, file int, u *Use, priv *Symbol) []Binding {
	if u.Via == ViaVariable {
		return s.bindVariableCall(out, file, u, priv)
	}
	b := Binding{Use: u, File: s.paths[file], Res: Resolved, Reason: ReasonDirect}
	// The target named inside an alias definition is one level down already:
	// a call to that alias resolves it at depth 1, so it must too, or the
	// alias would reach a function no call through it reaches.
	depth := 0
	if u.ViaAlias {
		depth = 1
	}
	if syms, followed := s.resolveCall(file, u.Name, depth); len(syms) > 0 {
		if followed {
			b.Reason = ReasonAlias
		}
		if r := u.Via.reason(); r != "" { // a call inside a string: found by name, not proven
			b.Res, b.Reason = Candidate, r
		}
		for _, sym := range syms {
			b.Sym = sym
			out = append(out, b)
		}
		return out
	}
	if IsBuiltin(u.Name) {
		return out
	}
	if s.incomplete[file] || (len(s.edges[file]) == 0 && !s.sourced[file]) {
		b.Res, b.Reason = Candidate, ReasonOutsideClosure
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

// SourceEdges returns the resolved source commands of path. The slice and each
// edge's Targets are copies; the nodes an edge refers to are shared and
// read-only.
func (s *Snapshot) SourceEdges(path string) []SourceEdge {
	i, ok := s.idx[path]
	if !ok {
		return nil
	}
	out := append([]SourceEdge(nil), s.edges[i]...)
	for j := range out {
		out[j].Targets = append([]string(nil), out[j].Targets...)
	}
	return out
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
func (s *Snapshot) BindingsOf(sym *Symbol) []Binding { return append([]Binding(nil), s.bySym[sym]...) }
