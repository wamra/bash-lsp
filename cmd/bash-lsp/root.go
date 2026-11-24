package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// rootMarkers are the entries that make a directory the root of a repository
// or of a bash-lsp project.
var rootMarkers = []string{".git", ".bash-lsp.toml"}

// findRoot is the root a command uses for the paths it was given. --root
// (flag) wins, and the root is walked. Otherwise it is the nearest ancestor of
// the first path (the path itself when it is a directory) that holds .git or
// .bash-lsp.toml, walked. With no such ancestor a first path that is a
// directory is the root and is walked, while a first path that is a file gives
// its own directory as the root and walk false: the command analyses exactly
// the files it was given and never lists the directories around them. With no
// paths the current directory stands for the first path.
func findRoot(paths []string, flag string) (root string, walk bool) {
	if flag != "" {
		return absResolved(flag), true
	}
	first := "."
	if len(paths) > 0 {
		first = paths[0]
	}
	abs := absResolved(first)
	isDir := true
	if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
		isDir = false
	} else if err != nil {
		isDir = false // a path that does not exist is reported by the caller
	}
	start := abs
	if !isDir {
		start = filepath.Dir(abs)
	}
	for d := start; ; {
		for _, m := range rootMarkers {
			if _, err := os.Lstat(filepath.Join(d, m)); err == nil {
				return d, true
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return start, isDir
}

// absResolved is p made absolute with the links of its directory resolved (the
// last element too when it exists); on any failure the cleaned absolute path.
func absResolved(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(r, filepath.Base(abs))
	}
	return abs
}

// profileFor is the PATH profile of the settings: path.profile of the user
// settings, else $PATH. A variable so a test can count the builds.
var profileFor = func(st config.Settings) *extool.Profile {
	if st.PathProfile != nil {
		return extool.NewProfile(st.PathProfile)
	}
	return extool.ProfileFromEnv()
}

// openFull is openWorkspace that also returns the settings and the profile it
// used, for `doctor`, which reports on them.
func openFull(root string, stderr io.Writer) (*workspace.Workspace, config.Settings, *extool.Profile, error) {
	st := config.Load(root, nil)
	for _, w := range st.Warnings {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	prof := profileFor(st)
	ws, err := workspace.OpenWith(root, st.Limits(), nil, st.Workspace(prof.Commands()))
	return ws, st, prof, err
}
