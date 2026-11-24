package lsp

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

// ---- call hierarchy ----
//
// An item is a graph node (analysis.Node): a function as defined in one file,
// or the top-level code of a file. Its data names the node by path and
// function name, so a follow-up request finds it in whatever generation is
// current; a node that no longer exists answers an empty list.

// itemData is the data of a call-hierarchy item. Name is "" for a file's
// top-level item.
type itemData struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// relPath is p relative to the root of the workspace folder that holds it,
// with forward slashes; the whole path outside every folder.
func (s *server) relPath(p string) string {
	if f := s.folderOf(p); f != nil {
		if rel, ok := under(f.ws.Root(), p); ok {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// item makes the item of node n. labels, when not empty, are the
// resolution/reason labels of the calls the item stands in for.
func (s *server) item(cs converters, snap *analysis.Snapshot, n analysis.Node, labels []string) callHierarchyItem {
	it := callHierarchyItem{Detail: s.relPath(n.Path), URI: s.uriOf(n.Path)}
	if len(labels) > 0 {
		it.Detail += " · " + strings.Join(labels, ", ")
	}
	data := itemData{Path: n.Path}
	if d := snap.FuncDecl(n); d != nil {
		it.Name, it.Kind, data.Name = n.Func.Name, kindFunction, n.Func.Name
		it.Range, it.SelectionRange = s.rangeIn(cs, snap, n.Path, d.Node.Range), s.rangeIn(cs, snap, n.Path, d.NameRange)
	} else {
		it.Name, it.Kind = s.relPath(n.Path), kindFile
		if f := fileOf(snap, n.Path); f != nil {
			whole := tree.Range{Start: f.PosAt(0), End: f.PosAt(len(f.Src))}
			it.Range = cs.rangeOf(f, whole)
			it.SelectionRange = Range{Start: it.Range.Start, End: it.Range.Start}
		}
	}
	it.Data, _ = json.Marshal(data)
	return it
}

// nodeOf finds the node an item stands for, and the snapshot of the
// workspace that holds it: by its data, else by its uri and (for a
// function) name. The data path is looked up in the snapshots (no file is
// touched): only a path the server produced names a file a snapshot holds.
// ok is false when the node is gone.
func (s *server) nodeOf(it callHierarchyItem) (*analysis.Snapshot, analysis.Node, bool) {
	var d itemData
	if len(it.Data) > 0 && json.Unmarshal(it.Data, &d) != nil {
		d = itemData{}
	}
	var snap *analysis.Snapshot
	if d.Path == "" {
		be, p, rerr := s.locate(it.URI)
		if rerr != nil {
			return nil, analysis.Node{}, false
		}
		snap, d.Path = be.Snapshot(), p
		if it.Kind != kindFile {
			d.Name = it.Name
		}
	} else {
		snap = s.snapshotHolding(d.Path)
	}
	if snap.Facts(d.Path) == nil {
		return snap, analysis.Node{}, false
	}
	if d.Name == "" {
		return snap, analysis.Node{Path: d.Path}, true
	}
	n, ok := snap.FuncNode(d.Path, d.Name)
	return snap, n, ok
}

func (s *server) prepareCallHierarchy(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return nil, rerr
	}
	if sym, ok := snap.SymbolAt(path, off); !ok || sym.Kind != analysis.DeclFunction {
		// Not on a function name: outside every function the file's
		// top-level item; inside a function body nothing.
		if snap.CallerAt(path, off).Func != nil {
			return nil, nil
		}
		return []callHierarchyItem{s.item(converters{}, snap, analysis.Node{Path: path}, nil)}, nil
	}
	// The function(s) the name at the cursor stands for: one per definition
	// file, several when the call's targets are ambiguous.
	var nodes []analysis.Node
	seen := map[analysis.Node]bool{}
	for _, l := range snap.Definition(path, off) {
		sym, ok := snap.SymbolAt(l.Path, l.Range.Start.Offset)
		if !ok || sym.Kind != analysis.DeclFunction {
			continue
		}
		if n := (analysis.Node{Path: l.Path, Func: sym}); !seen[n] {
			seen[n] = true
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	cs := converters{}
	out := make([]callHierarchyItem, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.item(cs, snap, n, nil))
	}
	return out, nil
}

// callGroup is the calls between the requested node and one other node:
// the sites (one file, document order, each once) and their edges' labels.
type callGroup struct {
	node   analysis.Node
	sites  []tree.Range
	last   int // offset of the last site added
	labels []string
	worst  []analysis.Resolution // per label
}

// groupCalls groups calls, invokes and sources steps (the steps `bash-lsp
// callers` and `callees` list; FDS-012 takes the call hierarchy from the
// FDS-010 edges) by the node at the other end: From for incoming, To for
// outgoing. Steps come in site order, so groups are in the order of their
// first site.
func groupCalls(steps []analysis.Step, incoming bool) []*callGroup {
	var groups []*callGroup
	byNode := map[analysis.Node]*callGroup{}
	for _, st := range steps {
		if st.Edge.Rel != analysis.RelCalls && st.Edge.Rel != analysis.RelInvokes && st.Edge.Rel != analysis.RelSources {
			continue
		}
		other := st.To
		if incoming {
			other = st.From
		}
		g := byNode[other]
		if g == nil {
			g = &callGroup{node: other, last: -1}
			byNode[other] = g
			groups = append(groups, g)
		}
		if r := st.Edge.Site.Range; r.Start.Offset != g.last {
			g.sites = append(g.sites, r)
			g.last = r.Start.Offset
		}
		e := st.Edge
		if e.Res == analysis.Resolved && e.Reason == analysis.ReasonDirect {
			continue
		}
		if l := e.Res.String() + "/" + e.Reason; !slices.Contains(g.labels, l) {
			g.labels = append(g.labels, l)
			g.worst = append(g.worst, e.Res)
		}
	}
	for _, g := range groups {
		// weakest first (candidate before resolved), then in site order
		idx := make([]int, len(g.labels))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return g.worst[idx[a]] > g.worst[idx[b]] })
		sorted := make([]string, len(idx))
		for i, k := range idx {
			sorted[i] = g.labels[k]
		}
		g.labels = sorted
	}
	return groups
}

func (s *server) callsOf(raw json.RawMessage, incoming bool) (snap *analysis.Snapshot, n analysis.Node, groups []*callGroup, rerr *rpcError) {
	var p callHierarchyCallsParams
	if rerr = decode(raw, &p); rerr != nil {
		return
	}
	s.wait()
	snap, n, ok := s.nodeOf(p.Item)
	if !ok {
		return snap, n, nil, nil
	}
	if incoming {
		return snap, n, groupCalls(snap.Callers(n, 1), true), nil
	}
	return snap, n, groupCalls(snap.Callees(n, 1), false), nil
}

func (s *server) incomingCalls(raw json.RawMessage) (any, *rpcError) {
	snap, _, groups, rerr := s.callsOf(raw, true)
	if rerr != nil {
		return nil, rerr
	}
	cs := converters{}
	out := make([]callHierarchyIncomingCall, 0, len(groups))
	for _, g := range groups {
		out = append(out, callHierarchyIncomingCall{From: s.item(cs, snap, g.node, g.labels),
			FromRanges: s.rangesIn(cs, snap, g.node.Path, g.sites)})
	}
	return out, nil
}

func (s *server) outgoingCalls(raw json.RawMessage) (any, *rpcError) {
	snap, n, groups, rerr := s.callsOf(raw, false)
	if rerr != nil {
		return nil, rerr
	}
	cs := converters{}
	out := make([]callHierarchyOutgoingCall, 0, len(groups))
	for _, g := range groups {
		out = append(out, callHierarchyOutgoingCall{To: s.item(cs, snap, g.node, g.labels),
			FromRanges: s.rangesIn(cs, snap, n.Path, g.sites)})
	}
	return out, nil
}

// rangesIn converts the ranges of one file through the request's converters.
func (s *server) rangesIn(cs converters, snap *analysis.Snapshot, path string, rs []tree.Range) []Range {
	out := make([]Range, len(rs))
	for i, r := range rs {
		out[i] = s.rangeIn(cs, snap, path, r)
	}
	return out
}
