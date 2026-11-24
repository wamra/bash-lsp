package analysis

import (
	"fmt"
	"sort"
)

// buildLimits counts, per file, the work bounds Build met: the first line and
// the number of hits. It is written only during Build.
type buildLimits struct {
	alias map[int]limCount // alias chains cut at maxAliasDepth
	glob  map[int]limCount // glob patterns cut at maxGlobMatches

	globTried int64 // names the glob index compared against source patterns in Build
}

type limCount struct{ line, n int }

func (l *buildLimits) note(m *map[int]limCount, file, line int) {
	if *m == nil {
		*m = map[int]limCount{}
	}
	c := (*m)[file]
	if c.n == 0 || line < c.line {
		c.line = line
	}
	c.n++
	(*m)[file] = c
}

func (l *buildLimits) aliasDepth(file, line int) { l.note(&l.alias, file, line) }
func (l *buildLimits) globCap(file, line int)    { l.note(&l.glob, file, line) }

// LimitsHit lists the work bounds this snapshot met, one line per file and
// bound, `path:line: what: detail`, sorted by path and line:
//   - alias depth: uses of an alias chain longer than 8 (or a cycle);
//   - glob cap: a source pattern that matched more than 1,000 files;
//   - every limit the parser recorded for the file (File.Limits);
//   - findings cap: a file whose findings went past the 2,000 Diagnostics
//     returns. That bound depends on the settings the findings were computed
//     with, so it is listed for the files Diagnostics has been asked about on
//     this snapshot, with the options of the call that cut them.
//
// Paths are relative to the root when they lie in it.
func (s *Snapshot) LimitsHit() []string {
	type line struct {
		path string
		at   int
		text string
	}
	var ls []line
	add := func(file, at int, format string, args ...any) {
		ls = append(ls, line{s.rel(s.paths[file]), at, fmt.Sprintf(format, args...)})
	}
	for file, c := range s.lim.alias {
		add(file, c.line, "alias depth: %d uses of an alias chain longer than %d (or a cycle) stop at the %dth alias", c.n, maxAliasDepth, maxAliasDepth)
	}
	for file, c := range s.lim.glob {
		add(file, c.line, "glob cap: %d source patterns match more than %d files; only the first %d are used", c.n, maxGlobMatches, maxGlobMatches)
	}
	for i, ff := range s.facts {
		if ff == nil || ff.File == nil {
			continue
		}
		for _, l := range ff.File.Limits {
			add(i, l.Range.Start.Line, "%s: %s", l.Name, l.Detail)
		}
	}
	s.capMu.Lock()
	for file, n := range s.capped {
		add(file, 1, "findings cap: %d findings past the %d shown for a file are not reported", n, maxFindings)
	}
	s.capMu.Unlock()
	sort.Slice(ls, func(a, b int) bool {
		if ls[a].path != ls[b].path {
			return ls[a].path < ls[b].path
		}
		if ls[a].at != ls[b].at {
			return ls[a].at < ls[b].at
		}
		return ls[a].text < ls[b].text
	})
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = fmt.Sprintf("%s:%d: %s", l.path, l.at, l.text)
	}
	return out
}
