package front

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

// dumpNodes prints every node of f: kind, range, name, name range and value.
func dumpNodes(sb *strings.Builder, f *tree.File) {
	var walk func(n *tree.Node, depth int)
	walk = func(n *tree.Node, depth int) {
		fmt.Fprintf(sb, "%s%v %d-%d name=%q %d-%d val=%q\n", strings.Repeat(" ", depth), n.Kind, n.Range.Start.Offset, n.Range.End.Offset, n.Name, n.NameRange.Start.Offset, n.NameRange.End.Offset, n.Value)
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(f.Root, 0)
}

// the NUL fix (a602bb9) took a name's source bytes whenever they
// differed from the lexer's value, so in a NUL-free file a name holding a line
// continuation became `fo\<newline>o` (a function), `my\<newline>v` (an
// assignment) or `fo\` (a POSIX `local`). On NUL-free sources every node must
// be what b414076 produced: testdata/continuation/b414076.txt is the output of
// this dump at b414076 (git archive b414076, the same dump code) for
// inputs.txt, which covers function declarations, assignments, the five
// declaration builtins in each dialect, for and select variables, arithmetic,
// parameter expansions, env prefixes, heredoc tags and coproc names.
func TestContinuationNamesMatchBase(t *testing.T) {
	in, err := os.Open("testdata/continuation/inputs.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	want, err := os.ReadFile("testdata/continuation/b414076.txt")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sc := bufio.NewScanner(in)
	n := 0
	for sc.Scan() {
		d, q, _ := strings.Cut(sc.Text(), " ")
		src, err := strconv.Unquote(q)
		if err != nil {
			t.Fatal(sc.Text(), err)
		}
		if strings.IndexByte(src, 0) >= 0 {
			t.Fatalf("%q: the inputs are NUL-free", src)
		}
		fmt.Fprintf(&sb, "== %s %q\n", d, src)
		dumpNodes(&sb, Parse("/ws/f.sh", []byte(src), map[string]Dialect{"bash": Bash, "posix": POSIX, "mksh": Mksh}[d]))
		n++
	}
	if n < 40 {
		t.Fatalf("%d inputs", n)
	}
	got := strings.Split(sb.String(), "\n")
	exp := strings.Split(string(want), "\n")
	for i := range max(len(got), len(exp)) {
		var g, e string
		if i < len(got) {
			g = got[i]
		}
		if i < len(exp) {
			e = exp[i]
		}
		if g != e {
			t.Errorf("line %d: got %s, want %s (b414076)", i+1, g, e)
		}
	}
}

// With a NUL byte as well, a name holding a line continuation is the name the
// shell reads: NULs and the continuation removed. Its range runs from its first
// byte to its last, past both.
func TestNULAndContinuationName(t *testing.T) {
	cases := []struct {
		d          Dialect
		src        string
		kind       tree.Kind
		name       string
		start, end int
	}{
		{Bash, "A\x00\\\nBCDE=1\n", tree.KAssign, "ABCDE", 0, 8},
		{Bash, "AB\\\nCD\x00E=1\n", tree.KAssign, "ABCDE", 0, 8},
		{Bash, "declare A\x00\\\nBCDE=1\n", tree.KAssign, "ABCDE", 8, 16},
		{POSIX, "local A\x00\\\nBCDE=1\n", tree.KAssign, "ABCDE", 6, 14},
		{POSIX, "local AB\\\nC\x00DE=1\n", tree.KAssign, "ABCDE", 6, 14},
		{Mksh, "typeset A\x00\\\nBCDE=1\n", tree.KAssign, "ABCDE", 8, 16},
		{Bash, "fo\x00\\\no() { :; }\n", tree.KFuncDecl, "foo", 0, 6},
		{Bash, "fo\\\no\x00x() { :; }\n", tree.KFuncDecl, "foox", 0, 7},
		{Bash, "for i\x00\\\nj in a; do :; done\n", tree.KFor, "ij", 4, 9},
		{Bash, "echo ${fo\x00\\\no}\n", tree.KParamExp, "foo", 7, 13},
		// a NUL alone keeps the source bytes; a continuation alone is as at b414076
		{Bash, "fo\x00o() { :; }\n", tree.KFuncDecl, "fo\x00o", 0, 4},
		{Bash, "fo\\\no\x00() { :; }\n", tree.KFuncDecl, "foo", 0, 5},
	}
	for _, c := range cases {
		f := Parse("/ws/f.sh", []byte(c.src), c.d)
		var found *tree.Node
		f.Root.Walk(func(n *tree.Node) bool {
			if found == nil && n.Kind == c.kind && n.Name != "" {
				found = n
			}
			return true
		})
		if found == nil {
			t.Errorf("%q (%v): no named %v", c.src, c.d, c.kind)
			continue
		}
		if found.Name != c.name || found.NameRange.Start.Offset != c.start || found.NameRange.End.Offset != c.end {
			t.Errorf("%q (%v): %v name %q range %d-%d, want %q %d-%d", c.src, c.d, c.kind, found.Name,
				found.NameRange.Start.Offset, found.NameRange.End.Offset, c.name, c.start, c.end)
		}
	}
}
