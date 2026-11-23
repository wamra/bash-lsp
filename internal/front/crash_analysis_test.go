package front_test

import (
	"fmt"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// The subprocess crash suite (safety_sub_test.go) binds and builds every
// case as well as parsing and walking it, lists its edges and the callers
// and callees of each function: Bind, Build and the edge queries walk the
// tree and the graph too, and must stay inside the same 64 MiB stack.
func init() {
	front.SetCrashAnalyse(func(f *tree.File) string {
		const p = "/ws/x.sh"
		ff := analysis.Bind(f)
		s := analysis.Build(map[string]*analysis.FileFacts{p: ff}, analysis.Options{Root: "/ws"}, 1)
		steps, seen := 0, map[string]bool{}
		for _, d := range ff.Decls {
			if d.Kind != analysis.DeclFunction || seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			if n, ok := s.FuncNode(p, d.Name); ok {
				steps += len(s.Callers(n, 0)) + len(s.Callees(n, 0))
			}
		}
		return fmt.Sprintf("bound: %d declarations, %d uses, %d document symbols, %d edges, %d caller and callee steps",
			len(ff.Decls), len(ff.Uses), len(s.DocumentSymbols(p)), len(s.Edges()), steps)
	})
}
