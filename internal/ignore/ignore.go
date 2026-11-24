// Package ignore implements .gitignore-style pattern matching, used for the
// .gitignore files the workspace walk honours.
//
// With fold (ParseFold) patterns and Base match case-insensitively by simple
// Unicode case folding. Git on a case-insensitive volume (core.ignoreCase)
// folds ASCII letters only, which was confirmed by experiment; the volume
// itself (APFS) folds fully. Neither this package nor git treats names that
// differ only by normalisation form (NFC against NFD) or by multi-letter
// folding (ß against SS) as equal.
package ignore

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

type rule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// Matcher holds the rules of one ignore file whose directory is Base
// (slash-separated, relative to the workspace root; "" for the root).
type Matcher struct {
	Base   string
	rules  []rule
	dirIdx []int // indexes in rules of the dir-only rules
	fold   bool
}

// Parse compiles the lines of an ignore file located in directory base.
// Parse(base, data) is ParseFold(base, data, false).
func Parse(base string, data []byte) *Matcher { return ParseFold(base, data, false) }

// ParseFold is Parse for a volume that does not tell upper and lower case
// apart: with fold every pattern compiles case-insensitively and Base is
// compared with strings.EqualFold.
func ParseFold(base string, data []byte, fold bool) *Matcher {
	return (*Budget)(nil).ParseFold(base, data, fold)
}

// Budget bounds the work of the ignore files of one walk, in counts. A
// repository controls its .gitignore files, and the cost is rules times paths
// times nesting, so the allowance grows with the entries the walk visits:
// evaluations are a fixed floor plus a per-entry allowance times entries
// visited (Visit), so an ordinary tree never meets it; compiled rules are
// capped. Rules that exclude a directory (dir-only, "node_modules/") have a
// budget of their own that is spent only on directories, and are the last
// to go: they keep a walk out of trees it must not enter. Past a limit the
// rules concerned are ignored from there on, which is not exact either way:
// a file a dropped rule would hide is included, and one a dropped negation
// would bring back stays ignored. The file limits still bound what is read.
// A nil Budget is unlimited.
type Budget struct {
	rulesLeft, dirRulesLeft int // compiled rules still allowed: all other rules, dir-only rules
	evalsLeft, dirEvalsLeft int // evaluations still allowed: all other rules, dir-only rules
	perEntry                int
	rulesHit, evalsHit      bool
	maxRules, floor         int
	evals                   int
}

// NewBudget returns a Budget of maxRules compiled rules of each kind and, for
// evaluations, floor plus perEntry for every entry passed to Visit.
func NewBudget(maxRules, floor, perEntry int) *Budget {
	return &Budget{rulesLeft: maxRules, dirRulesLeft: maxRules, evalsLeft: floor, dirEvalsLeft: floor,
		perEntry: perEntry, maxRules: maxRules, floor: floor}
}

// Visit adds the allowance of one more entry (a file or a directory) the walk
// is about to test.
func (b *Budget) Visit(isDir bool) {
	if b == nil {
		return
	}
	b.evalsLeft += b.perEntry
	if isDir {
		b.dirEvalsLeft += b.perEntry
	}
}

// Evals is how many rule evaluations have been charged.
func (b *Budget) Evals() int { return b.evals }

// Hit returns a line for every count that ran out, for the limits report.
func (b *Budget) Hit() []string {
	if b == nil {
		return nil
	}
	var out []string
	if b.rulesHit {
		out = append(out, fmt.Sprintf("ignore rule limit of %d reached: later .gitignore rules are ignored, so a later negation can be lost", b.maxRules))
	}
	if b.evalsHit {
		out = append(out, fmt.Sprintf("ignore evaluation limit reached (%d plus %d per entry): some .gitignore rules are not applied to later paths, so a file may be included that git ignores, or stay ignored after a negation", b.floor, b.perEntry))
	}
	return out
}

// ParseFold is ParseFold that charges each compiled rule to b.
func (b *Budget) ParseFold(base string, data []byte, fold bool) *Matcher {
	m := &Matcher{Base: strings.Trim(base, "/"), fold: fold}
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		if b != nil {
			// Decide by the text, before compiling: a dir-only rule ends in "/".
			t := bytes.TrimSpace(line)
			if len(t) == 0 || t[0] == '#' {
				continue
			}
			if bytes.HasSuffix(t, []byte("/")) {
				if b.dirRulesLeft <= 0 {
					b.rulesHit = true
					continue
				}
			} else if b.rulesLeft <= 0 {
				b.rulesHit = true
				continue
			}
		}
		if r, ok := parseLine(strings.TrimRight(string(line), "\r"), fold); ok {
			if b != nil {
				if r.dirOnly {
					b.dirRulesLeft--
				} else {
					b.rulesLeft--
				}
			}
			if r.dirOnly {
				m.dirIdx = append(m.dirIdx, len(m.rules))
			}
			m.rules = append(m.rules, r)
		}
	}
	return m
}

func parseLine(line string, fold bool) (rule, bool) {
	// Trailing spaces are ignored unless escaped with a backslash.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return rule{}, false
	}
	var r rule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return rule{}, false
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	expr := "^"
	if fold {
		expr = "(?i)^"
	}
	if !anchored {
		expr += "(?:.*/)?"
	}
	expr += globToRegexp(line) + "$"
	re, err := regexp.Compile(expr)
	if err != nil {
		return rule{}, false
	}
	r.re = re
	return r, true
}

func globToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				// "**/" matches zero or more directories, a trailing "/**"
				// matches everything inside.
				switch {
				case strings.HasPrefix(g[i:], "**/"):
					b.WriteString("(?:.*/)?")
					i += 2
				case i+2 == len(g):
					b.WriteString(".*")
					i++
				default:
					b.WriteString("[^/]*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case '\\':
			if i+1 < len(g) {
				i++
				_, n := utf8.DecodeRuneInString(g[i:])
				b.WriteString(regexp.QuoteMeta(g[i : i+n]))
				i += n - 1
			}
		default:
			// A literal is a whole rune, not one byte of it.
			_, n := utf8.DecodeRuneInString(g[i:])
			b.WriteString(regexp.QuoteMeta(g[i : i+n]))
			i += n - 1
		}
	}
	return b.String()
}

// Result of matching one path against a Matcher.
type Result int

const (
	None Result = iota
	Ignored
	Included // a negation rule matched
)

// inBase reports whether rel lies below Base, with the matcher's case rule.
func (m *Matcher) inBase(rel string) bool {
	if !m.fold {
		return strings.HasPrefix(rel, m.Base+"/")
	}
	n := len(m.Base)
	return len(rel) > n && rel[n] == '/' && strings.EqualFold(rel[:n], m.Base)
}

// Match evaluates rel (slash-separated, relative to the workspace root) and
// returns the verdict of the last rule that matches, or None.
func (m *Matcher) Match(rel string, isDir bool) Result {
	return m.match(rel, isDir, nil)
}

// match is Match that charges each rule it evaluates to b. It looks from
// the last rule back and stops at the first that matches, which is the last
// match. When the evaluations of b ran out, only dir-only rules are looked at
// (and only for a directory), while their own allowance lasts.
func (m *Matcher) match(rel string, isDir bool, b *Budget) Result {
	if m.Base != "" {
		if !m.inBase(rel) {
			return None
		}
		rel = rel[len(m.Base)+1:]
	}
	verdict := func(r *rule) Result {
		if r.negate {
			return Included
		}
		return Ignored
	}
	if b != nil && b.evalsLeft <= 0 {
		b.evalsHit = true
		if !isDir {
			return None
		}
		for i := len(m.dirIdx) - 1; i >= 0; i-- {
			r := &m.rules[m.dirIdx[i]]
			if b.dirEvalsLeft <= 0 {
				b.evalsHit = true
				return None
			}
			b.dirEvalsLeft--
			b.evals++
			if r.re.MatchString(rel) {
				return verdict(r)
			}
		}
		return None
	}
	for i := len(m.rules) - 1; i >= 0; i-- {
		r := &m.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		if b != nil {
			if r.dirOnly {
				if b.dirEvalsLeft <= 0 {
					b.evalsHit = true
					continue
				}
				b.dirEvalsLeft--
			} else {
				if b.evalsLeft <= 0 {
					b.evalsHit = true
					continue
				}
				b.evalsLeft--
			}
			b.evals++
		}
		if r.re.MatchString(rel) {
			return verdict(r)
		}
	}
	return None
}

// Set is an ordered list of matchers: outer directories first. Later (deeper)
// matchers override earlier ones, as git does.
type Set struct {
	ms     []*Matcher
	budget *Budget
}

// Add appends m (call with shallower directories first).
func (s *Set) Add(m *Matcher) { s.ms = append(s.ms, m) }

// WithBudget makes the set charge its evaluations to b, and returns s.
// Clones share it.
func (s *Set) WithBudget(b *Budget) *Set { s.budget = b; return s }

// Clone returns a copy that can be extended independently.
func (s *Set) Clone() *Set { return &Set{ms: append([]*Matcher(nil), s.ms...), budget: s.budget} }

// Ignored reports whether rel is ignored by the set: the verdict of the
// deepest matcher that has one. Matchers above it are not evaluated.
func (s *Set) Ignored(rel string, isDir bool) bool {
	for i := len(s.ms) - 1; i >= 0; i-- {
		switch s.ms[i].match(rel, isDir, s.budget) {
		case Ignored:
			return true
		case Included:
			return false
		}
	}
	return false
}
