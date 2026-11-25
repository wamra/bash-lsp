package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

var modNames = []struct {
	m TokenMod
	n string
}{
	{ModDeclaration, "declaration"}, {ModReadonly, "readonly"}, {ModModification, "modification"},
	{ModDefaultLibrary, "defaultLibrary"}, {ModLocal, "local"}, {ModExported, "exported"}, {ModArray, "array"},
	{ModAssociative, "associative"}, {ModInteger, "integer"}, {ModNameref, "nameref"},
	{ModEnvironment, "environment"}, {ModExternal, "external"}, {ModUnresolved, "unresolved"},
}

var typeNames = []string{"function", "variable", "parameter", "macro"}

// tokStr spells a token as "(line,col,len,type,{mods})", 0-based, for ASCII text.
func tokStr(t Token) string {
	var mods []string
	for _, mn := range modNames {
		if t.Mods&mn.m != 0 {
			mods = append(mods, mn.n)
		}
	}
	return fmt.Sprintf("(%d,%d,%d,%s,{%s})", t.Range.Start.Line-1, t.Range.Start.Col-1,
		t.Range.End.Offset-t.Range.Start.Offset, typeNames[t.Type], strings.Join(mods, ","))
}

func tokStrs(toks []Token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = tokStr(t)
	}
	return out
}

func tokensOf(t *testing.T, src string, opt Options) []string {
	t.Helper()
	p := "/w/a.sh"
	s := Analyze(map[string][]byte{p: []byte(src)}, opt)
	return tokStrs(s.Tokens(p, nil))
}

func wantToks(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tokens:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const sampleSrc = "alias ll=f\nf() { local -r x=1; echo \"$x\" \"$1\"; nosuch; }\nll\n"

func TestTokensSample(t *testing.T) {
	got := tokensOf(t, sampleSrc, Options{Root: "/w", Commands: func(string) (string, bool) { return "", false }})
	wantToks(t, got,
		"(0,6,2,macro,{declaration})",
		"(0,9,1,function,{})",
		"(1,0,1,function,{declaration})",
		"(1,15,1,variable,{declaration,readonly,local})",
		"(1,20,4,function,{defaultLibrary})",
		"(1,27,1,variable,{readonly,local})",
		"(1,32,1,parameter,{})",
		"(1,36,6,function,{unresolved})",
		"(2,0,2,macro,{})",
	)
}

func TestTokensNoProfile(t *testing.T) { // no PATH profile: no command edges, so never unresolved
	got := tokensOf(t, sampleSrc, Options{Root: "/w"})
	for _, g := range got {
		if strings.Contains(g, "unresolved") || strings.Contains(g, "external") {
			t.Fatalf("token %s without a PATH profile", g)
		}
	}
	found := false
	for _, g := range got {
		if g == "(1,36,6,function,{})" {
			found = true
		}
	}
	if !found {
		t.Fatalf("nosuch is not a plain function token: %v", got)
	}
}

func TestTokensExternal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	look := func(name string) (string, bool) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			return "", false
		}
		return p, true
	}
	got := tokensOf(t, "mytool a\nnosuch\n", Options{Root: "/w", Commands: look})
	wantToks(t, got, "(0,0,6,function,{external})", "(1,0,6,function,{unresolved})")
}

func TestTokensVariables(t *testing.T) {
	src := "export E=1\ndeclare -a A=(1 2)\ndeclare -A M\ndeclare -i N=3\ndeclare -n R=E\nf() {\n  local L\n  L=2\n  echo \"$E\" \"${A[0]}\" \"$HOME\" \"$L\"\n  N+=1\n  (( N++ ))\n  echo \"$@\" \"$*\" \"$#\" \"${10}\" \"$0\" \"$?\"\n}\n"
	got := tokensOf(t, src, Options{Root: "/w"})
	want := []string{
		"(0,7,1,variable,{declaration,exported})",
		"(1,11,1,variable,{declaration,array})",
		"(2,11,1,variable,{declaration,associative})",
		"(3,11,1,variable,{declaration,integer})",
		"(4,11,1,variable,{declaration,nameref})",
		"(5,0,1,function,{declaration})",
		"(6,8,1,variable,{declaration,local})",
		"(7,2,1,variable,{modification,local})",
		"(8,2,4,function,{defaultLibrary})",
		"(8,9,1,variable,{exported})",
		"(8,15,1,variable,{array})",
		"(8,24,4,variable,{environment})",
		"(8,32,1,variable,{local})",
		"(9,2,1,variable,{declaration,integer})", // another scope than the file-level declaration
		"(10,5,1,variable,{modification,integer})",
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s in\n%s", w, strings.Join(got, "\n"))
		}
	}
	for _, w := range []string{"(11,9,1,parameter,{})", "(11,14,1,parameter,{})", "(11,19,1,parameter,{})", "(11,25,2,parameter,{})"} {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s in\n%s", w, strings.Join(got, "\n"))
		}
	}
	for _, g := range got { // $0 and $? are not positional parameters
		if strings.Contains(g, "(11,32,") || strings.Contains(g, "(11,37,") || strings.Contains(g, "(11,38,") {
			t.Errorf("unexpected token %s", g)
		}
	}
}

func TestTokensModification(t *testing.T) {
	// The first declaration of a name in a scope is a declaration; a later
	// plain assignment to it in the same scope is a modification, and so is
	// arithmetic that changes it; a read is neither.
	got := tokensOf(t, "n=1\n(( n++ ))\necho \"$n\"\n", Options{Root: "/w"})
	wantToks(t, got,
		"(0,0,1,variable,{declaration})",
		"(1,3,1,variable,{modification})",
		"(2,0,4,function,{defaultLibrary})",
		"(2,7,1,variable,{})",
	)
	got = tokensOf(t, "n=1\nn=2\nf() { n=3; n=4; local m=1; m=2; declare m; }\nread r\nr=1\nfor i in 1; do i=2; done\nX=1 env\nX=2 env\n", Options{Root: "/w"})
	wantToks(t, got,
		"(0,0,1,variable,{declaration})",
		"(1,0,1,variable,{modification})",
		"(2,0,1,function,{declaration})",
		"(2,6,1,variable,{declaration})",
		"(2,11,1,variable,{modification})",
		"(2,22,1,variable,{declaration,local})",
		"(2,27,1,variable,{modification,local})",
		"(2,40,1,variable,{declaration,local})",
		"(3,0,4,function,{defaultLibrary})",
		"(3,5,1,variable,{declaration})",
		"(4,0,1,variable,{modification})",
		"(5,4,1,variable,{declaration})",
		"(5,15,1,variable,{modification})",
		"(6,0,1,variable,{declaration})",
		"(6,4,3,function,{})",
		"(7,0,1,variable,{declaration})",
		"(7,4,3,function,{})",
	)
}

// Names inside strings have no exact range and get no token.
func TestTokensNoStringNames(t *testing.T) {
	got := tokensOf(t, "f() { :; }\ncmd=f\neval \"f\"\n\"$cmd\"\ntrap \"f\" EXIT\nbash -c \"f\"\n", Options{Root: "/w"})
	for _, g := range got {
		if strings.HasPrefix(g, "(2,6,") || strings.HasPrefix(g, "(3,") && strings.Contains(g, ",function,") || strings.HasPrefix(g, "(4,6,") || strings.HasPrefix(g, "(5,9,") {
			t.Errorf("token %s on a name inside a string", g)
		}
	}
	wantToks(t, got[:3], "(0,0,1,function,{declaration})", "(0,6,1,function,{defaultLibrary})", "(1,0,3,variable,{declaration})")
}

// `$0` and a name with a leading zero are not positional parameters, and a
// subscripted number in arithmetic (`(( 1[2] ))`, a parameter node without
// a dollar sign) is not an expansion.
func TestTokensPositionalEdges(t *testing.T) {
	got := tokensOf(t, "echo $0 ${00} ${01} $1 ${10}\n(( 1 + 2 ))\n(( 1[2] ))\n", Options{Root: "/w"})
	wantToks(t, got,
		"(0,0,4,function,{defaultLibrary})",
		"(0,21,1,parameter,{})",
		"(0,25,2,parameter,{})",
	)
}

func TestBreaksLine(t *testing.T) {
	src := []byte("ab\rcd\r\nef\ngh")
	rg := func(a, b int) tree.Range { return tree.Range{Start: tree.Pos{Offset: a}, End: tree.Pos{Offset: b}} }
	for _, tc := range []struct {
		a, b int
		want bool
	}{{0, 2, false}, {0, 3, true}, {2, 3, true}, {3, 5, false}, {3, 6, true}, {5, 7, true}, {7, 9, false}, {7, 10, true}, {10, 12, false}} {
		if got := breaksLine(src, rg(tc.a, tc.b)); got != tc.want {
			t.Errorf("[%d,%d) %q: %v, want %v", tc.a, tc.b, src[tc.a:tc.b], got, tc.want)
		}
	}
}

// A lone CR ends an LSP line but not a tool line: the tokens around it are
// kept, each within one line of either kind.
func TestTokensAroundALoneCR(t *testing.T) {
	got := tokensOf(t, "x=1;\rf() { :; };\recho \"$x\"; f\n", Options{Root: "/w", Commands: func(string) (string, bool) { return "", false }})
	wantToks(t, got,
		"(0,0,1,variable,{declaration})",
		"(0,5,1,function,{declaration})",
		"(0,11,1,function,{defaultLibrary})",
		"(0,17,4,function,{defaultLibrary})",
		"(0,24,1,variable,{})",
		"(0,28,1,function,{})",
	)
}

func TestTokensSortedSingleLine(t *testing.T) {
	q := buildCorpus(t)
	for _, p := range q.s.Paths() {
		toks := q.s.Tokens(p, nil)
		prev := 0
		for i, tk := range toks {
			if tk.Range.Start.Line != tk.Range.End.Line || tk.Range.End.Offset <= tk.Range.Start.Offset ||
				breaksLine(q.s.Facts(p).File.Src, tk.Range) {
				t.Fatalf("%s: token %d %v is not a non-empty single-line range", p, i, tk.Range)
			}
			if tk.Range.Start.Offset < prev {
				t.Fatalf("%s: token %d overlaps or is out of order", p, i)
			}
			prev = tk.Range.End.Offset
		}
	}
}

// TestTokensRangeSubset: the tokens of a range are those of the whole file
// that touch it.
func TestTokensRangeSubset(t *testing.T) {
	q := buildCorpus(t)
	for _, p := range q.s.Paths() {
		ff := q.s.Facts(p)
		all := q.s.Tokens(p, nil)
		n := len(ff.File.Src)
		for _, w := range [][2]int{{0, n}, {0, n / 3}, {n / 3, 2 * n / 3}, {n / 2, n/2 + 1}, {n - 5, n}, {n / 4, n/4 + 40}} {
			if w[0] < 0 || w[1] < w[0] {
				continue
			}
			r := tree.Range{Start: ff.File.PosAt(w[0]), End: ff.File.PosAt(w[1])}
			var want []string
			for _, tk := range all {
				if tk.Range.End.Offset > r.Start.Offset && tk.Range.Start.Offset < r.End.Offset {
					want = append(want, tokStr(tk))
				}
			}
			got := tokStrs(q.s.Tokens(p, &r))
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("%s %v: range tokens\n%v\nwant\n%v", p, w, got, want)
			}
		}
	}
}

// TestTokensLinear: the allocations per token do not grow with the size of
// the file.
func TestTokensLinear(t *testing.T) {
	unit := "f%[1]d() {\n  local v%[1]d=1\n  echo \"$v%[1]d\" \"$1\"\n  nosuch%[1]d\n}\nf%[1]d\n"
	perToken := func(n int) float64 {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, unit, i)
		}
		p := "/w/a.sh"
		s := Analyze(map[string][]byte{p: []byte(sb.String())}, Options{Root: "/w", Commands: func(string) (string, bool) { return "", false }})
		count := len(s.Tokens(p, nil))
		allocs := testing.AllocsPerRun(5, func() { s.Tokens(p, nil) })
		return allocs / float64(count)
	}
	sizes := []int{100, 200, 400, 800}
	var ratios float64
	prev := perToken(sizes[0])
	if prev > 1 {
		t.Fatalf("%.2f allocations per token", prev)
	}
	for _, n := range sizes[1:] {
		cur := perToken(n)
		ratios += cur / prev
		prev = cur
	}
	if mean := ratios / float64(len(sizes)-1); mean > 1.3 {
		t.Errorf("allocations per token grow with size: mean doubling ratio %.2f", mean)
	}
}

// BenchmarkTokensBig is Tokens on the 2,000-function file of the scale
// benchmarks (about 560 KB), with a PATH profile that finds nothing.
func BenchmarkTokensBig(b *testing.B) {
	src := bigFunctions(2000)
	p := "/w/big.sh"
	s := Analyze(map[string][]byte{p: src}, Options{Root: "/w", Commands: func(string) (string, bool) { return "", false }})
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Tokens(p, nil)
	}
}

// TestTokensRangeSubsetBroken repeats the range check on the other test
// trees, which hold broken and odd scripts, with many windows per file.
func TestTokensRangeSubsetBroken(t *testing.T) {
	for _, dir := range []string{"../testdata/broken", "../testdata/diag", "../testdata/ac13", "../testdata/corpus-annotated"} {
		w := loadDir(t, dir)
		files := map[string]*FileFacts{}
		for _, ff := range w.facts {
			files[ff.Path] = ff
		}
		s := Build(files, Options{Root: w.root}, 1)
		for _, p := range s.Paths() {
			ff := s.Facts(p)
			all := s.Tokens(p, nil)
			n := len(ff.File.Src)
			for lo := 0; lo < n; lo += max(1, n/23) {
				for _, width := range []int{1, 7, 60, n} {
					hi := min(lo+width, n)
					r := tree.Range{Start: ff.File.PosAt(lo), End: ff.File.PosAt(hi)}
					var want []string
					for _, tk := range all {
						if tk.Range.End.Offset > lo && tk.Range.Start.Offset < hi {
							want = append(want, tokStr(tk))
						}
					}
					if got := tokStrs(s.Tokens(p, &r)); strings.Join(got, "|") != strings.Join(want, "|") {
						t.Fatalf("%s [%d,%d): range tokens\n%v\nwant\n%v", p, lo, hi, got, want)
					}
				}
			}
		}
	}
}

// A heredoc body lies inside a statement whose later parts start before it:
// the range walk must still reach the parameters in the body.
func TestTokensRangeHeredoc(t *testing.T) {
	src := "cat <<EOF | tr a b\nline $1 and $@\nEOF\nf() { cat <<-EOF2; echo \"$#\"; }\n\t$2\n\tEOF2\n"
	p := "/w/a.sh"
	s := Analyze(map[string][]byte{p: []byte(src)}, Options{Root: "/w"})
	ff := s.Facts(p)
	all := s.Tokens(p, nil)
	params := 0
	for _, tk := range all {
		if tk.Type == TokParameter {
			params++
		}
	}
	if params < 3 {
		t.Fatalf("only %d parameters found in the heredoc text: %v", params, tokStrs(all))
	}
	for lo := 0; lo < len(src); lo++ {
		for _, width := range []int{1, 3, 12, len(src)} {
			hi := min(lo+width, len(src))
			r := tree.Range{Start: ff.File.PosAt(lo), End: ff.File.PosAt(hi)}
			var want []string
			for _, tk := range all {
				if tk.Range.End.Offset > lo && tk.Range.Start.Offset < hi {
					want = append(want, tokStr(tk))
				}
			}
			if got := tokStrs(s.Tokens(p, &r)); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("[%d,%d): range tokens %v, want %v", lo, hi, got, want)
			}
		}
	}
}
