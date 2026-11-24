package ignore

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestGitignore(t *testing.T) {
	m := Parse("", []byte("# comment\n*.log\n/build\nnode_modules/\ndocs/**/*.tmp\n!keep.log\nsrc/gen?.go\n"))
	cases := []struct {
		rel string
		dir bool
		ign bool
	}{
		{"a.log", false, true},
		{"x/y/a.log", false, true},
		{"keep.log", false, false},
		{"build", true, true},
		{"sub/build", true, false}, // anchored
		{"node_modules", true, true},
		{"a/node_modules", true, true},
		{"node_modules", false, false}, // dir-only rule
		{"docs/a/b/c.tmp", false, true},
		{"docs/c.tmp", false, true},
		{"src/gen1.go", false, true},
		{"src/gen12.go", false, false},
		{"main.go", false, false},
	}
	for _, c := range cases {
		s := &Set{}
		s.Add(m)
		if got := s.Ignored(c.rel, c.dir); got != c.ign {
			t.Errorf("Ignored(%q, dir=%v) = %v, want %v", c.rel, c.dir, got, c.ign)
		}
	}
}

func TestNestedOverride(t *testing.T) {
	s := &Set{}
	s.Add(Parse("", []byte("*.gen\n")))
	s.Add(Parse("pkg", []byte("!*.gen\nlocal.txt\n")))
	if !s.Ignored("a.gen", false) {
		t.Error("root a.gen should be ignored")
	}
	if s.Ignored("pkg/a.gen", false) {
		t.Error("pkg/a.gen should be re-included by the nested file")
	}
	if !s.Ignored("pkg/local.txt", false) || s.Ignored("local.txt", false) {
		t.Error("nested pattern must only apply below its directory")
	}
}

func TestParseFold(t *testing.T) {
	cases := []struct {
		name, base, pat, rel string
		dir, fold, want      bool
	}{
		{"star fold", "", "*.LOG\n", "a.log", false, true, true},
		{"star exact", "", "*.LOG\n", "a.log", false, false, false},
		{"dir base fold", "Sub", "Build/\n", "sub/build", true, true, true},
		{"dir base exact", "Sub", "Build/\n", "sub/build", true, false, false},
		{"base other dir", "Sub", "Build/\n", "subx/build", true, true, false},
		{"base itself", "Sub", "Build/\n", "SUB", true, true, false},
	}
	for _, c := range cases {
		s := &Set{}
		s.Add(ParseFold(c.base, []byte(c.pat), c.fold))
		if got := s.Ignored(c.rel, c.dir); got != c.want {
			t.Errorf("%s: Ignored(%q) = %v, want %v", c.name, c.rel, got, c.want)
		}
	}
	if a, b := Parse("x", []byte("*.log\n")), ParseFold("x", []byte("*.log\n"), false); a.Base != b.Base || a.fold != b.fold {
		t.Error("Parse differs from ParseFold(false)")
	}
}

func TestNonASCIIPatterns(t *testing.T) {
	nfc, nfd := "é.txt", "é.txt"
	cases := []struct {
		name, pat, rel string
		fold, want     bool
	}{
		{"literal itself", nfc, nfc, false, true},
		{"literal in dir", "dossier/" + nfc, "dossier/" + nfc, false, true},
		{"literal other name", nfc, "e.txt", false, false},
		{"class", "[éa].txt", nfc, false, true},
		{"class miss", "[éa].txt", "b.txt", false, false},
		{"question mark is one rune", "?.txt", nfc, false, true},
		{"star", "*é.txt", "x" + nfc, false, true},
		{"escaped", "\\é.txt", nfc, false, true},
		{"fold upper pattern", "É.txt", nfc, true, true},
		{"fold off upper pattern", "É.txt", nfc, false, false},
		{"NFC pattern, NFD name", nfc, nfd, true, false},
		{"sharp s is not SS", "straße", "STRASSE", true, false},
	}
	for _, c := range cases {
		s := &Set{}
		s.Add(ParseFold("", []byte(c.pat+"\n"), c.fold))
		if got := s.Ignored(c.rel, false); got != c.want {
			t.Errorf("%s: pattern %q on %q (fold %v) = %v, want %v", c.name, c.pat, c.rel, c.fold, got, c.want)
		}
	}
}

// The deepest matcher with a verdict decides, and the matchers above it are
// not evaluated: the work does not multiply with the nesting.
func TestSetDoesNotReEvaluateAncestors(t *testing.T) {
	b := NewBudget(1<<20, 1<<20, 0)
	var rules strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&rules, "never%d\n", i)
	}
	set := (&Set{}).WithBudget(b)
	set.Add(b.ParseFold("", []byte(rules.String()), false))
	set.Add(b.ParseFold("a", []byte(rules.String()), false))
	set.Add(b.ParseFold("a/b", []byte(rules.String()+"*.log\n"), false))
	if !set.Ignored("a/b/x.log", false) {
		t.Fatal("x.log is not ignored")
	}
	if b.Evals() != 1 {
		t.Fatalf("%d evaluations for a path the deepest matcher decides, want 1", b.Evals())
	}
	// A path nothing matches is looked at once per rule, in each matcher.
	before := b.Evals()
	if set.Ignored("a/b/y.txt", false) || b.Evals()-before != 3001 {
		t.Fatalf("%d evaluations", b.Evals()-before)
	}
}

func TestBudgetStopsApplyingRules(t *testing.T) {
	b := NewBudget(3, 5, 0)
	m := b.ParseFold("", []byte("a\nb\n# c\nc\nd\ne\n"), false)
	if len(m.rules) != 3 {
		t.Fatalf("%d rules compiled, want 3", len(m.rules))
	}
	if h := b.Hit(); len(h) != 1 || !strings.Contains(h[0], "rule limit of 3") {
		t.Fatalf("hit %q", h)
	}
	set := (&Set{}).WithBudget(b)
	set.Add(m)
	for i := 0; i < 5; i++ { // 3 rules per path: the budget of 5 runs out in the second
		set.Ignored("zzz", false)
	}
	if set.Ignored("a", false) {
		t.Fatal("a rule was applied after the budget ran out")
	}
	if b.Evals() > 5 {
		t.Fatalf("%d evaluations over the budget of 5", b.Evals())
	}
	if h := b.Hit(); len(h) != 2 || !strings.Contains(h[1], "evaluation limit reached (5 plus 0") {
		t.Fatalf("hit %q", h)
	}
	if (*Budget)(nil).Hit() != nil {
		t.Fatal("nil budget hit")
	}
}

// A rule that excludes a directory outlives the budget: it has an allowance
// of its own, spent on directories only, and the rules past the limit are
// the ones other than dir-only.
func TestDirOnlyRulesOutliveTheBudget(t *testing.T) {
	b := NewBudget(2, 4, 0)
	m := b.ParseFold("", []byte("node_modules/\na\nb\nc\nd\nvendor/\n"), false)
	if len(m.rules) != 4 { // node_modules/, a, b, vendor/
		t.Fatalf("%d rules compiled, want 4", len(m.rules))
	}
	set := (&Set{}).WithBudget(b)
	set.Add(m)
	for i := 0; i < 10; i++ {
		set.Ignored("zzz", false)
	}
	if !set.Ignored("node_modules", true) || !set.Ignored("x/vendor", true) {
		t.Fatal("a dir-only rule was dropped by the budget")
	}
	if set.Ignored("a", false) {
		t.Fatal("a file rule applied after the budget ran out")
	}
	if len(b.Hit()) != 2 {
		t.Fatalf("hit %q", b.Hit())
	}
}

// Set.Ignored looks deepest first; it has to give the answer of the plain
// forward evaluation (every rule, last match wins) on any rules.
func TestSetMatchesForwardEvaluation(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pats := []string{"*.log", "build/", "!keep.log", "/a", "a/b", "!a/b", "*.sh", "!x.sh", "d/", "**/z", "q?", "!*.tmp", "tmp"}
	paths := []string{"a", "a/b", "a/b/c.log", "build", "x/build", "keep.log", "x.sh", "d/x.sh", "d", "z", "a/z", "qa", "t.tmp", "tmp/y", "s/keep.log"}
	for n := 0; n < 300; n++ {
		set := &Set{}
		var ms []*Matcher
		for _, base := range []string{"", "a", "a/b"}[:1+r.Intn(3)] {
			var lines []string
			for i := r.Intn(8); i > 0; i-- {
				lines = append(lines, pats[r.Intn(len(pats))])
			}
			m := Parse(base, []byte(strings.Join(lines, "\n")))
			set.Add(m)
			ms = append(ms, m)
		}
		for _, p := range paths {
			for _, dir := range []bool{false, true} {
				want := false
				for _, m := range ms {
					for _, rl := range m.rules {
						rel := p
						if m.Base != "" {
							if !m.inBase(p) {
								continue
							}
							rel = p[len(m.Base)+1:]
						}
						if rl.dirOnly && !dir || !rl.re.MatchString(rel) {
							continue
						}
						want = !rl.negate
					}
				}
				if got := set.Ignored(p, dir); got != want {
					t.Fatalf("round %d %q dir=%v: got %v want %v", n, p, dir, got, want)
				}
			}
		}
	}
}
