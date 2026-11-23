package analysis

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// naiveVisibility is the definition, spelled out with one traversal per file:
// Visible(A) is everything reached from any file that reaches A (A included).
func naiveVisibility(adj [][]int, unresolved []bool) (visible [][]int, incomplete []bool) {
	n := len(adj)
	reach := make([][]bool, n)
	for i := range reach {
		reach[i] = make([]bool, n)
		stack := []int{i}
		reach[i][i] = true
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, y := range adj[x] {
				if !reach[i][y] {
					reach[i][y] = true
					stack = append(stack, y)
				}
			}
		}
	}
	visible = make([][]int, n)
	incomplete = make([]bool, n)
	for a := 0; a < n; a++ {
		vis := make([]bool, n)
		for r := 0; r < n; r++ {
			if reach[r][a] {
				for f := 0; f < n; f++ {
					vis[f] = vis[f] || reach[r][f]
				}
			}
		}
		for f, v := range vis {
			if v {
				visible[a] = append(visible[a], f)
				if unresolved[f] {
					incomplete[a] = true
				}
			}
		}
	}
	return
}

// The component-wise visibility equals the definition on random graphs, with
// cycles, self edges, duplicate edges, isolated files and unresolved flags.
func TestComputeVisibilityMatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 300; trial++ {
		n := 1 + rng.Intn(70)
		adj := make([][]int, n)
		unresolved := make([]bool, n)
		density := []float64{0.0, 0.01, 0.03, 0.08}[rng.Intn(4)]
		for i := range adj {
			for j := 0; j < n; j++ {
				if rng.Float64() < density {
					adj[i] = append(adj[i], j)
					if rng.Intn(5) == 0 {
						adj[i] = append(adj[i], j)
					}
				}
			}
			unresolved[i] = rng.Intn(9) == 0
		}
		s := &Snapshot{paths: make([]string, n)}
		s.computeVisibility(adj, unresolved)
		wantVis, wantInc := naiveVisibility(adj, unresolved)
		for a := 0; a < n; a++ {
			var got []int
			s.visible[a].each(func(f int) { got = append(got, f) })
			if !reflect.DeepEqual(got, wantVis[a]) {
				t.Fatalf("trial %d, file %d: visible %v, want %v (adj %v)", trial, a, got, wantVis[a], adj)
			}
			if s.incomplete[a] != wantInc[a] {
				t.Fatalf("trial %d, file %d: incomplete %v, want %v", trial, a, s.incomplete[a], wantInc[a])
			}
		}
	}
}

// answers records every Definition, References and Visible result and every
// GraphIncomplete flag a snapshot gives, so two snapshots can be compared.
func answers(s *Snapshot) map[string]string {
	out := map[string]string{}
	locs := func(ls []Location) string {
		var parts []string
		for _, l := range ls {
			parts = append(parts, fmt.Sprintf("%s@%d-%d", filepath.Base(l.Path), l.Range.Start.Offset, l.Range.End.Offset))
		}
		sort.Strings(parts)
		return fmt.Sprint(parts)
	}
	for _, p := range s.Paths() {
		var vis []string
		for _, v := range s.Visible(p) {
			vis = append(vis, filepath.Base(v))
		}
		out[p+" visible"] = fmt.Sprint(vis)
		out[p+" incomplete"] = fmt.Sprint(s.GraphIncomplete(p))
		ff := s.Facts(p)
		for _, u := range ff.Uses {
			off := u.Range.Start.Offset
			out[fmt.Sprintf("%s def@%d", p, off)] = locs(s.Definition(p, off))
			out[fmt.Sprintf("%s refs@%d", p, off)] = locs(s.References(p, off, true))
		}
		for _, d := range ff.Decls {
			off := d.NameRange.Start.Offset
			out[fmt.Sprintf("%s refs@%d", p, off)] = locs(s.References(p, off, true))
		}
	}
	return out
}

// Snapshots built one after another, each with the previous as Options.Prev
// and with the facts of unchanged files shared as the workspace does, answer
// like a build from scratch of the same files, and older snapshots keep their
// answers (they are immutable) while later ones are built.
func TestIncrementalBuildsEqualScratchBuilds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	cache := map[string]*FileFacts{}
	facts := func(srcs map[string]string) map[string]*FileFacts {
		files := map[string]*FileFacts{}
		for name, s := range srcs {
			p := filepath.Join(root, filepath.FromSlash(name))
			k := name + "\x00" + s
			if cache[k] == nil {
				b := []byte(s)
				cache[k] = Bind(front.Parse(p, b, front.Detect(p, b)))
			}
			files[p] = cache[k]
		}
		return files
	}
	clone := func(m map[string]string) map[string]string {
		c := map[string]string{}
		for k, v := range m {
			c[k] = v
		}
		return c
	}
	cur := map[string]string{
		"a.sh": "source ./b.sh\nfa() { fb; fz; }\n",
		"b.sh": "source ./c.sh\nfb() { fc; }\n",
		"c.sh": "fc() { :; }\nfz() { :; }\n",
		"d.sh": "fd() { fa; fc; }\nfz() { :; }\n",
	}
	type step struct {
		name  string
		apply func(m map[string]string)
	}
	steps := []step{
		{"initial", func(m map[string]string) {}},
		{"body edit", func(m map[string]string) { m["b.sh"] = "source ./c.sh\n# note\nfb() { fc; }\n" }},
		{"add a source line", func(m map[string]string) { m["d.sh"] = "source ./a.sh\n" + m["d.sh"] }},
		{"remove it", func(m map[string]string) { m["d.sh"] = "fd() { fa; fc; }\nfz() { :; }\n" }},
		{"add it again", func(m map[string]string) { m["d.sh"] = "source ./a.sh\nfd() { fa; fc; }\nfz() { :; }\n" }},
		{"retarget it", func(m map[string]string) { m["d.sh"] = "source ./c.sh\nfd() { fa; fc; }\nfz() { :; }\n" }},
		{"add a file", func(m map[string]string) { m["e.sh"] = "source ./d.sh\nfe() { fd; fz; }\n" }},
		{"unresolvable source", func(m map[string]string) { m["e.sh"] = "source \"$x\"\nfe() { fd; fz; }\n" }},
		{"remove a file in the chain", func(m map[string]string) { delete(m, "b.sh") }},
		{"restore it", func(m map[string]string) { m["b.sh"] = "source ./c.sh\nfb() { fc; }\n" }},
		{"make a cycle", func(m map[string]string) { m["c.sh"] = "source ./a.sh\nfc() { :; }\nfz() { :; }\n" }},
		{"cycle through a self edge", func(m map[string]string) { m["a.sh"] = "source ./b.sh\nsource ./a.sh\nfa() { fb; fz; }\n" }},
		{"break the cycle", func(m map[string]string) {
			m["c.sh"] = "fc() { :; }\nfz() { :; }\n"
			m["a.sh"] = "source ./b.sh\nfa() { fb; fz; }\n"
		}},
		{"remove every file but one", func(m map[string]string) {
			for _, k := range []string{"a.sh", "b.sh", "c.sh", "d.sh"} {
				delete(m, k)
			}
		}},
	}
	var snaps []*Snapshot
	var recorded []map[string]string
	var prev *Snapshot
	for i, st := range steps {
		next := clone(cur)
		st.apply(next)
		cur = next
		s := Build(facts(cur), Options{Root: root, Prev: prev}, uint64(i+1))
		fresh := Build(facts(cur), Options{Root: root}, uint64(i+1))
		if got, want := answers(s), answers(fresh); !reflect.DeepEqual(got, want) {
			for k := range want {
				if got[k] != want[k] {
					t.Errorf("step %q: %s = %s, scratch build gives %s", st.name, k, got[k], want[k])
				}
			}
			t.Fatalf("step %q: incremental build differs from a scratch build", st.name)
		}
		snaps = append(snaps, s)
		recorded = append(recorded, answers(s))
		prev = s
		for j := range snaps {
			if !reflect.DeepEqual(answers(snaps[j]), recorded[j]) {
				t.Fatalf("after step %q: snapshot of step %q changed", st.name, steps[j].name)
			}
		}
	}
}
