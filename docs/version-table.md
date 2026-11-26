# Bash version table (WB1301) and bash-only list (WB1302)

The first versions come from the bash NEWS file. **They are confirmed against
real builds.** `bench/version_probe.py <bash>` runs one fixed snippet per row under
`env -i PATH=/usr/bin:/bin <bash> --noprofile --norc` and records the result in
`bench/results/version-probe-<v>.json`. It was run on `/bin/bash` 3.2.57 and
on builds of bash 4.0, 4.2, 4.3, 4.4, 5.0, 5.1 and 5.3 made from the
ftp.gnu.org tarballs (bash 4.1 and 5.2 were not built). In all eight runs every
row fails below its first version and passes from it
(`TestTableMatchesProbeResults` re-checks this against the result files). One
row was wrong and is fixed: `negative-subscript` was 4.3 and is
4.2 (it passes on 4.2.0 and fails on 4.0 and 3.2.57; there is no 4.1 build, so
4.1 itself is not measured).

The table is data: `analysis.FeatureTable()` in `analysis/versionrules.go`.
The vendored parser carries no per-feature version notes, so none is used.

## Version rows

| Key | First bash | Confirmed | On 3.2.57 | What is detected | Where |
|---|---|---|---|---|---|
| `assoc-array` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): `declare: -A: invalid option`, exit 2 | a declaration with `-A` (`declare`, `typeset`, `local`, `readonly`, `export`) | analysis (`VarAttrs.Assoc`) |
| `case-modification` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): `bad substitution`, exit 1 | `${v^}`, `${v^^}`, `${v,}`, `${v,,}` | front |
| `coproc` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): syntax error for `coproc { :; }`, exit 2 | the `coproc` keyword | front |
| `globstar` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): `invalid shell option name`, exit 1 | `shopt -s globstar` (not `-u`, `-q`) | analysis (command) |
| `case-fallthrough` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): syntax error for `;&` and for `;;&`, exit 2 | `;&` and `;;&` | front |
| `pipe-all` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): syntax error, exit 2 | `\|&` | front |
| `append-all` | 4.0 | fails on 3.2.57; passes on 4.0 to 5.3 | fails (measured): syntax error, exit 2 | `&>>` | front |
| `declare-g` | 4.2 | fails on 3.2.57 to 4.0; passes on 4.2 to 5.3 | fails (measured): `declare: -g: invalid option`, exit 2 | `declare`, `typeset`, `local` with `-g` | analysis (`VarAttrs.Global`) |
| `lastpipe` | 4.2 | fails on 3.2.57 to 4.0; passes on 4.2 to 5.3 | fails (measured): `invalid shell option name`, exit 1 | `shopt -s lastpipe` | analysis (command) |
| `nameref` | 4.3 | fails on 3.2.57 to 4.2; passes on 4.3 to 5.3 | fails (measured): `declare: -n: invalid option`, exit 2 | `declare`, `typeset`, `local` with `-n` | analysis (`VarAttrs.Nameref`) |
| `wait-n` | 4.3 | fails on 3.2.57 to 4.2; passes on 4.3 to 5.3 | fails (measured): `wait: -n: invalid option`, exit 2 | `wait` with an option word that holds `n` | analysis (command) |
| `negative-subscript` | 4.2 | fails on 3.2.57 to 4.0; passes on 4.2 to 5.3 | fails (measured): `bad array subscript`, exit 1 (the probe asserts the value) | a subscript that is `-` and an integer literal, in `${a[-1]}`, `a[-1]=x`, `$((a[-1]))` | front |
| `transform-op` | 4.4 | fails on 3.2.57 to 4.3; passes on 4.4 to 5.3 | fails (measured): `bad substitution`, exit 1 | `${v@Q}` only (the other `@` operators are not flagged, see below) | front |
| `epochseconds` | 5.0 | fails on 3.2.57 to 4.4; passes on 5.0 to 5.3 | fails (measured): the variable is unset (exit 1 from `[ -n "${EPOCHSECONDS-}" ]`) | a read of `EPOCHSECONDS` or `EPOCHREALTIME` | analysis (variable read) |
| `wait-p` | 5.1 | fails on 3.2.57 to 5.0; passes on 5.1 to 5.3 | fails (measured): `wait: -p: invalid option`, exit 2 | `wait` with an option word that holds `p` | analysis (command) |

Each row fails below its first version and passes from it on every build
probed. The "On 3.2.57" column keeps the exact message.

## Left out of the table

- **`mapfile` / `readarray`.** The sources conflict on their first version.
  The probe shows
  `mapfile` fails on 3.2.57 (`command not found`, exit 127), so a script that
  uses it does not run on 3.2, but WB1301 does not name a version for it. The
  probe (`mapfile -t a </dev/null`) passes on every build from 4.0 up, which
  would agree with a first version of 4.0; it stays out until the conflicting
  source is settled.

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
- **A version check silences only the code that runs after it, not the whole
  file.** A read of `BASH_VERSION` or `BASH_VERSINFO` in code that checks
  something (an `if ((...))`, a `[[ ... ]] || exit`, a `case`, arithmetic) is a
  check. What it covers:
  - At the top level (outside every function), the top-level code after it,
    and the features inside every function body of the file, wherever the
    function is defined or called: `f() { declare -A m; }` above
    `((BASH_VERSINFO[0] >= 4)) || exit 1` is silent, as is a `main` called
    from `if ((BASH_VERSINFO[0] >= 4)); then main; fi`. Top-level code before
    the check is still reported.
  - Inside a function, only what follows it in that function: in `g() {
    declare -A q; ((BASH_VERSINFO[0]>=4)) || exit 1; declare -A r; }; g`,
    `q` is reported and `r` is not; a function that holds a check and is
    never called guards only itself.
  - At a call of a function that holds a check, the calling scope from the
    call on (`check() { ... || exit 1; }; check` guards the code after
    `check`). The same holds for a wrapper: a function that calls a guard
    function as a statement of its own body, or as the first operand of an
    `&&`/`||` list there (`g || return 1`), and not inside an `if` or a loop,
    is itself a guard function, through any number of wrappers
    (`helper() { g; }; helper; declare -A m` is silent). A wrapper's own
    statements before its call of the guard function are reported.
  - A guard function called at the top level also covers every other
    function body, wherever defined or called, the same rule as for a check
    at the top level: `f() { declare -A m; }; h() { <check> }; f; h` is
    silent although `f` runs before `h`. It does not cover the guard
    function's own body or the bodies of the wrappers its guard comes
    through: those are covered from their own check or call on. A function
    the guard does not have to pass through stays covered: in `main() { req;
    maybe; }; main`, the body of `maybe` is covered by `req`'s check even if
    `maybe` holds a check of its own.

  Not a check: a comment, a
  single-quoted string, a heredoc body, a standalone argument of `echo`,
  `printf` or `log` (`echo "running on $BASH_VERSION"` only prints), and an
  assignment (`v=$BASH_VERSION`) unless the variable is later read in a test,
  arithmetic or a `case` word of the same file. A print piped into a command
  that decides an `||`, an `&&` or an `if` (`echo "$BASH_VERSION" | grep -q ^4
  || exit 1`) is a check.
- **A check that compares the major version with an integer** guards only the
  features that major version has: `[ "${BASH_VERSINFO[0]}" -ge 3 ] || exit 1`
  does not guard `declare -A` (4.0), `-ge 4` does. Decided for one mention of
  `BASH_VERSINFO[0]`, `$BASH_VERSINFO` or `${BASH_VERSION%%.*}` in a test or
  arithmetic expression, compared with a plain decimal number (no leading
  zero) by `-ge`, `-gt`, `>=`, `>`; by `-eq`, `==`, `=` only where the code
  after it runs when the test holds (a test, not negated, that is the
  condition of an `if` without `else` or `elif` whose body does not exit or
  return, of a `while`, or the left side of `||`); or by `-lt`,
  `-le`, `<`, `<=` when the statement also exits or returns. Every other form
  (minor or patch numbers, string comparison, `case`, several mentions, a
  number in another base such as `0x4`, `010` or `2#100`, an expansion such as
  `$min`, `[[ ${BASH_VERSINFO[0]} == 3 ]] && exit 1` and `! [[ ... == 3 ]] ||
  exit 1`) silences everything
  after it, as before; a major check also silences features of a later minor
  of the same major (`>= 4` silences `declare -n`, 4.3).
- **Known gaps of the guard.** A top-level version test used only to choose a
  message, and `echo "$BASH_VERSION" | tee log || exit 1`, count as guards and
  hide later features. `[ "$(echo "$BASH_VERSION" | cut -d. -f1)" -ge 4 ] ||
  exit 1` is not recognised as a guard (the read is a print inside a command
  substitution), so the features after it are reported. A guard function run
  in a subshell or by `trap` (`x=$(g)`, `( g )`, `trap g EXIT`), or called
  inside an `if` at the top level, counts as a guard although it does not stop
  the caller. Functions of the same name are merged: a check in any
  definition counts for every call of that name. The search for whether a
  top-level guard call has to pass through a function has a step budget per
  file (proportional to its calls); past it, in a long chain of wrappers that
  each use a feature before their call, a top-level guard call covers the
  remaining function bodies. A guard in a sourced file
  does not guard the sourcing file (see "Known gaps").
  Three more gaps, all missed reports: a wrapper's early feature stays
  hidden when a sibling wrapper calls the same guard function
  (`g() { <check>; }; w1() { declare -A a; g; }; w2() { g; }; w1; w2` is
  silent on `a`); a number with a leading zero in a major check
  (`[ "${BASH_VERSINFO[0]}" -ge 03 ] || exit 1`, which `[` reads as 3) is
  undecided and silences everything after it; and the word `exit` inside a
  string or an argument counts as an exit when a check is read
  (`... || echo "no exit"`).
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
    `${v@K}`, `${v@k}` (and `@a`, `@A`, `@E`, `@P`): only `${v@Q}`
    has a cited first version, and no source dates the others. Each fails on
    3.2.57 with `bad substitution` (checked for `@U`, `@L`, `@u`, `@K`, `@k`,
    `@Q`). **Unverified:** `@U`, `@u`, `@L`, `@K`, `@k`. They are neither
    flagged by WB1301 nor reported by WB1302 until a source is cited.
- **Two bash versions were not built.** Bash 4.1 and 5.2 were not built, so a
  first version is confirmed against the builds on either side of it, not
  against every release. The signatures of the downloaded source tarballs
  were not verified.
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
so the rule is on by default. `DiagOptions` with a zero `Target`
means the same default.

**Only MIN drives WB1301.** MAX is parsed and checked (MIN must not exceed
MAX) and shown in the settings, but no rule reads it. It is kept for rules
about what newer bash changes or removes, which do not exist yet. Narrowing MAX therefore does
not change any finding.