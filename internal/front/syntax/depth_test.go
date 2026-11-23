package syntax

// This file is not upstream's. It belongs to the depth-counter patch
// (UPGRADING.md).

import (
	"go/ast"
	"go/importer"
	"go/parser"
	gotoken "go/token"
	"go/types"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestEveryRecursionIsCounted fails if the parser can recurse without
// passing a function that counts depth: it type-checks this package, builds
// the call graph of everything reachable from the Parse methods (a mention
// of a function counts as a call, a call through an interface as a call to
// every method of that name), removes the functions whose body calls
// (*Parser).enterNode, and requires that what is left has no cycle.
func TestEveryRecursionIsCounted(t *testing.T) {
	fset := gotoken.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("syntax", fset, files, info); err != nil {
		t.Fatal(err)
	}
	edges := map[*types.Func][]*types.Func{}
	counted := map[*types.Func]bool{}
	byName := map[string][]*types.Func{} // concrete methods, for interface calls
	var roots []*types.Func
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := info.Defs[fd.Name].(*types.Func)
			edges[fn] = nil
			if fd.Recv != nil {
				byName[fn.Name()] = append(byName[fn.Name()], fn)
				if recv := fn.Type().(*types.Signature).Recv().Type().String(); recv == "*syntax.Parser" {
					switch fn.Name() {
					case "Parse", "Stmts", "StmtsSeq", "Words", "WordsSeq", "Document", "Arithmetic", "Interactive", "InteractiveSeq":
						roots = append(roots, fn)
					}
				}
			}
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := info.Defs[fd.Name].(*types.Func)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				callee, ok := info.Uses[id].(*types.Func)
				if !ok {
					return true
				}
				if callee.Name() == "enterNode" {
					counted[fn] = true
				}
				if _, declared := edges[callee]; declared {
					edges[fn] = append(edges[fn], callee)
				} else if sig, ok := callee.Type().(*types.Signature); ok && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) {
					edges[fn] = append(edges[fn], byName[callee.Name()]...)
				}
				return true
			})
		}
	}
	if len(roots) < 5 {
		t.Fatalf("found %d Parse methods", len(roots))
	}
	reach := map[*types.Func]bool{}
	var visit func(fn *types.Func)
	visit = func(fn *types.Func) {
		if reach[fn] {
			return
		}
		reach[fn] = true
		for _, c := range edges[fn] {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	// Tarjan over the reachable, uncounted functions.
	index, low := map[*types.Func]int{}, map[*types.Func]int{}
	on := map[*types.Func]bool{}
	var stack []*types.Func
	var cycles []string
	var strong func(v *types.Func)
	strong = func(v *types.Func) {
		index[v], low[v] = len(index)+1, len(index)+1
		stack, on[v] = append(stack, v), true
		self := false
		for _, w := range edges[v] {
			if counted[w] {
				continue
			}
			if w == v {
				self = true
			}
			if index[w] == 0 {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var comp []string
		for {
			w := stack[len(stack)-1]
			stack, on[w] = stack[:len(stack)-1], false
			comp = append(comp, w.FullName())
			if w == v {
				break
			}
		}
		if (len(comp) > 1 || self) && !treeWalk(comp) {
			sort.Strings(comp)
			cycles = append(cycles, strings.Join(comp, " "))
		}
	}
	for fn := range reach {
		if !counted[fn] && index[fn] == 0 {
			strong(fn)
		}
	}
	sort.Strings(cycles)
	for _, c := range cycles {
		t.Errorf("recursion that does not count depth: %s", c)
	}
	if len(counted) == 0 {
		t.Error("no function calls enterNode")
	}
}

// treeWalk reports whether every function of a cycle is one that recurses
// over syntax already built, not over input: its depth is at most the depth
// of the tree, which the counter has bounded.
//
//   - Pos and End of the nodes, with the stmtsEnd and wordLastEnd helpers.
//   - unquotedWordPart: descends from a DblQuoted into its parts, which are
//     never DblQuoted themselves.
//   - (*wrappedReader).Read: calls the reader it wraps; the call graph here
//     cannot tell that reader from itself.
func treeWalk(cycle []string) bool {
	for _, fn := range cycle {
		name := fn[strings.LastIndex(fn, ".")+1:]
		switch name {
		case "Pos", "End", "stmtsEnd", "wordLastEnd", "unquotedWordPart":
		case "Read":
			if !strings.Contains(fn, "wrappedReader") {
				return false
			}
		default:
			return false
		}
	}
	return true
}
