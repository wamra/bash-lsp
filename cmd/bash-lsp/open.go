package main

import (
	"io"

	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// openWorkspace opens root with its settings (the user file, then
// root/.bash-lsp.toml) and the PATH profile: path.profile of the user
// settings, else $PATH. What the settings ignored is written to stderr.
func openWorkspace(root string, stderr io.Writer) (*workspace.Workspace, error) {
	ws, _, _, err := query.Open(root, stderr)
	return ws, err
}
