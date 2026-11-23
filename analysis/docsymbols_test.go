package analysis

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// docSymbolsByWalk is DocumentSymbols as it was before Bind recorded each
// scope's function (review of m2a, leftover A): the enclosing function of a
// local found by walking up the tree per declaration. Kept as the oracle of
// TestDocumentSymbolsMatchWalk.
func docSymbolsByWalk(s *Snapshot, path string) []DocSymbol {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	var out []DocSymbol
	seen := map[symKey]bool{}
	fnAt := map[*tree.Node]int{}
	locals := map[*tree.Node]map[string]bool{}
	for _, d := range s.sortedDecls(file) {
		switch {
		case d.Kind == DeclFunction:
			fnAt[d.Node] = len(out)
			out = append(out, DocSymbol{Name: d.Name, Kind: d.Kind, Detail: s.signature(d),
				Range: d.Node.Range, Selection: d.NameRange})
		case atFileLevel(d):
			if seen[symKey{d.Kind, d.Name}] {
				continue
			}
			seen[symKey{d.Kind, d.Name}] = true
			out = append(out, DocSymbol{Name: d.Name, Kind: d.Kind, Detail: declDetail(d),
				Range: d.Node.Range, Selection: d.NameRange})
		}
	}
	for _, d := range s.sortedDecls(file) {
		if d.Kind != DeclVariable || atFileLevel(d) || s.declSym[d] == nil || s.declSym[d].Func == "" {
			continue
		}
		fn := d.Node.Enclosing(tree.KFuncDecl)
		i, ok := fnAt[fn]
		if !ok {
			continue
		}
		if locals[fn] == nil {
			locals[fn] = map[string]bool{}
		}
		if locals[fn][d.Name] {
			continue
		}
		locals[fn][d.Name] = true
		out[i].Children = append(out[i].Children, DocSymbol{Name: d.Name, Kind: d.Kind, Detail: declDetail(d),
			Range: d.Node.Range, Selection: d.NameRange})
	}
	return out
}

// randomScopeProgram is a program of nested functions (named, `function`
// keyword, subshell bodies), subshells, command and process substitutions,
// pipelines, coprocs, locals, declare, read, for, arithmetic writes and
// plain assignments, for the DocumentSymbols differential.
func randomScopeProgram(r *rand.Rand) string {
	var stmt func(d int) string
	block := func(d int) string {
		var parts []string
		for range 1 + r.Intn(3) {
			parts = append(parts, stmt(d))
		}
		return strings.Join(parts, "; ")
	}
	names := []string{"f", "g", "h"}
	vars := []string{"a", "b", "y"}
	stmt = func(d int) string {
		v, f := vars[r.Intn(len(vars))], names[r.Intn(len(names))]
		k := r.Intn(17)
		if d > 4 {
			k = r.Intn(8)
		}
		switch k {
		case 0:
			return v + "=1"
		case 1:
			return "local " + v + "=2"
		case 2:
			return "declare -i " + v
		case 3:
			return "((" + v + "++))"
		case 4:
			return "let " + v + "=3"
		case 5:
			return "read " + v
		case 6:
			return "echo $" + v
		case 7:
			return "export " + v
		case 8:
			return f + "() { " + block(d+1) + "; }"
		case 9:
			return "function " + f + " { " + block(d+1) + "; }"
		case 10:
			return f + "() ( " + block(d+1) + " )"
		case 11:
			return "( " + block(d+1) + " )"
		case 12:
			return "x=$( " + block(d+1) + " )"
		case 13:
			return "cat <( " + block(d+1) + " )"
		case 14:
			return "a | { " + block(d+1) + "; }"
		case 15:
			return "coproc { " + block(d+1) + "; }"
		default:
			return "for " + v + " in 1 2; do " + block(d+1) + "; done"
		}
	}
	var sb strings.Builder
	for range 2 + r.Intn(6) {
		sb.WriteString(block(0) + "\n")
	}
	return sb.String()
}

func TestDocumentSymbolsMatchWalk(t *testing.T) {
	check := func(name string, s *Snapshot) {
		t.Helper()
		for _, p := range s.Paths() {
			if got, want := s.DocumentSymbols(p), docSymbolsByWalk(s, p); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s:\ngot  %+v\nwant %+v", name, p, got, want)
			}
		}
	}
	for _, dir := range []string{"../testdata/corpus", "../testdata/corpus-annotated", "../testdata/broken"} {
		w := loadDir(t, dir)
		files := map[string]*FileFacts{}
		for _, ff := range w.facts {
			files[ff.Path] = ff
		}
		check(dir, Build(files, Options{Root: w.root}, 1))
	}
	r := rand.New(rand.NewSource(1))
	locals := 0
	for i := range 3000 {
		src := randomScopeProgram(r)
		p := fmt.Sprintf("/ws/r%d.sh", i)
		s := Build(map[string]*FileFacts{p: Bind(front.Parse(p, []byte(src), front.Bash))}, Options{Root: "/ws"}, 1)
		check(src, s)
		for _, d := range s.DocumentSymbols(p) {
			locals += len(d.Children)
		}
	}
	if locals < 1000 {
		t.Fatalf("the random programs list only %d locals", locals)
	}
}

// DocumentSymbols walked up the tree per local to find its function:
// O(declarations x depth), `bash-lsp symbols` 5.6 s on 2 MiB of arithmetic
// writes 3,900 subshells deep in a function, and 4.8 s on 1 MiB of plain
// assignments in the same shape.
func TestDocumentSymbolsInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	const depth = 3900
	deep := func(unit string) string {
		pre, post := "f() { "+strings.Repeat("( ", depth), strings.Repeat(" )", depth)+" }\n"
		return fillTo(size, pre, unit, post)
	}
	for name, src := range map[string]string{
		"arithmetic writes": deep("((y=1));"),
		"plain assignments": deep("y=1;"),
	} {
		ff := parseClean(t, name, src)
		s := Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1)
		var ds []DocSymbol
		if withinBound(t, name, bound, func() { ds = s.DocumentSymbols("/ws/a.sh") }) &&
			(len(ds) != 1 || len(ds[0].Children) != 1 || ds[0].Children[0].Name != "y") {
			t.Errorf("%s: %+v", name, ds)
		}
	}
}
