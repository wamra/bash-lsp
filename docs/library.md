# Using bash-lsp as a library

Four packages are public: the root package `bash-lsp`, `analysis`, `parse`
and `tree`. The parser behind them is internal and cannot be imported.

| Package | What it gives you |
|---|---|
| `github.com/wamra/bash-lsp` | `Load(root)`: discover the shell files under a directory the way the language server does (`.gitignore`, symlink policy, default size and count limits) and return an `*analysis.Snapshot`. A missing or non-directory root is an error; a file that does not parse is not. |
| `.../parse` | `Detect(path, src)`, `File(path, src)` (detect, then parse), `FileAs(path, src, dialect)` and `Fragment(src, dialect)` (parse the body of a string the shell runs as code, an `eval` argument, a `trap` action or the string of `bash -c`: the result has no path and its positions are relative to `src`). Never returns nil. `File.Dialect` says what was used; syntax problems are in `File.Errors`, a file that was not parsed says why in `File.Skipped`. |
| `.../analysis` | `Bind(file)` gives the facts of one file; `Analyze(files, opts)` parses and binds a set of in-memory files (keys are absolute paths), resolves `source` among them and returns generation 1. Files over `analysis.MaxFileBytes` (2 MiB) are in the snapshot as skipped. The snapshot answers `Definition`, `References`, `WorkspaceSymbols`, `DocumentSymbols`, `Visible`, `SourceEdges`, `BindingsOf`, and the call-graph queries listed under "The call graph" below. |
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

## The call graph

`Snapshot` holds an edge table built with every `Build`: one `Edge` per call,
`source`, execution of a file, or alias, each labelled with a `Resolution` and
a reason. The queries below read it. Everything a query returns is a copy; the
symbols and declarations an edge points to belong to the snapshot and are
read-only.

**Types.**

```go
type Relation uint8 // RelCalls, RelSources, RelInvokes, RelAliases; String() gives the name

type Caller struct { Path string; Func *Symbol }          // Func nil: the top level of file Path
type Node   struct { Path string; Func *Symbol }          // a function as defined in one file, or (Func nil) the file
type Edge   struct {
	From   Caller
	To     *Symbol    // nil for RelSources, RelInvokes, dynamic and unresolved edges
	ToDecl *Decl      // the one definition this edge goes to (one edge per definition in view)
	ToFile string     // target of RelSources and RelInvokes
	Rel    Relation
	Res    Resolution
	Reason string
	Site   Location
	Text   string     // expression of a dynamic edge; path word of an unresolved source
}
type Step struct { From, To Node; Edge Edge }              // From calls, sources or invokes To
```

A function defined in two files is two `Node`s and two `Caller`s. For
`RelAliases`, `Caller.Func` is the alias symbol (`Kind` `DeclAlias`); check
`Func.Kind` if you want function callers only.

**Reasons of edges.** `ReasonDirect`, `ReasonAlias`, `ReasonOutsideClosure`
(`name-match-outside-closure`), `ReasonConditional`
(`conditional-definition`: several definitions in view, or one confined to a
subshell or similar scope the call is outside of), `ReasonVariableConstant`,
`ReasonStringEval`, `ReasonStringTrap`, `ReasonStringShellC`,
`ReasonUnsetInView` (`unset-in-view`: some file in the site's visibility
closure removes the function with `unset -f`; order is not tracked, so the
edge is never resolved), `ReasonVariableUnknown` and `ReasonExpansion` (both
`Dynamic`: the command name is an expansion with no known target),
`ReasonNotFound` (a `source` whose file was not found) and
`ReasonDynamicScope` (variable reads only, never an edge). `ReasonEnvironment`
belongs to variable reads. A string body over the per-file string budget, or
nested more than three strings deep, is a `Dynamic`/`expansion` edge.

**Queries.** Signatures as in `go doc -all ./analysis`.

| Signature | Answers |
|---|---|
| `Edges() []Edge` | Every edge, sorted by site path and offset. |
| `EdgesFrom(c Caller) []Edge` | Edges whose caller is `c`, in `Edges` order. |
| `EdgesTo(sym *Symbol) []Edge` | Edges whose target symbol is `sym`. |
| `Unresolved(paths []string) []Edge` | Dynamic edges and `source`s with no target whose site is in `paths` (absolute or root-relative; nil or empty: the whole workspace). |
| `Callers(n Node, depth int) []Step` | Steps leading to `n` through resolved and candidate calls, sources and invokes, breadth-first, `depth` levels (1 is direct; 0 or less is unlimited), each edge once. `Step.From`/`Step.To` follow the edge's direction. Filter by `Step.Edge.Res`. |
| `Callees(n Node, depth int) []Step` | The same for the steps leaving `n`; a file node has the edges of its top level, a function node those of its body. |
| `Impact(n Node, depth int) []Node` | Every node that transitively calls, sources or invokes `n`, each once, breadth-first; `n` itself is not in the result. |
| `Trace(a, b Node) []Step` | A shortest path from `a` to `b` over resolved and candidate edges; nil when there is none or `a == b`. Ties go to the path through the earliest sites. |
| `SourceCycles() [][]string` | Groups of files that source each other (strongly connected components with more than one file, or a file that sources itself), sorted. |
| `Lookup(spec string) ([]Node, error)` | `"path:line:col"` (1-based, UTF-16 column) gives the function named at that position, else the function around it, else the file; `"path"` (absolute or root-relative) the file node; any other spec is a function name, one node per file that defines it. A spec that matches a file is never read as a name. An unknown spec is an error. |
| `FuncNode(path, name string) (Node, bool)` | The node of function `name` as defined in `path`; false when `path` is not in the snapshot or defines no such function. Unlike `Lookup`, it never reads `name` as a file or position spec. |
| `FuncDecl(n Node) *Decl` | The first definition of `n.Func` in `n.Path`; nil for a file node or a function with no definition there. |
| `CallerAt(path string, offset int) Caller` | The innermost function declaration around a site, else the file's top level; the zero `Caller` for an unknown path. |
| `ResolvedReferences(path string, offset int, includeDecl bool) []Location` | `References` without the uncertain sites of a function: a use is kept only when the call edge at that site to the symbol is resolved (conditional-definition and unset-in-view edges are not), so candidate sites and literal values (`cmd=f`) are left out. Declarations follow `includeDecl`; variables and aliases are as in `References`. |
| `Implementation(path string, offset int) []Location` | Every declaration of the function under the cursor. |
| `Hover(path string, offset int) (markdown string, rng tree.Range, ok bool)` | Markdown for the symbol under the cursor; `rng` is the hovered token (the whole definition when the cursor is on a function's name). |
| `SymbolAt(path string, offset int) (*Symbol, bool)` | The symbol under the cursor: a declaration name or a bound use. |
| `Facts(path string) *FileFacts` | The facts of `path`; nil when unknown. |
| `GraphIncomplete(path string) bool` | Whether some file in `Visible(path)` has a `source` that could not be resolved. |
| `Generation() uint64` | The number `Build` was given (1 from `Analyze`). |

**Other exported identifiers.**

- `func Build(files map[string]*FileFacts, opt Options, generation uint64) *Snapshot`
  builds a snapshot from facts (keys are absolute paths); `Options{Root,
  SearchPaths func(path string) []string, Prev *Snapshot}`. When `Prev` is
  set, `Build` reuses its visibility sets if the files and the source graph
  are unchanged; the result is the same as without it. `Analyze` is `Build`
  plus parsing and binding.
- `func ResolveSources(ff *FileFacts, root string, searchPaths []string, exists func(string) bool, glob func(string) []string) []SourceEdge`
  resolves every `source` command of one file; `SourceEdge{Ref SourceRef,
  Targets []string, Status SourceStatus, How string}` (`How` is `directive`,
  `literal`, `script-dir` or `loop-glob`; empty when unresolved);
  `SourceStatus` is `SourceResolved`, `SourceUnresolved` or `SourceIgnored`
  (directive `source=/dev/null`).
- `func IsBuiltin(name string) bool` reports whether `name` is a bash builtin
  or reserved word.
- `FileFacts` has `Path`, `File`, `Root`, `Decls`, `Uses`, `Sources`,
  `Dynamic []DynamicRef` (command names that are expansions with no known
  target), `Invokes []InvokeRef` (executions of a file by literal path:
  `./x.sh`, `bash x.sh`) and `Unsets []string` (names removed by
  `unset -f NAME`, literal names only; `builtin unset -f` and
  `command unset -f` are not seen).
- `Decl` has `Kind`, `Name`, `NameRange`, `Scope`, `Attrs VarAttrs`, `Doc`,
  `Node`, `AliasTo`, `Literal`, `Implicit`, `Temp` and `Confined *Scope` (the
  innermost subshell, command substitution, pipeline stage, process
  substitution or coproc around a function definition; nil when none: a call
  outside it cannot be sure to reach the definition). `Scope` has `Kind
  ScopeKind` (`ScopeFile`, `ScopeFunction`, `ScopeSubshell`, `ScopeCmdSubst`,
  `ScopePipeline`, `ScopeProcSubst`, `ScopeCoproc`; `Private()` is true for
  every kind except `ScopeFile` and `ScopeFunction`), `Parent`, `Node` and `Func`.
- `Use` has `Kind UseKind` (`UseCall`, `UseVarRead`, `UseVarWrite`), `Name`,
  `Range`, `Scope`, `Node`, `ViaAlias` and `Via Via`; `Via` is `ViaNone`,
  `ViaEval`, `ViaTrap`, `ViaShellC` (a command found inside the string),
  `ViaVariable` (`"$cmd"`: `Name` is the variable and the range text is not a
  function name) or `ViaValue` (made by `Build`: the literal `cmd=f` through
  which a `ViaVariable` use resolved; no edge has it as its site).
  `(*Use).NamesTarget() bool` is false for a `ViaVariable` word and for a
  name written with quotes, escapes or line continuations (`eval \f`):
  a rename must not rewrite such a site.
- `Symbol` has `Kind`, `Name`, `Decls`, `Func`, `File` and `Private
  ScopeKind`; `SymbolInfo{Name, Kind, Path, Range}` is a workspace-symbol hit;
  `DocSymbol{Name, Kind, Detail, Range, Selection, Children}` is one outline
  entry; `DeclKind` is `DeclFunction`, `DeclVariable` or `DeclAlias`;
  `Location{Path, Range}`.
- `Binding{Use, File, Sym, Res, Reason}`; `Resolution` is `Resolved`,
  `Candidate`, `Dynamic` or `Unresolved`, and has `String()`.
  `SourceRef{Node, Word, Range, Directive}` is one `source`/`.` (or bats
  `load`) command.
- `const MaxFileBytes = 2 << 20`.

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
