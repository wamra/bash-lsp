// Command shapeprobe times Parse, Bind, Build and the snapshot queries on one
// file through the public API: shapeprobe FILE. It prints one line of
// name=duration pairs (milliseconds). This is the probe behind
// bench/results/bind-shapes-*.txt; build it from this directory with
// `go build -o /private/tmp/bash-lsp/shapeprobe .` (the module replace points
// at the repository root). In a testdata directory so that the Go tooling of
// the main module and boundary_test.go leave it alone.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/parse"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	const path = "/ws/t.sh"
	var out []string
	tm := func(name string, fn func()) {
		t0 := time.Now()
		fn()
		out = append(out, fmt.Sprintf("%s=%d", name, time.Since(t0).Milliseconds()))
	}
	f := parse.File(path, b)
	tm("parse", func() { f = parse.File(path, b) })
	if f.Skipped != "" {
		fmt.Printf("bytes=%d skipped=%q\n", len(b), f.Skipped)
		return
	}
	var ff *analysis.FileFacts
	tm("bind", func() { ff = analysis.Bind(f) })
	var s *analysis.Snapshot
	tm("build", func() { s = analysis.Build(map[string]*analysis.FileFacts{path: ff}, analysis.Options{Root: "/ws"}, 1) })
	tm("edges", func() { _ = s.Edges() })
	// The query site: the last call use, else the last use (the innermost
	// one in the deep shapes); the function: the last function declared.
	off := -1
	for i := len(ff.Uses) - 1; i >= 0; i-- {
		if ff.Uses[i].Kind == analysis.UseCall {
			off = ff.Uses[i].Range.Start.Offset
			break
		}
	}
	if off < 0 && len(ff.Uses) > 0 {
		off = ff.Uses[len(ff.Uses)-1].Range.Start.Offset
	}
	fn := ""
	for i := len(ff.Decls) - 1; i >= 0; i-- {
		if ff.Decls[i].Kind == analysis.DeclFunction {
			fn = ff.Decls[i].Name
			break
		}
	}
	if fn != "" {
		if n, ok := s.FuncNode(path, fn); ok {
			tm("callers", func() { s.Callers(n, 0) })
			tm("callees", func() { s.Callees(n, 0) })
			tm("impact", func() { s.Impact(n, 0) })
		}
	}
	if off >= 0 {
		tm("def", func() { s.Definition(path, off) })
		tm("refs", func() { s.References(path, off, true) })
		tm("resrefs", func() { s.ResolvedReferences(path, off, true) })
		tm("hover", func() { s.Hover(path, off) })
		tm("callerat", func() { s.CallerAt(path, off) })
	}
	tm("docsyms", func() { s.DocumentSymbols(path) })
	fmt.Printf("bytes=%d decls=%d uses=%d %s\n", len(b), len(ff.Decls), len(ff.Uses), strings.Join(out, " "))
}
