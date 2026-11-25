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

func TestEmptyBodiesAreReportedAtTheClosingToken(t *testing.T) {
	for _, c := range []struct {
		src       string
		line, col int
	}{
		{"#!/bin/bash\nwhile true; do\ndone\n", 3, 1},
		{"#!/bin/bash\nuntil true; do\n  done\n", 3, 3},
		{"#!/bin/bash\nfor i in 1; do\ndone\n", 3, 1},
		{"#!/bin/bash\nf() {\n}\n", 3, 1},
		{"#!/bin/bash\n{\n}\n", 3, 1},
		{"#!/bin/bash\nf() ( )\n", 2, 7},
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

// bash -n 3.2 to 5.3 rejects `else() { :; }`; `function else { :; }` it accepts.
func TestElseAsFunctionName(t *testing.T) {
	if f := Parse("e.sh", []byte("#!/bin/bash\nelse() { :; }\n"), Bash); len(f.Errors) != 1 || f.Errors[0].Range.Start.Line != 2 {
		t.Errorf("else(): %+v", f.Errors)
	}
	if f := Parse("e.sh", []byte("#!/bin/bash\nfunction else { :; }\n"), Bash); len(f.Errors) != 0 {
		t.Errorf("function else: %+v", f.Errors)
	}
}

// bash -n 5.3 accepts `else` in backticks and in a here-document body (it does
// not read them until they run), and rejects it in $( ) and <( ) (4.x accepts).
func TestElseInStringsBashDoesNotParse(t *testing.T) {
	for src, errs := range map[string]int{
		"echo `else`\n":             0,
		"cat <<EOF\n$(else)\nEOF\n": 0,
		"x=$(else)\n":               1,
		"cat <(else)\n":             1,
	} {
		if f := Parse("e.sh", []byte("#!/bin/bash\n"+src), Bash); len(f.Errors) != errs {
			t.Errorf("%q: %+v", src, f.Errors)
		}
	}
}
