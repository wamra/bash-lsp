package main

import (
	"fmt"
	"io"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// openWorkspace opens root with its settings (the user file, then
// root/.bash-lsp.toml) and the PATH profile: path.profile of the user
// settings, else $PATH. What the settings ignored is written to stderr.
func openWorkspace(root string, stderr io.Writer) (*workspace.Workspace, error) {
	st := config.Load(root, nil)
	for _, w := range st.Warnings {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	prof := extool.ProfileFromEnv()
	if st.PathProfile != nil {
		prof = extool.NewProfile(st.PathProfile)
	}
	return workspace.OpenWith(root, st.Limits(), nil, st.Workspace(prof.Commands()))
}
