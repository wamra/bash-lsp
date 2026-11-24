package analysis

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Node is a vertex of the call/source graph: a function as defined in one file
// (Path is the file of the definition, Func the symbol) or, when Func is nil,
// the file itself. A function defined in two files is two Nodes. Paths are the
// snapshot's, as in Caller and Edge.
type Node struct {
	Path string
	Func *Symbol
}

// Step is one traversed edge: From calls, sources or invokes To.
type Step struct {
	From, To Node
	Edge     Edge
}

// graphAdj is the traversable part of the edge table, built on the first graph
// query: every resolved or candidate calls, sources or invokes edge as a Step,
// in Edges order, with per-node lists of step indexes. Dynamic and unresolved
// edges have no target node and are not in it; RelAliases edges are not steps
// (a call through an alias already is a RelCalls edge to the function).
type graphAdj struct {
	once  sync.Once
	steps []Step
	out   map[Node][]int32
	in    map[Node][]int32
}

func (s *Snapshot) adjacency() *graphAdj {
	g := &s.graphAdj
	g.once.Do(func() {
		g.out, g.in = map[Node][]int32{}, map[Node][]int32{}
		for _, e := range s.Edges() {
			if e.Rel == RelAliases || (e.Res != Resolved && e.Res != Candidate) {
				continue
			}
			from := Node{Path: e.From.Path, Func: e.From.Func}
			var to Node
			switch e.Rel {
			case RelCalls:
				if e.To == nil || e.ToDecl == nil {
					continue
				}
				to = Node{Path: s.paths[s.declFile[e.ToDecl]], Func: e.To}
			case RelSources, RelInvokes:
				if e.ToFile == "" {
					continue
				}
				to = Node{Path: e.ToFile}
			default:
				continue
			}
			i := int32(len(g.steps))
			g.steps = append(g.steps, Step{From: from, To: to, Edge: e})
			g.out[from] = append(g.out[from], i)
			g.in[to] = append(g.in[to], i)
		}
	})
	return g
}

// resolvePath maps a file spec (absolute, or relative to the workspace root,
// with forward slashes) to a path of the snapshot.
func (s *Snapshot) resolvePath(p string) (string, bool) {
	if _, ok := s.idx[p]; ok {
		return p, true
	}
	if s.root != "" && !filepath.IsAbs(p) {
		q := filepath.Join(s.root, filepath.FromSlash(p))
		if _, ok := s.idx[q]; ok {
			return q, true
		}
	}
	return "", false
}

// declOffset is the offset of the first declaration of sym in path, -1 when
// there is none.
func (s *Snapshot) declOffset(sym *Symbol, path string) int {
	file, ok := s.idx[path]
	if sym == nil || !ok {
		return -1
	}
	best := -1
	for _, d := range sym.Decls {
		if s.declFile[d] == file && (best < 0 || d.NameRange.Start.Offset < best) {
			best = d.NameRange.Start.Offset
		}
	}
	return best
}

// FuncNode returns the node of the function name as defined in path; false
// when path is no file of the snapshot or defines no function of that name.
// Unlike Lookup it never reads name as a file or position spec.
func (s *Snapshot) FuncNode(path, name string) (Node, bool) {
	file, ok := s.idx[path]
	if !ok {
		return Node{}, false
	}
	for _, sym := range s.funcSyms[name] {
		for _, d := range sym.Decls {
			if s.declFile[d] == file {
				return Node{Path: path, Func: sym}, true
			}
		}
	}
	return Node{}, false
}

// FuncDecl returns the first definition of n.Func in n.Path, nil for a file
// node or a function with no definition there.
func (s *Snapshot) FuncDecl(n Node) *Decl {
	file, ok := s.idx[n.Path]
	if n.Func == nil || !ok {
		return nil
	}
	for _, d := range n.Func.Decls { // in (path, offset) order
		if s.declFile[d] == file {
			return d
		}
	}
	return nil
}

// compareNodes orders nodes by path, the file itself before its functions,
// then by the offset of the definition.
func (s *Snapshot) compareNodes(a, b Node) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	if (a.Func == nil) != (b.Func == nil) {
		if a.Func == nil {
			return -1
		}
		return 1
	}
	if a.Func == nil {
		return 0
	}
	if c := cmp.Compare(s.declOffset(a.Func, a.Path), s.declOffset(b.Func, b.Path)); c != 0 {
		return c
	}
	return strings.Compare(a.Func.Name, b.Func.Name)
}

// Lookup resolves spec to graph nodes. "path:line:col" (1-based, the column in
// UTF-16 units, as the language server and the CLI print positions) gives the function named at that position, else the function around
// it, else the file; "path" (absolute or root-relative) gives the file node;
// any other spec is a function name and gives one node per file that defines
// it, sorted by path and offset. A spec that matches a file is never read as a
// name. An unknown spec is an error naming it.
func (s *Snapshot) Lookup(spec string) ([]Node, error) {
	if n, ok, err := s.lookupPosition(spec); ok {
		return n, err
	}
	if p, ok := s.resolvePath(spec); ok {
		return []Node{{Path: p}}, nil
	}
	var out []Node
	for _, sym := range s.funcSyms[spec] {
		seen := map[int]bool{}
		for _, d := range sym.Decls {
			if f := s.declFile[d]; !seen[f] {
				seen[f] = true
				out = append(out, Node{Path: s.paths[f], Func: sym})
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("analysis: no function or file matches %q", spec)
	}
	slices.SortFunc(out, s.compareNodes)
	return out, nil
}

// lookupPosition handles "path:line:col". ok is false when spec has no such
// shape or names no file of the snapshot (then it is tried as path or name);
// a line or column outside the file is an error naming spec.
func (s *Snapshot) lookupPosition(spec string) (nodes []Node, ok bool, err error) {
	i := strings.LastIndexByte(spec, ':')
	if i < 0 {
		return nil, false, nil
	}
	col, err1 := strconv.Atoi(spec[i+1:])
	rest := spec[:i]
	j := strings.LastIndexByte(rest, ':')
	if err1 != nil || j < 0 {
		return nil, false, nil
	}
	line, err2 := strconv.Atoi(rest[j+1:])
	path, ok := s.resolvePath(rest[:j])
	if err2 != nil || !ok || line < 1 || col < 1 {
		return nil, false, nil
	}
	file := s.idx[path]
	src := s.facts[file].File.Src
	off, l := 0, 1
	for l < line {
		k := slices.Index(src[off:], '\n')
		if k < 0 {
			return nil, true, fmt.Errorf("analysis: line %d of %q is past the end of the file", line, spec)
		}
		off += k + 1
		l++
	}
	if line > 1 && off == len(src) { // the empty "line" after a final newline
		return nil, true, fmt.Errorf("analysis: line %d of %q is past the end of the file", line, spec)
	}
	lineStart := off
	f := s.facts[file].File
	p := f.FromUTF16(line-1, col-1) // clamps a column past the line end
	if _, ch := f.ToUTF16(p); ch != col-1 {
		return nil, true, fmt.Errorf("analysis: column %d of %q is not a position of the line", col, spec)
	}
	off = p.Offset
	atEnd := off >= len(src) || src[off] == '\n' || (src[off] == '\r' && off+1 < len(src) && src[off+1] == '\n')
	if atEnd && off > lineStart { // the column one past the line end means its last byte
		off--
	}
	if off >= len(src) {
		off = max(len(src)-1, 0)
	}
	if sym, ok := s.SymbolAt(path, off); ok && sym.Kind == DeclFunction {
		var out []Node
		seen := map[string]bool{}
		for _, l := range s.Definition(path, off) {
			if !seen[l.Path] {
				seen[l.Path] = true
				out = append(out, Node{Path: l.Path, Func: sym})
			}
		}
		if len(out) == 0 {
			for _, d := range sym.Decls {
				if p := s.paths[s.declFile[d]]; !seen[p] {
					seen[p] = true
					out = append(out, Node{Path: p, Func: sym})
				}
			}
		}
		slices.SortFunc(out, s.compareNodes)
		return out, true, nil
	}
	c := s.CallerAt(path, off)
	return []Node{{Path: c.Path, Func: c.Func}}, true, nil
}

// sortSteps orders steps of one depth by site path, site offset, relation and
// finally table position.
func (g *graphAdj) sortSteps(idx []int32) {
	slices.SortFunc(idx, func(a, b int32) int {
		ea, eb := &g.steps[a].Edge, &g.steps[b].Edge
		if c := strings.Compare(ea.Site.Path, eb.Site.Path); c != 0 {
			return c
		}
		if c := cmp.Compare(ea.Site.Range.Start.Offset, eb.Site.Range.Start.Offset); c != 0 {
			return c
		}
		if c := cmp.Compare(ea.Rel, eb.Rel); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
}

// walk is the breadth-first traversal behind Callers and Callees: forward
// follows out-edges, else in-edges. Each node is expanded once, so each edge
// is returned once and cycles terminate. depth <= 0 is unlimited.
func (s *Snapshot) walk(n Node, depth int, forward bool) []Step {
	g := s.adjacency()
	adj := g.in
	if forward {
		adj = g.out
	}
	seen := map[Node]bool{n: true}
	frontier := []Node{n}
	var out []Step
	for level := 1; len(frontier) > 0 && (depth <= 0 || level <= depth); level++ {
		var idx []int32
		for _, x := range frontier {
			idx = append(idx, adj[x]...)
		}
		g.sortSteps(idx)
		frontier = frontier[:0:0]
		for _, i := range idx {
			st := g.steps[i]
			out = append(out, st)
			next := st.From
			if forward {
				next = st.To
			}
			if !seen[next] {
				seen[next] = true
				frontier = append(frontier, next)
			}
		}
	}
	return out
}

// Callers returns the steps leading to n through resolved and candidate
// calls, sources and invokes, breadth-first, up to depth levels (1 = direct;
// 0 or less = unlimited), each edge once. Step.From and Step.To always follow
// the edge's direction (From calls, sources or invokes To), so the first
// level's To is n and a deeper level's To is a node an earlier step had as
// its From. Consumers filter by Step.Edge.Res.
func (s *Snapshot) Callers(n Node, depth int) []Step { return s.walk(n, depth, false) }

// Callees returns the steps leaving n, as Callers does the steps entering it.
// A file node has the edges of its top level; a function node those of its
// body.
func (s *Snapshot) Callees(n Node, depth int) []Step { return s.walk(n, depth, true) }

// Impact returns every node that transitively calls, sources or invokes n,
// up to depth levels, each once, breadth-first and sorted by path and
// definition offset within a level. n itself is not in the result.
func (s *Snapshot) Impact(n Node, depth int) []Node {
	g := s.adjacency()
	seen := map[Node]bool{n: true}
	frontier := []Node{n}
	var out []Node
	for level := 1; len(frontier) > 0 && (depth <= 0 || level <= depth); level++ {
		var next []Node
		for _, x := range frontier {
			for _, i := range g.in[x] {
				if f := g.steps[i].From; !seen[f] {
					seen[f] = true
					next = append(next, f)
				}
			}
		}
		slices.SortFunc(next, s.compareNodes)
		out = append(out, next...)
		frontier = next
	}
	return out
}

// Trace returns a shortest path from a to b over resolved and candidate
// calls, sources and invokes edges, nil when there is none (or a == b).
// Among shortest paths it takes the one through the earliest sites.
func (s *Snapshot) Trace(a, b Node) []Step {
	if a == b {
		return nil
	}
	g := s.adjacency()
	via := map[Node]int32{} // node -> step that first reached it
	seen := map[Node]bool{a: true}
	frontier := []Node{a}
	for len(frontier) > 0 {
		var next []Node
		for _, x := range frontier {
			idx := slices.Clone(g.out[x])
			g.sortSteps(idx)
			for _, i := range idx {
				to := g.steps[i].To
				if seen[to] {
					continue
				}
				seen[to], via[to] = true, i
				if to == b {
					var path []Step
					for cur := b; cur != a; cur = g.steps[via[cur]].From {
						path = append(path, g.steps[via[cur]])
					}
					slices.Reverse(path)
					return path
				}
				next = append(next, to)
			}
		}
		frontier = next
	}
	return nil
}

// SourceCycles returns the groups of files that source each other, directly
// or through others: the strongly connected components of the source graph
// with more than one file, or one file that sources itself. Each group is
// sorted, and the groups are sorted by their first path.
func (s *Snapshot) SourceCycles() [][]string {
	g := s.adjacency()
	n := len(s.paths)
	succ := make([][]int, n)
	selfLoop := make([]bool, n)
	for _, st := range g.steps {
		if st.Edge.Rel != RelSources {
			continue
		}
		a, ok1 := s.idx[st.From.Path]
		b, ok2 := s.idx[st.To.Path]
		if !ok1 || !ok2 {
			continue
		}
		if a == b {
			selfLoop[a] = true
		}
		succ[a] = append(succ[a], b)
	}
	// Iterative Tarjan.
	const unvisited = -1
	index := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i] = unvisited
	}
	type frame struct{ v, next int }
	var stack []int
	var out [][]string
	counter := 0
	for root := 0; root < n; root++ {
		if index[root] != unvisited {
			continue
		}
		work := []frame{{v: root}}
		index[root], low[root] = counter, counter
		counter++
		stack = append(stack, root)
		onStack[root] = true
		for len(work) > 0 {
			f := &work[len(work)-1]
			if f.next < len(succ[f.v]) {
				w := succ[f.v][f.next]
				f.next++
				switch {
				case index[w] == unvisited:
					index[w], low[w] = counter, counter
					counter++
					stack = append(stack, w)
					onStack[w] = true
					work = append(work, frame{v: w})
				case onStack[w]:
					low[f.v] = min(low[f.v], index[w])
				}
				continue
			}
			v := f.v
			work = work[:len(work)-1]
			if len(work) > 0 {
				p := work[len(work)-1].v
				low[p] = min(low[p], low[v])
			}
			if low[v] != index[v] {
				continue
			}
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, s.paths[w])
				if w == v {
					break
				}
			}
			if len(comp) > 1 || selfLoop[v] {
				sort.Strings(comp)
				out = append(out, comp)
			}
		}
	}
	slices.SortFunc(out, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	return out
}

// Unresolved returns the edges no node stands for: dynamic edges and sources
// whose target could not be resolved, in Edges order, whose site is in one of
// paths (absolute or root-relative; nil or empty means the whole workspace).
// Command edges (Edge.IsCommand) are not listed: a command missing from the
// PATH profile is WB1102's, not part of the graph.
func (s *Snapshot) Unresolved(paths []string) []Edge {
	var want map[string]bool
	if len(paths) > 0 {
		want = map[string]bool{}
		for _, p := range paths {
			if q, ok := s.resolvePath(p); ok {
				want[q] = true
			}
		}
	}
	var out []Edge
	for _, e := range s.Edges() {
		if e.Res != Dynamic && e.Res != Unresolved || e.IsCommand() {
			continue
		}
		if want != nil && !want[e.Site.Path] {
			continue
		}
		out = append(out, e)
	}
	return out
}
