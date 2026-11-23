# Using bash-lsp as a library

Four packages are public: the root package `bash-lsp`, `analysis`, `parse`
and `tree`. The parser behind them is internal and cannot be imported.

| Package | What it gives you |
|---|---|
| `github.com/wamra/bash-lsp` | `Load(root)`: discover the shell files under a directory the way the language server does (`.gitignore`, symlink policy, default size and count limits) and return an `*analysis.Snapshot`. A missing or non-directory root is an error; a file that does not parse is not. |
| `.../parse` | `Detect(path, src)`, `File(path, src)` (detect, then parse) and `FileAs(path, src, dialect)`. Never returns nil. `File.Dialect` says what was used; syntax problems are in `File.Errors`, a file that was not parsed says why in `File.Skipped`. |
| `.../analysis` | `Bind(file)` gives the facts of one file; `Analyze(files, opts)` parses and binds a set of in-memory files (keys are absolute paths), resolves `source` among them and returns generation 1. Files over `analysis.MaxFileBytes` (2 MiB) are in the snapshot as skipped. The snapshot answers `Definition`, `References`, `WorkspaceSymbols`, `DocumentSymbols`, `Visible`, `SourceEdges`, `BindingsOf`. |
| `.../tree` | The syntax tree: `tree.File`, `tree.Node`, `tree.Kind`, `tree.Dialect`. Positions are byte offsets with 1-based line and column; `File.ToUTF16` converts one to an LSP line and UTF-16 column, and `File.NewConverter` returns a `tree.Converter` that converts many positions of one file, in any order, without rescanning a long line for each (results equal `File.ToUTF16`; not safe for concurrent use, make one per request). |

Use `bashlsp.Load` for a directory on disk, `analysis.Analyze` for files you
hold in memory, and `parse.File` for one file's tree.

Every slice a snapshot returns (`Paths`, `SourceEdges` with its `Targets`,
`Visible`, `BindingsOf`) is a copy you may modify. The facts, declarations and
tree nodes a snapshot hands out are read-only (see the doc comments).

Columns differ between the library and the tools. A library `tree.Range`
(and the `Range` of every `analysis.Location`) has a 1-based line, a 1-based
column counted in bytes, and a byte offset. Every position the CLI prints or
accepts is a 1-based line with a 1-based column counted in UTF-16 code units,
and every LSP position is a line with a UTF-16 column (0-based on the wire, as
the protocol defines). `tree.File.ToUTF16` and `tree.File.NewConverter` convert
a library position to the UTF-16 form.

`BindingsOf` returns `Binding`s, each with a `Resolution` and a reason. A
binding is `Resolved` (reason `direct`, or `alias` for a call reached through
an alias), `Candidate` or `Unresolved` (reason `environment`: a variable read
with no declaration in view). A `Candidate` binding has reason
`name-match-outside-closure` (a call that resolves to no function in view,
matched by name to every function of that name, in a file whose source graph
is incomplete or that neither sources nor is sourced by another file),
`string-eval`, `string-trap` or `string-shell-c` (a call found by name inside
an `eval`, `trap` or `sh -c` string), `variable-constant` (a `"$cmd"` call
whose variable holds a literal function name, and that literal itself) or
`dynamic-scope` (a variable read that may see a caller's local). No binding is
`Dynamic` and none has reason `not-found`: those appear only on edges
(`Edges`), where `Dynamic` marks a call whose target is not known and
`unresolved`/`not-found` a `source` whose file was not found.

Tree shapes you can rely on: one `tree.KAndOr` node spans a whole `&&`/`||`
chain (children are the operands in order, operators are not recorded);
`tree.KPipeline` is flat; every statement is a `tree.KStmt`.

## Example

A separate module, `example.com/consumer`, loads `testdata/corpus` and prints
where `build_release` is defined and referenced. Its `go.mod`:

```
module example.com/consumer

go 1.26.0

require github.com/wamra/bash-lsp v0.0.0

replace github.com/wamra/bash-lsp => ~/dev/bash-lsp
```

(bash-lsp has no dependencies of its own: its shell parser is vendored in
`internal/front/syntax`.) Its `main.go`:

```go
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

Output of `./consumer testdata/corpus` (go1.26.0):

```
definition: lib/util.sh:4:1
dialect: bash
references: 4
  dyn.sh:4:1
  main.sh:3:10
  main.sh:6:3
  main.sh:7:3
```

## Go version

This module's `go.mod` says `go 1.26.0`. `~/dev/code-index/go.mod`
says `go 1.24.1`. A module cannot import one that needs a newer Go than its own
`go` line, so linking bash-lsp into code-index needs a toolchain bump there.

## Deep nesting: what a host can rely on, and its limits

The shell parser is recursive, and a Go stack overflow ends the process. The
parser therefore counts its own nesting and stops at 10,000 levels (design:
`m2a-nesting-design.md`). Past that it reports the syntax error `nesting is
deeper than 10000 levels` at the place the limit was passed.

What a host can rely on:

- `parse.File` may be called from any number of goroutines. Parses share no
  state: there is no package lock, no goroutine is started, nothing is read
  from the runtime and no collection is forced. A file's result depends on
  the file alone, not on what is parsed beside it or on the host's other
  goroutines.
- Input of any size returns. Nesting past the limit is a syntax error in
  `File.Errors` like any other: the statements before it are analysed and
  parsing resumes on a later line. Only when the error leaves no statement
  at all is `File.Skipped` set, to the same text; the file then has its
  errors and no statements.
- Each operator of a chain is a level, so a pipeline, an `&&`/`||` list or
  an arithmetic chain of about 10,000 operators is that same syntax error.
  The exact figure per construct is in the design note ("The limit, per
  construct").
- Comment blocks, heredocs, strings and long runs of statements are not
  nesting and have no limit.
- The lowerer, which turns the parser's tree into `tree.Node`s, has a lower
  limit of its own: 4,000 lowering levels, where each statement, word, word
  part (expansion, quote or literal) and `if` or `elif` clause inside another
  is one level. A statement past it is left out of the tree with the error
  `statement not analysed: nested more than 4000 levels deep, counting each
  statement, word, word part (expansion, quote or literal) and if or elif
  clause inside another`; the rest of the file is analysed. A construct costs
  one to four lowering levels, so the depth a user meets depends on the
  construct. The first refused depth (`bench/results/lowerer-depth-m2a-final.txt`,
  from `TestLowererLimitPerConstruct`, which pins each figure):

  | Construct, nested n times | Refused from n = |
  |---|---|
  | `{ }`, `( )`, `while`, `for`, `case` | 3,998 |
  | an `if` with n `elif` branches (flat to the user) | 3,996 |
  | `if`, a function body `f() {`, `${x:-`, a pipeline stage `a \| (` | 1,999 |
  | `$( )`, `<( )` | 1,333 |
  | `"$( )"`, an `if` inside each function body | 1,000 |

  The tree handed to `analysis` is therefore at most about 12,000 nodes deep.

What it assumes of the host:

- The parser runs on the calling goroutine. At the limit it uses up to
  about 42 MiB of stack (measured; the design note has the figures) on top
  of what the caller already uses.
- `debug.SetMaxStack` is left at 64 MiB or more (the default is 1 GB). Under
  a lower limit, input nested close to 10,000 levels is a fatal stack
  overflow again.
- Parsing one file takes time in proportion to its size, except after a
  syntax error the parser cannot recover from: the rest of the file is then
  parsed again from up to 50 later lines. A 2 MiB file built for that takes
  up to 17.5 s (`bench/results/resync-cost-m2a-task5.txt`: one `front.Parse`
  of the slowest input known, 17.46 to 17.63 s over three runs).

## `bash-lsp ast` on deep trees

`bash-lsp ast FILE` and `ast --json FILE` refuse a tree deeper than 1,000
levels: exit status 1 and `tree too deep to print (depth N)`. Deeper trees are
still analysed; only the printed views are refused.
