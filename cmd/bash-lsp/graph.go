package main

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/tree"
)

// Graph commands: status, callers, callees, refs, impact, trace, unresolved
// and cycles (spec 5.2). Each loads the root with workspace.Open, asks one
// snapshot and prints. Nothing here executes or sources analysed code.

const graphUsage = "usage: bash-lsp status [--root DIR] [--json] | callers|callees SYMBOL [--depth N] [--root DIR] [--json] | refs SYMBOL | " +
	"impact SYMBOL|FILE [--depth N] | trace A B | unresolved [PATH...] | cycles   (all take --root DIR and --json)"

// graphOpts are the flags the graph commands share.
type graphOpts struct {
	json  bool
	root  string
	depth int // -1: not given
}

// graphSpec says what a command accepts: its positional count (max < 0: any
// number) and whether --depth is allowed.
type graphSpec struct {
	min, max int
	depth    bool
	defDepth int
}

var graphSpecs = map[string]graphSpec{
	"status":     {0, 0, false, 0},
	"callers":    {1, 1, true, 1},
	"callees":    {1, 1, true, 1},
	"refs":       {1, 1, false, 0},
	"impact":     {1, 1, true, 0},
	"trace":      {2, 2, false, 0},
	"unresolved": {0, -1, false, 0},
	"cycles":     {0, 0, false, 0},
}

func graphUsageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "bash-lsp: %s\n%s\n", msg, graphUsage)
	return 2
}

func parseGraphArgs(cmd string, sp graphSpec, args []string) (pos []string, o graphOpts, err error) {
	o = graphOpts{root: ".", depth: -1}
	value := func(i *int, name, inline string, hasInline bool) (string, error) {
		if hasInline {
			return inline, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := strings.Cut(a, "=")
		switch {
		case a == "--json":
			o.json = true
		case name == "--root":
			if o.root, err = value(&i, "--root", inline, hasInline); err != nil {
				return nil, o, err
			}
		case name == "--depth" && sp.depth:
			v, err := value(&i, "--depth", inline, hasInline)
			if err != nil {
				return nil, o, err
			}
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 0 {
				return nil, o, fmt.Errorf("--depth wants a number of levels, 0 or more, got %q", v)
			}
			o.depth = n
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, o, fmt.Errorf("unknown flag %q for %s", a, cmd)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < sp.min || (sp.max >= 0 && len(pos) > sp.max) {
		return nil, o, fmt.Errorf("%s: wrong number of arguments", cmd)
	}
	if o.depth < 0 {
		o.depth = sp.defDepth
	}
	return pos, o, nil
}

// graphCtx is one loaded root and its snapshot.
type graphCtx struct {
	ws   *workspace.Workspace
	snap *analysis.Snapshot
	root string
	conv map[string]*tree.Converter // per path, made on first use (pos)
}

func cmdGraph(cmd string, args []string, stdout, stderr io.Writer) int {
	sp := graphSpecs[cmd]
	pos, o, err := parseGraphArgs(cmd, sp, args)
	if err != nil {
		return graphUsageError(stderr, err.Error())
	}
	ws, err := workspace.Open(o.root, workspace.Limits{})
	if err != nil {
		return fail(stderr, err)
	}
	c := &graphCtx{ws: ws, snap: ws.Snapshot(), root: ws.Root()}
	switch cmd {
	case "status":
		return c.status(o, stdout, stderr)
	case "callers", "callees":
		return c.steps(cmd, pos[0], o, stdout, stderr)
	case "refs":
		return c.refs(pos[0], o, stdout, stderr)
	case "impact":
		return c.impact(pos[0], o, stdout, stderr)
	case "trace":
		return c.trace(pos[0], pos[1], o, stdout, stderr)
	case "unresolved":
		return c.unresolved(pos, o, stdout, stderr)
	default:
		return c.cycles(o, stdout, stderr)
	}
}

// --- output shapes ---

type jsonNode struct {
	Path     string  `json:"path"`
	Function *string `json:"function"`
}

type jsonSite struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

type jsonEdge struct {
	Caller     *jsonNode `json:"caller"`
	Callee     *jsonNode `json:"callee"`
	Relation   string    `json:"relation"`
	Resolution string    `json:"resolution"`
	Reason     string    `json:"reason"`
	Site       jsonSite  `json:"site"`
	Text       string    `json:"text,omitempty"`
}

func (c *graphCtx) rel(p string) string { return relSlash(c.root, p) }

// pos is a 1-based line and UTF-16 column of p in path.
func (c *graphCtx) pos(path string, p tree.Pos) (line, col int) {
	if ff := c.snap.Facts(path); ff != nil && ff.File != nil {
		// one converter per file: File.ToUTF16 scans the line from its start
		// per call, O(sites x line length) on a long line
		cv := c.conv[path]
		if cv == nil {
			if c.conv == nil {
				c.conv = map[string]*tree.Converter{}
			}
			cv = ff.File.NewConverter()
			c.conv[path] = cv
		}
		l, ch := cv.ToUTF16(p)
		return l + 1, ch + 1
	}
	return p.Line, p.Col
}

func (c *graphCtx) site(l analysis.Location) jsonSite {
	line, col := c.pos(l.Path, l.Range.Start)
	return jsonSite{c.rel(l.Path), line, col}
}

func (c *graphCtx) node(n analysis.Node) *jsonNode {
	out := &jsonNode{Path: c.rel(n.Path)}
	if n.Func != nil {
		name := n.Func.Name
		out.Function = &name
	}
	return out
}

func nodeText(n *jsonNode) string {
	if n == nil {
		return "-"
	}
	if n.Function == nil {
		return n.Path
	}
	return n.Path + ":" + *n.Function
}

func (c *graphCtx) callerNode(f analysis.Caller) *jsonNode {
	return c.node(analysis.Node{Path: f.Path, Func: f.Func})
}

func (c *graphCtx) stepEdge(s analysis.Step) jsonEdge {
	e := c.edge(s.Edge)
	e.Caller, e.Callee = c.node(s.From), c.node(s.To)
	return e
}

func (c *graphCtx) edge(e analysis.Edge) jsonEdge {
	return jsonEdge{
		Caller:     c.callerNode(e.From),
		Relation:   e.Rel.String(),
		Resolution: e.Res.String(),
		Reason:     e.Reason,
		Site:       c.site(e.Site),
		Text:       e.Text,
	}
}

func (e jsonEdge) line() string {
	callee := "(unknown)"
	switch {
	case e.Callee != nil:
		callee = nodeText(e.Callee)
	case e.Text != "":
		callee = e.Text
	}
	return fmt.Sprintf("%s:%d:%d  %s -> %s  %s/%s", e.Site.Path, e.Site.Line, e.Site.Col, nodeText(e.Caller), callee, e.Resolution, e.Reason)
}

// emit prints the results: one JSON object with generation and results, or
// one text line each.
func (c *graphCtx) emit(o graphOpts, stdout, stderr io.Writer, results any, lines []string) int {
	if o.json {
		if err := writeJSON(stdout, struct {
			Generation uint64 `json:"generation"`
			Results    any    `json:"results"`
		}{c.snap.Generation(), results}); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	return 0
}

// --- resolving specs ---

// checkInside refuses a spec whose file part lies outside the root, before
// the snapshot is asked: the root is the only tree these commands read.
func (c *graphCtx) checkInside(spec string) error {
	p := spec
	if i := strings.LastIndexByte(p, ':'); i >= 0 { // trim :line:col
		if _, err := strconv.Atoi(p[i+1:]); err == nil {
			p = p[:i]
			if j := strings.LastIndexByte(p, ':'); j >= 0 {
				if _, err := strconv.Atoi(p[j+1:]); err == nil {
					p = p[:j]
				}
			}
		}
	}
	if !filepath.IsAbs(p) && !strings.Contains(p, "..") {
		return nil
	}
	abs := p
	if filepath.IsAbs(p) {
		abs = c.ws.Canon(p)
	} else {
		abs = filepath.Join(c.root, filepath.FromSlash(p))
	}
	if r, err := filepath.Rel(c.root, abs); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: outside the root %s", spec, c.root)
	}
	return nil
}

// declLoc is where n is defined: the first declaration of its function in its
// file, the top of the file for a file node.
func (c *graphCtx) declLoc(n analysis.Node) jsonSite {
	top := jsonSite{c.rel(n.Path), 1, 1}
	ff := c.snap.Facts(n.Path)
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
	return c.site(analysis.Location{Path: n.Path, Range: best.NameRange})
}

// one resolves spec to exactly one node. Several candidates are listed on
// stderr (exit 2), no match is a failure (exit 1). code is 0 when ok.
func (c *graphCtx) one(spec string, stderr io.Writer) (n analysis.Node, code int) {
	if err := c.checkInside(spec); err != nil {
		return n, fail(stderr, err)
	}
	ns, err := c.snap.Lookup(spec)
	if err != nil {
		return n, fail(stderr, err)
	}
	if len(ns) > 1 {
		fmt.Fprintf(stderr, "bash-lsp: %q is ambiguous; use one of:\n", spec)
		for _, x := range ns {
			s := c.declLoc(x)
			name := s.Path
			if x.Func != nil {
				name = x.Func.Name
			}
			fmt.Fprintf(stderr, "%s:%d:%d  %s\n", s.Path, s.Line, s.Col, name)
		}
		return n, 2
	}
	return ns[0], 0
}

// --- commands ---

func (c *graphCtx) steps(cmd, spec string, o graphOpts, stdout, stderr io.Writer) int {
	n, code := c.one(spec, stderr)
	if code != 0 {
		return code
	}
	var st []analysis.Step
	if cmd == "callers" {
		st = c.snap.Callers(n, o.depth)
	} else {
		st = c.snap.Callees(n, o.depth)
	}
	return c.emitSteps(st, o, stdout, stderr)
}

func (c *graphCtx) emitSteps(st []analysis.Step, o graphOpts, stdout, stderr io.Writer) int {
	edges := []jsonEdge{}
	var lines []string
	for _, s := range st {
		e := c.stepEdge(s)
		edges = append(edges, e)
		lines = append(lines, e.line())
	}
	return c.emit(o, stdout, stderr, edges, lines)
}

type jsonRef struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
	Resolution string `json:"resolution,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Relation   string `json:"relation,omitempty"`
}

func (c *graphCtx) refs(spec string, o graphOpts, stdout, stderr io.Writer) int {
	n, code := c.one(spec, stderr)
	if code != 0 {
		return code
	}
	if n.Func == nil {
		return fail(stderr, fmt.Errorf("%s: refs wants a function, not a file", spec))
	}
	ff := c.snap.Facts(n.Path)
	var at *analysis.Decl
	if ff != nil {
		for _, d := range ff.Decls {
			if slices.Contains(n.Func.Decls, d) && (at == nil || d.NameRange.Start.Offset < at.NameRange.Start.Offset) {
				at = d
			}
		}
	}
	if at == nil {
		return fail(stderr, fmt.Errorf("%s: no definition found in %s", spec, c.rel(n.Path)))
	}
	// The edge to the queried function at a reference site gives it its
	// resolution and reason (another target's edge at the same site, such as
	// the other constant of a "$cmd", says nothing about this one).
	type siteKey struct {
		path string
		off  int
	}
	edgeAt := map[siteKey]analysis.Edge{}
	for _, e := range c.snap.EdgesTo(n.Func) {
		k := siteKey{e.Site.Path, e.Site.Range.Start.Offset}
		if _, ok := edgeAt[k]; !ok {
			edgeAt[k] = e
		}
	}
	refs := []jsonRef{}
	var lines []string
	for _, l := range c.snap.References(n.Path, at.NameRange.Start.Offset, false) {
		s := c.site(l)
		r := jsonRef{Path: s.Path, Line: s.Line, Col: s.Col}
		text := fmt.Sprintf("%s:%d:%d  %s", s.Path, s.Line, s.Col, n.Func.Name)
		if e, ok := edgeAt[siteKey{l.Path, l.Range.Start.Offset}]; ok {
			r.Resolution, r.Reason, r.Relation = e.Res.String(), e.Reason, e.Rel.String()
			text += fmt.Sprintf("  %s  %s/%s", r.Relation, r.Resolution, r.Reason)
		}
		refs = append(refs, r)
		lines = append(lines, text)
	}
	return c.emit(o, stdout, stderr, refs, lines)
}

func (c *graphCtx) impact(spec string, o graphOpts, stdout, stderr io.Writer) int {
	n, code := c.one(spec, stderr)
	if code != 0 {
		return code
	}
	nodes := []*jsonNode{}
	var lines []string
	for _, x := range c.snap.Impact(n, o.depth) {
		jn := c.node(x)
		nodes = append(nodes, jn)
		lines = append(lines, nodeText(jn))
	}
	return c.emit(o, stdout, stderr, nodes, lines)
}

func (c *graphCtx) trace(a, b string, o graphOpts, stdout, stderr io.Writer) int {
	na, code := c.one(a, stderr)
	if code != 0 {
		return code
	}
	nb, code := c.one(b, stderr)
	if code != 0 {
		return code
	}
	st := c.snap.Trace(na, nb)
	if !o.json {
		if len(st) == 0 {
			fmt.Fprintln(stdout, "no path")
			return 0
		}
		return c.emitSteps(st, o, stdout, stderr)
	}
	edges := []jsonEdge{}
	for _, s := range st {
		edges = append(edges, c.stepEdge(s))
	}
	if err := writeJSON(stdout, struct {
		Generation uint64     `json:"generation"`
		Found      bool       `json:"found"`
		Results    []jsonEdge `json:"results"`
	}{c.snap.Generation(), len(st) > 0, edges}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func (c *graphCtx) unresolved(paths []string, o graphOpts, stdout, stderr io.Writer) int {
	for _, p := range paths {
		if err := c.checkInside(p); err != nil {
			return fail(stderr, err)
		}
	}
	edges := []jsonEdge{}
	var lines []string
	for _, e := range c.snap.Unresolved(paths) {
		je := c.edge(e)
		edges = append(edges, je)
		lines = append(lines, je.line())
	}
	return c.emit(o, stdout, stderr, edges, lines)
}

func (c *graphCtx) cycles(o graphOpts, stdout, stderr io.Writer) int {
	type cycle struct {
		Files []string `json:"files"`
	}
	out := []cycle{}
	var lines []string
	for _, g := range c.snap.SourceCycles() {
		files := make([]string, len(g))
		for i, p := range g {
			files[i] = c.rel(p)
		}
		out = append(out, cycle{files})
		lines = append(lines, strings.Join(files, ", "))
	}
	return c.emit(o, stdout, stderr, out, lines)
}

// nestingMsg is the parser's error text for input nested past its limit
// (internal/front nestingError).
const nestingMsg = "nesting is deeper than"

type skippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (c *graphCtx) status(o graphOpts, stdout, stderr io.Writer) int {
	rep := c.ws.Report()
	skipped := []skippedFile{}
	for _, k := range c.ws.Skipped() {
		skipped = append(skipped, skippedFile{c.rel(k.Path), k.Reason})
	}
	nesting := 0
	paths := c.snap.Paths()
	sort.Strings(paths)
	for _, p := range paths {
		ff := c.snap.Facts(p)
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
	limits := rep.LimitsHit
	if limits == nil {
		limits = []string{}
	}
	dialects := rep.Dialects
	if dialects == nil {
		dialects = map[string]int{}
	}
	if o.json {
		if err := writeJSON(stdout, struct {
			Generation    uint64         `json:"generation"`
			Root          string         `json:"root"`
			Files         int            `json:"files"`
			Dialects      map[string]int `json:"dialects"`
			Skipped       int            `json:"skipped"`
			SkippedFiles  []skippedFile  `json:"skipped_files"`
			Limits        []string       `json:"limits"`
			NestingErrors int            `json:"nesting_errors"`
			Cache         string         `json:"cache"`
		}{c.snap.Generation(), c.root, rep.Files, dialects, rep.Skipped, skipped, limits, nesting, "none"}); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "generation: %d\n", c.snap.Generation())
	fmt.Fprintf(stdout, "root: %s\n", c.root)
	fmt.Fprintf(stdout, "files: %d\n", rep.Files)
	var ds []string
	for d := range dialects {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	for _, d := range ds {
		fmt.Fprintf(stdout, "%s: %d\n", d, dialects[d])
	}
	fmt.Fprintf(stdout, "skipped: %d\n", rep.Skipped)
	for _, s := range skipped {
		fmt.Fprintf(stdout, "  %s: %s\n", s.Path, s.Reason)
	}
	for _, l := range limits {
		fmt.Fprintf(stdout, "limit hit: %s\n", l)
	}
	fmt.Fprintf(stdout, "nesting errors: %d\n", nesting)
	fmt.Fprintln(stdout, "cache: none")
	return 0
}
