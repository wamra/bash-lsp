# Bash version table (WB1301) and bash-only list (WB1302)

Source of every first version: FDS-016 in
`docs/internal/specs/2026-11-22-bash-lsp-brd-fds-v0.2.md`, which takes the
table from the bash NEWS file. **The first versions are unconfirmed.** The
only bash on the development machine is 3.2.57, so each row is confirmed in
one direction only: the feature fails there
(`bench/results/version-probe-m3.json`, from `bench/version_probe.py
/bin/bash`; `$BASH_VERSION` in the file is `3.2.57(1)-release`). Whether a
feature first appears in the version named is not tested. The FDS-016 release
gate (each row confirmed against a real bash of that series) is still open
(plan Decision 15).

The table is data: `analysis.FeatureTable()` in `analysis/versionrules.go`.
The vendored parser carries no per-feature version notes, so none is used.

## Version rows

| Key | First bash | On 3.2.57 | What is detected | Where |
|---|---|---|---|---|
| `assoc-array` | 4.0 | fails (measured): `declare: -A: invalid option`, exit 2 | a declaration with `-A` (`declare`, `typeset`, `local`, `readonly`, `export`) | analysis (`VarAttrs.Assoc`) |
| `case-modification` | 4.0 | fails (measured): `bad substitution`, exit 1 | `${v^}`, `${v^^}`, `${v,}`, `${v,,}` | front |
| `coproc` | 4.0 | fails (measured): syntax error for `coproc { :; }`, exit 2 | the `coproc` keyword | front |
| `globstar` | 4.0 | fails (measured): `invalid shell option name`, exit 1 | `shopt -s globstar` (not `-u`, `-q`) | analysis (command) |
| `case-fallthrough` | 4.0 | fails (measured): syntax error for `;&` and for `;;&`, exit 2 | `;&` and `;;&` | front |
| `pipe-all` | 4.0 | fails (measured): syntax error, exit 2 | `\|&` | front |
| `append-all` | 4.0 | fails (measured): syntax error, exit 2 | `&>>` | front |
| `declare-g` | 4.2 | fails (measured): `declare: -g: invalid option`, exit 2 | `declare`, `typeset`, `local` with `-g` | analysis (`VarAttrs.Global`) |
| `lastpipe` | 4.2 | fails (measured): `invalid shell option name`, exit 1 | `shopt -s lastpipe` | analysis (command) |
| `nameref` | 4.3 | fails (measured): `declare: -n: invalid option`, exit 2 | `declare`, `typeset`, `local` with `-n` | analysis (`VarAttrs.Nameref`) |
| `wait-n` | 4.3 | fails (measured): `wait: -n: invalid option`, exit 2 | `wait` with an option word that holds `n` | analysis (command) |
| `negative-subscript` | 4.3 | fails (measured): `bad array subscript`, exit 1 (the probe asserts the value) | a subscript that is `-` and an integer literal, in `${a[-1]}`, `a[-1]=x`, `$((a[-1]))` | front |
| `transform-op` | 4.4 | fails (measured): `bad substitution`, exit 1 | `${v@Q}` and the other `@` operators | front |
| `epochseconds` | 5.0 | fails (measured): the variable is unset (exit 1 from `[ -n "${EPOCHSECONDS-}" ]`) | a read of `EPOCHSECONDS` or `EPOCHREALTIME` | analysis (variable read) |
| `wait-p` | 5.1 | fails (measured): `wait: -p: invalid option`, exit 2 | `wait` with an option word that holds `p` | analysis (command) |

Each row reads "fails on 3.2.57 (measured); first version per FDS-016,
unconfirmed".

## Left out of the table

- **`mapfile` / `readarray`.** FDS-016 records "one conflicting source" for
  their first version, and nothing on this machine settles it. The probe shows
  `mapfile` fails on 3.2.57 (`command not found`, exit 127), so a script that
  uses it does not run on 3.2, but WB1301 does not name a version for it. A
  row is added when a real bash of the series it names has been run.

## Bash-only syntax (WB1302)

Reported in a file whose dialect is `sh` (shebang `sh`, `dash`, `ash`, or
`# shellcheck shell=sh`). Bash 3.2 has all of these, so they carry no version
(the probe's `control:` rows pass on 3.2.57):

| Key | Construct |
|---|---|
| `double-bracket` | `[[` |
| `function-keyword` | `function f` |
| `array-assign` | `a=(1 2)` |
| `ansi-c-quote` | `$'...'` |
| `herestring` | `<<<` |
| `redirect-all` | `&>` |
| `process-subst` | `<(` and `>(` |
| `source-builtin` | `source` (instead of `.`) |

plus every syntactic key of the version rows (`case-modification`, `coproc`,
`case-fallthrough`, `pipe-all`, `append-all`, `negative-subscript`,
`transform-op`). ShellCheck codes that report the same: `SC3*` and `SC2113`.

## When a finding is not reported

- **WB1301 only for bash files** (dialect `bash`). A file with an `sh`, `zsh`,
  `ksh`/`mksh` or bats dialect gets none. WB1302 only for `sh` files.
- **A version check silences WB1301 for the whole file.** If the text mentions
  `BASH_VERSION` or `BASH_VERSINFO` anywhere (an `if`, `[[ ]] &&`, a `case`, an
  early `exit` before the feature, even a comment), the file is taken to guard
  its newer features. This also misses a real unguarded feature in a file that
  checks the version for another reason.
- **Not detected as guards:** feature probes (`declare -A x 2>/dev/null ||
  fallback`, `type mapfile`, `command -v`), `[ -n "${BASH_VERSION}" ]` kept in
  another file, and checks made through `$(bash --version)`. Each is a possible
  false positive.
- **A sourced file with no shebang** is silent for both rules: it runs in the
  shell that sources it.
- **Strings, heredocs, comments.** Syntax is recorded from parser tokens, so
  `echo 'declare -A m'`, a quoted heredoc and a comment give nothing. An
  expansion inside an unquoted heredoc or a double-quoted string is real
  (`"${v,,}"` is a finding).
- **Negative subscripts of associative arrays** (`m[-1]` is a string key) are
  not reported when the array is declared `-A` in the file or the name binds to
  such a declaration in a sourced file. An array declared where the analysis
  cannot see it is reported as 4.3.
- **`EPOCHSECONDS`/`EPOCHREALTIME`** are not reported when the script assigns
  them, or reads them with a fallback or test operator (`${EPOCHSECONDS:-...}`,
  `${EPOCHSECONDS+x}`).
- **Calls through wrappers** (`builtin shopt -s globstar`, `command wait -n`)
  and `shopt`/`wait` inside strings that are evaluated later are not seen.

## Target range

`bash.target` is a range `MIN-MAX`; a feature is reported when its first
version is above MIN (message: "associative arrays (`declare -A`) need bash
4.0; the target range starts at 3.2"). With no setting the target is `3.2-5.3`
(spec D-8), so the rule is on by default. `DiagOptions` with a zero `Target`
means the same default.
