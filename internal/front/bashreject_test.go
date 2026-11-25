package front

import (
	"testing"
)

// Constructs `bash -n` rejects must give an error at the token bash names.

func TestStrayElseIsAnError(t *testing.T) {
	f := Parse("e.sh", []byte("#!/bin/bash\necho a\nelse\necho b\n"), Bash)
	if len(f.Errors) != 1 {
		t.Fatalf("errors: %+v", f.Errors)
	}
	if e := f.Errors[0]; e.Range.Start.Line != 3 || e.Range.Start.Col != 1 || e.Internal {
		t.Fatalf("error: %+v", e)
	}
}

func TestElseThatIsNotAKeywordIsNotAnError(t *testing.T) {
	for _, src := range []string{
		"if true; then echo a; else echo b; fi\n",
		"\"else\" x\n",
		"echo else\n",
		"else=1\n",
		"\\else\n",
	} {
		if f := Parse("e.sh", []byte("#!/bin/bash\n"+src), Bash); len(f.Errors) != 0 {
			t.Errorf("%q: %+v", src, f.Errors)
		}
	}
}

func TestEmptyThenIsReportedAtTheNextKeyword(t *testing.T) {
	for _, c := range []struct {
		src       string
		line, col int
	}{
		{"#!/bin/bash\nif true; then\nfi\n", 3, 1},
		{"#!/bin/bash\nif true; then\n  :\nelse\nfi\n", 5, 1},
		{"#!/bin/bash\nif true; then\n  :\nelif true; then\nfi\n", 5, 1},
		{"#!/bin/bash\nif true; then\n  :\nelse\n  fi\n", 5, 3},
	} {
		f := Parse("e.sh", []byte(c.src), Bash)
		if len(f.Errors) != 1 || f.Errors[0].Msg != "missing command" {
			t.Errorf("%q: %+v", c.src, f.Errors)
			continue
		}
		if p := f.Errors[0].Range.Start; p.Line != c.line || p.Col != c.col {
			t.Errorf("%q: error at %d:%d, want %d:%d", c.src, p.Line, p.Col, c.line, c.col)
		}
	}
}
