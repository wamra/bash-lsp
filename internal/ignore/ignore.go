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
	Base  string
	rules []rule
	fold  bool
}

// Parse compiles the lines of an ignore file located in directory base.
// Parse(base, data) is ParseFold(base, data, false).
func Parse(base string, data []byte) *Matcher { return ParseFold(base, data, false) }

// ParseFold is Parse for a volume that does not tell upper and lower case
// apart: with fold every pattern compiles case-insensitively and Base is
// compared with strings.EqualFold.
func ParseFold(base string, data []byte, fold bool) *Matcher {
	m := &Matcher{Base: strings.Trim(base, "/"), fold: fold}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if r, ok := parseLine(line, fold); ok {
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
	if m.Base != "" {
		if !m.inBase(rel) {
			return None
		}
		rel = rel[len(m.Base)+1:]
	}
	res := None
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(rel) {
			if r.negate {
				res = Included
			} else {
				res = Ignored
			}
		}
	}
	return res
}

// Set is an ordered list of matchers: outer directories first. Later (deeper)
// matchers override earlier ones, as git does.
type Set struct{ ms []*Matcher }

// Add appends m (call with shallower directories first).
func (s *Set) Add(m *Matcher) { s.ms = append(s.ms, m) }

// Clone returns a copy that can be extended independently.
func (s *Set) Clone() *Set { return &Set{ms: append([]*Matcher(nil), s.ms...)} }

// Ignored reports whether rel is ignored by the set.
func (s *Set) Ignored(rel string, isDir bool) bool {
	ign := false
	for _, m := range s.ms {
		switch m.Match(rel, isDir) {
		case Ignored:
			ign = true
		case Included:
			ign = false
		}
	}
	return ign
}
