package analysis

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// globIndex lists the workspace paths by directory, so that a pattern whose
// directory part is literal looks only at the names of that directory instead
// of at every path (200 loaders over 1,000 files were 200,000 matches per
// build). It is made once per Build, on the first pattern, and is read-only.
type globIndex struct {
	paths []string
	once  sync.Once
	byDir map[string][]string // directory -> its files' paths, sorted; made on the first match
	tried atomic.Int64        // names compared against a pattern so far
}

func newGlobIndex(paths []string) *globIndex { return &globIndex{paths: paths} }

// matchMeta reports whether s holds a pattern character, with filepath.Match's
// rule: a backslash escapes except on Windows, where it is the separator.
func matchMeta(s string) bool {
	if runtime.GOOS == "windows" {
		return strings.ContainsAny(s, "*?[")
	}
	return strings.ContainsAny(s, `*?[\`)
}

// match returns the paths that filepath.Match accepts for pat, sorted, the
// first maxGlobMatches+1 of them at most (the caller cuts at maxGlobMatches
// and reports it). A pattern with a pattern character in its directory part
// is matched against every path.
func (g *globIndex) match(pat string) []string {
	dir, base := filepath.Split(pat)
	dir = filepath.Clean(dir)
	var out []string
	if matchMeta(dir) {
		for _, p := range g.paths { // g.paths is sorted
			g.tried.Add(1)
			if ok, _ := filepath.Match(pat, p); ok {
				if out = append(out, p); len(out) > maxGlobMatches {
					break
				}
			}
		}
		return out
	}
	g.once.Do(func() {
		g.byDir = make(map[string][]string, len(g.paths)/4+1)
		for _, p := range g.paths {
			d := filepath.Dir(p)
			g.byDir[d] = append(g.byDir[d], p)
		}
		for _, ps := range g.byDir {
			sort.Strings(ps)
		}
	})
	for _, p := range g.byDir[dir] {
		g.tried.Add(1)
		if ok, _ := filepath.Match(base, filepath.Base(p)); ok {
			if out = append(out, p); len(out) > maxGlobMatches {
				break
			}
		}
	}
	return out
}
