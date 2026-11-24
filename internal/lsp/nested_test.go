package lsp

import (
	"encoding/json"
	"fmt"
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
