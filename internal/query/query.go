// Package query holds the answers to graph, symbol, diagnostic and status
// questions as the JSON documents the command line and the MCP adapter both
// print: one encoder, so the same question gives the same bytes on both.
// Nothing here executes or sources analysed code.
package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/redact"
	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/tree"
)

// MaxResults is the most records any list carries; a list that was cut
// carries "truncated": true.
const MaxResults = 10000

// MaxDepth is the deepest graph walk a query runs. A depth of 0 (unlimited)
// or above 64 is run at 64, and the answer carries "truncated": true when
// more levels existed.
const MaxDepth = 64

// maxSignature is the most bytes of a symbol's source line an answer shows.
const maxSignature = 200

// --- output shapes ---

// Node is a file or a function of a file.
type Node struct {
	Path     string  `json:"path"`
	Function *string `json:"function"`
}

// Site is a 1-based line and UTF-16 column.
type Site struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// Edge is one call, source or invoke edge.
type Edge struct {
	Caller     *Node  `json:"caller"`
	Callee     *Node  `json:"callee"`
	Relation   string `json:"relation"`
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
	Site       Site   `json:"site"`
	Definition *Site  `json:"definition,omitempty"` // the callee's definition this edge goes to
	Text       string `json:"text,omitempty"`
	manyDefs   bool   // the callee has several definitions: the text line names Definition
}

// Ref is one reference to a function.
type Ref struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
	Resolution string `json:"resolution,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Relation   string `json:"relation,omitempty"`
}

// Cycle is a group of files that source each other.
type Cycle struct {
	Files []string `json:"files"`
}

// Symbol is one record of FindSymbol: a function, alias or file-level
// variable, with its source line (Signature) cut to 200 bytes.
type Symbol struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Signature string `json:"signature"`
}

// Finding is one record of Diagnostics.
type Finding struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"end_line"`
	EndCol   int    `json:"end_col"`
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Origin   string `json:"origin"`
}

// Doc is an answer that lists records. Found is set by Trace only,
// Truncated only when the list or the walk was cut. Lines is the text form
// the command line prints; it is not part of the JSON.
type Doc struct {
	Generation uint64   `json:"generation"`
	Found      *bool    `json:"found,omitempty"`
	Results    any      `json:"results"`
	Truncated  bool     `json:"truncated,omitempty"`
	Lines      []string `json:"-"`
}

// Len is the number of records of the list; Cut returns a copy of the answer
// with the list cut to k records and "truncated": true. The MCP adapter uses
// them to hold a reply to its size cap by dropping records, never bytes.
func (d *Doc) Len() int {
	if rv := reflect.ValueOf(d.Results); rv.Kind() == reflect.Slice {
		return rv.Len()
	}
	return 0
}

// Cut is described at Len.
func (d *Doc) Cut(k int) any {
	c := *d
	c.Lines = nil
	if rv := reflect.ValueOf(d.Results); rv.Kind() == reflect.Slice && k < rv.Len() {
		c.Results = rv.Slice(0, max(k, 0)).Interface()
		c.Truncated = true
	}
	return &c
}

// Marshal encodes v the way every command and tool prints it: two-space
// indent, a final newline.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := Write(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Write encodes v to w as Marshal does.
func Write(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- errors ---

// AmbiguousError says a spec matched several functions; Lines lists them as
// "path:line:col  name", the specs that name each one.
type AmbiguousError struct {
	Spec  string
	Lines []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q is ambiguous; use one of:\n%s", e.Spec, strings.Join(e.Lines, "\n"))
}

// --- the builder ---

// Builder converts snapshot values to output records for one root.
type Builder struct {
	snap *analysis.Snapshot
	root string
	conv map[string]*tree.Converter // per path, made on first use
}

// NewBuilder returns a Builder for snapshot s; root is the directory paths
// are printed relative to.
func NewBuilder(s *analysis.Snapshot, root string) *Builder {
	return &Builder{snap: s, root: root}
}

// Rel is p relative to the root, with forward slashes.
func (b *Builder) Rel(p string) string {
	if r, err := filepath.Rel(b.root, p); err == nil {
		p = r
	}
	return filepath.ToSlash(p)
}

// Pos is a 1-based line and UTF-16 column of p in path.
func (b *Builder) Pos(path string, p tree.Pos) (line, col int) {
	if ff := b.snap.Facts(path); ff != nil && ff.File != nil {
		// one converter per file: File.ToUTF16 scans the line from its start
		// per call, O(sites x line length) on a long line
		cv := b.conv[path]
		if cv == nil {
			if b.conv == nil {
				b.conv = map[string]*tree.Converter{}
			}
			cv = ff.File.NewConverter()
			b.conv[path] = cv
		}
		l, ch := cv.ToUTF16(p)
		return l + 1, ch + 1
	}
	return p.Line, p.Col
}

// Site is the position of l.
func (b *Builder) Site(l analysis.Location) Site {
	line, col := b.Pos(l.Path, l.Range.Start)
	return Site{b.Rel(l.Path), line, col}
}

// Node is the record of n.
func (b *Builder) Node(n analysis.Node) *Node {
	out := &Node{Path: b.Rel(n.Path)}
	if n.Func != nil {
		name := n.Func.Name
		out.Function = &name
	}
	return out
}

func (b *Builder) callerNode(f analysis.Caller) *Node {
	return b.Node(analysis.Node{Path: f.Path, Func: f.Func})
}

// StepEdge renders a step. A call's definition is the one of the callee
// the edge goes to (Edge.ToDecl, in the file of s.To); the text line names
// it only when the callee has several definitions, to tell them apart.
func (b *Builder) StepEdge(s analysis.Step) Edge {
	e := b.Edge(s.Edge)
	e.Caller, e.Callee = b.Node(s.From), b.Node(s.To)
	if d := s.Edge.ToDecl; d != nil {
		site := b.Site(analysis.Location{Path: s.To.Path, Range: d.NameRange})
		e.Definition = &site
		e.manyDefs = s.To.Func != nil && len(s.To.Func.Decls) > 1
	}
	return e
}

// Edge renders an edge with no callee node (a dynamic or unresolved one).
func (b *Builder) Edge(e analysis.Edge) Edge {
	return Edge{
		Caller:     b.callerNode(e.From),
		Relation:   e.Rel.String(),
		Resolution: e.Res.String(),
		Reason:     e.Reason,
		Site:       b.Site(e.Site),
		Text:       e.Text,
	}
}

// NodeText is the text form of a node: "path" or "path:function".
func NodeText(n *Node) string {
	if n == nil {
		return "-"
	}
	if n.Function == nil {
		return n.Path
	}
	return n.Path + ":" + *n.Function
}

// Line is the text form of an edge.
func (e Edge) Line() string {
	callee := "(unknown)"
	switch {
	case e.Callee != nil:
		callee = NodeText(e.Callee)
	case e.Text != "":
		callee = e.Text
	}
	l := fmt.Sprintf("%s:%d:%d  %s -> %s  %s/%s", e.Site.Path, e.Site.Line, e.Site.Col, NodeText(e.Caller), callee, e.Resolution, e.Reason)
	if e.manyDefs && e.Definition != nil {
		l += fmt.Sprintf("  definition %s:%d:%d", e.Definition.Path, e.Definition.Line, e.Definition.Col)
	}
	return l
}

// DeclLoc is where n is defined: the first declaration of its function in its
// file, the top of the file for a file node.
func (b *Builder) DeclLoc(n analysis.Node) Site {
	top := Site{b.Rel(n.Path), 1, 1}
	ff := b.snap.Facts(n.Path)
	if n.Func == nil || ff == nil {
		return top
	}
	var best *analysis.Decl
	for _, d := range ff.Decls {
		if slices.Contains(n.Func.Decls, d) && (best == nil || d.NameRange.Start.Offset < best.NameRange.Start.Offset) {
			best = d
		}
	}
	if best == nil {
		return top
	}
	return b.Site(analysis.Location{Path: n.Path, Range: best.NameRange})
}

// --- paths and specs ---

// Inside reports whether p, a root-relative or absolute path, lies in root
// once symlinks are resolved (in the longest existing prefix, so a path that
// does not exist yet is judged by where it would be). It returns the
// resolved path.
func Inside(root, p string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, filepath.FromSlash(p))
	}
	abs := resolveExisting(filepath.Clean(p))
	r := resolveExisting(filepath.Clean(root))
	rel, err := filepath.Rel(r, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs, false
	}
	return abs, true
}

// resolveExisting resolves symlinks in the longest existing prefix of p.
func resolveExisting(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(p))
}

// specPath is the file part of a spec: "path:line:col" and "path:line"
// without the numbers.
func specPath(spec string) string {
	p := spec
	if i := strings.LastIndexByte(p, ':'); i >= 0 {
		if _, err := strconv.Atoi(p[i+1:]); err == nil {
			p = p[:i]
			if j := strings.LastIndexByte(p, ':'); j >= 0 {
				if _, err := strconv.Atoi(p[j+1:]); err == nil {
					p = p[:j]
				}
			}
		}
	}
	return p
}

// CheckInside refuses a spec whose file part lies outside root, before the
// snapshot is asked: the root is the only tree these queries read. A spec
// is checked when it is absolute, holds "..", or names something that exists
// under the root (a symlink to a file outside is refused too). A plain
// function name passes.
func CheckInside(root, spec string) error {
	p := specPath(spec)
	if p == "" || strings.ContainsRune(p, 0) {
		return nil
	}
	check := filepath.IsAbs(p) || strings.Contains(p, "..") || strings.ContainsAny(p, `/\`)
	if !check {
		_, err := os.Lstat(filepath.Join(root, p))
		check = err == nil
	}
	if !check {
		return nil
	}
	if _, ok := Inside(root, p); !ok {
		return fmt.Errorf("%s: outside the root %s", spec, root)
	}
	return nil
}

// one resolves spec to exactly one node. Several candidates give an
// *AmbiguousError.
func (b *Builder) one(spec string) (analysis.Node, error) {
	if err := CheckInside(b.root, spec); err != nil {
		return analysis.Node{}, err
	}
	ns, err := b.snap.Lookup(spec)
	if err != nil {
		return analysis.Node{}, err
	}
	if len(ns) > 1 {
		e := &AmbiguousError{Spec: spec}
		for _, x := range ns {
			s := b.DeclLoc(x)
			name := s.Path
			if x.Func != nil {
				name = x.Func.Name
			}
			e.Lines = append(e.Lines, fmt.Sprintf("%s:%d:%d  %s", s.Path, s.Line, s.Col, name))
		}
		return analysis.Node{}, e
	}
	return ns[0], nil
}

// --- caps ---

func capMax(max int) int {
	if max <= 0 || max > MaxResults {
		return MaxResults
	}
	return max
}

// depthFor is the depth a walk runs at and whether the caller's depth was
// cut to get it.
func depthFor(depth int) (run int, cut bool) {
	if depth <= 0 || depth > MaxDepth {
		return MaxDepth, true
	}
	return depth, false
}

// cutList cuts list (a slice) to n records, reports whether it did.
func cutList[T any](list []T, n int) ([]T, bool) {
	if len(list) > n {
		return list[:n], true
	}
	return list, false
}

// --- graph queries ---

// StepsDoc answers Callers (kind "callers") or Callees (anything else) of spec.
func StepsDoc(s *analysis.Snapshot, root, kind, spec string, depth, max int) (*Doc, error) {
	b := NewBuilder(s, root)
	n, err := b.one(spec)
	if err != nil {
		return nil, err
	}
	walk := s.Callees
	if kind == "callers" {
		walk = s.Callers
	}
	run, clamped := depthFor(depth)
	st := walk(n, run)
	deeper := clamped && len(walk(n, run+1)) > len(st)
	return b.stepsDoc(st, max, deeper), nil
}

func (b *Builder) stepsDoc(st []analysis.Step, max int, deeper bool) *Doc {
	edges := []Edge{}
	var lines []string
	seen := map[string]bool{}
	for _, s := range st {
		e := b.StepEdge(s)
		line := e.Line()
		k := line
		if e.Definition != nil {
			k += fmt.Sprintf(" %v", *e.Definition)
		}
		if seen[k] { // one record per (site, target definition)
			continue
		}
		seen[k] = true
		edges = append(edges, e)
		lines = append(lines, line)
	}
	d := &Doc{Generation: b.snap.Generation(), Truncated: deeper}
	var cut bool
	max = capMax(max)
	edges, cut = cutList(edges, max)
	lines, _ = cutList(lines, max)
	d.Results, d.Lines, d.Truncated = edges, lines, d.Truncated || cut
	return d
}

// RefsDoc answers the references to the function spec names.
func RefsDoc(s *analysis.Snapshot, root, spec string, max int) (*Doc, error) {
	b := NewBuilder(s, root)
	n, err := b.one(spec)
	if err != nil {
		return nil, err
	}
	if n.Func == nil {
		return nil, fmt.Errorf("%s: refs wants a function, not a file", spec)
	}
	ff := s.Facts(n.Path)
	var at *analysis.Decl
	if ff != nil {
		for _, d := range ff.Decls {
			if slices.Contains(n.Func.Decls, d) && (at == nil || d.NameRange.Start.Offset < at.NameRange.Start.Offset) {
				at = d
			}
		}
	}
	if at == nil {
		return nil, fmt.Errorf("%s: no definition found in %s", spec, b.Rel(n.Path))
	}
	// The edge to the queried function at a reference site gives it its
	// resolution and reason (another target's edge at the same site, such as
	// the other constant of a "$cmd", says nothing about this one).
	type siteKey struct {
		path string
		off  int
	}
	edgeAt := map[siteKey]analysis.Edge{}
	for _, e := range s.EdgesTo(n.Func) {
		k := siteKey{e.Site.Path, e.Site.Range.Start.Offset}
		if _, ok := edgeAt[k]; !ok {
			edgeAt[k] = e
		}
	}
	refs := []Ref{}
	var lines []string
	for _, l := range s.References(n.Path, at.NameRange.Start.Offset, false) {
		st := b.Site(l)
		r := Ref{Path: st.Path, Line: st.Line, Col: st.Col}
		text := fmt.Sprintf("%s:%d:%d  %s", st.Path, st.Line, st.Col, n.Func.Name)
		if e, ok := edgeAt[siteKey{l.Path, l.Range.Start.Offset}]; ok {
			r.Resolution, r.Reason, r.Relation = e.Res.String(), e.Reason, e.Rel.String()
			text += fmt.Sprintf("  %s  %s/%s", r.Relation, r.Resolution, r.Reason)
		}
		refs = append(refs, r)
		lines = append(lines, text)
	}
	d := &Doc{Generation: s.Generation()}
	max = capMax(max)
	var cut bool
	refs, cut = cutList(refs, max)
	lines, _ = cutList(lines, max)
	d.Results, d.Lines, d.Truncated = refs, lines, cut
	return d, nil
}

// ImpactDoc answers the nodes that depend on spec.
func ImpactDoc(s *analysis.Snapshot, root, spec string, depth, max int) (*Doc, error) {
	b := NewBuilder(s, root)
	n, err := b.one(spec)
	if err != nil {
		return nil, err
	}
	run, clamped := depthFor(depth)
	got := s.Impact(n, run)
	deeper := clamped && len(s.Impact(n, run+1)) > len(got)
	nodes := []*Node{}
	var lines []string
	for _, x := range got {
		jn := b.Node(x)
		nodes = append(nodes, jn)
		lines = append(lines, NodeText(jn))
	}
	d := &Doc{Generation: s.Generation()}
	max = capMax(max)
	var cut bool
	nodes, cut = cutList(nodes, max)
	lines, _ = cutList(lines, max)
	d.Results, d.Lines, d.Truncated = nodes, lines, cut || deeper
	return d, nil
}

// TraceDoc answers the shortest path from a to b; Found says whether there is one.
func TraceDoc(s *analysis.Snapshot, root, a, bs string, max int) (*Doc, error) {
	b := NewBuilder(s, root)
	na, err := b.one(a)
	if err != nil {
		return nil, err
	}
	nb, err := b.one(bs)
	if err != nil {
		return nil, err
	}
	st := s.Trace(na, nb)
	edges := []Edge{}
	for _, x := range st {
		edges = append(edges, b.StepEdge(x))
	}
	found := len(st) > 0
	d := &Doc{Generation: s.Generation(), Found: &found}
	if found {
		d.Lines = b.stepsDoc(st, max, false).Lines
	} else {
		d.Lines = []string{"no path"}
	}
	var cut bool
	edges, cut = cutList(edges, capMax(max))
	d.Results, d.Truncated = edges, cut
	return d, nil
}

// UnresolvedDoc answers the dynamic and unresolved edges whose site is in
// paths (all files when none).
func UnresolvedDoc(s *analysis.Snapshot, root string, paths []string, max int) (*Doc, error) {
	for _, p := range paths {
		if err := CheckInside(root, p); err != nil {
			return nil, err
		}
	}
	b := NewBuilder(s, root)
	edges := []Edge{}
	var lines []string
	for _, e := range s.Unresolved(paths) {
		je := b.Edge(e)
		edges = append(edges, je)
		lines = append(lines, je.Line())
	}
	d := &Doc{Generation: s.Generation()}
	max = capMax(max)
	var cut bool
	edges, cut = cutList(edges, max)
	lines, _ = cutList(lines, max)
	d.Results, d.Lines, d.Truncated = edges, lines, cut
	return d, nil
}

// CyclesDoc answers the groups of files that source each other.
func CyclesDoc(s *analysis.Snapshot, root string, max int) *Doc {
	b := NewBuilder(s, root)
	out := []Cycle{}
	var lines []string
	for _, g := range s.SourceCycles() {
		files := make([]string, len(g))
		for i, p := range g {
			files[i] = b.Rel(p)
		}
		out = append(out, Cycle{files})
		lines = append(lines, strings.Join(files, ", "))
	}
	d := &Doc{Generation: s.Generation()}
	max = capMax(max)
	var cut bool
	out, cut = cutList(out, max)
	lines, _ = cutList(lines, max)
	d.Results, d.Lines, d.Truncated = out, lines, cut
	return d
}

func nilAny(d *Doc, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Callers answers the steps that lead to spec, depth levels deep (1 = direct;
// 0 or more than MaxDepth: MaxDepth, and the answer says when that cut).
// root is the directory paths are printed relative to. The result is a *Doc.
func Callers(s *analysis.Snapshot, root, spec string, depth int, max int) (any, error) {
	return nilAny(StepsDoc(s, root, "callers", spec, depth, max))
}

// Callees is Callers for the steps that leave spec.
func Callees(s *analysis.Snapshot, root, spec string, depth int, max int) (any, error) {
	return nilAny(StepsDoc(s, root, "callees", spec, depth, max))
}

// Refs answers the references to the function spec names.
func Refs(s *analysis.Snapshot, root, spec string, max int) (any, error) {
	return nilAny(RefsDoc(s, root, spec, max))
}

// Impact answers the nodes that transitively depend on spec.
func Impact(s *analysis.Snapshot, root, spec string, depth int, max int) (any, error) {
	return nilAny(ImpactDoc(s, root, spec, depth, max))
}

// Trace answers the shortest path from one spec to another.
func Trace(s *analysis.Snapshot, root, from, to string, max int) (any, error) {
	return nilAny(TraceDoc(s, root, from, to, max))
}

// Unresolved answers the dynamic and unresolved edges in paths.
func Unresolved(s *analysis.Snapshot, root string, paths []string, max int) (any, error) {
	return nilAny(UnresolvedDoc(s, root, paths, max))
}

// --- symbols and diagnostics ---

func kindName(k analysis.DeclKind) string {
	switch k {
	case analysis.DeclFunction:
		return "function"
	case analysis.DeclAlias:
		return "alias"
	}
	return "variable"
}

// signatureWindow is how much of a source line is looked at: a longer line
// is cut there, at its last blank, before redaction.
const signatureWindow = 8192

// sourceLine is the line of src holding offset, trimmed, with credentials
// redacted and then cut to maxSignature bytes at a rune boundary (redacting
// first, so a secret is never cut into a piece the patterns miss).
func sourceLine(src []byte, off int) string {
	if off < 0 || off > len(src) {
		return ""
	}
	start := bytes.LastIndexByte(src[:off], '\n') + 1
	end := len(src)
	if i := bytes.IndexByte(src[off:], '\n'); i >= 0 {
		end = off + i
	}
	line := bytes.TrimSpace(src[start:end])
	if len(line) > signatureWindow {
		line = line[:signatureWindow]
		if i := bytes.LastIndexAny(line, " \t"); i > 0 {
			line = line[:i]
		}
	}
	out := redact.String(strings.ToValidUTF8(string(line), ""))
	if len(out) > maxSignature {
		out = out[:maxSignature]
		for len(out) > 0 && !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
	}
	return out
}

// FindSymbol answers the functions, aliases and file-level variables whose
// name matches query (an empty query lists functions), at most max. The
// result is a *Doc of Symbol.
func FindSymbol(s *analysis.Snapshot, root, query string, max int) any {
	b := NewBuilder(s, root)
	max = capMax(max)
	infos := s.WorkspaceSymbols(query, max+1)
	out := []Symbol{}
	var lines []string
	for _, in := range infos {
		st := b.Site(analysis.Location{Path: in.Path, Range: in.Range})
		sig := ""
		if ff := s.Facts(in.Path); ff != nil && ff.File != nil {
			sig = sourceLine(ff.File.Src, in.Range.Start.Offset)
		}
		out = append(out, Symbol{Path: st.Path, Line: st.Line, Col: st.Col, Kind: kindName(in.Kind), Name: in.Name, Signature: sig})
		lines = append(lines, fmt.Sprintf("%s:%d:%d  %s  %s", st.Path, st.Line, st.Col, kindName(in.Kind), in.Name))
	}
	d := &Doc{Generation: s.Generation()}
	var cut bool
	out, cut = cutList(out, max)
	lines, _ = cutList(lines, max)
	d.Results, d.Lines, d.Truncated = out, lines, cut
	return d
}

func severityName(v analysis.Severity) string {
	switch v {
	case analysis.SevError:
		return "error"
	case analysis.SevWarning:
		return "warning"
	case analysis.SevInfo:
		return "info"
	}
	return "hint"
}

// snapshotPath is the snapshot's spelling of the file p names, "" when the
// snapshot holds no such file.
func snapshotPath(s *analysis.Snapshot, root, p string) string {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, filepath.FromSlash(p))
	}
	abs = filepath.Clean(abs)
	if s.Facts(abs) != nil {
		return abs
	}
	if r, ok := Inside(root, abs); ok && s.Facts(r) != nil {
		return r
	}
	return ""
}

// Diagnostics answers the native findings of the files in paths (every file
// of the snapshot when none), at most max findings; a path the snapshot does
// not hold gives no findings. The result is a *Doc of Finding.
func Diagnostics(s *analysis.Snapshot, root string, paths []string, opt analysis.DiagOptions, max int) any {
	b := NewBuilder(s, root)
	max = capMax(max)
	var files []string
	if len(paths) == 0 {
		files = s.Paths()
		sort.Strings(files)
	} else {
		seen := map[string]bool{}
		for _, p := range paths {
			if q := snapshotPath(s, root, p); q != "" && !seen[q] {
				seen[q] = true
				files = append(files, q)
			}
		}
	}
	out := []Finding{}
	var lines []string
	cut := false
	for _, f := range files {
		for _, d := range s.Diagnostics(f, opt) {
			if len(out) >= max {
				cut = true
				break
			}
			l, c := b.Pos(f, d.Range.Start)
			el, ec := b.Pos(f, d.Range.End)
			rel := b.Rel(f)
			out = append(out, Finding{rel, l, c, el, ec, severityName(d.Severity), d.Rule, d.Message, d.Origin})
			lines = append(lines, fmt.Sprintf("%s:%d:%d  %s  %s  %s", rel, l, c, severityName(d.Severity), d.Rule, d.Message))
		}
		if cut {
			break
		}
	}
	return &Doc{Generation: s.Generation(), Results: out, Truncated: cut, Lines: lines}
}

// --- status ---

// nestingMsg is the parser's error text for input nested past its limit
// (internal/front nestingError).
const nestingMsg = "nesting is deeper than"

// SkippedFile is one file discovery skipped.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// StatusDoc is the answer of Status.
type StatusDoc struct {
	Generation    uint64         `json:"generation"`
	Root          string         `json:"root"`
	Files         int            `json:"files"`
	Dialects      map[string]int `json:"dialects"`
	Skipped       int            `json:"skipped"`
	SkippedFiles  []SkippedFile  `json:"skipped_files"`
	Limits        []string       `json:"limits"`
	NestingErrors int            `json:"nesting_errors"`
	Cache         string         `json:"cache"`
	Truncated     bool           `json:"truncated,omitempty"`
}

// StatusOption adds what the workspace report does not carry.
type StatusOption func(*statusOpts)

type statusOpts struct {
	root    string
	skipped []workspace.Skip
}

// WithRoot sets the root the answer names and prints skipped paths relative to.
func WithRoot(root string) StatusOption { return func(o *statusOpts) { o.root = root } }

// WithSkipped lists the files discovery skipped (Workspace.Skipped).
func WithSkipped(skipped []workspace.Skip) StatusOption {
	return func(o *statusOpts) { o.skipped = skipped }
}

// Status answers what the workspace holds: the file counts of rep, the
// bounds it met (rep.LimitsHit, then the snapshot's own, Snapshot.LimitsHit)
// and how many files nest past the parser's limit. The result is a
// *StatusDoc. cfg is the effective settings; no field of the answer reads it
// yet.
func Status(s *analysis.Snapshot, rep workspace.Report, cfg config.Settings, opts ...StatusOption) any {
	_ = cfg
	var o statusOpts
	for _, f := range opts {
		f(&o)
	}
	b := NewBuilder(s, o.root)
	skipped := []SkippedFile{}
	for _, k := range o.skipped {
		skipped = append(skipped, SkippedFile{b.Rel(k.Path), k.Reason})
	}
	nesting := 0
	paths := s.Paths()
	sort.Strings(paths)
	for _, p := range paths {
		ff := s.Facts(p)
		if ff == nil || ff.File == nil {
			continue
		}
		for _, e := range ff.File.Errors {
			if strings.Contains(e.Msg, nestingMsg) {
				nesting++
				break
			}
		}
	}
	limits := append([]string{}, rep.LimitsHit...)
	limits = append(limits, s.LimitsHit()...)
	dialects := rep.Dialects
	if dialects == nil {
		dialects = map[string]int{}
	}
	return &StatusDoc{s.Generation(), o.root, rep.Files, dialects, rep.Skipped, skipped, limits, nesting, "none", false}
}

// Len is the longer of the two lists of the answer; Cut cuts both to k
// records. See Doc.Len.
func (d *StatusDoc) Len() int { return max(len(d.SkippedFiles), len(d.Limits)) }

// Cut is described at Len.
func (d *StatusDoc) Cut(k int) any {
	c := *d
	if k < len(c.SkippedFiles) {
		c.SkippedFiles, c.Truncated = c.SkippedFiles[:k], true
	}
	if k < len(c.Limits) {
		c.Limits, c.Truncated = c.Limits[:k], true
	}
	return &c
}

// Lines is the text form of the status.
func (d *StatusDoc) Lines() []string {
	out := []string{
		fmt.Sprintf("generation: %d", d.Generation),
		fmt.Sprintf("root: %s", d.Root),
		fmt.Sprintf("files: %d", d.Files),
	}
	var ds []string
	for k := range d.Dialects {
		ds = append(ds, k)
	}
	sort.Strings(ds)
	for _, k := range ds {
		out = append(out, fmt.Sprintf("%s: %d", k, d.Dialects[k]))
	}
	out = append(out, fmt.Sprintf("skipped: %d", d.Skipped))
	for _, s := range d.SkippedFiles {
		out = append(out, fmt.Sprintf("  %s: %s", s.Path, s.Reason))
	}
	for _, l := range d.Limits {
		out = append(out, fmt.Sprintf("limit hit: %s", l))
	}
	out = append(out, fmt.Sprintf("nesting errors: %d", d.NestingErrors), "cache: none")
	return out
}
