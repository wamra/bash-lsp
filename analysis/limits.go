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

	dynScope map[int]limCount // variable reads bound to the first maxDynScopeLocals caller locals only
	dynTotal map[*Use]int     // for those reads, how many caller locals the name had

	globTried int64 // names the glob index compared against source patterns in Build
}

type limCount struct{ line, n int }

// also counts one more hit at line.
func (c limCount) also(line int) limCount {
	if c.n == 0 || line < c.line {
		c.line = line
	}
	c.n++
	return c
}

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

// dynScopeCap records that the read u of file had total caller locals and was
// bound to the first maxDynScopeLocals.
func (l *buildLimits) dynScopeCap(file int, u *Use, total int) {
	if _, ok := l.dynTotal[u]; ok {
		return
	}
	if l.dynTotal == nil {
		l.dynTotal = map[*Use]int{}
	}
	l.dynTotal[u] = total
	l.note(&l.dynScope, file, u.Range.Start.Line)
}

// limitReasons lists the work bounds of the call graph and the rules that
// LimitsHit reports beyond the ones above, with the format of each line (after
// `path:line: `); the arguments are the count of sites or checks, then the bound.
// The two edge reasons are in reasonNames; the others are not edge reasons.
var limitReasons = []struct{ name, format string }{
	{ReasonCandidateCap, "candidate cap: %d call sites of names or variables with more than %d candidates list only their first %d"},
	{ReasonStringBudget, "string budget: %d string bodies past 2 MiB per file were not parsed"},
	{ReasonDynamicScopeCap, "dynamic-scope cap: %d variable reads bind to the first %d of more caller locals"},
	{"caller-local-search", "caller-local search: %d checks of a local that may reach a reader searched more than %d functions each way; the WB1401 proof they decide is dropped"},
	{"version-guard-search", "version-guard search: %d bash-version checks used up the file's search steps; features they may guard count as guarded"},
}

func limitFormat(name string) string {
	for _, lr := range limitReasons {
		if lr.name == name {
			return lr.format
		}
	}
	panic("analysis: limit reason without a format: " + name)
}

// LimitsHit lists the work bounds this snapshot met, one line per file and
// bound, `path:line: what: detail`, sorted by path and line:
//   - alias depth: uses of an alias chain longer than 8 (or a cycle);
//   - glob cap: a source pattern that matched more than 1,000 files;
//   - every limit the parser recorded for the file (File.Limits);
//   - candidate cap, string budget, dynamic-scope cap: call sites past the
//     per-site target cap, string bodies past the string-parse budget, and
//     variable reads bound to the first caller locals only (limitReasons);
//   - findings cap: a file whose findings went past the 2,000 Diagnostics
//     returns. That bound depends on the settings the findings were computed
//     with, so it is listed for the files Diagnostics has been asked about on
//     this snapshot, with the options of the call that cut them;
//   - rename others, sites, text, by-name and reach caps: renames that listed
//     only the first declarations of same-named symbols, sites of a refusal,
//     bytes of an excluded site's text, refused a name of too many functions,
//     or stopped a dynamic-scope check at its call-graph bound; listed, like
//     the findings cap, for the files the renames touched;
//   - caller-local search and version-guard search: WB1401 proofs dropped and
//     bash-version checks left undecided because a search passed its budget;
//     listed, like the findings cap, for the files Diagnostics has been asked
//     about.
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
	for file, c := range s.lim.dynScope {
		add(file, c.line, limitFormat(ReasonDynamicScopeCap), c.n, maxDynScopeLocals)
	}
	for file, sites := range s.dyn {
		var capped, budget limCount
		for _, d := range sites {
			switch d.reason {
			case codeCandidateCap:
				capped = capped.also(d.rng.Start.Line)
			case codeStringBudget:
				budget = budget.also(d.rng.Start.Line)
			}
		}
		if capped.n > 0 {
			add(file, capped.line, limitFormat(ReasonCandidateCap), capped.n, maxSiteTargets, maxSiteTargets)
		}
		if budget.n > 0 {
			add(file, budget.line, limitFormat(ReasonStringBudget), budget.n)
		}
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
	for file, r := range s.runLim {
		if r.reach.n > 0 {
			add(file, r.reach.line, limitFormat("caller-local-search"), r.reach.n, reachBudget)
		}
		if r.guard.n > 0 {
			add(file, r.guard.line, limitFormat("version-guard-search"), r.guard.n)
		}
	}
	for file, l := range s.renLim {
		for c, n := range l {
			if n.n > 0 {
				add(file, n.line, renameCapFormats[c], n.n, renameCapBounds[c])
			}
		}
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
