package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/tree"
)

// extract writes the call/source graph as JSON lines in the record format
// code-index reads from its PowerShell extractor (docs/code-index-integration.md).
// Nothing here executes or sources analysed code.

const extractUsage = "usage: bash-lsp extract [--root DIR] [--list-file FILE]"

// maxListFileBytes bounds the --list-file read: 8 MiB is about 100,000 paths.
const maxListFileBytes = 8 << 20

// maxExprRunes is the longest expr or message: the PowerShell extractor clips
// its text to 160 characters (extractors/powershell-ast.ps1:31-35).
const maxExprRunes = 160

// extractRec is one output line. The first group are code-index's astRec keys
// (internal/indexer/ast.go:36-52); to, resolution and reason are ours and are
// ignored by its reader (plain json.Unmarshal, ast.go:65). Field order is the
// key order on the line.
type extractRec struct {
	T          string `json:"t"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
	EndLine    int    `json:"endLine,omitempty"`
	EndCol     int    `json:"endCol,omitempty"`
	Name       string `json:"name,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Target     string `json:"target,omitempty"`
	Path       string `json:"path,omitempty"`
	Expr       string `json:"expr,omitempty"`
	In         string `json:"in,omitempty"`
	Message    string `json:"message,omitempty"`
	First      string `json:"first,omitempty"`
	Count      int    `json:"count,omitempty"`
	To         string `json:"to,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// typeRank orders the records of one offset.
var typeRank = map[string]int{"parse_error": 0, "error": 1, "func": 2, "alias": 3, "dotsource": 4, "invoke": 5, "call": 6, "dynamic": 7}

// sited is a record with the byte offset of its site, the sort key.
type sited struct {
	off int
	rec extractRec
}

// clip is the text of an expression the way the PowerShell extractor clips
// it: whitespace runs become one space, at most maxExprRunes characters.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxExprRunes {
		s = string([]rune(s)[:maxExprRunes])
	}
	return s
}

type extractArgs struct {
	root, listFile string
}

func parseExtractArgs(args []string) (extractArgs, error) {
	a := extractArgs{root: "."}
	for i := 0; i < len(args); i++ {
		name, inline, hasInline := strings.Cut(args[i], "=")
		value := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case name == "--root":
			a.root, err = value()
		case name == "--list-file":
			a.listFile, err = value()
		case strings.HasPrefix(args[i], "-") && args[i] != "-":
			err = fmt.Errorf("unknown flag %q for extract", args[i])
		default:
			err = fmt.Errorf("extract takes no arguments, got %q", args[i])
		}
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func cmdExtract(args []string, stdout, stderr io.Writer) int {
	a, err := parseExtractArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "bash-lsp: %v\n%s\n", err, extractUsage)
		return 2
	}
	var lines []string
	if a.listFile != "" {
		if lines, err = readListFile(a.listFile); err != nil {
			return fail(stderr, err)
		}
	}
	ws, err := openWorkspace(a.root, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	var only map[string]bool
	if a.listFile != "" {
		only = selectListed(ws, lines, stderr)
	}
	return extractWorkspace(ws, only, stdout, stderr)
}

// readListFile reads at most maxListFileBytes of path and splits it into
// lines (a trailing \r removed, empty lines dropped).
func readListFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxListFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxListFileBytes {
		return nil, fmt.Errorf("list file %s is larger than %d bytes", path, maxListFileBytes)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSuffix(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// selectListed maps the listed paths to the workspace's spelling. A path that
// is relative, outside the root or no file of the workspace is reported on
// stderr and left out.
func selectListed(ws *workspace.Workspace, lines []string, stderr io.Writer) map[string]bool {
	known := map[string]bool{}
	for _, p := range ws.Snapshot().Paths() {
		known[p] = true
	}
	for _, s := range ws.Skipped() {
		known[s.Path] = true
	}
	only := map[string]bool{}
	for _, l := range lines {
		if !filepath.IsAbs(l) {
			fmt.Fprintf(stderr, "bash-lsp: extract: %s: not an absolute path; skipped\n", l)
			continue
		}
		p := ws.Canon(l)
		if r, err := filepath.Rel(ws.Root(), p); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			fmt.Fprintf(stderr, "bash-lsp: extract: %s: outside the root %s; skipped\n", l, ws.Root())
			continue
		}
		if !known[p] {
			fmt.Fprintf(stderr, "bash-lsp: extract: %s: not a file of the workspace; skipped\n", l)
			continue
		}
		only[p] = true
	}
	return only
}

// extractWorkspace streams the records of ws to stdout, file by file in path
// order. only, when not nil, restricts the files written. Exit 1 when a write
// fails.
func extractWorkspace(ws *workspace.Workspace, only map[string]bool, stdout, stderr io.Writer) int {
	snap := ws.Snapshot()
	x := &extractor{snap: snap, declPath: map[*analysis.Decl]string{}, edges: map[string][]analysis.Edge{}}
	paths := snap.Paths()
	for _, p := range paths {
		if ff := snap.Facts(p); ff != nil {
			for _, d := range ff.Decls {
				if d.Kind == analysis.DeclFunction {
					x.declPath[d] = p
				}
			}
		}
	}
	for _, e := range snap.Edges() {
		x.edges[e.Site.Path] = append(x.edges[e.Site.Path], e)
	}
	skipReason := map[string]string{}
	for _, s := range ws.Skipped() {
		skipReason[s.Path] = s.Reason
		paths = append(paths, s.Path)
	}
	sort.Strings(paths)

	bw := bufio.NewWriter(stdout)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, p := range paths {
		if only != nil && !only[p] {
			continue
		}
		var recs []extractRec
		if reason, skipped := skipReason[p]; skipped {
			recs = []extractRec{{T: "error", File: p, Line: 1, Col: 1, Message: clip(reason)}}
		} else {
			recs = x.fileRecords(p)
		}
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				return fail(stderr, err)
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return 0
}

type extractor struct {
	snap     *analysis.Snapshot
	declPath map[*analysis.Decl]string // function declaration -> file
	edges    map[string][]analysis.Edge
}

// fileRecords returns the records of path in (offset, record type) order,
// identical neighbours (one call site with several candidate definitions of
// the same name) written once.
func (x *extractor) fileRecords(path string) []extractRec {
	ff := x.snap.Facts(path)
	if ff == nil || ff.File == nil {
		return nil
	}
	f := ff.File
	conv := f.NewToolConverter()
	at := func(p tree.Pos) (int, int) { pos := pos16(conv, p); return pos[0], pos[1] }
	var rs []sited
	add := func(off int, r extractRec) { r.File = path; rs = append(rs, sited{off, r}) }

	if len(f.Errors) > 0 {
		add(-1, extractRec{T: "parse_error", Line: 1, Col: 1, Count: len(f.Errors), First: clip(f.Errors[0].Msg)})
	}
	inFunc := func(c analysis.Caller) string {
		if c.Func != nil && c.Func.Kind == analysis.DeclFunction {
			return c.Func.Name
		}
		return ""
	}
	funcAt := funcsAround(ff)
	for _, d := range ff.Decls {
		switch d.Kind {
		case analysis.DeclFunction:
			rng := d.Node.Range
			l, c := at(rng.Start)
			el, ec := at(rng.End)
			add(rng.Start.Offset, extractRec{T: "func", Name: d.Name, Line: l, Col: c, EndLine: el, EndCol: ec})
		case analysis.DeclAlias:
			if d.AliasTo == "" {
				continue
			}
			l, c := at(d.NameRange.Start)
			el, ec := at(d.NameRange.End)
			add(d.NameRange.Start.Offset, extractRec{T: "alias", Alias: d.Name, Target: d.AliasTo, Line: l, Col: c, EndLine: el, EndCol: ec,
				In: funcAt(d.NameRange.Start.Offset)})
		}
	}
	siteText := func(r tree.Range) string {
		if a, b := r.Start.Offset, r.End.Offset; a >= 0 && a <= b && b <= len(f.Src) {
			return clip(string(f.Src[a:b]))
		}
		return ""
	}
	for _, e := range x.edges[path] {
		if e.IsCommand() { // calls of external commands are not part of the graph
			continue
		}
		l, c := at(e.Site.Range.Start)
		base := extractRec{Line: l, Col: c, In: inFunc(e.From), Resolution: e.Res.String(), Reason: e.Reason}
		off := e.Site.Range.Start.Offset
		switch e.Rel {
		case analysis.RelCalls:
			switch {
			case e.Res == analysis.Dynamic:
				base.T, base.Expr = "dynamic", clip(e.Text)
			case (e.Res == analysis.Resolved || e.Res == analysis.Candidate) && e.To != nil:
				base.T, base.Name, base.To = "call", e.To.Name, x.declPath[e.ToDecl]
			default:
				continue
			}
		case analysis.RelSources:
			base.T = "dotsource"
			if e.ToFile != "" {
				base.Path, base.Expr = e.ToFile, siteText(e.Site.Range)
			} else {
				base.Path, base.Expr = literalPath(e.Text), clip(e.Text)
			}
		case analysis.RelInvokes:
			base.T, base.Path, base.Expr = "invoke", e.ToFile, siteText(e.Site.Range)
		default: // RelAliases: the alias record carries it
			continue
		}
		if base.Expr == "" && base.T != "call" {
			base.Expr = siteText(e.Site.Range)
		}
		add(off, base)
	}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		switch {
		case a.off != b.off:
			return a.off < b.off
		case typeRank[a.rec.T] != typeRank[b.rec.T]:
			return typeRank[a.rec.T] < typeRank[b.rec.T]
		case a.rec.To != b.rec.To:
			return a.rec.To < b.rec.To
		case a.rec.Path != b.rec.Path:
			return a.rec.Path < b.rec.Path
		case a.rec.Name != b.rec.Name:
			return a.rec.Name < b.rec.Name
		}
		return a.rec.Reason < b.rec.Reason
	})
	out := make([]extractRec, 0, len(rs))
	for i, r := range rs {
		if i > 0 && rs[i-1].rec == r.rec {
			continue
		}
		out = append(out, r.rec)
	}
	return out
}

// funcsAround returns a function giving the name of the innermost function
// of ff whose definition spans an offset ("" when none), what CallerAt says
// for that offset. The offsets must be asked in ascending order: one sweep
// over the function definitions sorted by start, instead of a walk down and
// up the tree per alias (O(aliases x depth): an alias run 3,900 subshells
// deep took minutes).
func funcsAround(ff *analysis.FileFacts) func(off int) string {
	var fs []*analysis.Decl
	for _, d := range ff.Decls {
		if d.Kind == analysis.DeclFunction && d.Node != nil {
			fs = append(fs, d)
		}
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Node.Range.Start.Offset < fs[j].Node.Range.Start.Offset })
	// open holds the definitions spanning the last offset, outermost first
	// (definitions nest or are disjoint, so the ones still open form a chain).
	var open []*analysis.Decl
	pop := func(off int) {
		for len(open) > 0 && open[len(open)-1].Node.Range.End.Offset <= off {
			open = open[:len(open)-1]
		}
	}
	next, last := 0, -1
	return func(off int) string {
		if off < last { // not ascending: start over
			open, next = open[:0], 0
		}
		last = off
		for ; next < len(fs) && fs[next].Node.Range.Start.Offset <= off; next++ {
			pop(fs[next].Node.Range.Start.Offset)
			open = append(open, fs[next])
		}
		pop(off)
		if len(open) == 0 {
			return ""
		}
		return open[len(open)-1].Name
	}
}

// literalPath is the path an unresolved source names when its word is a
// plain literal, else "": the PowerShell extractor likewise leaves path empty
// when the dot-source has no string literal (powershell-ast.ps1:121-123).
func literalPath(word string) string {
	word = strings.TrimSpace(word)
	if word == "" || strings.ContainsAny(word, "$`*?[{}()\"' \t\n\\") {
		return ""
	}
	return word
}
