// Package bashlsp is the library entry point: it loads a directory of shell
// files into an analysis.Snapshot that answers definition, references, symbol
// and visibility queries. See docs/library.md.
//
// To analyse files that are not on disk, use analysis.Analyze; to parse a
// single file, use package parse.
package bashlsp

import (
	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// Load discovers the shell files under root the way the language server does
// (.gitignore, the symlink policy and the default size and count limits),
// parses and analyses them and returns the snapshot. Only discovery problems
// are errors (root missing or not a directory); a file that does not parse is
// in the snapshot with its syntax errors, or with File.Skipped set.
func Load(root string) (*analysis.Snapshot, error) {
	ws, err := workspace.Open(root, workspace.Limits{})
	if err != nil {
		return nil, err
	}
	return ws.Snapshot(), nil
}
