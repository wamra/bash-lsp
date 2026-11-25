package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/internal/redact"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// Graph commands: status, callers, callees, refs, impact, trace, unresolved
// and cycles (spec 5.2). Each loads the root with workspace.Open, asks one
// snapshot through internal/query and prints. Nothing here executes or sources analysed code.

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

// graphCtx is one loaded root and its snapshot. The answers are built by
// internal/query, which the MCP adapter shares.
type graphCtx struct {
	ws   *workspace.Workspace
	snap *analysis.Snapshot
	root string
}

func cmdGraph(cmd string, args []string, stdout, stderr io.Writer) int {
	sp := graphSpecs[cmd]
	pos, o, err := parseGraphArgs(cmd, sp, args)
	if err != nil {
		return graphUsageError(stderr, err.Error())
	}
	ws, err := openWorkspace(o.root, stderr)
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

// --- output ---

// emit prints an answer: its JSON document, or one text line per record. A
// text answer that was cut says so on stderr.
func (c *graphCtx) emit(o graphOpts, stdout, stderr io.Writer, d *query.Doc) int {
	if o.json {
		if err := query.Write(stdout, d); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, l := range d.Lines {
		fmt.Fprintln(stdout, l)
	}
	if d.Truncated {
		fmt.Fprintf(stderr, "bash-lsp: the answer was cut (at most %d results, depth %d)\n", query.MaxResults, query.MaxDepth)
	}
	return 0
}

// failQuery reports an error of internal/query: a spec that names several
// functions lists them (exit 2), anything else is a failure (exit 1).
func failQuery(stderr io.Writer, err error) int {
	var amb *query.AmbiguousError
	if errors.As(err, &amb) {
		fmt.Fprint(stderr, redact.String(fmt.Sprintf("bash-lsp: %v\n", amb)))
		return 2
	}
	fmt.Fprint(stderr, redact.String(fmt.Sprintf("bash-lsp: %v\n", err)))
	return 1
}

// --- commands ---

func (c *graphCtx) steps(cmd, spec string, o graphOpts, stdout, stderr io.Writer) int {
	d, err := query.StepsDoc(c.snap, c.root, cmd, spec, o.depth, 0)
	if err != nil {
		return failQuery(stderr, err)
	}
	return c.emit(o, stdout, stderr, d)
}

func (c *graphCtx) refs(spec string, o graphOpts, stdout, stderr io.Writer) int {
	d, err := query.RefsDoc(c.snap, c.root, spec, 0)
	if err != nil {
		return failQuery(stderr, err)
	}
	return c.emit(o, stdout, stderr, d)
}

func (c *graphCtx) impact(spec string, o graphOpts, stdout, stderr io.Writer) int {
	d, err := query.ImpactDoc(c.snap, c.root, spec, o.depth, 0)
	if err != nil {
		return failQuery(stderr, err)
	}
	return c.emit(o, stdout, stderr, d)
}

func (c *graphCtx) trace(a, b string, o graphOpts, stdout, stderr io.Writer) int {
	d, err := query.TraceDoc(c.snap, c.root, a, b, 0)
	if err != nil {
		return failQuery(stderr, err)
	}
	return c.emit(o, stdout, stderr, d)
}

func (c *graphCtx) unresolved(paths []string, o graphOpts, stdout, stderr io.Writer) int {
	d, err := query.UnresolvedDoc(c.snap, c.root, paths, 0)
	if err != nil {
		return failQuery(stderr, err)
	}
	return c.emit(o, stdout, stderr, d)
}

func (c *graphCtx) cycles(o graphOpts, stdout, stderr io.Writer) int {
	return c.emit(o, stdout, stderr, query.CyclesDoc(c.snap, c.root, 0))
}

func (c *graphCtx) status(o graphOpts, stdout, stderr io.Writer) int {
	d := query.Status(c.snap, c.ws.Report(), config.Settings{}, query.WithRoot(c.root), query.WithSkipped(c.ws.Skipped())).(*query.StatusDoc)
	if o.json {
		if err := query.Write(stdout, d); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, l := range d.Lines() {
		fmt.Fprintln(stdout, l)
	}
	return 0
}
