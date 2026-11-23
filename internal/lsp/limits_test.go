package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// review C1: a deeply nested file overflowed the parser's stack, which killed
// the server whether the file was in the workspace at start or opened later.
func TestDeeplyNestedFileKeepsServerAlive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "deep.sh"), []byte(strings.Repeat("(", 200000)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.sh"), []byte("still_here() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	wsym := func(q string) int {
		var out []SymbolInformation
		if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": q}), &out); err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	if wsym("still_here") != 1 {
		t.Fatal("workspace with a deep file not indexed")
	}
	c.openText("opened.sh", "echo "+strings.Repeat("$(", 500000)+"\n")
	d := c.waitDiag("opened.sh")
	found := false
	for _, x := range d {
		if x.Message == "nesting is deeper than 10000 levels" && x.Code == "WB1001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no nesting diagnostic: %+v", d)
	}
	if wsym("still_here") != 1 {
		t.Fatal("server stopped answering")
	}
}

// review C1 / T9: an open document is held to the same size limit as a file
// on disk, so the parser never sees more than 2 MiB from either.
func TestOversizeDocumentIsSkipped(t *testing.T) {
	big := "too_big_fn() { :; }\n" + strings.Repeat("# pad\n", (2<<20)/6+1)
	for name, root := range map[string]string{"workspace": "corpus", "no root": ""} {
		t.Run(name, func(t *testing.T) {
			if root != "" {
				root = copyCorpus(t)
			}
			c := client(t, root)
			c.openText("big.sh", big)
			d := c.waitDiag("big.sh")
			if len(d) == 0 || d[0].Code != "WB1002" || !strings.Contains(d[0].Message, "exceeds the 2097152 byte file limit") {
				t.Fatalf("%+v", d)
			}
			if got := c.symbolNames("big.sh"); len(got) != 0 {
				t.Fatalf("symbols of a skipped document: %v", got)
			}
			c.change("big.sh", fullText("small_fn() { :; }\n"))
			if d := c.waitDiag("big.sh"); len(d) != 0 {
				t.Fatalf("%+v", d)
			}
			if got := c.symbolNames("big.sh"); !eq(got, "small_fn") {
				t.Fatal(got)
			}
		})
	}
}

// Nesting within the parser's limit, as deep as the lowerer keeps
// (just under 4,000 loops), reaches every request that walks the tree:
// diagnostics, the outline, hover and definition. Run under a 64 MiB stack
// limit, an eighth of what a stack can reach by default.
func TestDeepestTreeIsServed(t *testing.T) {
	defer debug.SetMaxStack(debug.SetMaxStack(64 << 20))
	const n = 3900
	text := strings.Repeat("while a; do\n", n) + "inner_most() { local v=1; echo \"$v\"; }\ninner_most now\n" + strings.Repeat("done\n", n) + "after_all() { :; }\n"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "deep.sh"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	c.open("deep.sh")
	for _, d := range c.waitDiag("deep.sh") {
		if d.Severity == 1 {
			t.Fatalf("error diagnostic: %+v", d)
		}
	}
	names := strings.Join(c.symbolNames("deep.sh"), " ")
	if !strings.Contains(names, "inner_most") || !strings.Contains(names, "after_all") {
		t.Fatalf("outline: %.200s", names)
	}
	if got := c.definition("deep.sh", "inner_most now", 0); len(got) != 1 {
		t.Fatalf("definition of the innermost call: %v", got)
	}
	var hover struct{ Contents struct{ Value string } }
	raw := c.call("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("deep.sh")}, "position": c.pos("deep.sh", "inner_most now", 0)})
	if err := json.Unmarshal(raw, &hover); err != nil || !strings.Contains(hover.Contents.Value, "inner_most") {
		t.Fatalf("hover: %s (%v)", raw, err)
	}
}
