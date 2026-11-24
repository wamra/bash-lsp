// Package extool finds external programs. This file holds the PATH profile:
// the names in a list of directories, read once, so that
// the analysis can tell a command that exists from one that does not. It
// starts no process.
package extool

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Limits of one profile (maxProfileNames and profileDirTimeout are variables
// so a test can lower them).
const (
	maxProfileDirs = 64
	profileChunk   = 1024 // entries asked of the directory at a time
)

var (
	maxProfileNames   = 50000
	profileDirTimeout = 2 * time.Second // wall-clock bound for reading one directory

	// profileTotalBudget bounds the time of all directories together: hung
	// directories would otherwise add up, one time-out after the other.
	profileTotalBudget = 5 * time.Second
)

// dirReader is an open directory: *os.File, or a test's stand-in.
type dirReader interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

func osOpenDir(dir string) (dirReader, error) { return os.Open(dir) }

// openDir opens a directory for reading; a variable so a test can inject a
// slow or endless reader.
var openDir = osOpenDir

// Profile is the set of names in an ordered list of directories, each checked
// for being a program when it is first looked up (a PATH of Homebrew's size
// holds two thousand names, most of them links, and a stat per name at start
// cost 12 ms of the first answer). The first directory that holds a program
// of that name wins, as in a shell. A Profile is safe for concurrent use.
type Profile struct {
	cands   map[string][]string // name -> the files of that name, in directory order
	skipped []string
	dirs    int      // directories given
	exts    []string // Windows: PATHEXT, lower-cased; nil elsewhere

	mu    sync.Mutex
	known map[string]string // name -> the path of the program, "" when none; filled by Lookup
}

// NewProfile reads each directory of dirs once, in chunks of 1,024 entries
// and for at most 2 s each and 5 s in all (hung mounts), without looking at the files. A relative
// directory is skipped, and so is one that cannot be listed or does not answer
// in time; beyond 64 directories or 50,000 names the rest is skipped (a
// directory is not read past twice that many entries). Each skip is reported by
// Skipped. Lookup decides, per name and once, whether a file is
// a program: a regular file (after following a link) with an execute bit (any
// of the three), or on Windows with a PATHEXT extension; a directory, a dangling
// link or a data file is not.
func NewProfile(dirs []string) *Profile {
	p := &Profile{cands: map[string][]string{}, known: map[string]string{}, dirs: len(dirs)}
	if runtime.GOOS == "windows" {
		p.exts = windowsExts()
	}
	count := 0
	begun := time.Now()
	for i, dir := range dirs {
		switch {
		case i >= maxProfileDirs:
			p.skip(dir, fmt.Sprintf("more than %d directories", maxProfileDirs))
			continue
		case dir == "" || !filepath.IsAbs(dir):
			p.skip(dir, "relative directory")
			continue
		case count >= maxProfileNames:
			p.skip(dir, fmt.Sprintf("more than %d names", maxProfileNames))
			continue
		}
		left := profileTotalBudget - time.Since(begun)
		if left <= 0 {
			p.skip(dir, fmt.Sprintf("time budget of %v for the PATH directories used up", profileTotalBudget.Round(time.Millisecond)))
			continue
		}
		res, err := p.readDirBounded(dir, maxProfileNames-count, min(left, profileDirTimeout))
		if err != nil {
			p.skip(dir, err.Error())
			continue
		}
		for _, n := range res.names {
			count++
			p.cands[n.name] = append(p.cands[n.name], n.path)
		}
		if res.over {
			p.skip(dir, fmt.Sprintf("more than %d names", maxProfileNames))
		}
	}
	return p
}

type profName struct{ name, path string }
type dirResult struct {
	names []profName
	over  bool
	err   error
}

// readDirBounded reads dir in chunks, keeping at most budget program-like
// names (over is set when there were more), within profileDirTimeout. The
// read runs in its own goroutine that touches only its own values: on a
// time-out it is left to finish (or hang) on its own.
func (p *Profile) readDirBounded(dir string, budget int, limit time.Duration) (dirResult, error) {
	done := make(chan dirResult, 1)
	go func() { done <- p.readNames(dir, budget) }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case r := <-done:
		return r, r.err
	case <-timer.C:
		return dirResult{}, fmt.Errorf("timed out after %v", limit.Round(time.Millisecond))
	}
}

func (p *Profile) readNames(dir string, budget int) (res dirResult) {
	r, err := openDir(dir)
	if err != nil {
		res.err = err
		return
	}
	defer r.Close()
	read := 0
	for {
		ents, err := r.ReadDir(profileChunk)
		for _, e := range ents {
			if read++; read > 2*maxProfileNames {
				res.over = true
				return
			}
			if e.IsDir() { // a directory that is not a link: nothing to run
				continue
			}
			name := e.Name()
			if p.exts != nil {
				ext := strings.ToLower(filepath.Ext(name))
				if !slices.Contains(p.exts, ext) {
					continue
				}
				name = strings.TrimSuffix(name, filepath.Ext(name))
			}
			if len(res.names) >= budget {
				res.over = true
				return
			}
			res.names = append(res.names, profName{name, filepath.Join(dir, e.Name())})
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			res.err = err
			return
		}
		if len(ents) == 0 {
			return
		}
	}
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

// Lookup returns the path of the program called name. It answers by name
// only: a directory inside a workspace root counts (a repository's own bin/
// commands are not missing). The first call for a name looks at its files
// (one stat each, until a program is found); the answer is kept. A name with a
// path separator is never found. Safe on a nil Profile.
func (p *Profile) Lookup(name string) (string, bool) {
	if p == nil || name == "" || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	path, ok := p.known[name]
	if !ok {
		for _, c := range p.cands[name] {
			if isProgram(c, p.exts) {
				path = c
				break
			}
		}
		p.known[name] = path
	}
	return path, path != ""
}

// Skipped lists the directories that were not read, as "dir: reason".
func (p *Profile) Skipped() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.skipped...)
}

// Dirs is the number of directories the profile was given, those skipped
// included.
func (p *Profile) Dirs() int {
	if p == nil {
		return 0
	}
	return p.dirs
}

// Len is the number of distinct names in the directories read (not all of
// them are programs).
func (p *Profile) Len() int {
	if p == nil {
		return 0
	}
	return len(p.cands)
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

// isProgram reports whether the file at full (links followed) is a regular
// file with an execute bit, or on Windows (exts set; the name was matched
// against PATHEXT already) a regular file.
func isProgram(full string, exts []string) bool {
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return exts != nil || fi.Mode().Perm()&0o111 != 0
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
