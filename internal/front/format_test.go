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
		"messy":    "#!/bin/bash\n\n\nf(){\nif [ -n \"$1\" ];then echo   \"a  b\"   # keep\nelse\n  echo 'x'\nfi\n}\nfor i in 1 2 3;do echo $i;done\ncase $x in a|b)echo ab;;*)echo other;; esac\nwhile read -r l; do echo \"$l\"; done <<EOF\n  body   with   spaces\nEOF\ncat <<-'EOT'\n\ttabbed   body\n\tEOT\nx=$(echo   $(date))\n[[ \"$a\" == b && -n $c ]] && echo yes || echo no\necho $((1+2*3)) ${a:-b} ${#c} `uname`\na | b |\n  c\n",
		"redirs":   "echo a>f 2>&1\ncmd <in >out 2>>err\nexec 3<&-\n",
		"funcs":    "function a { :; }\nb() { :; }\nc()\n{\n  :\n}\n",
		"arrays":   "a=(1   2 3)\ndeclare -A m=([k]=v)\nfor ((i=0;i<3;i++)); do echo $i; done\n",
		"comments": "# one\n\n# two\necho a # trailing\n\n\n# end\n",
		"continue": "echo a \\\n  b \\\n  c\nif a &&\n  b; then :; fi\n",
		"empty":    "",
		"blank":    "\n\n\n",
		"nonl":     "echo   a",
	} {
		out["inline:"+name] = s
	}
	return out
}

// loweredSig is a second view of a parse, through the lowered tree the
// analysis uses: kinds, names and values, no positions.
func loweredSig(f *tree.File) string {
	var b strings.Builder
	var walk func(n *tree.Node, heredoc bool)
	walk = func(n *tree.Node, heredoc bool) {
		v := n.Value
		if heredoc {
			// the tabs before a `<<-` delimiter are not part of the body
			v = strings.TrimRight(v, "\t")
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
		debug.SetMaxStack(safetyStack)
	}
	_, err = Format("x.sh", []byte(atLimit(i, n)), FormatOptions{})
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

func runFormatChild(i, n int) (string, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestFormatHelper$", "-test.v")
	cmd.Env = append(os.Environ(), fmtCaseEnv+"="+strconv.Itoa(i), fmtLevelsEnv+"="+strconv.Itoa(n), maxStackEnv+"=1")
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
			out, err := runFormatChild(i, l.ok)
			if err != nil {
				t.Fatalf("child died: %v\n%s", err, firstLines(out, 15))
			}
			formatted, refused := strings.Contains(out, "\nformatted\n"), strings.Contains(out, "\nrefused: ")
			if formatted == refused {
				t.Fatalf("child printed neither or both:\n%s", firstLines(out, 15))
			}
			if chains[l.name] && !formatted {
				t.Fatalf("chain refused:\n%s", firstLines(out, 15))
			}
			// One more level is the parser's nesting error.
			_, err = Format("x.sh", []byte(atLimit(i, l.ok+1)), FormatOptions{})
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
	}
	for _, c := range differ {
		if treeSig(parse(c[0])) == treeSig(parse(c[1])) {
			t.Errorf("%q and %q are the same", c[0], c[1])
		}
	}
}
