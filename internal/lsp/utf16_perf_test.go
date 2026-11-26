package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A 2 MiB single-line file with a million references did not
// finish: every location was converted to UTF-16 by rescanning its line from
// the start. Every request that converts many positions must stay linear.
func TestManyLocationsOnOneLongLineAreFast(t *testing.T) {
	const bound = 5 * time.Second
	// The race detector slows this several times over: a quarter of the size,
	// and no wall-clock bound (as elsewhere in this package).
	refs, funcs := 400_000, 60_000
	if raceEnabled {
		refs, funcs = refs/4, funcs/4
	}
	timed := func(what string, f func()) {
		t.Helper()
		start := time.Now()
		f()
		if d := time.Since(start); d > bound && !raceEnabled {
			t.Errorf("%s took %v, want under %v", what, d, bound)
		}
	}

	t.Run("references", func(t *testing.T) {
		c := client(t, "")
		c.openText("r.sh", "x=1\necho"+strings.Repeat(" $x", refs)+"\n")
		c.waitDiag("r.sh")
		timed("references", func() {
			var out []Location
			raw := c.call("textDocument/references", map[string]any{
				"textDocument": map[string]any{"uri": c.uri("r.sh")}, "position": Position{0, 0},
				"context": map[string]any{"includeDeclaration": true}})
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if len(out) != refs+1 {
				t.Fatalf("%d references", len(out))
			}
		})
	})

	for _, hier := range []bool{false, true} {
		name := "documentSymbol flat"
		if hier {
			name = "documentSymbol hierarchical"
		}
		t.Run(name, func(t *testing.T) {
			c := clientOpt(t, "", hier)
			c.openText("s.sh", strings.Repeat("é() { :; }; ", funcs)+"\n")
			c.waitDiag("s.sh")
			timed(name, func() {
				if n := len(c.docSymbols("s.sh")); n != funcs {
					t.Fatalf("%d symbols", n)
				}
			})
		})
	}

	t.Run("workspace/symbol", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "w.sh", strings.Repeat("é() { :; }; ", funcs)+"\n")
		c := client(t, root)
		timed("workspace/symbol", func() {
			var out []SymbolInformation
			if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": "é"}), &out); err != nil {
				t.Fatal(err)
			}
			if len(out) == 0 {
				t.Fatal("no symbols")
			}
		})
	})
}

func writeFile(t *testing.T, root, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
