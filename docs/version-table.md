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
| `transform-op` | 4.4 | fails (measured): `bad substitution`, exit 1 | `${v@Q}` only (the other `@` operators are not flagged, see below) | front |
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
`transform-op`, which is `${v@Q}` only). ShellCheck codes that report the same:
`SC3*` and `SC2113`.

An `sh` file is read with the POSIX grammar first. Only when that parse has
errors is it read again with the bash grammar, and that result is used if it
has fewer errors; so bash-only syntax in an sh file is one WB1302 finding
instead of a run of syntax errors, and a valid sh file gives the same tree,
symbols and findings as a POSIX-only parse. The second parse costs one more parse of the file, only for a file that needs
it: a bash-only hint (`[[`, `$'`, `coproc`, `source`) or an error in the POSIX
parse. Measured with `bash-lsp ast` (mean of 8 runs) on a valid 1.1 MB sh
file: 0.77 s with nothing to re-parse, 0.83 s with one `[[` in it, 0.84 s with
one error in it, against 0.74 s before the change (a bash-grammar parse
only). The 2 MiB and time budgets apply to each parse.

An sh shebang is `sh`, `dash`, `ash`, `busybox sh`, `busybox ash`, and the same
behind `env` (its options `-i`, `-v`, `-0`, `-C dir`, `-P path`, `-u name`,
`VAR=value`, and `-S` strings).

## When a finding is not reported

- **WB1301 only for bash files** (dialect `bash`). A file with an `sh`, `zsh`,
  `ksh`/`mksh` or bats dialect gets none. WB1302 only for `sh` files.
- **A version check silences the features after it, not the whole file.**
  A read of `BASH_VERSION` or `BASH_VERSINFO` in code that checks something
  (an `if ((...))`, a `[[ ... ]] || exit`, a `case`, arithmetic) silences every
  feature at or after the first such read, in source order. A check at the top
  level (outside every function) also silences the features inside every
  function body of the file, wherever the function is defined: `f() { declare
  -A m; }` above `((BASH_VERSINFO[0] >= 4)) || exit 1` is silent, as is a
  `main` called from `if ((BASH_VERSINFO[0] >= 4)); then main; fi`. Top-level
  code before the check is still reported. A check inside a function guards
  only what follows it in source order. Not a check: a comment, a
  single-quoted string, a heredoc body, a standalone argument of `echo`,
  `printf` or `log` (`echo "running on $BASH_VERSION"` only prints), and an
  assignment (`v=$BASH_VERSION`) unless the variable is later read in a test,
  arithmetic or a `case` word of the same file. A print piped into a command
  that decides an `||`, an `&&` or an `if` (`echo "$BASH_VERSION" | grep -q ^4
  || exit 1`) is a check.
- **Tolerated uses.** A feature is not reported when its command is the left
  side of `||` (`shopt -s globstar 2>/dev/null || true`) or sends standard
  error to `/dev/null` (`2>/dev/null`, `2>>/dev/null`, `&>/dev/null`). A test
  in front of a feature does **not** tolerate it: what a test checks cannot be
  told from a version probe, and treating every test as one hid real findings
  (`[ $# -gt 0 ] && declare -A m`, `if [ -f x ]; then declare -A m; fi`,
  `[[ -n $HOME ]] && shopt -s globstar`, `test -d /tmp && declare -A m`, a
  `case` arm, a whole `if [[ $x == y ]]` body, and `[ -n "$x" ] && shopt -s
  globstar`) and these are reported. The right side of `||` is reported too.
  Only a version variable guards.
- **`EPOCHSECONDS`/`EPOCHREALTIME` inside a test** (`[[ -z $EPOCHSECONDS ]]`,
  `[ "$EPOCHSECONDS" -gt 5 ]`, `test -n ...`, a `(( ))` condition) is how a
  script finds out whether it has the variable, so it is not reported.
- **Not flagged, with the reason** (no citable first version on this machine;
  each of these fails on 3.2.57 when run, which says nothing of the version
  that added it):
  - `[[ -v x ]]`: fails on 3.2.57 (`conditional binary operator expected`).
  - `mapfile` / `readarray`: see "Left out of the table".
  - `$BASHPID`: empty on 3.2.57.
  - bats files: a bats dialect gets neither rule.
  - the `@` operators other than `Q`, `${v@U}`, `${v@u}`, `${v@L}`,
    `${v@K}`, `${v@k}` (and `@a`, `@A`, `@E`, `@P`): FDS-016 names only
    `${v@Q}`, and no source on this machine dates the others. Each fails on
    3.2.57 with `bad substitution` (checked for `@U`, `@L`, `@u`, `@K`, `@k`,
    `@Q`). **Unverified:** `@U`, `@u`, `@L`, `@K`, `@k`. They are neither
    flagged by WB1301 nor reported by WB1302 until a source is cited.
- **The release gate is open.** FDS-016 requires each row to be confirmed
  against real bash 4.x and 5.x builds before release. Only 3.2.57 is
  installed here and installing another bash needs the network, so this needs
  the owner.
- **A directive after a command on the same line covers nothing** for WB1301
  and WB1302, as for every rule. ShellCheck 0.11.0 on a file with `echo $x #
  shellcheck disable=SC2086` prints `SC1126: Place shellcheck directives
  before commands, not after` (and `SC1073`, `SC1072` for the line), so it
  does not apply the directive to that line or the next. Write the directive
  alone on the line before the command.
- **Not detected as guards:** feature probes other than a test or `||`
  (`type mapfile`, `command -v`), version checks made through `$(bash
  --version)`, and a `BASH_VERSION` check kept in another file. Each is a
  possible false positive.
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
  them, or reads them with a default operator (`${EPOCHSECONDS:-...}`,
  `${EPOCHSECONDS+x}`).
- **Calls through wrappers** (`builtin shopt -s globstar`, `command wait -n`)
  and `shopt`/`wait` inside strings that are evaluated later are not seen.

## Known gaps (no code change)

- A mixed sh file where the POSIX and the bash parse have the same number of
  errors keeps the POSIX parse: its tree, its syntax errors and the symbols
  after the error are POSIX's, and the bash-only evidence is lost (no WB1302).
- A clean POSIX file whose bash parse has errors reports no WB1302: the
  features are taken only from a bash parse without errors.
- `select`, `time` and brace expansion (`{a,b}`) in an sh file are not in the
  WB1302 table.
- A byte order mark before the shebang makes the first line no shebang, so the
  file is classed bash.
- A version check in a sourced file does not guard the sourcing file (and the
  other way round).

## Target range

`bash.target` is a range `MIN-MAX`; a feature is reported when its first
version is above MIN (message: "associative arrays (`declare -A`) need bash
4.0; the target range starts at 3.2"). With no setting the target is `3.2-5.3`
(spec D-8), so the rule is on by default. `DiagOptions` with a zero `Target`
means the same default.

**Only MIN drives WB1301.** MAX is parsed and checked (MIN must not exceed
MAX) and shown in the settings, but no rule reads it. Neither the spec (FDS-004,
D-8) nor the task brief gives it a use; it is kept for rules about what newer
bash changes or removes, which do not exist yet. Narrowing MAX therefore does
not change any finding.