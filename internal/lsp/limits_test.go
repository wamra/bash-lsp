package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		if x.Message == "nesting too deep to parse safely" {
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
