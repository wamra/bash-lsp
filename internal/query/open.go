package query

import (
	"fmt"
	"io"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// LoadSettings reads the user settings and root/.bash-lsp.toml; what the
// files could not say is written to stderr, one line each.
func LoadSettings(root string, stderr io.Writer) config.Settings {
	st := config.Load(root, nil)
	for _, w := range st.Warnings {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	return st
}

// NoteRefused says on stderr which settings of the repo file were ignored
// because only the user file may carry them (a program path, for one).
func NoteRefused(st config.Settings, stderr io.Writer) {
	for _, r := range st.Refused {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s: %s ignored: %s\n", r.File, r.Key, r.Reason)
	}
}

// OpenWith opens root with the settings st and returns the PATH profile it
// used: path.profile of the user settings, else $PATH.
func OpenWith(root string, st config.Settings) (*workspace.Workspace, *extool.Profile, error) {
	prof := extool.ProfileFromEnv()
	if st.PathProfile != nil {
		prof = extool.NewProfile(st.PathProfile)
	}
	ws, err := workspace.OpenWith(root, st.Limits(), nil, st.Workspace(prof.Commands()))
	return ws, prof, err
}

// Open is LoadSettings and OpenWith.
func Open(root string, stderr io.Writer) (*workspace.Workspace, config.Settings, *extool.Profile, error) {
	st := LoadSettings(root, stderr)
	ws, prof, err := OpenWith(root, st)
	return ws, st, prof, err
}
