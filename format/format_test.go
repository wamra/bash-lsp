package format

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

func u(n uint) *uint { return &n }
func b(v bool) *bool { return &v }

func TestFormatRefusesBroken(t *testing.T) {
	cases := map[string]string{
		"unclosed-quote": "echo ok\necho \"abc\necho more\n",
		"stray-paren":    "echo ok\n)\necho ok2\n",
		"recovered-fi":   "if true; then\n  echo a\n",
	}
	for _, p := range []string{"../testdata/corpus/broken_exp.sh", "../testdata/corpus/broken_fi.sh", "../testdata/broken/junk.sh"} {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		cases[filepath.Base(p)] = string(src)
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			in := []byte(src)
			orig := bytes.Clone(in)
			out, err := Source("x.sh", in, Options{})
			if !errors.Is(err, ErrSyntax) || out != nil {
				t.Fatalf("out %q err %v, want ErrSyntax", out, err)
			}
			if !bytes.Equal(in, orig) {
				t.Fatal("the input changed")
			}
			if _, _, text, err := Range("x.sh", in, 0, len(in), Options{}); !errors.Is(err, ErrSyntax) || text != nil {
				t.Fatalf("Range: %q %v", text, err)
			}
		})
	}
}

func TestFormatKeepsCRLFAndHeredoc(t *testing.T) {
	src := "#!/bin/sh\r\n# keep   this\r\nf(){\r\ncat <<EOF\r\n  body   as is\r\nEOF\r\n}\r\n"
	out, err := Source("x.sh", []byte(src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\r\n# keep   this\r\nf() {\r\n\tcat <<EOF\r\n  body   as is\r\nEOF\r\n}\r\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

// big is the 2,000-function script of bench/gen_big.py, byte for byte.
func big() string {
	var out []string
	out = append(out, "#!/usr/bin/env bash", "echo start")
	for i := 0; i < 2000; i++ {
		out = append(out, fmt.Sprintf("# Function number %d.", i), fmt.Sprintf("fn_%d() {", i), `  local a="$1" b="$2"`,
			fmt.Sprintf(`  if [[ -n "$a" && "$b" != "x%d" ]]; then`, i), `    printf '%s %s\n' "$a" "$b" | tr a-z A-Z`, "  else",
			`    for f in "$@"; do echo "${f:-none}" >&2; done`, "  fi",
			fmt.Sprintf(`  case "$a" in start|stop) echo "$a";; *) return %d;; esac`, i%250), "}", "")
		if i > 0 {
			out = append(out, fmt.Sprintf("fn_%d a b", i-1))
		}
	}
	out = append(out, "fn_1999 a b")
	return strings.Join(out, "\n") + "\n"
}

func TestBigFileIsTheBenchmarkFile(t *testing.T) {
	if n := len(big()); n != 560711 {
		t.Fatalf("%d bytes, want 560711", n)
	}
}

func TestIdempotent(t *testing.T) {
	inputs := map[string]string{"big": big()}
	err := filepath.Walk("../testdata/corpus", func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			src, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			inputs[p] = string(src)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	formatted := 0
	for name, src := range inputs {
		for _, o := range []Options{{}, {Indent: 2, BinaryNextLine: true, SwitchCaseIndent: true, SpaceRedirects: true, FunctionNextLine: true, Simplify: true}} {
			once, err := Source(name, []byte(src), o)
			if err != nil {
				if !strings.Contains(name, "broken") {
					t.Errorf("%s: %v", name, err)
				}
				continue
			}
			twice, err := Source(name, once, o)
			if err != nil || !bytes.Equal(once, twice) {
				t.Errorf("%s %+v: not idempotent (err %v)", name, o, err)
			}
			formatted++
		}
	}
	if formatted < 12 {
		t.Fatalf("only %d formatted", formatted)
	}
}

func TestSimplifyOption(t *testing.T) {
	src := []byte("[[ \"$a\" == b ]]\n")
	out, err := Source("x.sh", src, Options{})
	if err != nil || !bytes.Equal(out, src) {
		t.Fatalf("without: %q %v", out, err)
	}
	out, err = Source("x.sh", src, Options{Simplify: true})
	if err != nil || string(out) != "[[ $a == b ]]\n" {
		t.Fatalf("with: %q %v", out, err)
	}
}

func TestUnsupported(t *testing.T) {
	for name, src := range map[string]string{"zsh": "#!/bin/zsh\necho a\n", "bom": "\xef\xbb\xbfecho a\n", "nul": "echo \x00\n", "large": "#" + strings.Repeat("x", 2<<20)} {
		out, err := Source("x.sh", []byte(src), Options{})
		if !errors.Is(err, ErrUnsupported) || out != nil {
			t.Errorf("%s: %q %v", name, out, err)
		}
	}
}

func TestRangeTopLevel(t *testing.T) {
	src := "a()   {\necho   one\n}\n\nb()   {\necho   two\n}\n\nc()   {\necho   three\n}\n"
	i := strings.Index(src, "two")
	start, end, text, err := Range("x.sh", []byte(src), i, i+3, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := src[:start] + string(text) + src[end:]
	want := "a()   {\necho   one\n}\n\nb() {\n\techo two\n}\n\nc()   {\necho   three\n}\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	// An empty range (a cursor) selects the statement it is in.
	start, end, text, err = Range("x.sh", []byte(src), i, i, Options{})
	if err != nil || src[:start]+string(text)+src[end:] != want {
		t.Fatalf("cursor: %d %d %q %v", start, end, text, err)
	}
	// A range over the blank line between functions touches nothing.
	j := strings.Index(src, "}\n\nb") + 2
	start, end, text, err = Range("x.sh", []byte(src), j, j, Options{})
	if err != nil || start != end || len(text) != 0 {
		t.Fatalf("blank: %d %d %q %v", start, end, text, err)
	}
	// Out of bounds.
	if _, _, _, err := Range("x.sh", []byte(src), 5, len(src)+1, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("bounds: %v", err)
	}
}

func TestRangeKeepsLeadingCommentsAndSemicolons(t *testing.T) {
	src := "# lead   one\n# lead two\nx=1;   y=2   # trail\necho   z\n"
	i := strings.Index(src, "y=2")
	start, end, text, err := Range("x.sh", []byte(src), i, i+1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := src[:start] + string(text) + src[end:]
	if got != "# lead   one\n# lead two\nx=1;   y=2 # trail\necho   z\n" {
		t.Fatalf("got %q", got)
	}
	// A range that covers the whole file does what Source does.
	start, end, text, err = Range("x.sh", []byte(src), 0, len(src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	whole, _ := Source("x.sh", []byte(src), Options{})
	if got := src[:start] + string(text) + src[end:]; got == "" || strings.TrimRight(got, "\n") != strings.TrimRight(string(whole), "\n") {
		t.Fatalf("whole range %q vs Source %q", got, whole)
	}
}

func TestRangeCRLF(t *testing.T) {
	src := "a()   {\r\necho   one\r\n}\r\n\r\nb()   {\r\necho   two\r\n}\r\n"
	i := strings.Index(src, "two")
	start, end, text, err := Range("x.sh", []byte(src), i, i+3, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := src[:start] + string(text) + src[end:]
	if got != "a()   {\r\necho   one\r\n}\r\n\r\nb() {\r\n\techo two\r\n}\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEdit(t *testing.T) {
	for _, c := range []struct {
		old, new string
		start    int
		end      int
	}{
		{"a\nb\nc\n", "a\nB\nc\n", 2, 4},
		{"a\nb\nc\n", "a\nb\nc\n", 0, 0},
		{"a", "a\n", 0, 1},
		{"", "x\n", 0, 0},
		{"x\ny\n", "", 0, 4},
		{"a\n  b\nc\n", "a\nb\nc\n", 2, 6},
		{"echo  a\necho b\n", "echo a\necho b\n", 0, 8},
	} {
		start, end, text := Edit([]byte(c.old), []byte(c.new))
		if c.old == c.new {
			if start != end || len(text) != 0 {
				t.Errorf("%q->%q: %d %d %q, want no edit", c.old, c.new, start, end, text)
			}
			continue
		}
		if start != c.start || end != c.end {
			t.Errorf("%q->%q: [%d,%d), want [%d,%d)", c.old, c.new, start, end, c.start, c.end)
		}
		got := c.old[:start] + string(text) + c.old[end:]
		if got != c.new {
			t.Errorf("%q->%q: applying %d %d %q gives %q", c.old, c.new, start, end, text, got)
		}
		if start > 0 && c.old[start-1] != '\n' {
			t.Errorf("%q->%q: start %d is not at a line start", c.old, c.new, start)
		}
		if end > 0 && end < len(c.old) && c.old[end-1] != '\n' {
			t.Errorf("%q->%q: end %d is not at a line end", c.old, c.new, end)
		}
	}
}

func TestOptionsForPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte("[*.sh]\nindent_style=space\nindent_size=2\nswitch_case_indent=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.sh")
	lsp := &LSPOptions{TabSize: 8, InsertSpaces: true}
	s := Settings{Indent: u(3), BinaryNextLine: b(true), SwitchCaseIndent: b(false)}
	got := OptionsFor(file, dir, s, lsp)
	want := Options{Indent: 2, BinaryNextLine: true, SwitchCaseIndent: true}
	if got != want {
		t.Fatalf(".editorconfig > settings > LSP: got %+v, want %+v", got, want)
	}
	// Other file: no .editorconfig section; settings over LSP over tabs.
	other := filepath.Join(dir, "a.bash")
	if got := OptionsFor(other, dir, s, lsp); got != (Options{Indent: 3, BinaryNextLine: true}) {
		t.Fatalf("settings: got %+v", got)
	}
	if got := OptionsFor(other, dir, Settings{}, lsp); got != (Options{Indent: 8}) {
		t.Fatalf("LSP spaces: got %+v", got)
	}
	if got := OptionsFor(other, dir, Settings{}, &LSPOptions{TabSize: 4}); got != (Options{}) {
		t.Fatalf("LSP tabs: got %+v", got)
	}
	if got := OptionsFor(other, dir, Settings{}, nil); got != (Options{}) {
		t.Fatalf("default: got %+v (shfmt's default is tabs)", got)
	}
}

func TestOptionsForEditorconfigKeys(t *testing.T) {
	dir := t.TempDir()
	ec := "root = true\n[*]\nindent_style=space\ncase_indent=true\nspace_redirects=true\nfunction_next_line=true\nsimplify=true\nbinary_next_line=true\nkeep_padding=true\nminify=true\n[b.sh]\nindent_style=tab\n[c.sh]\nindent_style=space\n[d.sh]\nindent_style=space\nindent_size=tab\ntab_width=3\n"
	if err := os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte(ec), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func(name string) Options { return OptionsFor(filepath.Join(dir, name), dir, Settings{}, nil) }
	all := Options{Indent: 8, BinaryNextLine: true, SwitchCaseIndent: true, SpaceRedirects: true, FunctionNextLine: true, Simplify: true}
	if got := get("a.sh"); got != all {
		t.Fatalf("a.sh: %+v", got)
	}
	if got := get("c.sh"); got != all {
		t.Fatalf("c.sh: %+v (indent_style=space without a size is 8, as shfmt)", got)
	}
	tabs := all
	tabs.Indent = 0
	if got := get("b.sh"); got != tabs {
		t.Fatalf("b.sh: %+v", got)
	}
	three := all
	three.Indent = 3
	if got := get("d.sh"); got != three {
		t.Fatalf("d.sh: %+v", got)
	}
}

func TestRangeHeredoc(t *testing.T) {
	for name, c := range map[string]struct{ src, sel, want string }{
		"plain":  {"echo   a\ncat <<EOF\n  body   x\nEOF\necho   b\n", "body", "echo   a\ncat <<EOF\n  body   x\nEOF\necho   b\n"},
		"piped":  {"echo   a\ncat   <<EOF |   tr a b\nbody\nEOF\necho   b\n", "body", "echo   a\ncat <<EOF | tr a b\nbody\nEOF\necho   b\n"},
		"two":    {"cat <<A   <<B\n1\nA\n2\nB\necho   z\n", "echo", "cat <<A   <<B\n1\nA\n2\nB\necho z\n"},
		"dash":   {"if a;then\n\tcat <<-EOF\n\tx\n\tEOF\nfi\n", "cat", "if a; then\n\tcat <<-EOF\n\t\tx\n\tEOF\nfi\n"},
		"nested": {"f()   {\n  cat <<EOF\nin  f\nEOF\n}\n", "cat", "f() {\n\tcat <<EOF\nin  f\nEOF\n}\n"},
		"in-and": {"true   && cat <<EOF\nb\nEOF\n", "true", "true && cat <<EOF\nb\nEOF\n"},
	} {
		t.Run(name, func(t *testing.T) {
			i := strings.Index(c.src, c.sel)
			start, end, text, err := Range("x.sh", []byte(c.src), i, i+1, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got := c.src[:start] + string(text) + c.src[end:]; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Whatever the range, the result of applying the edit is a script that
// parses to the same tree, or the call refuses with ErrUnsupported (never
// ErrSyntax: the file has no syntax error).
func TestRangeEverywhere(t *testing.T) {
	srcs := []string{
		"a   &\nb   ;   c\n# x\nd |   e   # t\n",
		"cat <<EOF |   tr a b\nbody\nEOF\necho   z\n",
		"x=1;   y=2\nif a;then b;fi;   echo   q\n",
		"f()   { cat <<E\n  x\nE\n}\n   g   &   h\n",
		"echo   a   &&\n  echo   b\n\n\n# end\n",
	}
	for _, src := range srcs {
		for i := 0; i <= len(src); i++ {
			for _, j := range []int{i, i + 1, len(src)} {
				if j > len(src) || j < i {
					continue
				}
				start, end, text, err := Range("x.sh", []byte(src), i, j, Options{})
				if err != nil {
					if errors.Is(err, ErrSyntax) {
						t.Errorf("%q [%d,%d): %v", src, i, j, err)
					}
					continue
				}
				got := src[:start] + string(text) + src[end:]
				if _, err := Source("x.sh", []byte(got), Options{}); err != nil {
					t.Errorf("%q [%d,%d): the result %q does not format: %v", src, i, j, got, err)
				}
			}
		}
	}
}

// bash.dialect names the grammar of a file that names none itself: the With
// variants take it, Source and Range keep detecting bash.
func TestFormatWithFallbackDialect(t *testing.T) {
	src := []byte("[[ $a<$b ]]\n")
	if out, err := Source("x.sh", src, Options{}); err != nil || string(out) != "[[ $a < $b ]]\n" {
		t.Fatalf("default: %q %v", out, err)
	}
	if out, err := SourceWith("x.sh", src, Options{}, tree.Bash); err != nil || string(out) != "[[ $a < $b ]]\n" {
		t.Fatalf("bash fallback: %q %v", out, err)
	}
	if out, err := SourceWith("x.sh", src, Options{}, 0); err != nil || string(out) != "[[ $a < $b ]]\n" {
		t.Fatalf("zero fallback is bash: %q %v", out, err)
	}
	out, err := SourceWith("x.sh", src, Options{}, tree.POSIX)
	if !errors.Is(err, ErrUnsupported) || out != nil || !strings.Contains(err.Error(), "grammar-dependent") {
		t.Fatalf("sh fallback: %q %v", out, err)
	}
	if _, _, _, err := RangeWith("x.sh", src, 0, len(src), Options{}, tree.POSIX); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("range with sh fallback: %v", err)
	}
	if _, _, text, err := RangeWith("x.sh", src, 0, len(src), Options{}, tree.Bash); err != nil || string(text) != "[[ $a < $b ]]" {
		t.Fatalf("range with bash fallback: %q %v", text, err)
	}
	// A file that names its dialect keeps it whatever the fallback says.
	named := []byte("#!/bin/bash\n[[ $a<$b ]]\n")
	if out, err := SourceWith("x.sh", named, Options{}, tree.POSIX); err != nil || string(out) != "#!/bin/bash\n[[ $a < $b ]]\n" {
		t.Fatalf("shebang wins: %q %v", out, err)
	}
}

// An .editorconfig is repo-controlled: an indent beyond MaxIndent is clamped
// to it, and the clamp is reported in the editorconfig notes.
func TestEditorconfigIndentIsClamped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte("[*]\nindent_style = space\nindent_size = 10000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.sh")
	o := OptionsFor(file, dir, Settings{}, nil)
	if o.Indent != MaxIndent {
		t.Fatalf("indent %d, want %d", o.Indent, MaxIndent)
	}
	out, err := Source(file, []byte("f() {\necho a\n}\n"), o)
	if err != nil || len(out) > 64 {
		t.Fatalf("%d bytes, err %v", len(out), err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte("[*]\nindent_style = space\nindent_size = tab\ntab_width = 99999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if o := OptionsFor(file, dir, Settings{}, nil); o.Indent != MaxIndent {
		t.Fatalf("tab_width: indent %d", o.Indent)
	}
	notes := EditorconfigNotes(file, dir)
	if len(notes) != 1 || !strings.Contains(notes[0], "clamped to 16") {
		t.Fatalf("notes %q", notes)
	}
}
