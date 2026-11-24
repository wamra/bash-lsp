# bash-lsp and code-index: the `extract` record format

`bash-lsp extract` writes the call/source graph as JSON lines in the record
format that code-index (`~/dev/code-index`, HEAD `d6ba2fd`) reads
from its PowerShell extractor. This document records what that format is,
what `extract` emits, where it differs, and the changes code-index needs
before it accepts bash. Every code-index citation was re-read from source for
this document (paths relative to the code-index root); nothing in code-index
was run, built or modified.

**Status: AC-14 and AC-21 are unmet.** code-index cannot run a bash extractor
today (section 6). What is proven here is that every line `extract` writes
decodes into code-index's own `astRec` and carries the fields its reader
uses (`cmd/bash-lsp/extract_test.go`).

## 1. The command

```
bash-lsp extract [--root DIR] [--list-file FILE]
```

- Writes one compact JSON object per line (`\n`-terminated, UTF-8) to stdout,
  streamed file by file. Nothing is built as one string.
- `--root` defaults to `.`; it is the workspace root, as for the graph commands.
- `--list-file FILE`: one absolute path per line (a trailing `\r` is
  tolerated, empty lines are skipped); only those files are written.
  code-index does the same for its extractor: it writes the absolute paths
  to a temp file, joined by `\n`, and passes `-ListFile <file>`
  (`internal/indexer/ast.go:122-131,139`), and the PowerShell script reads
  one path per line (`extractors/powershell-ast.ps1:19,83-84`). A listed path that is
  relative, outside the root or not a file of the workspace is reported on
  stderr and skipped; the exit code stays 0. An unreadable list file, or one
  over 8 MiB, is exit 1.
- Exit codes: 0 success, 1 runtime failure, 2 usage error. Same as the other commands.
- Output order is deterministic: file path, then byte offset of the site,
  then record type (`parse_error`, `error`, `func`, `alias`, `dotsource`,
  `invoke`, `call`, `dynamic`). Identical neighbouring records (one call site
  with several candidate definitions of the same name in one file) are written once.
- Nothing analysed is executed or sourced.

## 2. What code-index reads: `astRec`

Decode struct, `internal/indexer/ast.go:34-52` (a copy sits in
`cmd/bash-lsp/extract_test.go`, with its origin). Reader,
`internal/indexer/ast.go:54-72`.

| key | JSON type | consumed by | cite |
|---|---|---|---|
| `t` | string | record type; a line with empty `t` is skipped | `ast.go:37,65` |
| `file` | string | absolute path, matched to an indexed file | `edges.go:138-151,436,492` |
| `name` | string | `call`: callee name; empty skips the record | `edges.go:507-510` |
| `alias` | string | `alias`: alias name; empty skips | `edges.go:433` |
| `target` | string | `alias`: target text; empty skips | `edges.go:433,442,503` |
| `path` | string | `dotsource`/`import`: the file named | `edges.go:525-537,550-580` |
| `expr` | string | `dynamic`: becomes `dst_name` | `edges.go:523-524` |
| `in` | string | enclosing function name, fallback source lookup | `edges.go:477-489` |
| `message` | string | `error`: reported as a note | `edges.go:297-301` |
| `first`, `count` | string, int | `parse_error`: decoded, not used | `ast.go:46,51`; no `case` in `edges.go:498-538` |
| `line`, `col` | int | 1-based; stored as `max(v-1, 0)` | `ast.go:34-35`; `edges.go:440,496` |
| `endLine`, `endCol` | int | `alias`: the alias symbol's range | `edges.go:441` |

Parse rules (`ast.go:56-72`): a line is accepted when, after trimming, it is
non-empty, starts with `{`, `json.Unmarshal` into `astRec` succeeds and `t`
is non-empty. Anything else is counted and reported as "ignored N non-JSON
line(s)" (`edges.go:294-296`). Lines up to 8 MiB (`ast.go:58`).

- **Unknown keys are ignored.** The decode is a plain `json.Unmarshal`
  (`ast.go:65`); `DisallowUnknownFields` appears only for the config file
  (`internal/config/config.go:85`). So `to`, `resolution` and `reason` on our
  lines are harmless today.
- **A known key of the wrong JSON type** (for example `"line":"3"`) makes
  the whole line skipped (`ast.go:65`). `extract` writes numbers for the
  integer keys.
- **Unknown `t`** is accepted by the reader and then ignored: `astEdges` has
  no case for it (`edges.go:538-539`, `default: continue`). `func` and
  `parse_error` are in that group today.
- **There is no confidence key.** code-index computes `exact`, `alias`,
  `dynamic` or `unresolved` itself from the record type and its symbol
  lookup (`edges.go:28-31,498-537`).

Record types code-index acts on (`edges.go:428-540`):

| `t` | effect |
|---|---|
| `alias` | new symbol of kind `alias` (`edges.go:442-446`) and an `alias_of` edge (`:498-506`) |
| `call` | `calls` edge when `name` matches a workspace function or alias, by lower-cased name only (`:507-522`); otherwise dropped (`:521`) |
| `dynamic` | `calls` edge with `dst_name = expr`, confidence `dynamic` (`:523-524`) |
| `dotsource` | `dot_sources` edge when `path` resolves to an indexed file, else dropped (`:525-537`) |
| `import` | PowerShell module import; `extract` never writes it |
| `error` | a note naming `file` and `message` only (`:297-301`) |

## 3. What `extract` writes

Positions are 1-based `line` and `col` on every record. **Column unit:** the
PowerShell extractor reports `StartColumnNumber` of a string parsed from
UTF-8 text (`powershell-ast.ps1:87-90,96,119`), a .NET string position, so
UTF-16 code units; code-index's own LSP-side columns are UTF-16 (`edges.go:404`)
and it compares both kinds in `container()` (`edges.go:163-179`) without
converting. code-index's source does not state the unit for the extractor
side; this is read from the script, not from a comment. `extract` emits UTF-16
code units, which is also this repository's rule for printed positions
(`pos16` in `cmd/bash-lsp/main.go`). There is no difference to document for
ASCII; for text with astral characters (an emoji counts 2) both agree.
`TestExtractNonASCIIBeforeCallSite` pins this.

**Paths:** `file` (and every path-valued key) is absolute, in the
workspace's canonical spelling, native separators. code-index needs
absolute paths: `fileForAbs` does `filepath.Rel(root, abs)` and rejects
`..` (`edges.go:138-151`). The recorded PowerShell sample uses relative
paths but its test rewrites them to absolute before feeding the reader
(`internal/indexer/testdata/powershell-ast.sample.jsonl`;
`internal/indexer/edges_test.go:205`). The golden file for this repository
replaces the root with `<root>/` so it is the same on every machine.

Keys are written in this fixed order, each omitted when empty (the order is
not significant to code-index, which decodes by name; `TestExtractRawKeyOrder`
pins it): `t`, `file`, `line`, `col`, `endLine`, `endCol`, `name`, `alias`,
`target`, `path`, `expr`, `in`, `message`, `first`, `count`, `to`,
`resolution`, `reason`.

| `t` | keys written | from |
|---|---|---|
| `func` | `name`, `line`, `col`, `endLine`, `endCol` | every function declaration; span of the whole definition. Ignored by code-index (function symbols come from the LSP). |
| `alias` | `alias`, `target`, `line`, `col` (the alias name), `endLine`, `endCol`, `in` | every `alias` declaration with a literal target word |
| `call` | `name`, `to`, `resolution`, `reason`, `in`, `line`, `col` | a `RelCalls` edge that is `resolved` or `candidate`. `name` is the function name, also for a call through an alias (`reason` is then `alias`); `to` is the file of the definition the edge goes to. A function with several definitions in view gives one record each (different `to`). |
| `dynamic` | `expr`, `resolution` (`dynamic`), `reason`, `in`, `line`, `col` | a `RelCalls` edge with resolution `dynamic`; `expr` is the site text, whitespace collapsed, at most 160 characters (as `Clip`, `powershell-ast.ps1:31-35`). |
| `dotsource` | `path`, `expr`, `resolution`, `reason`, `in`, `line`, `col` | a `source` or `.` command. Resolved: `path` is the absolute target file, one record per target. Unresolved: `path` is the word when it is a plain literal (`./missing.sh`), else absent; `expr` is the word text; `resolution` is `unresolved`, `reason` `not-found`. |
| `invoke` | `path`, `expr`, `resolution`, `reason`, `in`, `line`, `col` | execution of a workspace file by literal path (`./tool.sh`). |
| `parse_error` | `count`, `first`, `line` 1, `col` 1 | a file with syntax errors; the rest of the file is still extracted from the recovered tree. |
| `error` | `message`, `line` 1, `col` 1 | a file the workspace listed but did not load (zsh, over the size cap, over the file-count limit). |

Choices where the PowerShell extractor gives no direct model:

- **Unresolved `dotsource`.** The PowerShell extractor writes `path` = the
  literal name, else the last string literal in the expression, else `''`,
  and `expr` = the clipped text (`powershell-ast.ps1:44-50,121-124`).
  `extract` does the same: a plain literal word goes to `path`; an
  expression (`"$DIR/x.sh"`) leaves `path` empty. code-index drops an empty
  `path` (`edges.go:551-553`). A literal `path` is tried relative to the
  importing file, then `path.Clean(lit)` relative to the root, each also
  case-insensitively (`edges.go:576-583`); it is dropped when none matches,
  but if code-index indexed a file bash-lsp does not (an extension the
  workspace skips, say), it could store an `exact` edge for a source
  bash-lsp calls unresolved.
- **Call-site column.** The PowerShell `call` and `dotsource` columns are those
  of the first command element (`powershell-ast.ps1:119`), after the `.` for a
  dot-source. For bash a `call` is at the command word and a `dotsource` at the
  path word (`source ./lib.sh` is at col 8), the same convention.
- **`invoke` is a new record type.** code-index has no edge kind for
  executing another file (its edge kinds are `calls`, `alias_of`, `imports`,
  `dot_sources`; `edges.go:22-26`). Writing it as `dotsource` would claim the
  callee shares the caller's functions and variables, which a child process
  does not. Writing it as `call` would need a function name. An unknown
  `t` is accepted and ignored (`edges.go:538-539`), so `invoke` is safe
  today and a patch can map it to a new edge kind.
- **What is never a `call`.** An unresolved edge, a builtin and an external
  command. A name match outside the sourced closure
  (`candidate`/`name-match-outside-closure`) is a `call` like any other
  candidate, one record per candidate definition, with its `resolution` and
  `reason` (spec 4.2 maps this reason to code-index confidence `possible`;
  AC-17: candidates are returned, not dropped); `TestExtractOutsideClosureCandidates`
  asserts it. It does not reach `orphan.sh` from `main.sh` on
  `testdata/corpus`: FDS-008 makes such an edge only for a call that resolves
  to no function in view, and every `build_release` call in `main.sh`
  resolves inside its closure; `TestExtractNoCallToOrphanFromMain` asserts
  that no call record goes into `orphan.sh` from another file (AC-03).
- **`candidate` calls are written as `call`.** code-index has no notion of
  candidate: it would store them as `exact` (`edges.go:519`). The
  `resolution` and `reason` keys carry the truth for any consumer that reads
  them. This applies to `call` only: a `RelSources` edge is `resolved` or
  `unresolved` (`analysis/edges.go:377,383`, also for glob-loop targets) and a
  `RelInvokes` edge is always `resolved` (`analysis/dynamic.go:897`); the
  `unset -f` relabelling to candidate touches only edges with a target
  function (`analysis/dynamic.go:587-589`). So `dotsource` and `invoke` never
  carry `candidate`, and code-index storing a resolvable `dotsource` as
  `exact` (`edges.go:537`) loses nothing.
- **Calls through an alias lose the `alias` confidence.** The PowerShell
  extractor writes `name` as the alias as written (`gw`, sample line 4);
  code-index finds it in `aliasTarget` and stores confidence `alias`
  (`edges.go:512-514`). `extract` writes `name` as the function (`reason`
  `alias` says how it was reached; golden `main.sh` line 7), so code-index
  takes the `pickFunc` path and stores `exact` (`edges.go:518-519`). See
  change 5.

## 4. How it was verified

- `TestExtractCorpusDecodes` and every other extract test decode each line
  with the copied `astRec` (no failure, `t` set, absolute `file`, 1-based
  `line`/`col`) and check the keys the reader needs per type.
- `TestExtractGolden` compares the output for `testdata/corpus` with
  `testdata/extract.golden.jsonl`. The test writes the golden file itself:
  `go test ./cmd/bash-lsp -run TestExtractGolden -update`.
- Determinism across runs and the sort order, a non-ASCII character before a
  call site, CRLF files, `--list-file` with a path outside the root, usage
  errors, and a failing writer are covered in the same file.
- Not done: feeding the output to code-index. That needs the changes below.

## 5. Gaps even after the extractor is accepted

- **Name-only resolution.** The `call` case looks the callee up by name
  (`pickFunc`, `edges.go:183-193`: a definition in the same file, else the
  first one). A name defined in two files, as `build_release` is in
  `lib/util.sh` and `orphan.sh`, can be bound to the wrong file. `extract`
  writes `to` for this; code-index does not read it yet (change 5 below).
- **Candidate calls become `exact`** (`edges.go:507-522`), as above.
- **Alias-reached calls become `exact`, not `alias`**: `extract` names the
  function, not the alias, so the `ConfAlias` branch (`edges.go:512-514`) is
  never taken (section 3). The alias symbol and its `alias_of` edge are still
  created from the `alias` record.
- **Function symbols come from bash-language-server** (`edges.go:90-95,130-133`),
  so `name` must match what that server reports for the function. For bash
  function names that is the declared word.
- **Alias targets.** `alias.target` is the first word of the alias value. An
  alias whose target is not a workspace function still gives an `alias`
  symbol and an `unresolved` `alias_of` edge (`edges.go:503-506`).

## 6. Changes code-index needs

The extractor is hardcoded to the name `powershell-ast`; no configuration
alone can plug in another one. Each item is a change the code-index owner
would make; none was made.

1. **Config validation**, `internal/config/config.go:237-239`: the check
   `l.Extractor != "powershell-ast"` rejects any other value. Accept a bash
   extractor name (for example `bash-lsp`). The `Language` struct
   (`config.go:35-47`) holds only `Extractor string`, and the config is
   decoded with `DisallowUnknownFields` (`config.go:85`), so naming a
   command and arguments needs new fields in that struct. The `bash` entry
   in `KnownServers` (`config.go:371`) has no extractor.
2. **Runner**, `internal/indexer/ast.go:99-150` (`defaultExtract`): line
   `100-102` returns `unknown extractor` for any name but
   `ExtractorPowerShellAST`. Add a branch that runs `bash-lsp extract`
   with `--root <root> --list-file <temp file>`: `ast.go:122-131` already
   writes the list file in the right form. The interpreter lookup
   (`ast.go:76-94`), the embedded script (`extractors/extractors.go:5-10`)
   and the `-File`/`-ListFile` command line (`ast.go:135-139`) are
   PowerShell-specific; the working directory is not set
   (`ast.go:139`) so `--root` must be passed. The 10-minute timeout
   (`ast.go:133`) and the stdout buffer (`ast.go:140-141`) can stay; a
   non-zero exit is fatal for the edge rebuild (`edges.go:286-287`), a missing
   binary should return `ErrExtractorUnavailable` as the missing `pwsh` does
   (`edges.go:284-285`).
3. **File filter**, `internal/indexer/edges.go:272-276`: only files ending
   in `.ps1` or `.psm1` are given to the extractor, whatever the language's
   `extensions` say. Select by the language's own extension list. As
   written, that list for bash includes `.zsh` (`config.go:371`), and `extract`
   answers each zsh file with an `error` record ("zsh is not supported yet"),
   which code-index turns into a note (`edges.go:297-301`): noisy, harmless.
   Avoid it by dropping `.zsh` from the files passed.
4. **Toolchain**: code-index's `go.mod` says `go 1.24.1` (line 3); this
   repository's says `go 1.26.0` (line 3). A code-index that imports
   bash-lsp as a library (AC-21) must raise its `go` line, and
   `extract` is built on `internal/workspace`, which a module outside this
   repository cannot import, so code-index can only run the binary.
5. **Use `to`, `resolution` and `reason`** (optional, for exactness):
   add the keys to `astRec` (`ast.go:36-52`); in the `call` case
   (`edges.go:507-522`) pick the symbol in the file `to` instead of
   `pickFunc`, map `candidate` to a confidence other than `exact` (the four
   constants at `edges.go:28-31` have no candidate value), and when
   `reason == "alias"` set `ConfAlias` so alias-reached calls keep the
   confidence the PowerShell path gives them.
6. **`invoke` records** (optional): add a case in `astEdges`
   (`edges.go:498-538`) and an edge kind next to `dot_sources`
   (`edges.go:22-26`, `internal/store/schema.sql:43`), or keep ignoring
   them.

**AC-14 and AC-21 stay unmet until items 1 to 4 are done in code-index.**
