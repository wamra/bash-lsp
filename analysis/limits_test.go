package analysis

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/parse"
)

func limitLines(s *Snapshot, kind string) []string {
	var out []string
	for _, l := range s.LimitsHit() {
		if strings.Contains(l, ": "+kind+":") {
			out = append(out, l)
		}
	}
	return out
}

func TestAliasDepthReported(t *testing.T) {
	deep := diagWorld(map[string]string{"x.sh": aliasChain(9)}, Options{})
	got := limitLines(deep, "alias depth")
	if len(got) != 1 || !strings.HasPrefix(got[0], "x.sh:2: alias depth: 2 uses") {
		t.Fatalf("9-deep chain: LimitsHit = %q", deep.LimitsHit())
	}
	if shallow := diagWorld(map[string]string{"x.sh": aliasChain(8)}, Options{}); len(shallow.LimitsHit()) != 0 {
		t.Errorf("8-deep chain reports %q", shallow.LimitsHit())
	}
	// A chain that ends at a command, not at anything the next step could follow, is no loss.
	end := strings.Replace(aliasChain(9), "alias a9=f", "alias a9='ls -l'", 1)
	if s := diagWorld(map[string]string{"x.sh": end}, Options{}); len(s.LimitsHit()) != 0 {
		t.Errorf("a chain ending at a command reports %q", s.LimitsHit())
	}
}

func TestAliasCycleReportedOncePerFile(t *testing.T) {
	src := "alias a=b\nalias b=a\na\na\n"
	s := diagWorld(map[string]string{"x.sh": src}, Options{})
	if got := limitLines(s, "alias depth"); len(got) != 1 || !strings.HasPrefix(got[0], "x.sh:1: alias depth: 4 uses") {
		t.Fatalf("LimitsHit = %q, want one alias depth line for the file", s.LimitsHit())
	}
}

func globWorld(n int) map[string]string {
	files := map[string]string{"main.sh": "#!/bin/bash\nfor f in lib/*.sh; do\n  source \"$f\"\ndone\n"}
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("lib/f%04d.sh", i)] = fmt.Sprintf("f%04d() { :; }\n", i)
	}
	return files
}

func TestGlobCap(t *testing.T) {
	s := diagWorld(globWorld(1200), Options{})
	main := filepath.Join(diagRoot, "main.sh")
	es := s.SourceEdges(main)
	if len(es) != 1 || es[0].Status != SourceResolved || len(es[0].Targets) != 1000 {
		t.Fatalf("source edges: %d, targets %d", len(es), len(es[0].Targets))
	}
	if !slices.IsSorted(es[0].Targets) || !strings.HasSuffix(es[0].Targets[999], "f0999.sh") {
		t.Errorf("targets are not the first 1000 in path order: last %s", es[0].Targets[999])
	}
	got := limitLines(s, "glob cap")
	if len(got) != 1 || !strings.HasPrefix(got[0], "main.sh:3: glob cap: 1 source patterns match more than 1000 files") {
		t.Fatalf("LimitsHit = %q", s.LimitsHit())
	}
	if s := diagWorld(globWorld(1000), Options{}); len(s.LimitsHit()) != 0 {
		t.Errorf("exactly 1000 matches reports %q", s.LimitsHit())
	}
}

// The index answers as filepath.Match over every path does, for patterns
// with and without pattern characters in the directory part.
func TestGlobIndexEqualsFullScan(t *testing.T) {
	var paths []string
	for _, d := range []string{"/w", "/w/lib", "/w/lib/a", "/w/lib/b", "/w/x[y]", "/w/plug-1", "/w/plug-2"} {
		for _, n := range []string{"a.sh", "b.sh", "ab.bash", "c1.sh", "c2.sh", "data.txt", "[z].sh"} {
			paths = append(paths, d+"/"+n)
		}
	}
	sort.Strings(paths)
	g := newGlobIndex(paths)
	pats := []string{"/w/*.sh", "/w/lib/*.sh", "/w/lib/*/*.sh", "/w/*/a.sh", "/w/plug-?/c?.sh", "/w/lib/[ab].sh", "/w/lib/a/*", "/w/nope/*.sh",
		"/w/x\\[y\\]/*.sh", "/w/lib/*.{sh}", "/w/plug-[12]/*.bash", "/*/lib/b.sh", "/w/lib/[", "relative/*.sh"}
	for _, pat := range pats {
		var want []string
		for _, p := range paths {
			if ok, _ := filepath.Match(pat, p); ok {
				want = append(want, p)
			}
		}
		if got := g.match(pat); !slices.Equal(got, want) {
			t.Errorf("%s: index %q, scan %q", pat, got, want)
		}
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		pat := "/w/" + []string{"lib", "*", "plug-?", "lib/*"}[rng.Intn(4)] + "/" + []string{"*.sh", "a*", "?.sh", "c[12].sh"}[rng.Intn(4)]
		var want []string
		for _, p := range paths {
			if ok, _ := filepath.Match(pat, p); ok {
				want = append(want, p)
			}
		}
		if got := g.match(pat); !slices.Equal(got, want) {
			t.Errorf("%s: index %q, scan %q", pat, got, want)
		}
	}
}

func TestFindingsCapInLimitsHit(t *testing.T) {
	src := "#!/bin/bash\n" + strings.Repeat("local x=1\n", 2100) // WB1105 outside a function: 2,100 findings
	s := diagWorld(map[string]string{"x.sh": src, "y.sh": "echo hi\n"}, Options{})
	if got := s.LimitsHit(); len(got) != 0 {
		t.Fatalf("before any Diagnostics: %q", got)
	}
	p := filepath.Join(diagRoot, "x.sh")
	if n := len(s.Diagnostics(p, DiagOptions{})); n != maxFindings+1 {
		t.Fatalf("%d findings, want %d and the WB1007 notice", n, maxFindings)
	}
	s.Diagnostics(filepath.Join(diagRoot, "y.sh"), DiagOptions{})
	got := limitLines(s, "findings cap")
	if len(got) != 1 || got[0] != "x.sh:1: findings cap: 100 findings past the 2000 shown for a file are not reported" {
		t.Fatalf("LimitsHit = %q", s.LimitsHit())
	}
	// With WB1007 turned off the notice is gone but the cut is still listed.
	q := diagWorld(map[string]string{"x.sh": src}, Options{})
	if n := len(q.Diagnostics(p, DiagOptions{Disabled: map[string]bool{"WB1007": true}})); n != maxFindings {
		t.Fatalf("%d findings with WB1007 disabled", n)
	}
	if len(limitLines(q, "findings cap")) != 1 {
		t.Errorf("a quiet WB1007 hides the cut: %q", q.LimitsHit())
	}
	// A file under the cap is not listed.
	r := diagWorld(map[string]string{"x.sh": "#!/bin/bash\n" + strings.Repeat("local x=1\n", 5)}, Options{})
	r.Diagnostics(p, DiagOptions{})
	if len(r.LimitsHit()) != 0 {
		t.Errorf("LimitsHit = %q", r.LimitsHit())
	}
}

func TestParserLimitsInLimitsHit(t *testing.T) {
	src := resyncBudgetInput()
	f := parse.File("/w/x.sh", []byte(src))
	if len(f.Limits) == 0 {
		t.Skip("input no longer hits a resync limit")
	}
	s := diagWorld(map[string]string{"x.sh": src}, Options{})
	got := s.LimitsHit()
	if len(got) != len(f.Limits) || !strings.HasPrefix(got[0], "x.sh:") || !strings.Contains(got[0], f.Limits[0].Name+": ") {
		t.Fatalf("LimitsHit = %q for limits %+v", got, f.Limits)
	}
}

func TestLimitsHitSortedByPathThenLine(t *testing.T) {
	files := map[string]string{"b.sh": aliasChain(9), "a.sh": aliasChain(9), "m.sh": "for f in lib/*.sh; do source \"$f\"; done\n"}
	for i := 0; i < 1001; i++ {
		files[fmt.Sprintf("lib/f%04d.sh", i)] = ":\n"
	}
	s := diagWorld(files, Options{})
	got := s.LimitsHit()
	if len(got) != 3 || !strings.HasPrefix(got[0], "a.sh:") || !strings.HasPrefix(got[1], "b.sh:") || !strings.HasPrefix(got[2], "m.sh:") {
		t.Fatalf("LimitsHit = %q", got)
	}
}

// globLoaderWorld is the workspace of TestGlobIndexLimitsComparisons: 1,000 library files in 20
// directories of 50, and 200 loader files, ten per directory, each sourcing
// every file of its directory.
func globLoaderWorld() map[string]*FileFacts {
	files := map[string]*FileFacts{}
	for d := 0; d < 20; d++ {
		for i := 0; i < 50; i++ {
			p := fmt.Sprintf("%s/d%02d/f%02d.sh", diagRoot, d, i)
			files[p] = Bind(parse.File(p, []byte(fmt.Sprintf("d%02d_f%02d() { :; }\n", d, i))))
		}
	}
	for l := 0; l < 200; l++ {
		p := fmt.Sprintf("%s/load%03d.sh", diagRoot, l)
		src := fmt.Sprintf("for f in d%02d/*.sh; do\n  source \"$f\"\ndone\n", l%20)
		files[p] = Bind(parse.File(p, []byte(src)))
	}
	return files
}

func globEdit(files map[string]*FileFacts, prev *Snapshot, gen uint64) *Snapshot {
	p := fmt.Sprintf("%s/d05/f07.sh", diagRoot)
	edited := make(map[string]*FileFacts, len(files))
	for k, v := range files {
		edited[k] = v
	}
	edited[p] = Bind(parse.File(p, []byte(fmt.Sprintf("d05_f07() { echo %d; }\n", gen))))
	return Build(edited, Options{Root: diagRoot, Prev: prev}, gen)
}

func BenchmarkGlobEdit(b *testing.B) {
	files := globLoaderWorld()
	prev := Build(files, Options{Root: diagRoot}, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prev = globEdit(files, prev, uint64(i+2))
	}
}

// 1,000 files and 200 loaders of 50 files each: the index compares the names
// of one directory per pattern (50), where a scan of every path compares all
// 1,000, so a Build compares 10,000 names and not 200,000. The count does not
// depend on the load of the machine; it fails when the index is bypassed.
func TestGlobIndexLimitsComparisons(t *testing.T) {
	files := globLoaderWorld()
	s := Build(files, Options{Root: diagRoot}, 1)
	if n := len(s.SourceEdges(diagRoot + "/load000.sh")[0].Targets); n != 50 {
		t.Fatalf("loader resolves %d files, want 50", n)
	}
	if got := s.lim.globTried; got == 0 || got > 200*50 {
		t.Errorf("Build compared %d names for 200 patterns of 50-file directories, want 1..%d (a full scan is %d)", got, 200*50, 200*1000)
	}
	// A pattern with a pattern character in its directory part scans every path.
	g := newGlobIndex(s.paths)
	g.match(diagRoot + "/d0?/f00.sh")
	if got := g.tried.Load(); got != int64(len(s.paths)) {
		t.Errorf("a pattern directory scan compared %d names, want %d", got, len(s.paths))
	}
}
