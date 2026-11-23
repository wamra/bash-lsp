package front

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

// Dialect is the shell language variant a file is parsed as.
type Dialect uint8

const (
	Bash Dialect = iota + 1
	POSIX
	Mksh
	Bats
	Zsh
)

// String returns the dialect's conventional shell name.
func (d Dialect) String() string {
	switch d {
	case Bash:
		return "bash"
	case POSIX:
		return "sh"
	case Mksh:
		return "mksh"
	case Bats:
		return "bats"
	case Zsh:
		return "zsh"
	}
	return "Dialect(?)"
}

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
// the first 5 lines; the shebang interpreter (basename, `env NAME` unwrapped);
// the file extension; else Bash.
func Detect(path string, src []byte) Dialect {
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
	case ".bash", ".sh":
		return Bash
	case ".bats":
		return Bats
	case ".ksh", ".mksh":
		return Mksh
	case ".zsh":
		return Zsh
	}
	return Bash
}

func byShebang(line string) (Dialect, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}
	name := baseName(fields[0])
	if name == "env" {
		name = ""
		for _, a := range fields[1:] {
			if strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
				continue // env flags and VAR=value assignments
			}
			name = baseName(a)
			break
		}
	}
	return byShellName(name)
}

// baseName is the last element of a slash-separated path (shebangs always use '/').
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
