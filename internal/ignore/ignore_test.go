package ignore

import "testing"

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
