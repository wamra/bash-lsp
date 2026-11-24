package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- nested workspace folders (ruling R58) ----
//
// A folder nested inside another workspace folder is served by the outer
// folder's backend: it is not excluded from the outer walk and has no
// backend of its own. Removing the outer folder while the inner remains
// gives the inner its own backend; removing the inner while the outer
// remains changes nothing; adding an outer folder after the inner absorbs
// the inner.

// nestedTree is a/m.sh sourcing a/nested/n.sh and calling its function,
// and a broken file in the nested folder (for diagnostics).
func nestedTree(t *testing.T) (a, nested string) {
	a = filepath.Join(t.TempDir(), "a")
	nested = filepath.Join(a, "nested")
	put(t, filepath.Join(a, "m.sh"), "source ./nested/n.sh\nnf\n")
	put(t, filepath.Join(nested, "n.sh"), "nf() { :; }\n")
	put(t, filepath.Join(nested, "bad.sh"), "if then\n")
	return a, nested
}

// nestedAnswers opens the three files and renders every answer the
// ruling covers: definition, references, call hierarchy, hover,
// workspace/symbol and the diagnostics pushed for each open file.
func (c *testClient) nestedAnswers() string {
	c.t.Helper()
	for _, rel := range []string{"m.sh", "nested/n.sh", "nested/bad.sh"} {
		c.open(rel)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "definition %v\n", c.definition("m.sh", "nf", 0))
	fmt.Fprintf(&b, "references %v\n", c.references("nested/n.sh", "nf()", 0))
	fmt.Fprintf(&b, "references from call %v\n", c.references("m.sh", "nf", 0))
	for _, at := range []struct{ rel, text string }{{"m.sh", "nf"}, {"nested/n.sh", "nf()"}} {
		for _, it := range c.prepare(at.rel, at.text, 0) {
			fmt.Fprintf(&b, "prepare %s: %s\n", at.rel, c.itemText(it))
			for _, in := range c.incoming(it) {
				fmt.Fprintf(&b, "  incoming %s %s\n", c.itemText(in.From), lines(in.FromRanges))
			}
		}
	}
	fmt.Fprintf(&b, "hover %q\n", c.hoverText("nested/n.sh", "nf()", 0))
	fmt.Fprintf(&b, "symbols %v\n", c.symbols(""))
	c.mu.Lock()
	for _, rel := range []string{"m.sh", "nested/n.sh", "nested/bad.sh"} {
		pushes := c.diags[c.uri(rel)]
		raw, _ := json.Marshal(pushes)
		fmt.Fprintf(&b, "diagnostics %s: %d pushes %s\n", rel, len(pushes), raw)
	}
	c.mu.Unlock()
	return b.String()
}

func TestNestedFoldersServedByOuter(t *testing.T) {
	a, nested := nestedTree(t)
	answers := func(folders []string) string {
		c := clientWith(t, clientCfg{folders: folders})
		c.root = a
		return c.nestedAnswers()
	}
	want := answers([]string{a})
	for _, must := range []string{"definition [nested/n.sh:1]", "references [m.sh:2 nested/n.sh:1]",
		"incoming m.sh 1 m.sh [m.sh] 2:1", "Called from 1 site", "symbols [nested/n.sh:1 nf]",
		"diagnostics nested/bad.sh: 1 pushes"} {
		if !strings.Contains(want, must) {
			t.Fatalf("single root answers lack %q:\n%s", must, want)
		}
	}
	for name, folders := range map[string][]string{
		"outer then inner": {a, nested},
		"inner then outer": {nested, a},
	} {
		if got := answers(folders); got != want {
			t.Errorf("%s:\n%s\nwant (root a alone):\n%s", name, got, want)
		}
	}
}

func TestNestedFoldersAtRuntime(t *testing.T) {
	a, nested := nestedTree(t)
	alone := func(folders []string) string {
		c := clientWith(t, clientCfg{folders: folders})
		c.root = a
		return c.nestedAnswers()
	}
	outerOnly := alone([]string{a})

	// Removing the inner folder while the outer remains changes nothing.
	c := clientWith(t, clientCfg{folders: []string{a, nested}})
	c.root = a
	c.folders(nil, []string{nested})
	if got := c.nestedAnswers(); got != outerOnly {
		t.Errorf("inner removed:\n%s\nwant:\n%s", got, outerOnly)
	}

	// Adding an outer folder after the inner absorbs the inner.
	c = clientWith(t, clientCfg{folders: []string{nested}})
	c.root = a
	c.folders([]string{a}, nil)
	if got := c.nestedAnswers(); got != outerOnly {
		t.Errorf("outer added after inner:\n%s\nwant:\n%s", got, outerOnly)
	}

	// Removing the outer folder while the inner remains gives the inner its
	// own backend: n.sh is served there, m.sh (now outside every folder)
	// alone.
	c = clientWith(t, clientCfg{folders: []string{a, nested}})
	c.root = a
	c.open("m.sh")
	c.open("nested/n.sh")
	c.folders(nil, []string{a})
	if got := c.symbols(""); !eq(got, "nested/n.sh:1 nf") {
		t.Fatalf("inner alone, symbols: %v", got)
	}
	if got := c.definition("m.sh", "nf", 0); len(got) != 0 {
		t.Fatalf("m.sh outside every folder reaches the inner one: %v", got)
	}
	if got := c.references("nested/n.sh", "nf()", 0); !eq(got, "nested/n.sh:1") {
		t.Fatalf("inner alone, references: %v", got)
	}
	// And adding the outer again absorbs it once more.
	c.folders([]string{a}, nil)
	if got := c.references("nested/n.sh", "nf()", 0); !eq(got, "m.sh:2", "nested/n.sh:1") {
		t.Fatalf("outer re-added, references: %v", got)
	}
	if got := c.symbols(""); !eq(got, "nested/n.sh:1 nf") {
		t.Fatalf("outer re-added, symbols once: %v", got)
	}
}

// ---- folders inside directories the outer walk skips ----

// skippedTree is g with a git-ignored vendor directory and a node_modules/nm
// directory; each holds v.sh (defines vf), w.sh (sources it and calls vf) and
// bad.sh (a syntax error). g/m.sh is outside both.
func skippedTree(t *testing.T) (g string) {
	g = filepath.Join(t.TempDir(), "g")
	put(t, filepath.Join(g, ".gitignore"), "vendor/\n")
	put(t, filepath.Join(g, "m.sh"), "echo outer\n")
	for _, sub := range []string{"vendor", "node_modules/nm"} {
		put(t, filepath.Join(g, sub, "v.sh"), "vf() { :; }\n")
		put(t, filepath.Join(g, sub, "w.sh"), "source ./v.sh\nvf\n")
		put(t, filepath.Join(g, sub, "bad.sh"), "if then\n")
	}
	return g
}

// vendorAnswers renders what the server says about the folder sub of the
// client's root: first with no document open, then after opening its three
// files.
func (c *testClient) vendorAnswers(sub string) string {
	c.t.Helper()
	var b strings.Builder
	ask := func(phase string) {
		fmt.Fprintf(&b, "%s: symbols %v\n", phase, c.symbols(""))
		fmt.Fprintf(&b, "%s: definition %v\n", phase, c.definition(sub+"/w.sh", "vf", 0))
		fmt.Fprintf(&b, "%s: references %v\n", phase, c.references(sub+"/v.sh", "vf()", 0))
		fmt.Fprintf(&b, "%s: hover %q\n", phase, c.hoverText(sub+"/v.sh", "vf()", 0))
	}
	ask("closed")
	for _, f := range []string{"v.sh", "w.sh", "bad.sh"} {
		c.open(sub + "/" + f)
	}
	ask("open")
	fmt.Fprintf(&b, "open: symbols once %v\n", c.symbols("vf"))
	c.mu.Lock()
	pushes := c.diags[c.uri(sub+"/bad.sh")]
	raw, _ := json.Marshal(pushes)
	fmt.Fprintf(&b, "diagnostics %s/bad.sh: %d pushes %s\n", sub, len(pushes), raw)
	c.mu.Unlock()
	return b.String()
}

func checkSkippedNestedRoot(t *testing.T, sub string) {
	g := skippedTree(t)
	nested := filepath.Join(g, filepath.FromSlash(sub))
	answers := func(folders []string) string {
		c := clientWith(t, clientCfg{folders: folders})
		c.root = g
		return c.vendorAnswers(sub)
	}
	want := answers([]string{nested})
	one := sub + "/v.sh:1 vf"
	for _, must := range []string{
		"closed: symbols [" + one + "]", "open: symbols [" + one + "]",
		"open: definition [" + sub + "/v.sh:1]", "open: references [" + sub + "/v.sh:1 " + sub + "/w.sh:2]",
		"Called from 1 site", "diagnostics " + sub + "/bad.sh: 1 pushes"} {
		if !strings.Contains(want, must) {
			t.Fatalf("the nested root alone lacks %q:\n%s", must, want)
		}
	}
	for name, folders := range map[string][]string{
		"outer then nested": {g, nested},
		"nested then outer": {nested, g},
	} {
		if got := answers(folders); got != want {
			t.Errorf("%s:\n%s\nwant (nested root alone):\n%s", name, got, want)
		}
	}
}

func TestIgnoredNestedRootHasOwnBackend(t *testing.T) { checkSkippedNestedRoot(t, "vendor") }

func TestNodeModulesNestedRootHasOwnBackend(t *testing.T) {
	checkSkippedNestedRoot(t, "node_modules/nm")
}

func TestOpenDocInSkippedNestedRootRoutesInnermost(t *testing.T) {
	g := skippedTree(t)
	c := clientWith(t, clientCfg{folders: []string{g, filepath.Join(g, "vendor")}})
	c.root = g
	c.open("vendor/v.sh")
	c.waitDiag("vendor/v.sh")
	c.change("vendor/v.sh", fullText("vf() { :; }\nvg() { :; }\n"))
	c.waitDiag("vendor/v.sh")
	if got := c.symbols("vf"); !eq(got, "vendor/v.sh:1 vf") {
		t.Fatalf("vf listed other than once: %v", got)
	}
	if got := c.hoverText("vendor/v.sh", "vf()", 0); !strings.Contains(got, "Defined in `v.sh:1`") {
		t.Fatalf("hover is not the nested backend's (paths relative to its root): %q", got)
	}
	c.mu.Lock()
	extra := len(c.diags[c.uri("vendor/v.sh")])
	c.mu.Unlock()
	if extra != 0 {
		t.Fatalf("%d further pushes for one change", extra)
	}
}

func TestSkippedNestedRootSequences(t *testing.T) {
	g := skippedTree(t)
	v := filepath.Join(g, "vendor")
	const want = "vendor/v.sh:1 vf"
	type step struct {
		name           string
		added, removed []string
		count          int
	}
	run := func(t *testing.T, start []string, steps []step) {
		c := clientWith(t, clientCfg{folders: start})
		c.root = g
		c.open("vendor/bad.sh")
		check := func(name string, count int) {
			t.Helper()
			n := 0
			for _, s := range c.symbols("") {
				if s == want {
					n++
				}
			}
			if n != count {
				t.Errorf("%s: %q listed %d times, want %d (%v)", name, want, n, count, c.symbols(""))
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			for uri, pushes := range c.diags {
				if len(pushes) > 1 {
					t.Errorf("%s: %d pushes for %s", name, len(pushes), uri)
				}
				c.diags[uri] = nil
			}
		}
		c.waitDiag("vendor/bad.sh") // the push for the open itself
		for _, s := range steps {
			c.folders(s.added, s.removed)
			check(s.name, s.count)
		}
	}
	steps := []step{
		{"add vendor", []string{v}, nil, 1},
		{"add vendor again", []string{v}, nil, 1},
		{"remove vendor", nil, []string{v}, 0},
		{"add vendor", []string{v}, nil, 1},
		{"remove g", nil, []string{g}, 1},
		{"add g back", []string{g}, nil, 1},
		{"remove vendor", nil, []string{v}, 0},
		{"remove g", nil, []string{g}, 0},
	}
	t.Run("outer first", func(t *testing.T) { run(t, []string{g}, steps) })
	t.Run("nested declared first", func(t *testing.T) {
		run(t, []string{v, g}, append([]step{{"initial", nil, nil, 1}}, steps[2:]...))
	})
}

func TestNestedRootInReachedDirStaysServedByOuter(t *testing.T) {
	g := filepath.Join(t.TempDir(), "g")
	put(t, filepath.Join(g, ".gitignore"), "vendor/\n")
	put(t, filepath.Join(g, "m.sh"), "source ./nested/v.sh\nvf\n")
	put(t, filepath.Join(g, "nested", "v.sh"), "vf() { :; }\n")
	put(t, filepath.Join(g, "nested", "w.sh"), "source ./v.sh\nvf\n")
	put(t, filepath.Join(g, "nested", "bad.sh"), "if then\n")
	answers := func(folders []string) string {
		c := clientWith(t, clientCfg{folders: folders})
		c.root = g
		return c.vendorAnswers("nested")
	}
	want := answers([]string{g})
	if !strings.Contains(want, "references [m.sh:2 nested/v.sh:1 nested/w.sh:2]") {
		t.Fatalf("outer alone does not cross the folder:\n%s", want)
	}
	for _, folders := range [][]string{{g, filepath.Join(g, "nested")}, {filepath.Join(g, "nested"), g}} {
		if got := answers(folders); got != want {
			t.Errorf("%v:\n%s\nwant:\n%s", folders, got, want)
		}
	}
}

// ---- duplicate and case-variant roots ----

func foldsCase(t *testing.T, dir string) bool {
	t.Helper()
	a, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(filepath.Dir(dir), strings.ToUpper(filepath.Base(dir))))
	return err == nil && os.SameFile(a, b)
}

func TestNestedFolderCaseVariants(t *testing.T) {
	a, nested := nestedTree(t)
	variant := filepath.Join(a, "NESTED")
	t.Run("removal by variant spelling", func(t *testing.T) {
		if !foldsCase(t, nested) {
			t.Skip("volume does not fold case")
		}
		c := clientWith(t, clientCfg{folders: []string{a, nested}})
		c.root = a
		c.folders(nil, []string{variant})
		c.folders(nil, []string{a})
		if got := c.symbols(""); len(got) != 0 {
			t.Fatalf("nothing should be served: %v", got)
		}
	})
	t.Run("add by two spellings", func(t *testing.T) {
		if !foldsCase(t, nested) {
			t.Skip("volume does not fold case")
		}
		c := clientWith(t, clientCfg{folders: []string{a}})
		c.root = a
		c.folders([]string{variant}, nil)
		c.folders([]string{nested}, nil)
		c.folders(nil, []string{nested})
		c.folders(nil, []string{a})
		if got := c.symbols(""); len(got) != 0 {
			t.Fatalf("nothing should be served: %v", got)
		}
	})
}

func TestInitializeDuplicateRootsRemovedOnce(t *testing.T) {
	a, _ := nestedTree(t)
	for name, folders := range map[string][]string{
		"same spelling":  {a, a},
		"trailing slash": {a, a + string(filepath.Separator)},
	} {
		t.Run(name, func(t *testing.T) {
			c := clientWith(t, clientCfg{folders: folders})
			c.root = a
			c.folders(nil, []string{a})
			if got := c.symbols(""); len(got) != 0 {
				t.Fatalf("a is still served: %v", got)
			}
			c.open("m.sh")
			if got := c.definition("m.sh", "nf", 0); len(got) != 0 {
				t.Fatalf("m.sh is not served by the loose backend: %v", got)
			}
		})
	}
}
