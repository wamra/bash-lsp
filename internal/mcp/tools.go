package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/query"
)

// Bounds on tool arguments.
const (
	maxArgString = 4096 // bytes of one string argument
	maxArgPaths  = 256  // elements of a paths argument
)

// The tools. All read the workspace; none writes a file or starts a program
// of the workspace (search_code starts the user's code-index, see
// query.Search). Every path-like argument goes through query.CheckInside,
// which resolves symlinks, before anything else is done with it.
func init() {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc, "maxLength": maxArgString}
	}
	depth := func(def string) map[string]any {
		return map[string]any{"type": "integer", "minimum": 0, "description": "Levels to follow. " + def + " 0 or more than 64 runs 64 levels and says \"truncated\" when more existed."}
	}
	paths := map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": maxArgString}, "maxItems": maxArgPaths,
		"description": "Files, root-relative or absolute inside the root. Empty or absent: the whole workspace."}
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	const spec = " A symbol is a function name, a root-relative file path, or path:line:col (UTF-16 column) as other answers print them."

	Register(Tool{Name: "find_symbol",
		Description: "Find functions, aliases and file-level variables by name (case-insensitive match; an empty query lists the functions). Each record has path, line, col, kind, name and its source line.",
		InputSchema: obj(map[string]any{"query": str("Name or part of a name.")}, "query"),
		Call:        tool(findSymbol)})
	Register(Tool{Name: "find_references",
		Description: "List the references to a function, with how each call site was resolved." + spec,
		InputSchema: obj(map[string]any{"symbol": str("The function.")}, "symbol"),
		Call:        tool(findReferences)})
	Register(Tool{Name: "get_callers",
		Description: "List the call, source and invoke edges that lead to a function or file, breadth first." + spec,
		InputSchema: obj(map[string]any{"symbol": str("The function or file."), "depth": depth("Default 1 (direct callers).")}, "symbol"),
		Call:        tool(getCallers)})
	Register(Tool{Name: "get_callees",
		Description: "List the call, source and invoke edges that leave a function or file, breadth first." + spec,
		InputSchema: obj(map[string]any{"symbol": str("The function or file."), "depth": depth("Default 1 (direct callees).")}, "symbol"),
		Call:        tool(getCallees)})
	Register(Tool{Name: "trace_dependency",
		Description: "Find the shortest chain of calls, sources and invokes from one symbol to another; found is false when there is none." + spec,
		InputSchema: obj(map[string]any{"from": str("Where the chain starts."), "to": str("Where it ends.")}, "from", "to"),
		Call:        tool(traceDependency)})
	Register(Tool{Name: "analyze_impact",
		Description: "List every function and file that transitively calls, sources or invokes a symbol: what a change to it can affect." + spec,
		InputSchema: obj(map[string]any{"target": str("The function or file."), "depth": depth("Default 0 (all, up to 64 levels).")}, "target"),
		Call:        tool(analyzeImpact)})
	Register(Tool{Name: "list_unresolved",
		Description: "List the dynamic calls and the sources whose target could not be resolved, with the reason.",
		InputSchema: obj(map[string]any{"paths": paths}),
		Call:        tool(listUnresolved)})
	Register(Tool{Name: "get_index_status",
		Description: "Report what the workspace holds: snapshot generation, file counts by dialect, skipped files, limits that were met.",
		InputSchema: obj(map[string]any{}),
		Call:        tool(getIndexStatus)})
	Register(Tool{Name: "search_code",
		Description: "Search by name natively and, when the code-index program is installed, by its full-text index. codeindex.used says which; codeindex.reason says why not.",
		InputSchema: obj(map[string]any{"text": map[string]any{"type": "string", "description": "What to look for.", "maxLength": query.MaxText}}, "text"),
		Call:        tool(searchCode)})
	Register(Tool{Name: "get_diagnostics",
		Description: "List the native findings (syntax, sources, commands, scope, unused names, bash version, dialect) of files; ShellCheck is not run here.",
		InputSchema: obj(map[string]any{"paths": paths}),
		Call:        tool(getDiagnostics)})
}

// tool adapts a function of the session and the raw arguments to a Tool.Call.
func tool(f func(ctx context.Context, s *Session, args json.RawMessage) (any, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		s := SessionFrom(ctx)
		if s == nil {
			return nil, errors.New("no session")
		}
		return f(ctx, s, args)
	}
}

// decode reads the arguments into v: an object with only the known members,
// nothing after it.
func decode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("invalid arguments: more than one value")
	}
	return nil
}

// text checks one string argument; empty is allowed only when allowEmpty.
func text(name, s string, allowEmpty bool) error {
	switch {
	case s == "" && !allowEmpty:
		return fmt.Errorf("%s is required", name)
	case len(s) > maxArgString:
		return fmt.Errorf("%s is longer than %d bytes", name, maxArgString)
	case strings.ContainsRune(s, 0):
		return fmt.Errorf("%s contains a NUL byte", name)
	}
	return nil
}

// symbolArg checks a spec argument and keeps it inside the root.
func symbolArg(s *Session, name, v string) error {
	if err := text(name, v, false); err != nil {
		return err
	}
	return query.CheckInside(s.root, v)
}

func pathsArg(s *Session, ps []string) error {
	if len(ps) > maxArgPaths {
		return fmt.Errorf("paths has more than %d entries", maxArgPaths)
	}
	for _, p := range ps {
		if err := text("a path", p, false); err != nil {
			return err
		}
		if err := query.CheckInside(s.root, p); err != nil {
			return err
		}
	}
	return nil
}

func depthArg(d *int, def int) (int, error) {
	if d == nil {
		return def, nil
	}
	if *d < 0 {
		return 0, errors.New("depth must be 0 or more")
	}
	return *d, nil
}

func findSymbol(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a struct {
		Query *string `json:"query"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Query == nil {
		return nil, errors.New("query is required")
	}
	if err := text("query", *a.Query, true); err != nil {
		return nil, err
	}
	return query.FindSymbol(s.snap, s.root, *a.Query, query.MaxResults), nil
}

func findReferences(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a struct {
		Symbol string `json:"symbol"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := symbolArg(s, "symbol", a.Symbol); err != nil {
		return nil, err
	}
	return query.Refs(s.snap, s.root, a.Symbol, query.MaxResults)
}

type symbolDepth struct {
	Symbol string `json:"symbol"`
	Depth  *int   `json:"depth"`
}

func getCallers(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	return steps(s, args, query.Callers)
}

func getCallees(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	return steps(s, args, query.Callees)
}

func steps(s *Session, args json.RawMessage, f func(*analysis.Snapshot, string, string, int, int) (any, error)) (any, error) {
	var a symbolDepth
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := symbolArg(s, "symbol", a.Symbol); err != nil {
		return nil, err
	}
	d, err := depthArg(a.Depth, 1)
	if err != nil {
		return nil, err
	}
	return f(s.snap, s.root, a.Symbol, d, query.MaxResults)
}

func traceDependency(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	for _, x := range [][2]string{{"from", a.From}, {"to", a.To}} {
		if err := symbolArg(s, x[0], x[1]); err != nil {
			return nil, err
		}
	}
	return query.Trace(s.snap, s.root, a.From, a.To, query.MaxResults)
}

func analyzeImpact(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a struct {
		Target string `json:"target"`
		Depth  *int   `json:"depth"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := symbolArg(s, "target", a.Target); err != nil {
		return nil, err
	}
	d, err := depthArg(a.Depth, 0)
	if err != nil {
		return nil, err
	}
	return query.Impact(s.snap, s.root, a.Target, d, query.MaxResults)
}

type pathsArgs struct {
	Paths []string `json:"paths"`
}

func listUnresolved(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a pathsArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := pathsArg(s, a.Paths); err != nil {
		return nil, err
	}
	return query.Unresolved(s.snap, s.root, a.Paths, query.MaxResults)
}

func getIndexStatus(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a struct{}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	return query.Status(s.snap, s.ws.Report(), s.cfg, query.WithRoot(s.root), query.WithSkipped(s.ws.Skipped())), nil
}

func searchCode(ctx context.Context, s *Session, args json.RawMessage) (any, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := text("text", a.Text, false); err != nil {
		return nil, err
	}
	return query.Search(ctx, s.snap, s.root, a.Text, s.cfg, s.prof), nil
}

func getDiagnostics(_ context.Context, s *Session, args json.RawMessage) (any, error) {
	var a pathsArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if err := pathsArg(s, a.Paths); err != nil {
		return nil, err
	}
	opt := analysis.DiagOptions{Roots: []string{s.root}, Target: s.cfg.Target, Disabled: s.cfg.Disabled, Severity: s.cfg.Severity}
	return query.Diagnostics(s.snap, s.root, a.Paths, opt, query.MaxResults), nil
}
