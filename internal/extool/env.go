package extool

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ChildEnv is the environment for an external program: the variables a
// program or the platform needs (a search path, the home and config
// directories where it reads its own settings, locale, temp and Windows
// system directories) and extra. Everything else is dropped, so runtime
// options such as GHCRTS or a tool's own option variable never reach it. PATH
// keeps only the absolute entries that are not inside one of roots (the
// workspace folders), by their own spelling or with links resolved: an empty
// or relative entry means the current directory, and an entry in the
// workspace would let the repository supply a program that a wrapper script
// starts through the search path. Pass roots whenever the program may be a
// script.
func ChildEnv(roots []string, extra ...string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || !passes(name) {
			continue
		}
		if strings.EqualFold(name, "PATH") {
			kv = name + "=" + SafePath(value, roots)
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func passes(name string) bool {
	if runtime.GOOS == "windows" {
		name = strings.ToUpper(name)
	}
	switch name {
	case "PATH", "HOME", "XDG_CONFIG_HOME", "LANG", "LANGUAGE", "TMPDIR", "TEMP", "TMP",
		"SYSTEMROOT", "USERPROFILE", "APPDATA", "LOCALAPPDATA":
		return true
	}
	return strings.HasPrefix(name, "LC_")
}

// SafePath filters a search path as ChildEnv describes.
func SafePath(path string, roots []string) string {
	var keep []string
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		forms := []string{filepath.Clean(dir)}
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			forms = append(forms, r)
		}
		inside := false
		for _, f := range forms {
			for _, root := range roots {
				inside = inside || insideRoot(f, root)
			}
		}
		if !inside {
			keep = append(keep, dir)
		}
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// NeutralDir is a working directory for a child that must not read anything
// from the workspace by relative name or by searching upwards from where it
// runs: the root of the file system (on Windows the system directory, else
// the root of its volume). It exists on every machine the server runs on.
func NeutralDir() string {
	if runtime.GOOS == "windows" {
		if sys := os.Getenv("SystemRoot"); sys != "" {
			if fi, err := os.Stat(sys); err == nil && fi.IsDir() {
				return sys
			}
			return filepath.VolumeName(sys) + `\`
		}
		return `C:\`
	}
	return "/"
}
