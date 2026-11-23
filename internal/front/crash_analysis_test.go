package front_test

import (
	"fmt"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// The subprocess crash suite (safety_sub_test.go) binds and builds every
// case as well as parsing and walking it: Bind and Build walk the tree too,
// and must stay inside the same 64 MiB stack.
func init() {
	front.SetCrashAnalyse(func(f *tree.File) string {
		ff := analysis.Bind(f)
		s := analysis.Build(map[string]*analysis.FileFacts{"/ws/x.sh": ff}, analysis.Options{Root: "/ws"}, 1)
		return fmt.Sprintf("bound: %d declarations, %d uses, %d document symbols", len(ff.Decls), len(ff.Uses), len(s.DocumentSymbols("/ws/x.sh")))
	})
}
