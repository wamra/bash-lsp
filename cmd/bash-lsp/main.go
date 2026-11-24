// Command bash-lsp is a Bash language server and analysis CLI.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/lsp"
	"github.com/wamra/bash-lsp/internal/redact"
	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = "usage: bash-lsp [serve|version|ast FILE [--json]|symbols [PATH...] [--json]|doctor [PATH]|check [paths] [--format text|json] [--shellcheck auto|off] [--fail-on error|warning|info|hint] [--root DIR]|fmt [-w] FILE...|extract [--root DIR] [--list-file FILE]|status|callers|callees|refs|impact|trace|unresolved|cycles|search TEXT|mcp]"

// Exit codes: 0 success, 1 runtime failure, 2 usage error.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// No arguments means serve: editors start the binary bare.
	if len(args) == 0 {
		args = []string{"serve"}
	}
	if isHelp(args) {
		if args[0] == "check" || len(args) > 1 && args[0] == "help" && args[1] == "check" {
			fmt.Fprintln(stdout, checkUsage)
		} else {
			fmt.Fprintln(stdout, usage)
		}
		return 0
	}
	switch args[0] {
	case "serve":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "bash-lsp: serve takes no arguments\n%s\n", usage)
			return 2
		}
		logw := redact.Writer(stderr) // credential-looking text never reaches the log
		err := lsp.Serve(stdin, stdout, logw, version)
		if f, ok := logw.(interface{ Flush() error }); ok {
			f.Flush()
		}
		if err != nil {
			fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
			return 1
		}
		return 0
	case "version":
		fmt.Fprintf(stdout, "bash-lsp %s\n", version)
		return 0
	case "ast":
		return cmdAst(args[1:], stdout, stderr)
	case "symbols":
		return cmdSymbols(args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "fmt":
		return cmdFmt(args[1:], stdout, stderr)
	case "extract":
		return cmdExtract(args[1:], stdout, stderr)
	case "search":
		return cmdSearch(args[1:], stdout, stderr)
	case "mcp":
		return cmdMCP(args[1:], stdin, stdout, stderr)
	case "status", "callers", "callees", "refs", "impact", "trace", "unresolved", "cycles":
		return cmdGraph(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "bash-lsp: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// isHelp reports whether args ask for the usage: "help" or a help flag first,
// or "help"/"-h"/"--help" anywhere before a "--".
func isHelp(args []string) bool {
	switch args[0] {
	case "help", "--help", "-h":
		return true
	}
	for _, a := range args[1:] {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// parseArgs splits args into positionals and the --json flag. Any other
// dash-prefixed argument is a usage error. allowJSON false rejects --json.
func parseArgs(args []string, allowJSON bool) (pos []string, asJSON bool, err error) {
	for _, a := range args {
		switch {
		case a == "--json" && allowJSON:
			asJSON = true
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, false, fmt.Errorf("unknown flag %q", a)
		default:
			pos = append(pos, a)
		}
	}
	return pos, asJSON, nil
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "bash-lsp: %s\n%s\n", msg, usage)
	return 2
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
	return 1
}

func writeJSON(stdout io.Writer, v any) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// relSlash is p relative to root with forward slashes on every OS.
func relSlash(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		p = r
	}
	return filepath.ToSlash(p)
}

// pos16 is p as a 1-based line and UTF-16 column, the position every command
// prints and every "path:line:col" spec reads.
//
// c is one converter per file and command: File.ToUTF16 scans the line from
// its start, so a long line with many printed positions cost O(positions x
// line length).
func pos16(c *tree.Converter, p tree.Pos) [2]int {
	l, ch := c.ToUTF16(p)
	return [2]int{l + 1, ch + 1}
}

// --- ast ---

type astNode struct {
	Kind      string    `json:"kind"`
	Start     [2]int    `json:"start"`
	End       [2]int    `json:"end"`
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	Recovered bool      `json:"recovered"`
	Children  []astNode `json:"children"`
}

type astError struct {
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Msg  string `json:"msg"`
}

type astDoc struct {
	Path    string     `json:"path"`
	Dialect string     `json:"dialect"`
	Errors  []astError `json:"errors"`
	Root    astNode    `json:"root"`
}

// astView is a tree node plus its children with comments merged in: the
// parser keeps comments in File.Comments, outside the node tree.
type astView struct {
	n    *tree.Node
	kids []*astView
}

// buildView builds the view of n. byParent maps a node to the comments that
// belong directly under it, in source order.
func buildView(n *tree.Node, byParent map[*tree.Node][]*tree.Node) *astView {
	v := &astView{n: n}
	cs := byParent[n]
	i := 0
	for _, c := range n.Children {
		// A comment goes before the first child that starts after it.
		for i < len(cs) && cs[i].Range.Start.Offset < c.Range.Start.Offset {
			v.kids = append(v.kids, &astView{n: cs[i]})
			i++
		}
		v.kids = append(v.kids, buildView(c, byParent))
	}
	for ; i < len(cs); i++ {
		v.kids = append(v.kids, &astView{n: cs[i]})
	}
	return v
}

// fileView merges File.Comments into the node tree in one pass. Each comment
// goes under the deepest node whose range holds it (found with the indexed
// tree.Locator), and before the first sibling that starts after it. Both
// File.Comments and every node's children are in source order.
func fileView(f *tree.File) *astView {
	loc := f.NewLocator()
	byParent := map[*tree.Node][]*tree.Node{}
	for _, c := range f.Comments {
		p := commentParent(f, loc, c)
		byParent[p] = append(byParent[p], c)
	}
	return buildView(f.Root, byParent)
}

// commentParent is the node a comment is printed under.
func commentParent(f *tree.File, loc *tree.Locator, c *tree.Node) *tree.Node {
	p := loc.At(c.Range.Start.Offset)
	for p != f.Root && !(p.Range.Start.Offset <= c.Range.Start.Offset && c.Range.End.Offset <= p.Range.End.Offset) {
		p = p.Parent
	}
	return p
}

func (v *astView) json(f *tree.Converter) astNode {
	n := v.n
	out := astNode{
		Kind:      n.Kind.String(),
		Start:     pos16(f, n.Range.Start),
		End:       pos16(f, n.Range.End),
		Name:      n.Name,
		Value:     n.Value,
		Recovered: n.Recovered,
		Children:  []astNode{},
	}
	for _, k := range v.kids {
		out.Children = append(out.Children, k.json(f))
	}
	return out
}

func (v *astView) print(w io.Writer, f *tree.Converter, depth int) {
	n := v.n
	label := n.Name
	if label == "" {
		label = n.Value
	}
	s, e := pos16(f, n.Range.Start), pos16(f, n.Range.End)
	line := fmt.Sprintf("%s%s %d:%d-%d:%d", strings.Repeat("  ", depth), n.Kind, s[0], s[1], e[0], e[1])
	if label != "" {
		line += " " + strconv.Quote(label)
	}
	if n.Recovered {
		line += " (recovered)"
	}
	fmt.Fprintln(w, line)
	for _, k := range v.kids {
		k.print(w, f, depth+1)
	}
}

// astMaxDepth is the deepest tree `ast` prints. A deeper one would print
// hundreds of megabytes of indentation or fail inside encoding/json.
const astMaxDepth = 1000

// nodeDepths returns the depth of every node under root (root is 1) and the
// deepest level. It walks with an explicit stack: the tree can be far deeper
// than is safe to recurse over.
func nodeDepths(root *tree.Node) (map[*tree.Node]int, int) {
	type frame struct {
		n *tree.Node
		d int
	}
	depths := map[*tree.Node]int{}
	max := 0
	stack := []frame{{root, 1}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		depths[f.n] = f.d
		if f.d > max {
			max = f.d
		}
		for _, c := range f.n.Children {
			stack = append(stack, frame{c, f.d + 1})
		}
	}
	return depths, max
}

// viewDepth is the number of levels of what `ast` prints: the node tree with
// each comment one level under the node it is printed under.
func viewDepth(f *tree.File) int {
	depths, max := nodeDepths(f.Root)
	loc := f.NewLocator()
	for _, c := range f.Comments {
		if d := depths[commentParent(f, loc, c)] + 1; d > max {
			max = d
		}
	}
	return max
}

func cmdAst(args []string, stdout, stderr io.Writer) int {
	pos, asJSON, err := parseArgs(args, true)
	if err != nil {
		return usageError(stderr, err.Error())
	}
	if len(pos) != 1 {
		return usageError(stderr, "ast takes exactly one FILE")
	}
	src, err := os.ReadFile(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	f := parse.File(pos[0], src)
	if f.Skipped != "" {
		return fail(stderr, fmt.Errorf("%s: %s", pos[0], f.Skipped))
	}
	if depth := viewDepth(f); depth > astMaxDepth {
		return fail(stderr, fmt.Errorf("tree too deep to print (depth %d)", depth))
	}
	conv := f.NewConverter()
	if asJSON {
		doc := astDoc{Path: filepath.ToSlash(pos[0]), Dialect: f.Dialect.String(), Errors: []astError{}, Root: fileView(f).json(conv)}
		for _, e := range f.Errors {
			p := pos16(conv, e.Range.Start)
			doc.Errors = append(doc.Errors, astError{p[0], p[1], e.Msg})
		}
		if err := writeJSON(stdout, doc); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fileView(f).print(stdout, conv, 0)
	for _, e := range f.Errors {
		p := pos16(conv, e.Range.Start)
		fmt.Fprintf(stderr, "%s:%d:%d: %s\n", filepath.ToSlash(pos[0]), p[0], p[1], e.Msg)
	}
	return 0
}

// --- symbols ---

type symOut struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container"`
}

func kindName(k analysis.DeclKind) string {
	switch k {
	case analysis.DeclFunction:
		return "function"
	case analysis.DeclAlias:
		return "alias"
	}
	return "variable"
}

// flattenSymbols lists a file's outline in source order, each function
// followed by its locals (Container set). Depth 1 marks a local.
func flattenSymbols(rel string, f *tree.File, ds []analysis.DocSymbol) (out []symOut, depth []int) {
	conv := f.NewConverter()
	for _, d := range ds {
		sp := pos16(conv, d.Selection.Start)
		out = append(out, symOut{rel, sp[0], sp[1], kindName(d.Kind), d.Name, ""})
		depth = append(depth, 0)
		for _, c := range d.Children {
			cp := pos16(conv, c.Selection.Start)
			out = append(out, symOut{rel, cp[0], cp[1], kindName(c.Kind), c.Name, d.Name})
			depth = append(depth, 1)
		}
	}
	return out, depth
}

func cmdSymbols(args []string, stdout, stderr io.Writer) int {
	pos, asJSON, err := parseArgs(args, true)
	if err != nil {
		return usageError(stderr, err.Error())
	}
	if len(pos) == 0 {
		pos = []string{"."}
	}
	var all []symOut
	var depths []int
	var gen uint64
	for _, p := range pos {
		fi, err := os.Stat(p)
		if err != nil {
			return fail(stderr, err)
		}
		root, only := p, ""
		if !fi.IsDir() {
			root, only = filepath.Dir(p), filepath.Base(p)
		}
		ws, err := openWorkspace(root, stderr)
		if err != nil {
			return fail(stderr, err)
		}
		snap := ws.Snapshot()
		if snap.Generation() > gen {
			gen = snap.Generation()
		}
		paths := snap.Paths()
		if only != "" {
			target := filepath.Join(ws.Root(), only)
			found := false
			for _, sp := range paths {
				if sp == target {
					found = true
				}
			}
			if !found {
				return fail(stderr, fmt.Errorf("%s: not a shell file the workspace loads", p))
			}
			paths = []string{target}
		}
		sort.Strings(paths)
		for _, sp := range paths {
			ff := snap.Facts(sp)
			if ff == nil || ff.File == nil {
				continue
			}
			syms, ds := flattenSymbols(relSlash(ws.Root(), sp), ff.File, snap.DocumentSymbols(sp))
			all = append(all, syms...)
			depths = append(depths, ds...)
		}
	}
	if asJSON {
		if all == nil {
			all = []symOut{}
		}
		if err := writeJSON(stdout, struct {
			Generation uint64   `json:"generation"`
			Symbols    []symOut `json:"symbols"`
		}{gen, all}); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for i, s := range all {
		fmt.Fprintf(stdout, "%s%s:%d:%d  %s  %s\n", strings.Repeat("  ", depths[i]), s.Path, s.Line, s.Col, s.Kind, s.Name)
	}
	return 0
}

// --- doctor ---

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	pos, _, err := parseArgs(args, false)
	if err != nil {
		return usageError(stderr, err.Error())
	}
	if len(pos) > 1 {
		return usageError(stderr, "doctor takes at most one PATH")
	}
	root := "."
	if len(pos) == 1 {
		root = pos[0]
	}
	ws, st, prof, err := openFull(root, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	rep := ws.Report()
	snap := ws.Snapshot()
	fmt.Fprintf(stdout, "version: %s\n", version)
	fmt.Fprintf(stdout, "root: %s\n", ws.Root())
	fmt.Fprintf(stdout, "files: %d\n", rep.Files)
	fmt.Fprintf(stdout, "skipped: %d\n", rep.Skipped)
	var ds []string
	for d := range rep.Dialects {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	for _, d := range ds {
		fmt.Fprintf(stdout, "%s: %d\n", d, rep.Dialects[d])
	}
	for _, l := range rep.LimitsHit {
		fmt.Fprintf(stdout, "limit hit: %s\n", redact.String(l))
	}
	for _, l := range snap.LimitsHit() {
		fmt.Fprintf(stdout, "limit hit: %s\n", redact.String(l))
	}
	for _, n := range rep.Notices {
		fmt.Fprintf(stdout, "notice: %s\n", redact.String(n))
	}
	errFiles := 0
	var unresolved []string
	for _, p := range snap.Paths() {
		if ff := snap.Facts(p); ff != nil && ff.File != nil && len(ff.File.Errors) > 0 {
			errFiles++
		}
		for _, e := range snap.SourceEdges(p) {
			if e.Status == analysis.SourceUnresolved {
				unresolved = append(unresolved, fmt.Sprintf("%s:%d", relSlash(ws.Root(), p), e.Ref.Range.Start.Line))
			}
		}
	}
	sort.Strings(unresolved)
	fmt.Fprintf(stdout, "syntax errors: %d files\n", errFiles)
	fmt.Fprintf(stdout, "unresolved sources: %d\n", len(unresolved))
	for _, u := range unresolved {
		fmt.Fprintf(stdout, "  %s\n", u)
	}
	doctorTools(ws.Root(), st, prof, stdout)
	return 0
}
