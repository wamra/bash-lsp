// Command consumer loads a directory of shell files and prints where
// build_release is defined and every place that refers to it.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wamra/bash-lsp"
	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

func main() {
	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		panic(err)
	}
	snap, err := bashlsp.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	hits := snap.WorkspaceSymbols("build_release", 5)
	if len(hits) == 0 {
		fmt.Println("build_release not found")
		return
	}
	def := hits[0]
	fmt.Printf("definition: %s:%d:%d\n", rel(root, def.Path), def.Range.Start.Line, def.Range.Start.Col)

	// References are answered for a position, so find the name inside the
	// definition with the public parser, then ask the snapshot.
	src, err := os.ReadFile(def.Path)
	if err != nil {
		panic(err)
	}
	f := parse.File(def.Path, src)
	off := nameOffset(f, "build_release")
	fmt.Printf("dialect: %s\n", f.Dialect)
	show(root, "references", snap.References(def.Path, off, false))
}

func show(root, label string, locs []analysis.Location) {
	fmt.Printf("%s: %d\n", label, len(locs))
	for _, l := range locs {
		fmt.Printf("  %s:%d:%d\n", rel(root, l.Path), l.Range.Start.Line, l.Range.Start.Col)
	}
}

// nameOffset is the offset of the first function declaration named name.
func nameOffset(f *tree.File, name string) int {
	var walk func(n *tree.Node) int
	walk = func(n *tree.Node) int {
		if n.Kind == tree.KFuncDecl && n.Name == name {
			return n.Range.Start.Offset
		}
		for _, c := range n.Children {
			if o := walk(c); o >= 0 {
				return o
			}
		}
		return -1
	}
	return walk(f.Root)
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}
```

Output:

```
definition: lib/util.sh:4:1
dialect: bash
references: 4
  dyn.sh:4:1
  main.sh:3:10
  main.sh:6:3
  main.sh:7:3
```

The consumer's `go.mod`:

```
module example.com/consumer

go 1.26.0

require github.com/wamra/bash-lsp v0.0.0

replace github.com/wamra/bash-lsp => ~/dev/bash-lsp
```

(`go build` also adds `require mvdan.cc/sh/v3 v3.14.1 // indirect`.)

## Go version

This module's `go.mod` says `go 1.26.0`. `~/dev/code-index/go.mod`
says `go 1.24.1`. A module cannot import one that needs a newer Go than its own
`go` line, so linking bash-lsp into code-index needs a toolchain bump there.

## Limitation: hosts that already hold 64 MiB of goroutine stacks

`parse` guards the parser's stack through its reader (design:
`m2a-nesting-design.md`, "Mechanism" and "Limits a user can meet"). The guard
reads the process's total goroutine stack memory. If the host process already
holds 64 MiB or more of it, the guard cannot tell whether that memory is the
parser's, so it falls back to counting frames: at most 65,536 frames, with a
budget of 6,000,000 frames counted per file. In that regime a very long run of
comment lines can be refused (`Skipped` is "nesting too deep to parse safely"):
a comment block is refused from 12,000 to 55,000 lines on (sooner the longer
its lines), and nesting from about 2,200 arithmetic parentheses. Alone in the
process the limit is 32 MiB of parser stack, about 250,000 comment lines in one
block. A host that parses untrusted files in a process with large goroutine
stacks should expect such files to come back skipped, not crash it. The figures
are the design note's, measured there; removing the limit needs a per-goroutine
stack depth the Go runtime does not expose, or a patched parser.

## `bash-lsp ast` on deep trees

`bash-lsp ast FILE` and `ast --json FILE` refuse a tree deeper than 1,000
levels: exit status 1 and `tree too deep to print (depth N)`. Deeper trees are
still analysed; only the printed views are refused.
