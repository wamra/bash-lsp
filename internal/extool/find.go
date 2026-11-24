package extool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Refusal says why a program that exists was not accepted.
type Refusal struct{ Name, Path, Reason string }

// Find chooses the program to run for name. The only inputs are configured
// (a path or a bare name from the user's settings or the client's init
// options) and prof (the PATH profile); never a file of the analysed code. A
// configured value wins over name: an absolute path is taken as it is, a bare
// name is looked up in prof, anything else is refused. The result is
// symlink-resolved. A program that lies inside one of roots (the workspace
// folders), by its own path or after resolving links, is refused: a repository
// must not decide what the server runs, even through a PATH that lists one of
// its directories. When nothing is found Find returns ok false and no
// refusal; a refusal is returned with ok false too.
func Find(name, configured string, prof *Profile, roots []string) (Tool, *Refusal, bool) {
	path, bare := configured, false
	switch {
	case configured == "":
		path, bare = name, true
	case !strings.ContainsAny(configured, `/\`):
		bare = true
	}
	if bare {
		var found bool
		if path, found = prof.Lookup(path); !found {
			return Tool{}, nil, false
		}
	} else if !filepath.IsAbs(path) {
		return Tool{}, &Refusal{Name: name, Path: path, Reason: "a configured path must be absolute"}, false
	}
	refuse := func(p, why string) (Tool, *Refusal, bool) {
		return Tool{}, &Refusal{Name: name, Path: p, Reason: why}, false
	}
	var exts []string
	if runtime.GOOS == "windows" {
		exts = windowsExts()
	}
	if _, err := os.Stat(path); err != nil {
		return refuse(path, "not found")
	}
	if !isProgram(path, exts) {
		return refuse(path, "not an executable file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return refuse(path, err.Error())
	}
	for _, p := range []string{filepath.Clean(path), resolved} {
		for _, root := range roots {
			if insideRoot(p, root) {
				return refuse(resolved, fmt.Sprintf("it is inside the workspace folder %s; a workspace cannot supply a program the server runs", root))
			}
		}
	}
	return Tool{Name: name, Path: resolved}, nil, true
}

// insideRoot reports whether path is root or below it. root is compared as
// given and with its links resolved; a case variant on a case-insensitive
// volume is caught by comparing each ancestor of path with root on disk.
func insideRoot(path, root string) bool {
	if root == "" {
		return false
	}
	roots := []string{filepath.Clean(root)}
	if r, err := filepath.EvalSymlinks(root); err == nil && r != roots[0] {
		roots = append(roots, r)
	}
	for _, r := range roots {
		if rel, err := filepath.Rel(r, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, rootInfo) {
			return true
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}
