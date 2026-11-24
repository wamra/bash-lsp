package front

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front/syntax"
	"github.com/wamra/bash-lsp/tree"
)

const (
	fmtCaseEnv   = "WB_FMT_CASE"   // index into limits
	fmtLevelsEnv = "WB_FMT_LEVELS" // repetitions of the construct
)

// fmtSources are the formatter inputs of the property tests: the repository's
// test scripts that parse cleanly, and a few shapes of their own.
func fmtSources(t testing.TB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"../../testdata"} {
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".sh") || info.Size() > 1<<20 {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			out[p] = string(b)
			return nil
		})
	}
	for name, s := range map[string]string{
		"messy":       "#!/bin/bash\n\n\nf(){\nif [ -n \"$1\" ];then echo   \"a  b\"   # keep\nelse\n  echo 'x'\nfi\n}\nfor i in 1 2 3;do echo $i;done\ncase $x in a|b)echo ab;;*)echo other;; esac\nwhile read -r l; do echo \"$l\"; done <<EOF\n  body   with   spaces\nEOF\ncat <<-'EOT'\n\ttabbed   body\n\tEOT\nx=$(echo   $(date))\n[[ \"$a\" == b && -n $c ]] && echo yes || echo no\necho $((1+2*3)) ${a:-b} ${#c} `uname`\na | b |\n  c\n",
		"redirs":      "echo a>f 2>&1\ncmd <in >out 2>>err\nexec 3<&-\n",
		"funcs":       "function a { :; }\nb() { :; }\nc()\n{\n  :\n}\n",
		"arrays":      "a=(1   2 3)\ndeclare -A m=([k]=v)\nfor ((i=0;i<3;i++)); do echo $i; done\n",
		"comments":    "# one\n\n# two\necho a # trailing\n\n\n# end\n",
		"continue":    "echo a \\\n  b \\\n  c\nif a &&\n  b; then :; fi\n",
		"dash-nested": "if a;then\n\tcat <<-EOF\n\t\tx $(date)\n  y\n\tEOF\n\t\tcat   <<-'Q'\n\tz\n\t\tQ\nfi\nf(){\ncat <<-E | tr a b\n\t\t1\n\tE\n}\n",
		"empty":       "",
		"blank":       "\n\n\n",
		"nonl":        "echo   a",
	} {
		out["inline:"+name] = s
	}
	return out
}

// loweredSig is a second view of a parse, through the lowered tree the
// analysis uses: kinds, names and values, no positions.
func loweredSig(f *tree.File) string {
	dash := bytes.Contains(f.Src, []byte("<<-"))
	var b strings.Builder
	var walk func(n *tree.Node, heredoc bool)
	walk = func(n *tree.Node, heredoc bool) {
		v := n.Value
		if heredoc {
			// the tabs at the start of a `<<-` body line are not part of the body
			if dash {
				v = stripLineTabs(v, true)
			}
		}
		fmt.Fprintf(&b, "(%v %q %q", n.Kind, n.Name, v)
		for _, c := range n.Children {
			walk(c, heredoc || n.Kind == tree.KHeredoc)
		}
		b.WriteString(")")
	}
	walk(f.Root, false)
	return b.String()
}

func TestFormatRefusesBroken(t *testing.T) {
	cases := map[string]string{
		"unclosed-quote": "echo ok\necho \"abc\necho more\n",
		"recovered-fi":   "if true; then\n  echo a\n",
		"stray-paren":    "echo ok\n)\necho ok2\n",
	}
	files, _ := filepath.Glob("../../testdata/broken/*.sh")
	more, _ := filepath.Glob("../../testdata/corpus/broken_*.sh")
	for _, p := range append(files, more...) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		cases[filepath.Base(p)] = string(b)
	}
	if len(cases) < 8 {
		t.Fatalf("only %d broken inputs", len(cases))
	}
	sawGap, sawRecovered := false, false
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			in := []byte(src)
			orig := bytes.Clone(in)
			f := Parse("x.sh", in, Bash)
			f.Root.Walk(func(n *tree.Node) bool {
				sawGap = sawGap || n.Kind == tree.KErrorGap
				sawRecovered = sawRecovered || n.Recovered
				return true
			})
			out, err := Format("x.sh", in, FormatOptions{})
			if !errors.Is(err, ErrFormatSyntax) {
				t.Fatalf("err = %v, want ErrFormatSyntax", err)
			}
			if out != nil {
				t.Fatalf("returned %q with an error", out)
			}
			if !bytes.Equal(in, orig) {
				t.Fatal("the input bytes changed")
			}
			if !strings.Contains(err.Error(), ":") {
				t.Fatalf("message %q has no line:col", err)
			}
		})
	}
	if !sawGap || !sawRecovered {
		t.Fatalf("the inputs held no ErrorGap (%v) or no Recovered node (%v)", sawGap, sawRecovered)
	}
}

func TestFormatKeepsCRLFAndHeredoc(t *testing.T) {
	lf := "#!/bin/bash\nif true;then\n# comment  with   spaces\ncat <<EOF\n  heredoc   body\n\tkept\nEOF\nfi\necho   \"a\r\"\n"
	crlf := strings.ReplaceAll(strings.ReplaceAll(lf, "\r", ""), "\n", "\r\n")
	// The lone \r inside the string is a lone CR in a CRLF file: refused as mixed endings.
	if _, err := Format("x.sh", []byte(crlf+"echo \"a\rb\"\r\n"), FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) {
		t.Fatalf("lone CR: %v", err)
	}
	lf = strings.Replace(lf, "echo   \"a\r\"\n", "echo   \"a\"\n", 1)
	crlf = strings.ReplaceAll(lf, "\n", "\r\n")
	out, err := Format("x.sh", []byte(crlf), FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/bash\r\nif true; then\r\n\t# comment  with   spaces\r\n\tcat <<EOF\r\n  heredoc   body\r\n\tkept\r\nEOF\r\nfi\r\necho \"a\"\r\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
	if bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("a bare LF in CRLF output")
	}
	// LF in, LF out; same text.
	out, err = Format("x.sh", []byte(lf), FormatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != strings.ReplaceAll(want, "\r\n", "\n") {
		t.Fatalf("LF: got %q", out)
	}
	// Mixed endings are refused, not guessed at.
	if _, err := Format("x.sh", []byte("echo a\r\necho b\n"), FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) {
		t.Fatalf("mixed endings: %v", err)
	}
}

func TestFormatEdgeBytes(t *testing.T) {
	for _, c := range []struct {
		name, src, want string
		err             error
	}{
		{"missing-final-newline", "echo   a", "echo a", nil},
		{"final-newline", "echo   a\n", "echo a\n", nil},
		{"shebang", "#!/usr/bin/env   bash\necho  a\n", "#!/usr/bin/env   bash\necho a\n", nil},
		{"empty", "", "", nil},
		{"blank-only", "\n\n", "\n\n", nil},
		{"comment-only", "# x   \n", "# x\n", nil},
		{"bom", "\xef\xbb\xbfecho a\n", "", ErrFormatUnsupported},
		{"nul", "echo a\x00b\n", "", ErrFormatUnsupported},
		{"zsh", "#!/bin/zsh\necho a\n", "", ErrFormatUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := Format("x.sh", []byte(c.src), FormatOptions{})
			if c.err != nil {
				if !errors.Is(err, c.err) || out != nil {
					t.Fatalf("out %q err %v, want %v", out, err, c.err)
				}
				return
			}
			if err != nil || string(out) != c.want {
				t.Fatalf("out %q err %v, want %q", out, err, c.want)
			}
		})
	}
	big := "echo a\n" + strings.Repeat("#", 2<<20)
	if _, err := Format("x.sh", []byte(big), FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over the size cap: %v", err)
	}
}

// The grammar the file was parsed with is the grammar it is formatted with: an
// sh file with a bash-only form is formatted with bash, an sh file that the
// POSIX grammar reads is formatted with it (and `((a; b) | c)` stays a
// subshell pipeline there, not an arithmetic command).
func TestFormatUsesTheParsedGrammar(t *testing.T) {
	out, err := Format("x.sh", []byte("#!/bin/sh\n((echo a; echo b)|cat)\n"), FormatOptions{})
	if err != nil || string(out) != "#!/bin/sh\n( (\n\techo a\n\techo b\n) | cat)\n" {
		t.Fatalf("posix subshell: %q %v", out, err)
	}
	out, err = Format("x.sh", []byte("#!/bin/sh\nif [[ -n $a ]];then source  x;fi\n"), FormatOptions{})
	if err != nil || string(out) != "#!/bin/sh\nif [[ -n $a ]]; then source x; fi\n" {
		t.Fatalf("bash form in sh: %q %v", out, err)
	}
	if _, err := Format("x.sh", []byte("#!/bin/sh\nif then\n"), FormatOptions{}); !errors.Is(err, ErrFormatSyntax) {
		t.Fatalf("sh error: %v", err)
	}
}

func TestFormatPreservesTreeAndIsIdempotent(t *testing.T) {
	variants := []FormatOptions{{}, {Indent: 2}, {Indent: 4, BinaryNextLine: true, SwitchCaseIndent: true, SpaceRedirects: true, FunctionNextLine: true}}
	n := 0
	for name, src := range fmtSources(t) {
		for vi, o := range variants {
			out, err := Format(name, []byte(src), o)
			if err != nil {
				if f := Parse(name, []byte(src), Detect(name, []byte(src))); len(f.Errors) == 0 && f.Skipped == "" {
					t.Errorf("%s: clean file refused: %v", name, err)
				}
				continue
			}
			n++
			d := Detect(name, []byte(src))
			a, b := Parse(name, []byte(src), d), Parse(name, out, d)
			if len(b.Errors) != 0 {
				t.Errorf("%s v%d: output has errors %v", name, vi, firstErrors(b, 2))
				continue
			}
			if loweredSig(a) != loweredSig(b) {
				t.Errorf("%s v%d: lowered tree changed", name, vi)
			}
			again, err := Format(name, out, o)
			if err != nil || !bytes.Equal(again, out) {
				t.Errorf("%s v%d: not idempotent (err %v)", name, vi, err)
			}
		}
	}
	if n < 20 {
		t.Fatalf("only %d formatted", n)
	}
}

func TestFormatSimplifyOption(t *testing.T) {
	src := []byte("[[ \"$a\" == b ]]\n")
	out, err := Format("x.sh", src, FormatOptions{})
	if err != nil || string(out) != string(src) {
		t.Fatalf("without Simplify: %q %v", out, err)
	}
	out, err = Format("x.sh", src, FormatOptions{Simplify: true})
	if err != nil || string(out) != "[[ $a == b ]]\n" {
		t.Fatalf("with Simplify: %q %v", out, err)
	}
}

// FuzzFormat: formatting never panics, and what it accepts keeps the tree and
// is a fixed point.
func FuzzFormat(f *testing.F) {
	for _, s := range fmtSources(f) {
		if len(s) < 4096 {
			f.Add(s)
		}
	}
	f.Add("a | b &&\n c; d &\nif x; then y; elif z; then w; fi\n")
	f.Add("case $x in\n a) b;;\n c|d) e ;&\n *) ;;\nesac\n")
	f.Add("cat <<-EOF > f\n\t$(echo   x)\nEOF\n")
	f.Add("f() { local x=${1:-$(date)}; ((x++)); echo $((x*2)); }\n")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<14 {
			return
		}
		start := time.Now()
		out, err := Format("x.sh", []byte(s), FormatOptions{})
		if time.Since(start) > 5*time.Second {
			t.Fatalf("took %v", time.Since(start))
		}
		if err != nil {
			if out != nil {
				t.Fatalf("output with error %v", err)
			}
			return
		}
		d := Detect("x.sh", []byte(s))
		a, b := Parse("x.sh", []byte(s), d), Parse("x.sh", out, d)
		if len(a.Errors) != 0 || len(b.Errors) != 0 {
			t.Fatalf("errors: input %v, output %v", firstErrors(a, 1), firstErrors(b, 1))
		}
		if loweredSig(a) != loweredSig(b) {
			t.Fatalf("lowered tree changed:\n%q\n%q", s, out)
		}
		again, err := Format("x.sh", out, FormatOptions{})
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", s, out, again, err)
		}
	})
}

// TestFormatHelper is the child side of TestFormatDeepInputsSubprocess.
func TestFormatHelper(t *testing.T) {
	c := os.Getenv(fmtCaseEnv)
	if c == "" {
		t.Skip("child side of TestFormatDeepInputsSubprocess")
	}
	i, err := strconv.Atoi(c)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(os.Getenv(fmtLevelsEnv))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv(maxStackEnv) == "1" {
		stack := safetyStack
		if v, err := strconv.Atoi(os.Getenv("WB_FMT_STACK")); err == nil { // a smaller limit, to measure the margin
			stack = v
		}
		debug.SetMaxStack(stack)
	}
	_, err = Format("x.sh", []byte(atLimit(i, n)), FormatOptions{Simplify: os.Getenv("WB_FMT_SIMPLIFY") == "1"})
	if err != nil {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		fmt.Println("refused: " + msg)
		return
	}
	fmt.Println("formatted")
}

func runFormatChild(i, n int, simplify bool) (string, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestFormatHelper$", "-test.v")
	cmd.Env = append(os.Environ(), fmtCaseEnv+"="+strconv.Itoa(i), fmtLevelsEnv+"="+strconv.Itoa(n), maxStackEnv+"=1")
	if simplify {
		cmd.Env = append(cmd.Env, "WB_FMT_SIMPLIFY=1")
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Every construct at the deepest the parser takes, formatted in a child whose
// stack is limited to 64 MiB: the child exits normally, whether it formats or
// refuses. The five long chains are what the printer recurses on deepest, and
// are accepted by analysis, so they must format.
func TestFormatDeepInputsSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("runs one child process per construct")
	}
	chains := map[string]bool{"andor": true, "pipes": true, "pipes-commented": true, "arith-plus": true, "arith-comma": true}
	for i, l := range limits {
		t.Run(l.name, func(t *testing.T) {
			for _, simplify := range []bool{false, true} {
				out, err := runFormatChild(i, l.ok, simplify)
				if err != nil {
					t.Fatalf("simplify=%v: child died: %v\n%s", simplify, err, firstLines(out, 15))
				}
				formatted, refused := strings.Contains(out, "\nformatted\n"), strings.Contains(out, "\nrefused: ")
				if formatted == refused {
					t.Fatalf("simplify=%v: child printed neither or both:\n%s", simplify, firstLines(out, 15))
				}
				if chains[l.name] && !formatted {
					t.Fatalf("simplify=%v: chain refused:\n%s", simplify, firstLines(out, 15))
				}
			}
			// One more level is the parser's nesting error.
			_, err := Format("x.sh", []byte(atLimit(i, l.ok+1)), FormatOptions{})
			if !errors.Is(err, ErrFormatSyntax) || !strings.Contains(err.Error(), "nesting is deeper than 10000 levels") {
				t.Fatalf("%d levels: %v", l.ok+1, err)
			}
		})
	}
}

func TestFormatRefusesLowererDepth(t *testing.T) {
	src := strings.Repeat("while a;do\n", 4100) + ":\n" + strings.Repeat("done\n", 4100)
	_, err := Format("x.sh", []byte(src), FormatOptions{})
	if !errors.Is(err, ErrFormatSyntax) || !strings.Contains(err.Error(), "nested more than 4000 levels deep") {
		t.Fatalf("err = %v", err)
	}
}

// treeSigOf is what the formatter checks its output with.
func TestTreeSigIgnoresPositionsOnly(t *testing.T) {
	parse := func(s string) *syntax.File {
		f, err := syntax.NewParser(syntax.KeepComments(true), syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(s), "x")
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	same := [][2]string{
		{"echo   a\n", "echo a\n"},
		{"if a;then b;fi\n", "if a; then\n  b\nfi\n"},
		{"# c   \necho a\n", "# c\necho a\n"},
		{"cat <<-A\n\tx\n\tA\n", "cat <<-A\n\t\t\tx\nA\n"},
		{"echo `a`\n", "echo $(a)\n"},
	}
	for _, c := range same {
		if treeSig(parse(c[0])) != treeSig(parse(c[1])) {
			t.Errorf("%q and %q differ", c[0], c[1])
		}
	}
	differ := [][2]string{
		{"echo a b\n", "echo ab\n"},
		{"echo a; echo b\n", "echo a && echo b\n"},
		{"echo \"a b\"\n", "echo 'a b'\n"},
		{"a | b\n", "a |& b\n"},
		{"# one\n", "# two\n"},
		{"a &\n", "a\n"},
		{"cat <<A\nx\nA\n", "cat <<A\ny\nA\n"},
		{"cat <<-A\n\tx\nA\n", "cat <<-A\n\ty\nA\n"},
		{"cat <<A\n\tx\nA\n", "cat <<A\nx\nA\n"},
		{"cat <<-A\nx  y\nA\n", "cat <<-A\nx y\nA\n"},
	}
	for _, c := range differ {
		if treeSig(parse(c[0])) == treeSig(parse(c[1])) {
			t.Errorf("%q and %q are the same", c[0], c[1])
		}
	}
}

// BenchmarkFormatPhases times, on the file named by WB_BIG (bench/gen_big.py:
// the 561 KB script), the strict parse alone, the parse and print (the
// reference of 29 ms for parse and print is this row), the analysis guard
// alone, and Format whole (guard, parse, print, check, second print).
func BenchmarkFormatPhases(b *testing.B) {
	path := os.Getenv("WB_BIG")
	if path == "" {
		b.Skip("WB_BIG names the file to format")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("parse", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, err := strictParse("x.sh", src, Bash, FormatOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parse+print", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f, _, err := strictParse("x.sh", src, Bash, FormatOptions{})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := printTree(f, FormatOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("guard", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := prepare("x.sh", src); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("format", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := Format("x.sh", src, FormatOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// An sh file is read with the POSIX grammar, which takes `[[` for a command:
// `[[ $a<$b ]]` would be a test and a redirection, `=~ a|b` a pipe. A file
// that the two grammars read differently is not formatted.
func TestFormatShGrammarDependent(t *testing.T) {
	bodies := []string{"[[ $a<$b ]]\n", "[[ $x =~ a|b ]]\n", "if [[ $a>$b ]]; then :; fi\n"}
	for _, body := range bodies {
		for name, c := range map[string]struct{ path, head string }{
			"noext":   {"script", ""},
			"nosheb":  {"x.sh", ""},
			"binsh":   {"x.sh", "#!/bin/sh\n"},
			"envsh":   {"x", "#!/usr/bin/env sh\n"},
			"directv": {"x.sh", "# shellcheck shell=sh\n"},
		} {
			src := []byte(c.head + body)
			out, err := Format(c.path, src, FormatOptions{})
			if err != nil {
				if !errors.Is(err, ErrFormatUnsupported) || !strings.Contains(err.Error(), "grammar-dependent") {
					t.Errorf("%s %q: %v", name, body, err)
				}
				continue
			}
			if bytes.Contains(out, []byte("| b")) || bytes.Contains(out, []byte("]] <")) || bytes.Contains(out, []byte("]] >")) {
				t.Errorf("%s %q: meaning changed: %q", name, body, out)
			}
		}
	}
	for _, c := range []struct{ src string }{{"#!/bin/sh\n[[ $a<$b ]]\n"}, {"#!/bin/sh\n[[ $x =~ a|b ]]\n"}} {
		if _, err := Format("x.sh", []byte(c.src), FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) || !strings.Contains(err.Error(), "grammar-dependent") {
			t.Errorf("%q: %v", c.src, err)
		}
		if _, _, _, err := FormatRange("x.sh", []byte(c.src), 0, len(c.src), FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) {
			t.Errorf("range %q: %v", c.src, err)
		}
	}
	// A plain POSIX file, and sh text both grammars print alike, format as before.
	out, err := Format("x.sh", []byte("#!/bin/sh\nif [ -n \"$a\" ];then echo   a;fi\nf(){ echo   b; }\n"), FormatOptions{})
	if err != nil || string(out) != "#!/bin/sh\nif [ -n \"$a\" ]; then echo a; fi\nf() { echo b; }\n" {
		t.Fatalf("%q %v", out, err)
	}
}

// A here-document delimiter that is quoted in one part and not in another is
// not formatted: the shell takes the body as literal, and the tree check
// cannot be trusted on a delimiter the parser may read differently.
func TestFormatRefusesMixedHeredocDelimiter(t *testing.T) {
	for _, d := range []string{`"E"F`, `'E'F`, `E"F"`, `E'F'`} {
		src := []byte("cat <<" + d + "\n$(echo   a;b)  `c`\nEF\necho   x\n")
		if _, err := Format("x.sh", src, FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) || !strings.Contains(err.Error(), "delimiter") {
			t.Errorf("%s: %v", d, err)
		}
		if _, _, _, err := FormatRange("x.sh", src, 0, len(src), FormatOptions{}); !errors.Is(err, ErrFormatUnsupported) {
			t.Errorf("range %s: %v", d, err)
		}
	}
	for _, d := range []string{`"EF"`, `'EF'`, `\EF`, `EF`} {
		src := []byte("cat <<" + d + "\n$(echo   a;b)\nEF\necho   x\n")
		out, err := Format("x.sh", src, FormatOptions{})
		if err != nil || !strings.Contains(string(out), "$(echo   a;b)") && !strings.Contains(string(out), "echo a") {
			t.Errorf("%s: %q %v", d, out, err)
		}
	}
	if out, err := Format("x.sh", []byte("cat <<\"EF\"\n$(echo   a;b)\nEF\necho   x\n"), FormatOptions{}); err != nil || string(out) != "cat <<\"EF\"\n$(echo   a;b)\nEF\necho x\n" {
		t.Errorf("quoted body: %q %v", out, err)
	}
}

// A comment printed without its leading space can become a shebang or a
// directive that the input did not have; the dialect of the output would then
// differ from the one the text was read with, so the file is not formatted.
func TestFormatKeepsDialect(t *testing.T) {
	src := []byte(" #!sh\n!(0)")
	out, err := Format("x.sh", src, FormatOptions{})
	if err == nil {
		again, err2 := Format("x.sh", out, FormatOptions{})
		if err2 != nil || !bytes.Equal(again, out) {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", src, out, again, err2)
		}
		return
	}
	if out != nil || !errors.Is(err, ErrFormatUnsupported) {
		t.Fatalf("got %q, %v", out, err)
	}
}

// A text with no final newline stays without one only when the text left
// after dropping the printed newline does not end in a newline itself: after a
// line continuation at the end the next pass would add one, so the file is not
// formatted.
func TestFormatNoFinalNewlineIsFixedPoint(t *testing.T) {
	for _, src := range []string{"\\\\\\\n ", "a \\\n ", "a\\\n", "echo \\\n  "} {
		out, err := Format("x.sh", []byte(src), FormatOptions{})
		if err != nil {
			continue
		}
		again, err := Format("x.sh", out, FormatOptions{})
		if err != nil || !bytes.Equal(again, out) {
			t.Errorf("%q -> %q -> %q (%v)", src, out, again, err)
		}
	}
}
