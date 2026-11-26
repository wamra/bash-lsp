# `bash-lsp extract`: the record format

`bash-lsp extract` writes the call and source graph of a workspace as JSON
lines, one record per line, in the record format that code-index reads from an
extractor that prints symbols and call edges (an `exec` extractor). This
document says what `extract` emits. code-index runs it as

```
bash-lsp extract --root {root} --list-file {listFile}
```

with `bash-lsp version` as the version probe; see the code-index README for
the extractor configuration.

## 1. The command

```
bash-lsp extract [--root DIR] [--list-file FILE]
```

- Writes one compact JSON object per line (`\n`-terminated, UTF-8) to stdout,
  streamed file by file. Nothing is built as one string.
- `--root` defaults to `.`; it is the workspace root, as for the graph commands.
- `--list-file FILE`: one absolute path per line (a trailing `\r` is
  tolerated, empty lines are skipped); only those files are written. A listed
  path that is relative, outside the root or not a file of the workspace is
  reported on stderr and skipped; the exit code stays 0. An unreadable list
  file, or one over 8 MiB, is exit 1.
- Exit codes: 0 success, 1 runtime failure, 2 usage error. Same as the other
  commands.
- Output order is deterministic: file path, then byte offset of the site,
  then record type (`parse_error`, `error`, `func`, `alias`, `dotsource`,
  `invoke`, `call`, `dynamic`). Identical neighbouring records (one call site
  with several candidate definitions of the same name in one file) are written
  once.
- Nothing analysed is executed or sourced.

## 2. The records

Positions are 1-based `line` and `col` on every record. The column unit is
UTF-16 code units, which is also the rule for every position this program
prints (`pos16` in `cmd/bash-lsp/main.go`): for ASCII there is no difference;
for text with astral characters (an emoji counts 2) the units matter.
`TestExtractNonASCIIBeforeCallSite` pins this.

**Paths:** `file` (and every path-valued key) is absolute, in the workspace's
canonical spelling, with native separators. The golden file for this
repository replaces the root with `<root>/` so it is the same on every
machine.

Keys are written in this fixed order, each omitted when empty
(`TestExtractRawKeyOrder` pins it): `t`, `file`, `line`, `col`, `endLine`,
`endCol`, `name`, `alias`, `target`, `path`, `expr`, `in`, `message`, `first`,
`count`, `to`, `resolution`, `reason`.

| `t` | keys written | from |
|---|---|---|
| `func` | `name`, `line`, `col`, `endLine`, `endCol` | every function declaration; span of the whole definition. |
| `alias` | `alias`, `target`, `line`, `col` (the alias name), `endLine`, `endCol`, `in` | every `alias` declaration with a literal target word |
| `call` | `name`, `to`, `resolution`, `reason`, `in`, `line`, `col` | a call edge that is `resolved` or `candidate`. `name` is the function name, also for a call through an alias (`reason` is then `alias`); `to` is the file of the definition the edge goes to. A function with several definitions in view gives one record each (different `to`). |
| `dynamic` | `expr`, `resolution` (`dynamic`), `reason`, `in`, `line`, `col` | a call edge with resolution `dynamic`; `expr` is the site text, whitespace collapsed, at most 160 characters. |
| `dotsource` | `path`, `expr`, `resolution`, `reason`, `in`, `line`, `col` | a `source` or `.` command. Resolved: `path` is the absolute target file, one record per target. Unresolved: `path` is the word when it is a plain literal (`./missing.sh`), else absent; `expr` is the word text; `resolution` is `unresolved`, `reason` `not-found`. A source of an existing file the workspace skipped for its size: `path` is that file's absolute path, `resolution` `unresolved`, `reason` `skipped-for-size`. |
| `invoke` | `path`, `expr`, `resolution`, `reason`, `in`, `line`, `col` | execution of a workspace file by literal path (`./tool.sh`). |
| `parse_error` | `count`, `first`, `line` 1, `col` 1 | a file with syntax errors; the rest of the file is still extracted from the recovered tree. |
| `error` | `message`, `line` 1, `col` 1 | a file the workspace listed but did not load (zsh, over the size cap, over the file-count limit). |

Choices worth knowing:

- **Call-site column.** A `call` is at the command word and a `dotsource` at
  the path word (`source ./lib.sh` is at col 8).
- **`invoke` is its own record type.** Executing another file starts a child
  process; it does not share the caller's functions and variables, so it is
  not written as `dotsource`, and it has no function name to be a `call`.
- **What is never a `call`.** An unresolved edge, a builtin and an external
  command. A name match outside the sourced closure
  (`candidate`/`name-match-outside-closure`) is a `call` like any other
  candidate, one record per candidate definition, with its `resolution` and
  `reason` (`TestExtractOutsideClosureCandidates`). Such an edge exists only
  for a call that resolves to no function in view: on `testdata/corpus` no
  call record goes into `orphan.sh` from another file
  (`TestExtractNoCallToOrphanFromMain`).
- **`candidate` calls are written as `call`.** The `resolution` and `reason`
  keys carry the distinction for a consumer that reads them. A `source` edge is
  `resolved` or `unresolved` (also for glob-loop targets) and an `invoke` edge
  is always `resolved`, so `dotsource` and `invoke` never carry `candidate`.
- **Calls through an alias** name the function in `name`; `reason` `alias`
  says how it was reached.
- **A name defined in two files**, as `build_release` is in `lib/util.sh` and
  `orphan.sh`, is told apart by `to`, the file of the definition.

## 3. How it is verified

- `TestExtractCorpusDecodes` and every other extract test decode each line
  into the record shape of an extractor reader (no failure, `t` set, absolute
  `file`, 1-based `line`/`col`) and check the keys needed per type.
- `TestExtractGolden` compares the output for `testdata/corpus` with
  `testdata/extract.golden.jsonl`. The test writes the golden file itself:
  `go test ./cmd/bash-lsp -run TestExtractGolden -update`.
- Determinism across runs and the sort order, a non-ASCII character before a
  call site, CRLF files, `--list-file` with a path outside the root, usage
  errors, and a failing writer are covered in the same file.
