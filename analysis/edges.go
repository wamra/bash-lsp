package analysis

import (
	"slices"
	"sort"
	"sync"

	"github.com/wamra/bash-lsp/tree"
)

// Relation says what an Edge records.
type Relation uint8

const (
	RelCalls   Relation = iota + 1 // a command calls a function
	RelSources                     // `source`/`.` of a file
	RelInvokes                     // execution of a workspace file (Task 2)
	RelAliases                     // an alias names a function
)

func (r Relation) String() string {
	switch r {
	case RelCalls:
		return "calls"
	case RelSources:
		return "sources"
	case RelInvokes:
		return "invokes"
	case RelAliases:
		return "aliases"
	}
	return ""
}

func (r Resolution) String() string {
	switch r {
	case Resolved:
		return "resolved"
	case Candidate:
		return "candidate"
	case Dynamic:
		return "dynamic"
	case Unresolved:
		return "unresolved"
	}
	return ""
}

// Caller is the source end of an edge: the innermost named function around
// the site, or the top level of the file Path when Func is nil. Path is
// always the file of the site, so a function whose definitions lie in two
// files is two Callers (one per file); the definitions of one file (both
// branches of an `if`) share one.
//
// Func is a function symbol (Kind DeclFunction) for every relation except
// RelAliases: there it is the alias symbol (Kind DeclAlias) whose value
// names the target. Consumers that want function callers must check
// Func.Kind; an alias is never a function caller.
type Caller struct {
	Path string
	Func *Symbol
}

// Edge is one labelled edge of the call graph. From is described on Caller
// (for RelAliases, From.Func is the alias symbol). To is the target symbol and
// ToDecl the one definition of it this edge goes to: a function with several
// definitions in view gets one edge per definition. To and ToDecl are nil for
// RelSources, RelInvokes and dynamic or unresolved edges; ToFile is the target
// of RelSources and RelInvokes. Text is the expression text of a dynamic edge
// and the path word of an unresolved source.
type Edge struct {
	From   Caller
	To     *Symbol
	ToDecl *Decl
	ToFile string
	Rel    Relation
	Res    Resolution
	Reason string
	Site   Location
	Text   string
}

// edgeRec is an Edge as Build stores it: the target declaration instead of
// a symbol, and the index of the binding or source command the edge comes
// from instead of a copied site and caller. Edge values are made from it on
// demand (edgeOf): Build writes 24 bytes per edge, not 160.
type edgeRec struct {
	to     *Decl // target definition; nil for source edges
	at     int32 // call and alias edges: index into the file's bindings; source edges: into its source edges
	tgt    int32 // source edges: index into that edge's Targets, -1 when unresolved
	rel    Relation
	res    Resolution
	reason uint8 // index into reasonNames
}

// reasonNames are the reasons an edge can carry, by edgeRec.reason.
var reasonNames = []string{"", ReasonDirect, ReasonAlias, ReasonOutsideClosure, ReasonNotFound, ReasonConditional,
	ReasonVariableConstant, ReasonStringEval, ReasonStringTrap, ReasonStringShellC, ReasonVariableUnknown, ReasonExpansion}

func reasonCode(r string) uint8 {
	for i, n := range reasonNames {
		if n == r {
			return uint8(i)
		}
	}
	panic("analysis: edge reason without a code: " + r)
}

var (
	codeDirect      = reasonCode(ReasonDirect)
	codeNotFound    = reasonCode(ReasonNotFound)
	codeConditional = reasonCode(ReasonConditional)
)

// graphIndex is built on the first EdgesFrom or EdgesTo call: Build itself
// only fills the per-file edge records.
type graphIndex struct {
	once sync.Once
	from map[Caller][]Edge
	to   map[*Symbol][]Edge
}

// buildEdges derives the edge table from the bindings and the source graph;
// it resolves nothing itself. hint is the expected number of edges (the
// previous snapshot's count).
func (s *Snapshot) buildEdges(hint int) {
	n := len(s.paths)
	s.recAt = make([]int, n+1)
	recs := make([]edgeRec, 0, hint)
	for i := 0; i < n; i++ {
		s.recAt[i] = len(recs)
		recs = s.fileEdges(recs, i)
	}
	s.recAt[n] = len(recs)
	s.recs = recs
}

// fileEdges appends the edges whose site is in file to out, sorted by site
// offset and relation. Each producer appends its own kind; string, variable,
// invokes and dynamic edges are further producers.
func (s *Snapshot) fileEdges(out []edgeRec, file int) []edgeRec {
	start := len(out)
	out = s.appendSourceEdges(out, file) // source lines usually come first
	out = s.appendCallEdges(out, file)
	out = s.appendDynamicEdges(out, file)
	recs := out[start:]
	cmp := func(a, b edgeRec) int {
		if d := s.recOffset(file, &a) - s.recOffset(file, &b); d != 0 {
			return d
		}
		return int(a.rel) - int(b.rel)
	}
	if !slices.IsSortedFunc(recs, cmp) {
		slices.SortStableFunc(recs, cmp)
	}
	return out
}

func (s *Snapshot) recOffset(file int, r *edgeRec) int {
	switch {
	case r.rel == RelSources:
		return s.edges[file][r.at].Ref.Range.Start.Offset
	case r.rel == RelInvokes:
		return s.invokes[file][r.at].ref.Range.Start.Offset
	case r.tgt == tgtDynamic:
		return s.dyn[file][r.at].rng.Start.Offset
	}
	return s.bindings[file][r.at].Use.Range.Start.Offset
}

// edgeOf makes the Edge of record r of file.
func (s *Snapshot) edgeOf(file int, r *edgeRec) Edge {
	e := Edge{From: Caller{Path: s.paths[file]}, Rel: r.rel, Res: r.res, Reason: reasonNames[r.reason]}
	if r.to != nil {
		e.To, e.ToDecl = s.declSym[r.to], r.to
	}
	switch {
	case r.rel == RelInvokes:
		iv := &s.invokes[file][r.at]
		if from := s.callerOfScope(file, iv.ref.Scope); from != nil {
			e.From.Func = s.declSym[from]
		}
		e.Site, e.ToFile = Location{Path: s.paths[file], Range: iv.ref.Range}, iv.target
		return e
	case r.tgt == tgtDynamic && r.rel == RelCalls:
		d := &s.dyn[file][r.at]
		if from := s.callerOfScope(file, d.sc); from != nil {
			e.From.Func = s.declSym[from]
		}
		e.Site = Location{Path: s.paths[file], Range: d.rng}
		src := s.facts[file].File.Src
		if a, b := d.rng.Start.Offset, d.rng.End.Offset; a >= 0 && a <= b && b <= len(src) {
			e.Text = string(src[a:b])
		}
		return e
	}
	if r.rel != RelSources {
		u := s.bindings[file][r.at].Use
		from := s.callerOfScope(file, u.Scope)
		if r.rel == RelAliases {
			from = s.aliasDeclOf(file, u)
		}
		if from != nil {
			e.From.Func = s.declSym[from]
		}
		e.Site = Location{Path: s.paths[file], Range: u.Range}
		return e
	}
	se := &s.edges[file][r.at]
	if d := s.funcDeclAt(file, se.Ref.Range.Start.Offset); d != nil {
		e.From.Func = s.declSym[d]
	}
	e.Site = Location{Path: s.paths[file], Range: se.Ref.Range}
	if r.tgt >= 0 {
		e.ToFile = se.Targets[r.tgt]
		return e
	}
	src := s.facts[file].File.Src
	if a, b := se.Ref.Range.Start.Offset, se.Ref.Range.End.Offset; a >= 0 && a <= b && b <= len(src) {
		e.Text = string(src[a:b])
	}
	return e
}

// appendCallEdges turns the call bindings of file into RelCalls edges and the
// targets named inside alias definitions into RelAliases edges. A binding to
// an alias symbol (an alias whose target is no function, or a chain longer
// than maxAliasDepth or cyclic) gives no edge; so do builtins and external
// commands, which have no binding.
func (s *Snapshot) appendCallEdges(out []edgeRec, file int) []edgeRec {
	bs := s.bindings[file]
	for k := range bs {
		b := &bs[k]
		u := b.Use
		if u.Kind != UseCall || b.Sym == nil || b.Sym.Kind != DeclFunction || u.Via == ViaValue {
			continue
		}
		r := edgeRec{at: int32(k), rel: RelCalls}
		if u.Via != ViaNone && b.Reason != ReasonOutsideClosure {
			out = s.appendViaTargets(out, file, r, b)
			continue
		}
		if u.ViaAlias {
			if s.aliasDeclOf(file, u) == nil {
				continue
			}
			r.rel = RelAliases
		}
		out = s.appendTargets(out, file, r, b)
	}
	return out
}

// appendTargets appends one copy of r per definition b's call goes to.
//   - name-match-outside-closure: every definition of the symbol, with that label.
//   - one definition in view: resolved, with the binding's reason (direct or alias).
//   - several: candidate/conditional-definition, one edge each.
//
// A definition inside a subshell (or command substitution, pipeline stage,
// process substitution, coproc body) is in view but not sure for calls
// outside that scope: a lone such definition is still a
// candidate/conditional-definition target there. For a call inside it, the
// definitions of the innermost such scope around the call shadow all others.
func (s *Snapshot) appendTargets(out []edgeRec, file int, r edgeRec, b *Binding) []edgeRec {
	reason := codeDirect
	if b.Reason != ReasonDirect {
		reason = reasonCode(b.Reason)
	}
	if b.Res == Candidate {
		r.res, r.reason = Candidate, reason
		for _, d := range b.Sym.Decls {
			r.to = d
			out = append(out, r)
		}
		return out
	}
	off := b.Use.Range.Start.Offset
	if len(b.Sym.Decls) == 1 { // the common case, without allocating
		d := b.Sym.Decls[0]
		r.to = d
		if d.Confined == nil || s.encloses(d, file, off) {
			r.res, r.reason = Resolved, reason
		} else {
			r.res, r.reason = Candidate, codeConditional
		}
		out = append(out, r)
		return out
	}
	var inView, inner []*Decl
	var innerNode *tree.Node
	for _, d := range b.Sym.Decls {
		if !s.visible[file].has(s.declFile[d]) {
			continue
		}
		inView = append(inView, d)
		if d.Confined == nil || !s.encloses(d, file, off) {
			continue
		}
		pn := d.Confined.Node
		switch {
		case innerNode == pn:
			inner = append(inner, d)
		case innerNode == nil || pn.Range.Start.Offset > innerNode.Range.Start.Offset ||
			(pn.Range.Start.Offset == innerNode.Range.Start.Offset && pn.Range.End.Offset < innerNode.Range.End.Offset):
			innerNode, inner = pn, []*Decl{d}
		}
	}
	defs := inView
	if innerNode != nil {
		defs = inner
	}
	if len(defs) == 1 && (innerNode != nil || defs[0].Confined == nil) {
		r.to, r.res, r.reason = defs[0], Resolved, reason
		out = append(out, r)
		return out
	}
	r.res, r.reason = Candidate, codeConditional
	for _, d := range defs {
		r.to = d
		out = append(out, r)
	}
	return out
}

// encloses reports whether the scope that confines declaration d contains
// offset off of file.
func (s *Snapshot) encloses(d *Decl, file, off int) bool {
	n := d.Confined.Node
	return n != nil && s.declFile[d] == file && n.Range.Start.Offset <= off && off < n.Range.End.Offset
}

// aliasDeclOf returns the alias declaration whose value names u (one `alias`
// command may define several).
func (s *Snapshot) aliasDeclOf(file int, u *Use) *Decl {
	var found *Decl
	for _, d := range s.aliasDecls[file] {
		if d.Node == u.Node && d.NameRange.Start.Offset <= u.Range.Start.Offset {
			found = d
		}
	}
	return found
}

// callerOfScope returns the declaration of the innermost named function
// around scope sc, nil at the file's top level.
func (s *Snapshot) callerOfScope(file int, sc *Scope) *Decl {
	for ; sc != nil; sc = sc.Parent {
		if sc.Kind == ScopeFunction && sc.Func != "" {
			return s.funcDeclOf(file, sc.Node)
		}
	}
	return nil
}

// funcDeclOf returns the declaration of the function node n of file.
func (s *Snapshot) funcDeclOf(file int, n *tree.Node) *Decl {
	ds := s.funcDecls[file]
	off := n.NameRange.Start.Offset
	i := sort.Search(len(ds), func(i int) bool { return ds[i].NameRange.Start.Offset >= off })
	for ; i < len(ds) && ds[i].NameRange.Start.Offset == off; i++ {
		if ds[i].Node == n {
			return ds[i]
		}
	}
	return nil
}

// appendSourceEdges adds one RelSources edge per resolved target of each
// source command of file, and an unresolved/not-found edge, with the path
// word as Text, for one that could not be resolved.
func (s *Snapshot) appendSourceEdges(out []edgeRec, file int) []edgeRec {
	for k, se := range s.edges[file] {
		r := edgeRec{at: int32(k), rel: RelSources}
		switch se.Status {
		case SourceResolved:
			r.res, r.reason = Resolved, codeDirect
			for t := range se.Targets {
				r.tgt = int32(t)
				out = append(out, r)
			}
		case SourceUnresolved:
			r.res, r.reason, r.tgt = Unresolved, codeNotFound, -1
			out = append(out, r)
		}
	}
	return out
}

// funcDeclAt returns the innermost function declaration of file whose node
// contains offset, nil when there is none.
func (s *Snapshot) funcDeclAt(file, offset int) *Decl {
	var best *Decl
	for _, d := range s.funcDecls[file] {
		r := d.Node.Range
		if r.Start.Offset <= offset && offset < r.End.Offset && (best == nil || r.Start.Offset >= best.Node.Range.Start.Offset) {
			best = d
		}
	}
	return best
}

// CallerAt returns the caller a site at offset of path belongs to: the
// innermost function declaration around it, else the file's top level. The
// zero Caller for an unknown path.
func (s *Snapshot) CallerAt(path string, offset int) Caller {
	file, ok := s.idx[path]
	if !ok {
		return Caller{}
	}
	c := Caller{Path: path}
	if d := s.funcDeclAt(file, offset); d != nil {
		c.Func = s.declSym[d]
	}
	return c
}

// Edges returns every edge, sorted by (Site.Path, site offset). The slice is
// a copy; the symbols and declarations it points to are the snapshot's.
func (s *Snapshot) Edges() []Edge {
	out := make([]Edge, 0, len(s.recs))
	for file := range s.paths {
		for k := s.recAt[file]; k < s.recAt[file+1]; k++ {
			out = append(out, s.edgeOf(file, &s.recs[k]))
		}
	}
	return out
}

func (s *Snapshot) index() *graphIndex {
	ix := &s.graphIx
	ix.once.Do(func() {
		ix.from, ix.to = map[Caller][]Edge{}, map[*Symbol][]Edge{}
		for _, e := range s.Edges() {
			ix.from[e.From] = append(ix.from[e.From], e)
			if e.To != nil {
				ix.to[e.To] = append(ix.to[e.To], e)
			}
		}
	})
	return ix
}

// EdgesFrom returns the edges whose caller is c, in Edges order.
func (s *Snapshot) EdgesFrom(c Caller) []Edge {
	return append([]Edge(nil), s.index().from[c]...)
}

// EdgesTo returns the edges whose target symbol is sym, in Edges order.
func (s *Snapshot) EdgesTo(sym *Symbol) []Edge {
	if sym == nil {
		return nil
	}
	return append([]Edge(nil), s.index().to[sym]...)
}
