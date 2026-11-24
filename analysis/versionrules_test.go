package analysis

import (
	"path/filepath"
	"strings"
	"testing"
)

// diagsWith returns the findings of file in a workspace of files, with the
// target range given as "MIN-MAX" ("" leaves DiagOptions at its zero value).
func diagsWith(t *testing.T, files map[string]string, file, target string) []Diagnostic {
	t.Helper()
	var opt DiagOptions
	if target != "" {
		r, err := ParseVersionRange(target)
		if err != nil {
			t.Fatal(err)
		}
		opt.Target = r
	}
	return diagWorld(files, Options{}).Diagnostics(filepath.Join(diagRoot, file), opt)
}

func oneFile(src string) map[string]string { return map[string]string{"x.sh": src} }

// AC-10: `declare -A m` against the range 3.2-5.3 is one WB1301 that names
// bash 4.0, in the documented message form.
func TestAC10(t *testing.T) {
	src := "declare -A m\n"
	ds := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301")
	if len(ds) != 1 {
		t.Fatalf("%d findings:\n%s", len(ds), describe(src, ds))
	}
	const want = "associative arrays (`declare -A`) need bash 4.0; the target range starts at 3.2"
	if ds[0].Message != want || ds[0].Severity != SevWarning || ds[0].Origin != "bash-lsp" {
		t.Fatalf("%+v", ds[0])
	}
	if got := rangeText(src, ds[0].Range); got != "declare -A" {
		t.Fatalf("range text %q", got)
	}
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "4.0-5.3"), "WB1301"); len(got) != 0 {
		t.Fatalf("target 4.0-5.3: %s", describe(src, got))
	}
	// Two names in one declaration are still one finding; so is a local.
	for _, s := range []string{"declare -A a b\n", "f() { local -A m; }\n", "typeset -A m=([k]=v)\n"} {
		if got := ofRule(diagsWith(t, oneFile(s), "x.sh", "3.2-5.3"), "WB1301"); len(got) != 1 {
			t.Errorf("%q: %d findings:\n%s", s, len(got), describe(s, got))
		}
	}
}

// With no target given (a zero DiagOptions) the default range of D-8 applies:
// 3.2-5.3, so the rule is on, as in AC-10.
func TestDefaultTargetIsD8(t *testing.T) {
	src := "declare -A m\n"
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", ""), "WB1301"); len(got) != 1 {
		t.Fatalf("zero DiagOptions: %s", describe(src, got))
	}
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", DefaultTarget.String()), "WB1301"); len(got) != 1 {
		t.Fatalf("DefaultTarget: %s", describe(src, got))
	}
}

// The version table: one row per key, the first version it names, and the
// range text of the finding. Each is a trigger with the range 3.2-5.3 and
// silent when the target starts at that version.
var versionCases = []struct {
	src, text string
	min       string
	label     string
}{
	{"echo \"${v,,}\"\n", "${v,,}", "4.0", "case-modification expansions (`${v^^}`, `${v,,}`)"},
	{"echo \"${v^^}\"\n", "${v^^}", "4.0", ""},
	{"coproc cat\n", "coproc", "4.0", "coprocesses (`coproc`)"},
	{"case x in a) :;& b) :;; esac\n", ";&", "4.0", "case fallthrough terminators (`;&`, `;;&`)"},
	{"case x in a) :;;& b) :;; esac\n", ";;&", "4.0", ""},
	{"a |& b\n", "|&", "4.0", "pipes of both output streams (`|&`)"},
	{"a &>> f\n", "&>>", "4.0", "appending redirections of both output streams (`&>>`)"},
	{"shopt -s globstar\n", "shopt -s globstar", "4.0", "globstar patterns (`shopt -s globstar`)"},
	{"f() { declare -g x=1; }\n", "declare -g", "4.2", "global declarations (`declare -g`)"},
	{"shopt -s lastpipe\n", "shopt -s lastpipe", "4.2", "lastpipe settings (`shopt -s lastpipe`)"},
	{"declare -n r=x\n", "declare -n", "4.3", "namerefs (`declare -n`)"},
	{"sleep 0 & wait -n\n", "wait -n", "4.3", "waiting for any job (`wait -n`)"},
	{"echo \"${a[-1]}\"\n", "${a[-1]}", "4.3", "negative array subscripts (`${a[-1]}`)"},
	{"echo \"${v@Q}\"\n", "${v@Q}", "4.4", "parameter transformations (`${v@Q}`)"},
	{"echo \"$EPOCHSECONDS\"\n", "$EPOCHSECONDS", "5.0", "the EPOCHSECONDS and EPOCHREALTIME variables (`$EPOCHSECONDS`)"},
	{"echo \"${EPOCHREALTIME}\"\n", "${EPOCHREALTIME}", "5.0", ""},
	{"sleep 0 & wait -p v\n", "wait -p", "5.1", "job ids captured by wait (`wait -p`)"},
}

func TestWB1301Table(t *testing.T) {
	for _, c := range versionCases {
		t.Run(strings.TrimSpace(c.src), func(t *testing.T) {
			min, err := ParseVersionRange(c.min + "-5.3")
			if err != nil {
				t.Fatal(err)
			}
			ds := ofRule(diagsWith(t, oneFile(c.src), "x.sh", "3.2-5.3"), "WB1301")
			if len(ds) != 1 {
				t.Fatalf("%d findings:\n%s", len(ds), describe(c.src, ds))
			}
			if got := rangeText(c.src, ds[0].Range); got != c.text {
				t.Fatalf("range text %q, want %q", got, c.text)
			}
			if c.label != "" {
				want := c.label + " need bash " + c.min + "; the target range starts at 3.2"
				if ds[0].Message != want {
					t.Fatalf("message %q, want %q", ds[0].Message, want)
				}
			} else if !strings.Contains(ds[0].Message, "need bash "+c.min+";") {
				t.Fatalf("message %q", ds[0].Message)
			}
			if got := ofRule(diagsWith(t, oneFile(c.src), "x.sh", min.String()), "WB1301"); len(got) != 0 {
				t.Fatalf("target %s: %s", min, describe(c.src, got))
			}
			// A target that starts between 3.2 and the version still flags it.
			if got := ofRule(diagsWith(t, oneFile(c.src), "x.sh", "3.9-5.3"), "WB1301"); len(got) != 1 {
				t.Fatalf("target 3.9: %s", describe(c.src, got))
			}
		})
	}
}

// Every version in the table has a key and a name, and the keys are the
// ones this file tests.
func TestFeatureTable(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range FeatureTable() {
		if f.Key == "" || f.Name == "" || seen[f.Key] {
			t.Errorf("bad row %+v", f)
		}
		seen[f.Key] = true
		if f.MinBash != (BashVersion{}) && !(BashVersion{3, 2}).Less(f.MinBash) {
			t.Errorf("%s: first version %s is not above 3.2", f.Key, f.MinBash)
		}
	}
	for _, key := range []string{"assoc-array", "case-modification", "coproc", "globstar", "case-fallthrough", "pipe-all",
		"append-all", "declare-g", "lastpipe", "nameref", "wait-n", "negative-subscript", "transform-op", "epochseconds", "wait-p",
		"double-bracket", "function-keyword", "array-assign", "ansi-c-quote", "herestring", "redirect-all", "process-subst", "source-builtin"} {
		if !seen[key] {
			t.Errorf("no row for %s", key)
		}
	}
	// FDS-016 records one conflicting source for the first version of mapfile:
	// it is left out until a real bash of that series confirms it.
	if seen["mapfile"] {
		t.Error("mapfile has a row")
	}
	for _, src := range []string{"mapfile -t a < f\n", "readarray -t a < f\n"} {
		if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301"); len(got) != 0 {
			t.Errorf("%q: %s", src, describe(src, got))
		}
	}
}

// What must stay silent for WB1301: a version guard, a target at the
// version, another dialect, text that is not code, and uses that need
// nothing newer.
func TestWB1301Silent(t *testing.T) {
	const v3 = "3.2-5.3"
	silent := []struct{ name, src string }{
		{"BASH_VERSINFO in an if", "if ((BASH_VERSINFO[0] >= 4)); then declare -A m; fi\n"},
		{"BASH_VERSION in [[ ]] and &&", "[[ ${BASH_VERSION%%.*} -ge 4 ]] && declare -A m\n"},
		{"early exit on an old bash", "if [ \"${BASH_VERSINFO[0]}\" -lt 4 ]; then echo 'bash 4 needed' >&2; exit 1; fi\ndeclare -A m\necho \"${v,,}\"\n"},
		{"version case", "case $BASH_VERSION in 3.*) fallback;; *) shopt -s globstar;; esac\n"},
		{"in a string", "echo 'declare -A m'\necho \"declare -A m\"\necho 'shopt -s globstar ${v,,} ${a[-1]} $EPOCHSECONDS'\n"},
		{"in a comment", "# declare -A m ${v,,} coproc\n:\n"},
		{"quoted heredoc", "cat <<'EOF'\ndeclare -A m\n${v,,}\n${a[-1]}\nshopt -s globstar\nEOF\n"},
		{"eval string is text", "x='declare -A m'\necho \"$x\"\n"},
		{"EPOCHSECONDS with a fallback", "now=${EPOCHSECONDS:-$(date +%s)}\necho \"$now\"\n"},
		{"EPOCHSECONDS tested", "echo \"${EPOCHSECONDS+x}\" \"${EPOCHSECONDS-}\" \"${EPOCHSECONDS:=1}\"\n"},
		{"own EPOCHSECONDS", "EPOCHSECONDS=$(date +%s)\necho \"$EPOCHSECONDS\"\n"},
		{"own local EPOCHREALTIME", "f() { local EPOCHREALTIME=1; echo \"$EPOCHREALTIME\"; }\nf\n"},
		{"shopt -u and -q", "shopt -u globstar\nshopt -q lastpipe\nshopt -s extglob nullglob\n"},
		{"plain wait", "sleep 0 &\nwait\nwait %1\nwait $!\n"},
		{"declare without the newer flags", "declare -a a\ndeclare -x x=1\ndeclare -i n\nlocal -r r=1\ndeclare -p\ndeclare -f f\n"},
		{"plain expansions", "echo ${v:-x} ${v#a} ${v%b} ${v//a/b} ${a[1]} ${a[i]} ${a[@]} ${#a[@]}\n"},
		{"export -n is not a nameref", "export -n x\n"},
		{"function named like a command", "wait() { :; }\ncoproc_start() { :; }\n"},
	}
	for _, c := range silent {
		t.Run(c.name, func(t *testing.T) {
			if got := ofRule(diagsWith(t, oneFile(c.src), "x.sh", v3), "WB1301"); len(got) != 0 {
				t.Fatalf("findings:\n%s", describe(c.src, got))
			}
		})
	}
	// A string key of an associative array that looks like a negative
	// subscript is not one: only 4.0 is needed.
	src := "declare -A m\nm[-1]=x\necho \"${m[-1]}\" \"$((m[-2]))\"\n"
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "4.0-5.3"), "WB1301"); len(got) != 0 {
		t.Fatalf("assoc key:\n%s", describe(src, got))
	}
	// The same text on an indexed array needs 4.3.
	src = "declare -a m\necho \"${m[-1]}\"\n"
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "4.0-5.3"), "WB1301"); len(got) != 1 {
		t.Fatalf("indexed array:\n%s", describe(src, got))
	}
	// An associative array declared in a sourced library is found through the binding.
	files := map[string]string{
		"main.sh": "#!/bin/bash\nsource ./lib.sh\necho \"${m[-1]}\"\n",
		"lib.sh":  "#!/bin/bash\ndeclare -A m\n",
	}
	if got := ofRule(diagsWith(t, files, "main.sh", "4.0-5.3"), "WB1301"); len(got) != 0 {
		t.Fatalf("assoc key through a source:\n%s", describe(files["main.sh"], got))
	}
}

// Other dialects, and sourced libraries without a shebang.
func TestWB1301OtherFiles(t *testing.T) {
	const body = "declare -A m\necho \"${v,,}\"\nshopt -s globstar\n"
	silent := map[string]map[string]string{
		"sh shebang":     {"x.sh": "#!/bin/sh\n" + body},
		"dash shebang":   {"x.sh": "#!/bin/dash\n" + body},
		"zsh shebang":    {"x.sh": "#!/bin/zsh\n" + body},
		"ksh shebang":    {"x.sh": "#!/usr/bin/env ksh\n" + body},
		"mksh extension": {"x.mksh": body},
		"bats":           {"x.bats": "#!/usr/bin/env bats\n@test \"t\" {\n" + body + "}\n"},
		"directive sh":   {"x.sh": "# shellcheck shell=sh\n" + body},
		"sourced library without a shebang": {
			"x.sh": body, "main.sh": "#!/bin/bash\nsource ./x.sh\n"},
	}
	for name, files := range silent {
		t.Run(name, func(t *testing.T) {
			file := "x.sh"
			for f := range files {
				if strings.HasPrefix(f, "x.") {
					file = f
				}
			}
			if got := ofRule(diagsWith(t, files, file, "3.2-5.3"), "WB1301"); len(got) != 0 {
				t.Fatalf("findings:\n%s", describe(files[file], got))
			}
		})
	}
	// The same body in bash files is flagged: with a bash shebang, with none,
	// and in a sourced file that has its own bash shebang.
	flagged := map[string]map[string]string{
		"bash shebang":                {"x.sh": "#!/bin/bash\n" + body},
		"env bash":                    {"x.sh": "#!/usr/bin/env bash\n" + body},
		"no shebang, nothing sources": {"x.sh": body},
		"sourced, with a shebang":     {"x.sh": "#!/bin/bash\n" + body, "main.sh": "#!/bin/bash\nsource ./x.sh\n"},
		"directive bash":              {"x.sh": "#!/bin/sh\n# shellcheck shell=bash\n" + body},
	}
	for name, files := range flagged {
		t.Run(name, func(t *testing.T) {
			if got := ofRule(diagsWith(t, files, "x.sh", "3.2-5.3"), "WB1301"); len(got) != 3 {
				t.Fatalf("%d findings, want 3:\n%s", len(got), describe(files["x.sh"], got))
			}
		})
	}
}

func TestWB1301Suppression(t *testing.T) {
	src := "#!/bin/bash\necho start\n# bash-lsp disable=WB1301\ndeclare -A m\ndeclare -A n\n"
	got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301")
	if len(got) != 1 || got[0].Range.Start.Line != 5 {
		t.Fatalf("directive covers the next command only:\n%s", describe(src, got))
	}
	src = "#!/bin/bash\n# bash-lsp disable=WB1301\ndeclare -A m\ndeclare -A n\n"
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1301"); len(got) != 0 {
		t.Fatalf("a directive before the first command is file-wide:\n%s", describe(src, got))
	}
	opt := DiagOptions{Target: DefaultTarget, Disabled: map[string]bool{"WB1301": true}}
	s := diagWorld(oneFile("declare -A m\n"), Options{})
	if got := ofRule(s.Diagnostics(filepath.Join(diagRoot, "x.sh"), opt), "WB1301"); len(got) != 0 {
		t.Fatalf("diagnostics.disable: %v", got)
	}
	opt = DiagOptions{Target: DefaultTarget, Severity: map[string]Severity{"WB1301": SevError}}
	if got := ofRule(s.Diagnostics(filepath.Join(diagRoot, "x.sh"), opt), "WB1301"); len(got) != 1 || got[0].Severity != SevError {
		t.Fatalf("severity setting: %v", got)
	}
}

// WB1302: each bash-only key in an sh file, and the same under bash.
var dialectCases = []struct{ src, text string }{
	{"[[ -n $x ]]\n", "[["},
	{"function f { :; }\n", "function f"},
	{"a=(1 2)\n", "a=(1 2)"},
	{"echo $'x'\n", "$'x'"},
	{"cat <<< hi\n", "<<<"},
	{"a &> f\n", "&>"},
	{"a &>> f\n", "&>>"},
	{"cat <(echo hi)\n", "<("},
	{"source ./lib.sh\n", "source"},
	{"echo \"${v,,}\"\n", "${v,,}"},
	{"case x in a) :;& b) :;; esac\n", ";&"},
	{"a |& b\n", "|&"},
	{"coproc cat\n", "coproc"},
	{"echo \"${v@Q}\"\n", "${v@Q}"},
	{"echo \"${a[-1]}\"\n", "${a[-1]}"},
}

func TestWB1302Table(t *testing.T) {
	for _, c := range dialectCases {
		t.Run(strings.TrimSpace(c.src), func(t *testing.T) {
			src := "#!/bin/sh\n" + c.src
			ds := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), "WB1302")
			if len(ds) != 1 {
				t.Fatalf("%d findings:\n%s", len(ds), describe(src, ds))
			}
			if got := rangeText(src, ds[0].Range); got != c.text {
				t.Fatalf("range text %q, want %q", got, c.text)
			}
			if ds[0].Severity != SevWarning || !strings.Contains(ds[0].Message, "bash only") || !strings.Contains(ds[0].Message, "sh script") {
				t.Fatalf("%+v", ds[0])
			}
			// The same under bash is not a dialect finding, and in an sh file
			// the target range plays no part.
			bash := "#!/bin/bash\n" + c.src
			if got := ofRule(diagsWith(t, oneFile(bash), "x.sh", "3.2-5.3"), "WB1302"); len(got) != 0 {
				t.Fatalf("bash: %s", describe(bash, got))
			}
			if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "5.0-5.3"), "WB1301"); len(got) != 0 {
				t.Fatalf("an sh file has no bash target: %s", describe(src, got))
			}
		})
	}
}

func TestWB1302Message(t *testing.T) {
	src := "#!/bin/sh\n[[ -n $x ]]\n"
	ds := ofRule(diagsWith(t, oneFile(src), "x.sh", ""), "WB1302")
	if len(ds) != 1 || ds[0].Message != "double-bracket tests (`[[ ]]`) are bash only; this file is an sh script" {
		t.Fatalf("%v", ds)
	}
}

func TestWB1302Silent(t *testing.T) {
	silent := []struct{ name, src string }{
		{"POSIX forms", "#!/bin/sh\n[ -n \"$x\" ] && echo hi\nf() { echo \"${x#a}\"; }\n. ./lib.sh\ncase x in a) :;; esac\necho $((1 + 2))\nx=$(echo hi)\ncat <<EOF\n$x\nEOF\n"},
		{"in strings", "#!/bin/sh\necho '[[ x ]] <<< a source ${v,,} <(a) $'\\''x'\\'' a |& b'\necho \"[[ x ]] source <(a) function f\"\n"},
		{"in comments", "#!/bin/sh\n# [[ -n $x ]] source ./x <<< a function f coproc\n:\n"},
		{"quoted heredoc", "#!/bin/sh\ncat <<'EOF'\n[[ -n $x ]]\nsource x\n<(a)\n${v,,}\nEOF\n"},
		{"source as an argument", "#!/bin/sh\necho source\ngrep source file\n"},
		{"declaration forms bash 3 has", "#!/bin/sh\nexport A=1\nreadonly B=2\nlocal_x=1\n"},
	}
	for _, c := range silent {
		t.Run(c.name, func(t *testing.T) {
			if got := ofRule(diagsWith(t, oneFile(c.src), "x.sh", "3.2-5.3"), "WB1302"); len(got) != 0 {
				t.Fatalf("findings:\n%s", describe(c.src, got))
			}
		})
	}
	body := "[[ -n $x ]]\nfunction f { :; }\nsource ./a\n"
	others := map[string]map[string]string{
		"bash":                {"x.sh": "#!/bin/bash\n" + body},
		"no shebang":          {"x.sh": body},
		"zsh":                 {"x.sh": "#!/bin/zsh\n" + body},
		"ksh":                 {"x.sh": "#!/bin/ksh\n" + body},
		"bats":                {"x.bats": "#!/usr/bin/env bats\n@test \"t\" {\n" + body + "}\n"},
		"directive bash":      {"x.sh": "#!/bin/sh\n# shellcheck shell=bash\n" + body},
		"sourced, no shebang": {"x.sh": "# shellcheck shell=sh\n" + body, "m.sh": "#!/bin/sh\n. ./x.sh\n"},
	}
	for name, files := range others {
		t.Run(name, func(t *testing.T) {
			file := "x.sh"
			if _, ok := files["x.bats"]; ok {
				file = "x.bats"
			}
			if got := ofRule(diagsWith(t, files, file, "3.2-5.3"), "WB1302"); len(got) != 0 {
				t.Fatalf("findings:\n%s", describe(files[file], got))
			}
		})
	}
	// An sh script that is sourced and has its own shebang is still sh.
	files := map[string]string{"x.sh": "#!/bin/sh\n" + body, "m.sh": "#!/bin/sh\n. ./x.sh\n"}
	if got := ofRule(diagsWith(t, files, "x.sh", "3.2-5.3"), "WB1302"); len(got) != 3 {
		t.Fatalf("%d findings, want 3", len(got))
	}
}

func TestWB1302Suppression(t *testing.T) {
	for dir, src := range map[string]string{
		"SC3010":        "#!/bin/sh\n# shellcheck disable=SC3010\n[[ -n $x ]]\n",
		"SC3000-SC3999": "#!/bin/sh\n# shellcheck disable=SC3000-SC3999\n[[ -n $x ]]\n",
		"SC2113":        "#!/bin/sh\n# shellcheck disable=SC2113\nfunction f { :; }\n",
		"bash-lsp":     "#!/bin/sh\n# bash-lsp disable=WB1302\n[[ -n $x ]]\n",
	} {
		if got := ofRule(diagsWith(t, oneFile(src), "x.sh", ""), "WB1302"); len(got) != 0 {
			t.Errorf("%s: %s", dir, describe(src, got))
		}
	}
	src := "#!/bin/sh\necho hi\n# shellcheck disable=SC2086\n[[ -n $x ]]\n"
	if got := ofRule(diagsWith(t, oneFile(src), "x.sh", ""), "WB1302"); len(got) != 1 {
		t.Errorf("an unrelated code silences it: %s", describe(src, got))
	}
}

// Merge drops the native finding when ShellCheck reports its mapped code on
// the same range.
func TestWB1302MergeWithShellCheck(t *testing.T) {
	src := "#!/bin/sh\n[[ -n $x ]]\n"
	native := ofRule(diagsWith(t, oneFile(src), "x.sh", ""), "WB1302")
	if len(native) != 1 {
		t.Fatal(native)
	}
	sc := []Diagnostic{{Rule: "SC3010", Severity: SevWarning, Range: native[0].Range, Origin: "shellcheck", Message: "x"}}
	if out := Merge(native, sc); len(out) != 1 || out[0].Origin != "shellcheck" {
		t.Fatalf("%+v", out)
	}
}

// A file that parses as sh but is read with the bash grammar gets no syntax
// error for its bash forms; they are WB1302 findings.
func TestShFileFindingsAreDialectNotSyntax(t *testing.T) {
	src := "#!/bin/sh\nf(){ :; }\n[[ -n $x ]]\ng(){ :; }\n"
	ds := diagsWith(t, oneFile(src), "x.sh", "")
	if got := ofRule(ds, "WB1001"); len(got) != 0 {
		t.Fatalf("syntax findings:\n%s", describe(src, ds))
	}
	if got := ofRule(ds, "WB1302"); len(got) != 1 {
		t.Fatalf("dialect findings:\n%s", describe(src, ds))
	}
}

// A feature inside an error gap is not evidence.
func TestVersionRulesIgnoreGaps(t *testing.T) {
	src := "#!/bin/bash\necho \"unclosed ${v,,}\n) ) fi done\n"
	for _, rule := range []string{"WB1301", "WB1302"} {
		if got := ofRule(diagsWith(t, oneFile(src), "x.sh", "3.2-5.3"), rule); len(got) != 0 {
			t.Errorf("%s in a gap:\n%s", rule, describe(src, got))
		}
	}
}
