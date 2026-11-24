# The vendored shell parser

This directory is a copy of the package `mvdan.cc/sh/v3/syntax`
(https://github.com/mvdan/sh, BSD 3-Clause, `LICENSE`) with a small patch.
Only `internal/front` imports it (`boundary_test.go` at the repository root).

## What is here

- Upstream version: pseudo-version `v3.14.1-0.20260928185915-0e0eb36bb6dd`,
  commit `0e0eb36bb6dd` ("syntax: limit nesting to 10000 levels when
  parsing"). `go.sum` lines of that version:
  `h1:KNSILCN8TpA1+ytVyUR+OfaGV1Fl8myrnaOVgmc1K6M=` and
  `/go.mod h1:fiX5Z1I9lku4M1awdjesFL8qRLU32fJSTJuOoj2dzIk=`.
- Copied from the module's `syntax/` directory: `doc.go`, `lexer.go`,
  `nodes.go`, `parser.go`, `parser_arithm.go`, `tokens.go`, `tokens_parse.go`,
  `token_string.go`, `walk.go` and `simplify.go`, and `LICENSE` from the
  module root. Commit 67e84c7 is the copy of the first nine, byte for byte
  (`cmp` against the module cache); `simplify.go` is copied unchanged too.
  `printer.go` is copied and patched (its import of
  `mvdan.cc/sh/v3/fileutil` is gone and its one `fileutil.Shebang` call is
  `Shebang`), so no commit holds it pristine: the patch has the hunk.
- Why each: `parser.go`, `parser_arithm.go` and `lexer.go` are the parser;
  `nodes.go`, `tokens.go`, `token_string.go` and `tokens_parse.go` are the
  types and tables it uses; `walk.go` is what `visitAll` in `internal/front`
  mirrors and is tested against (`TestVisitAllMatchesWalk`); `doc.go` is the
  package documentation; the licence requires `LICENSE`; `printer.go` and
  `simplify.go` are the formatter (`internal/front/format.go`), the
  simplifier being what the `Simplify` option runs.
- Left out, because nothing here uses them: `braces.go`, `quote.go`,
  `gen_token_parse.go` (a generator, not part of the build), the `typedjson`
  package, and upstream's tests and test data.
- Ours: `shebang.go` (`Shebang` and `shebangRe`, verbatim from
  `mvdan.cc/sh/v3/fileutil/file.go`, with an origin comment: the printer
  calls it and nothing here may import the upstream module),
  `bash-lsp.patch` (the changes to upstream's files),
  `depth_test.go`, `bash-lsp_test.go`, this file.

## Why it is vendored

A Go stack overflow is fatal, and upstream's parser is recursive. Commit
`0e0eb36bb6dd` makes it count nesting (`enterNode`, `leaveNode`,
`maxNesting = 10_000`) and return the error `nesting is deeper than 10000
levels`. That commit is not in a release, and three things remained that a
release would not fix either. They are the patch.

## The patch (`bash-lsp.patch`: 4 files, 14 lines added, 5 removed)

1. `lexer.go`, `(*Parser).next`: after a comment the lexer called itself to
   read the next token, one stack frame per comment line. 2 MiB of `#` lines
   took 256 MiB of stack; the default limit is 1 GB, and a host can set less.
   The call is now `goto again` to a label at the top of the function.
2. `parser.go`, `(*Parser).errPass`: `p.tok = _EOF` is now set on every call,
   not only for the first error. Callers of the form
   `p.tok = p.paramToken(p.r)` overwrite the `_EOF` that an error inside the
   lexer function (invalid UTF-8) had set; `callExpr` then reported the token
   as unexpected, which is a no-op once an error is set, and never consumed
   it: `Parse` did not return. Input:
   `"${0=$((((\x11ke1\x05]@\x00\x00\x00\xad\xaeq\x16@\"\x80>\x00\x83|k(((($"`.
3. `parser.go`, `(*Parser).doHeredocs`, and `parser_arithm.go`,
   `(*Parser).arithmExprValue` (the `++`/`--` case): `enterNode` and
   `leaveNode` around recursion that upstream does not count. No input was
   found that recurses deeply through either (after `++` anything but a
   literal is an error, which ends the parse two levels down; a heredoc body
   reaches `doHeredocs` again only through word parts, which are counted).
   They are counted so that `TestEveryRecursionIsCounted` needs no exception
   for them.
4. `printer.go`: the import of `mvdan.cc/sh/v3/fileutil` is removed and the
   call `fileutil.Shebang` (in `(*Printer).comments`, only reached with
   `Minify`) is `Shebang`, defined in `shebang.go`. Nothing else in the
   printer differs from upstream.

Items 1 and 2 are bugs in upstream at `0e0eb36bb6dd`, and also at `v3.14.1`
and at `95154f9b8122` (the tip when this was written). They are worth
reporting upstream. Nobody has been contacted: whether to file is the
repository owner's decision.

## Upgrading

A pristine `printer.go` imports `mvdan.cc/sh/v3/fileutil` and does not build
here, so there is no commit that holds a pristine copy of all files. The git
index holds it instead.

1. Fetch the new version into the module cache (network access needed):
   `go mod download mvdan.cc/sh/v3@<version>` from a scratch module outside
   this repository.
2. Copy all the upstream files listed above (`syntax/*.go` that are listed
   as copied, and `LICENSE`) over the ones here, `cmp` each against the module
   cache, and `git add` them. The index now holds the pristine bytes. If
   upstream added a file that these need, copy it too and list it above.
3. `git apply internal/front/syntax/bash-lsp.patch`. For each hunk that no
   longer applies, read upstream's new code: if upstream fixed the problem,
   drop the hunk; otherwise redo it by hand. The package must build (the
   `printer.go` hunk and `shebang.go` are what make it build).
4. Regenerate the patch file before committing, so the diff is the working
   tree against the pristine index:
   `git diff -- internal/front/syntax/lexer.go internal/front/syntax/parser.go internal/front/syntax/parser_arithm.go internal/front/syntax/printer.go > internal/front/syntax/bash-lsp.patch`.
   Commit the copied files, the patched files and the patch together.
5. Update the version, hashes and commit ids in this file, in `NOTICE` and in
   `docs/m2a-nesting-design.md`.

## The tests that prove an upgrade

- `go test ./internal/front/syntax`:
  - `TestEveryRecursionIsCounted` (`depth_test.go`) type-checks this package,
    builds the call graph reachable from the `Parser` entry points and fails
    on any recursion cycle that does not pass a function calling `enterNode`.
    The cycles it accepts by name are walks of the tree (`Pos`, `End` and
    their helpers), which recurse as deep as the tree is, and the parser
    deepens the tree only in counted steps (every operator of a chain is
    counted for this reason); and an `io.Reader` wrapper, which is a false
    positive of matching interface calls by method name. A new upstream recursion that is not
    counted fails this test: count it, make it a loop, or show it is bounded
    and add it to the list with the reason. The test works per function, not
    per call: a function that calls `enterNode` on one path and recurses on
    another passes. `arithmExprValue` is such a function since item 3 of the
    patch (its other paths reach themselves only through `arithmExpr`, which
    counts). When upstream changes a function that calls `enterNode`, read
    the change.
  - `bash-lsp_test.go`: the limit and its error text, the input that hung,
    and the stack a block of comments uses.
- `go test ./internal/front`:
  - `TestNestingLimit` pins, per construct, how many repetitions parse and
    that one more is the nesting error. If upstream starts or stops counting
    something, the numbers move: update them and the table in
    `docs/m2a-nesting-design.md`, and check that nothing in
    `TestNoFalseRefusal` became an error.
  - `TestNoCrashSubprocess` parses every known crash input, every construct
    at its limit and megabytes of comments in a child process whose stack is
    limited to 64 MiB. `FuzzNestingGuard` does the same for generated input.
  - `TestDeepNestingIsSkipped`, `TestRefusalMessage`, `TestNoFalseRefusal`,
    `TestParseReturnsOnFuzzBindInput`.
- `visitAll` in `internal/front/visit.go` mirrors `walk.go`: compare the two
  when `walk.go` or `nodes.go` changes.
- The 28-case benchmark and the corpus scan (`bench/README.md`).
