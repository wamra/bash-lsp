package front

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// Dialect is the shell language variant a file is parsed as. It is an alias of
// tree.Dialect, which carries the type for library consumers.
type Dialect = tree.Dialect

const (
	Bash  = tree.Bash
	POSIX = tree.POSIX
	Mksh  = tree.Mksh
	Bats  = tree.Bats
	Zsh   = tree.Zsh
)

var shellDirective = regexp.MustCompile(`^\s*#\s*shellcheck\s+(?:.*\s)?shell=([A-Za-z0-9_.-]+)`)

// byShellName maps an interpreter or `shell=` name to a dialect.
func byShellName(name string) (Dialect, bool) {
	switch name {
	case "bash":
		return Bash, true
	case "sh", "dash", "ash":
		return POSIX, true
	case "ksh", "mksh":
		return Mksh, true
	case "bats":
		return Bats, true
	case "zsh":
		return Zsh, true
	}
	return 0, false
}

// Detect picks the dialect, in order: a `# shellcheck shell=NAME` comment in
// the first 5 lines; the shebang interpreter (basename; `env` options, `env -S` strings and
// `busybox APPLET` unwrapped);
// the file extension; else Bash.
func Detect(path string, src []byte) Dialect { return DetectWith(path, src, Bash) }

// DetectWith is Detect with fallback in place of Bash for a file that names no
// dialect itself: no directive, no shebang that names one, and the extension
// .sh or none that says anything (.bash, .ksh, .mksh, .bats and .zsh still
// decide). It is what the bash.dialect setting reaches.
func DetectWith(path string, src []byte, fallback Dialect) Dialect {
	if fallback == 0 {
		fallback = Bash
	}
	lines := bytes.SplitN(src, []byte("\n"), 6)
	if len(lines) > 5 {
		lines = lines[:5]
	}
	for _, ln := range lines {
		if m := shellDirective.FindSubmatch(bytes.TrimRight(ln, "\r")); m != nil {
			if d, ok := byShellName(string(m[1])); ok {
				return d
			}
		}
	}
	if len(lines) > 0 && bytes.HasPrefix(lines[0], []byte("#!")) {
		if d, ok := byShebang(strings.TrimSpace(string(lines[0][2:]))); ok {
			return d
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bash":
		return Bash
	case ".sh":
		return fallback
	case ".bats":
		return Bats
	case ".ksh", ".mksh":
		return Mksh
	case ".zsh":
		return Zsh
	}
	return fallback
}

func byShebang(line string) (Dialect, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}
	name, fields := baseName(fields[0]), fields[1:]
	for name == "env" {
		name, fields = envCommand(fields)
	}
	if name == "busybox" && len(fields) > 0 {
		name = baseName(strings.Trim(fields[0], `"'`)) // `busybox sh`: the applet is the shell
	}
	return byShellName(name)
}

// envCommand skips the options and VAR=value assignments of an `env`
// shebang and returns the command name with the words after it. The options
// are env(1)'s: -C, -P and -u take an argument, -S holds a string that env
// splits into more options and the command (`env -S sh -e`), the others
// (-i, -v, -0) stand alone.
func envCommand(fields []string) (string, []string) {
	for len(fields) > 0 {
		a := strings.Trim(fields[0], `"'`)
		switch {
		case a == "-C" || a == "-P" || a == "-u":
			if len(fields) < 2 {
				return "", nil
			}
			fields = fields[2:]
		case strings.HasPrefix(a, "-S") && len(a) > 2:
			fields = append([]string{a[2:]}, fields[1:]...)
		case strings.HasPrefix(a, "-") || strings.Contains(a, "="):
			fields = fields[1:] // other env flags and VAR=value assignments
		default:
			return baseName(a), fields[1:]
		}
	}
	return "", nil
}

// baseName is the last element of a slash-separated path (shebangs always use '/').
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
