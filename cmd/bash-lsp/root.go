package main

import (
	"os"
	"path/filepath"
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
