// Package extool finds external programs. This file holds the PATH profile:
// the names of the executables in a list of directories, read once, so that
// the analysis can tell a command that exists from one that does not. It
// starts no process.
package extool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Limits of one profile (maxProfileNames is a variable so a test can lower it).
const maxProfileDirs = 64

var maxProfileNames = 50000

// Profile is the set of executable names in an ordered list of directories.
// The first directory that holds a name wins, as in a shell. A Profile is
// read-only after it is made and safe for concurrent use.
type Profile struct {
	names   map[string]string // name -> absolute path
	skipped []string
}

// NewProfile reads each directory of dirs once (os.ReadDir). A relative
// directory is skipped, and so is one that cannot be listed; beyond 64
// directories or 50,000 names the rest is skipped. Each skip is reported by
// Skipped. A name counts when its file has an execute bit (any of the three),
// or on Windows when its extension is in PATHEXT; a directory or a link that
// does not lead to a regular file does not.
func NewProfile(dirs []string) *Profile {
	p := &Profile{names: map[string]string{}}
	var pathext []string
	if runtime.GOOS == "windows" {
		pathext = windowsExts()
	}
	for i, dir := range dirs {
		switch {
		case i >= maxProfileDirs:
			p.skip(dir, fmt.Sprintf("more than %d directories", maxProfileDirs))
			continue
		case dir == "" || !filepath.IsAbs(dir):
			p.skip(dir, "relative directory")
			continue
		case len(p.names) >= maxProfileNames:
			p.skip(dir, fmt.Sprintf("more than %d names", maxProfileNames))
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			p.skip(dir, err.Error())
			continue
		}
		for _, e := range ents {
			if len(p.names) >= maxProfileNames {
				p.skip(dir, fmt.Sprintf("more than %d names", maxProfileNames))
				break
			}
			name := e.Name()
			full := filepath.Join(dir, name)
			if !executable(e, full, pathext) {
				continue
			}
			if pathext != nil {
				name = strings.TrimSuffix(name, filepath.Ext(name))
			}
			if _, dup := p.names[name]; !dup {
				p.names[name] = full
			}
		}
	}
	return p
}

// ProfileFromEnv is NewProfile of $PATH split at os.PathListSeparator. An
// empty entry (the current directory, in a shell) is relative and skipped.
func ProfileFromEnv() *Profile {
	path := os.Getenv("PATH")
	if path == "" {
		return NewProfile(nil)
	}
	return NewProfile(filepath.SplitList(path))
}

// Lookup returns the path of the executable called name. It answers by name
// only: it does not look at the file again, and a directory inside a
// workspace root counts (a repository's own bin/ commands are not missing).
// A name with a path separator is never found. Safe on a nil Profile.
func (p *Profile) Lookup(name string) (string, bool) {
	if p == nil || name == "" || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	path, ok := p.names[name]
	return path, ok
}

// Skipped lists the directories that were not read, as "dir: reason".
func (p *Profile) Skipped() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.skipped...)
}

// Len is the number of names in the profile.
func (p *Profile) Len() int {
	if p == nil {
		return 0
	}
	return len(p.names)
}

// Commands is Lookup as the function the analysis takes, or nil when the
// profile holds no name at all: a PATH that is empty or unreadable would
// otherwise make every command look missing.
func (p *Profile) Commands() func(name string) (string, bool) {
	if p.Len() == 0 {
		return nil
	}
	return p.Lookup
}

func (p *Profile) skip(dir, reason string) {
	p.skipped = append(p.skipped, dir+": "+reason)
}

// executable reports whether the directory entry e (at path full) is a
// program: a regular file (after following a link) with an execute bit, or
// with a PATHEXT extension on Windows.
func executable(e os.DirEntry, full string, pathext []string) bool {
	if e.IsDir() {
		return false
	}
	mode := e.Type()
	var fi os.FileInfo
	if mode&os.ModeSymlink != 0 { // Homebrew's bin is all links: follow one
		var err error
		if fi, err = os.Stat(full); err != nil {
			return false
		}
		mode = fi.Mode()
	}
	if !mode.IsRegular() {
		return false
	}
	if pathext != nil {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		for _, x := range pathext {
			if x == ext {
				return true
			}
		}
		return false
	}
	if fi == nil {
		var err error
		if fi, err = e.Info(); err != nil {
			return false
		}
	}
	return fi.Mode().Perm()&0o111 != 0
}

// windowsExts is PATHEXT, lower-cased; the usual list when it is unset.
func windowsExts() []string {
	v := os.Getenv("PATHEXT")
	if v == "" {
		v = ".COM;.EXE;.BAT;.CMD"
	}
	var out []string
	for _, x := range strings.Split(v, ";") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			out = append(out, x)
		}
	}
	return out
}
