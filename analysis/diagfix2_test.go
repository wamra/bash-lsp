package analysis

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// diagsOpt runs file of files with options opt.
func diagsOpt(files map[string]string, file string, opt DiagOptions) []Diagnostic {
	return diagWorld(files, Options{}).Diagnostics(filepath.Join(diagRoot, file), opt)
}

// Fix round 2, item 1: a directive after a keyword or opener on its line
// (`do`, `then`, `else`, `{`, `(`, a case pattern, a function header)
// covers the next command, as ShellCheck 0.11.0 does (each form checked
// with the binary); one on its own line before `else`, `elif` or a case
// arm covers nothing (ShellCheck refuses it: SC1123, SC1124).
func TestSameLineDirectiveAfterOpener(t *testing.T) {
	d := "# shellcheck disable=SC2086"
	cases := map[string]struct {
		src  string
		want []string
	}{
		"do":             {"f=$1\nfor x in 1; do " + d + "\n  rm $f\ndone\nrm $f\n", []string{"5:$f"}},
		"function {":     {"f=$1\ng() { " + d + "\n  rm $f\n}\ng\nrm $f\n", []string{"6:$f"}},
		"case pattern":   {"f=$1\ncase $1 in\n  a) " + d + "\n    rm $f ;;\nesac\nrm $f\n", []string{"6:$f"}},
		"then":           {"f=$1\nif true; then " + d + "\n  rm $f\nfi\nrm $f\n", []string{"5:$f"}},
		"else":           {"f=$1\nif false; then :; else " + d + "\n  rm $f\nfi\nrm $f\n", []string{"5:$f"}},
		"elif then":      {"f=$1\nif false; then :; elif true; then " + d + "\n  rm $f\nfi\nrm $f\n", []string{"5:$f"}},
		"(":              {"f=$1\n( " + d + "\n  rm $f\n)\nrm $f\n", []string{"5:$f"}},
		"{":              {"f=$1\n{ " + d + "\n  rm $f\n}\nrm $f\n", []string{"5:$f"}},
		"brace own line": {"f=$1\ng()\n{ " + d + "\n  rm $f\n}\ng\nrm $f\n", []string{"7:$f"}},
		// still covers nothing: after a command
		"after a command":          {"f=$1\necho a\nrm $f " + d + "\nrm $f\n", []string{"3:$f", "4:$f"}},
		"after a subshell command": {"f=$1\necho a\n(cd x) " + d + "\nrm $f\n", []string{"4:$f"}},
		// refused positions
		"before else":     {"f=$1\nif true; then\n  :\n" + d + "\nelse\n  rm $f\nfi\n", []string{"6:$f"}},
		"before elif":     {"f=$1\nif true; then\n  :\n" + d + "\nelif rm $f; then :\nfi\n", []string{"5:$f"}},
		"between arms":    {"f=$1\ncase $1 in\n  a) : ;;\n  " + d + "\n  b) rm $f ;;\nesac\n", []string{"5:$f"}},
		"comment between": {"f=$1\necho a\n" + d + "\n# another comment\nrm $f\nrm $f\n", []string{"6:$f"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) { expectRule(t, sh(c.src), "x.sh", "WB1401", c.want...) })
	}
}

// Item 2: a loop-variable loader outside the workspace silences nothing; a
// literal relative pattern only the files it matches; a computed directory
// every script.
func TestLoopLoaderScope(t *testing.T) {
	ws1 := map[string]string{
		"dotfiles/bashrc.sh": "#!/bin/bash\nfor f in \"$HOME\"/.bashrc.d/*.sh; do\n  [ -r \"$f\" ] && . \"$f\"\ndone\n",
		"lib/plugin.sh":      "p(){ :; }\n",
		"scripts/build.sh":   "#!/bin/bash\nnever_called(){ :; }\necho build\n",
		"scripts/deploy.sh":  "#!/bin/bash\nold_deploy(){ echo legacy; }\nunused_count=0\necho deploying\n",
	}
	expectRule(t, ws1, "scripts/build.sh", "WB1201", "2:never_called")
	expectRule(t, ws1, "scripts/deploy.sh", "WB1201", "2:old_deploy")
	expectRule(t, ws1, "scripts/deploy.sh", "WB1202", "3:unused_count")
	ws3 := map[string]string{"env.sh": "#!/bin/sh\nfor i in /etc/profile.d/*.sh; do . \"$i\"; done\n", "tool.sh": "#!/bin/bash\nold(){ :; }\necho x\n"}
	expectRule(t, ws3, "tool.sh", "WB1201", "2:old")
	tilde := map[string]string{"env.sh": "#!/bin/bash\nfor i in ~/.config/x/*.sh; do . \"$i\"; done\n", "tool.sh": "#!/bin/bash\nold(){ :; }\necho x\n"}
	expectRule(t, tilde, "tool.sh", "WB1201", "2:old")
	// a literal relative pattern: only the scripts it matches (here against the root)
	rel := map[string]string{
		"bin/main.sh":    "#!/bin/bash\nfor p in plugins/*.sh; do . \"$p\"; done\n",
		"plugins/a.sh":   "#!/bin/bash\nplugin_init(){ :; }\necho x\n",
		"tools/other.sh": "#!/bin/bash\nold(){ :; }\necho x\n",
	}
	expectRule(t, rel, "plugins/a.sh", "WB1201")
	expectRule(t, rel, "tools/other.sh", "WB1201", "2:old")
	// a computed directory: every script may be one it loads
	comp := map[string]string{"main.sh": "#!/bin/bash\nfor p in \"$DIR\"/*.sh; do . \"$p\"; done\n", "a.sh": "#!/bin/bash\nplugin_init(){ :; }\necho x\n"}
	expectRule(t, comp, "a.sh", "WB1201")
}

// Item 3: the one-word proof does not hold when IFS is set, when the value
// decodes to more than one word, when the variable is unset, may come from
// the environment, or may be a caller's local.
func TestOneWordProofLimits(t *testing.T) {
	for src, text := range map[string]string{
		"IFS=,\nlist=a,b\nrm $list\n":                         "$list",
		"x=$'a\\tb'\nrm $x\n":                                 "$x",
		"x=safe\nunset x\nrm $x\n":                            "$x",
		"[ -n \"$1\" ] && x=safe\nrm $x\n":                    "$x",
		"if [ -n \"$1\" ]; then x=safe; fi\nrm $x\n":          "$x",
		"rm $x\nx=safe\n":                                     "$x",
		"x=safe\nf(){ local x=$1; g; }\ng(){ rm $x; }\nf a\n": "$x",
		"x=a\\ b\nrm $x\n":                                    "$x",
	} {
		lines := strings.Count(src[:strings.Index(src, "rm ")], "\n") + 1
		expectRule(t, sh(src), "x.sh", "WB1401", strconv.Itoa(lines)+":"+text)
	}
	// still silent: provably one word, assigned before the read, no IFS
	for _, src := range []string{
		"dir=build\nrm -rf $dir\n",
		"mode=755\nchmod $mode a\n",
		"n=$((1+2))\ntouch file$n\n",
		"f(){ local d; d=out; rm -r $d; }\nf\n",
		"dir=build\nf(){ rm -rf $dir; }\nf\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401")
	}
}

// Item 4: combined option letters open the walk (`declare -xp`, `set -ea`).
func TestCombinedFlagsAreMentions(t *testing.T) {
	expectRule(t, sh("f(){ local foo=1; declare -xp foo; }; f\n"), "x.sh", "WB1202")
	expectRule(t, sh("#!/bin/bash\nset -ea\ncfg=1\necho hi\n"), "x.sh", "WB1202")
	expectRule(t, sh("#!/bin/bash\nset -eua\ncfg=1\necho hi\n"), "x.sh", "WB1202")
	// still fires: a -p elsewhere does not read the variable
	expectRule(t, sh("f(){ local foo=1; mkdir -p x; }; f\n"), "x.sh", "WB1202", "1:foo")
	expectRule(t, sh("#!/bin/bash\nset -eu\ncfg=1\necho hi\n"), "x.sh", "WB1202", "3:cfg")
}

// Item 5 (R73): a library local is silent only when an unresolved callee
// is a function defined somewhere in the workspace.
func TestLibraryLocalsAndWorkspaceFunctions(t *testing.T) {
	for _, src := range []string{
		"f(){ local unused=1; grep -q x /etc/hosts; }\n",
		"f(){ local unused=1; curl -s https://x; }\n",
		"f(){ local unused=1; mkdir -p /tmp/x; rm -f /tmp/y; }\n",
		"f(){ local unused=1; run_step; }\n", // run_step is defined nowhere: an external command
	} {
		expectRule(t, map[string]string{"lib.sh": src}, "lib.sh", "WB1202", "1:unused")
	}
	expectRule(t, map[string]string{"lib.sh": "f(){ local opt_quiet=1; run_step; }\n", "steps.sh": "run_step(){ [ -z \"$opt_quiet\" ] && echo s; }\n"}, "lib.sh", "WB1202")
}

// Item 6: `$name` in a comment, a single-quoted string that no command
// evaluates, or a quoted heredoc body is not a read.
func TestTextMentionsSkipCommentsAndLiterals(t *testing.T) {
	expectRule(t, map[string]string{"a.sh": "source ./b.sh\nf(){ local foo=1; }\nf\n", "b.sh": "# reads $foo? no, a comment\n"}, "a.sh", "WB1202", "2:foo")
	expectRule(t, sh("f(){ local msg=1; echo '$msg is literal'; }; f\n"), "x.sh", "WB1202", "1:msg")
	expectRule(t, sh("f(){ local retries=3; }\n# set $retries via env\nf\n"), "x.sh", "WB1202", "1:retries")
	expectRule(t, sh("f(){ local u=1; cat <<'EOF'\n$u\nEOF\n}; f\n"), "x.sh", "WB1202", "1:u")
	// still silent: strings a command evaluates, and an unquoted heredoc
	for _, src := range []string{
		"#!/bin/bash\ntmp=$(mktemp)\ntrap 'rm -f \"$tmp\"' EXIT\necho hi\n",
		"f(){ local u=1; eval 'echo $u'; }; f\n",
		"f(){ local u=1; bash -c 'echo $u'; }; f\n",
		"f(){ local u=1; cat <<EOF\n$u\nEOF\n}; f\n",
		"f(){ local u=1; eval \"echo \\$u\"; }; f\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1202")
	}
}

// Minor: guard polarity, bats load, directive text after ids, the source=
// path, roots, and the split of SC1090 and SC1091.
func TestSourceMinors(t *testing.T) {
	sev := func(files map[string]string, file string, opt DiagOptions) []Severity {
		var out []Severity
		for _, d := range ofRule(diagsOpt(files, file, opt), "WB1101") {
			out = append(out, d.Severity)
		}
		return out
	}
	for src, want := range map[string]Severity{
		"[ ! -f ./c.sh ] && . ./c.sh\n":                 SevWarning,
		"[ -f ./d.sh ] || . ./d.sh\n":                   SevWarning,
		"if [ -f ./e.sh ]; then :; else . ./e.sh; fi\n": SevWarning,
		"if [ -f ./e.sh ]; then . ./e.sh; fi\n":         SevInfo,
		"[ -f ./c.sh ] && . ./c.sh\n":                   SevInfo,
	} {
		if got := sev(sh(src), "x.sh", DiagOptions{}); len(got) != 1 || got[0] != want {
			t.Errorf("%q: %v, want %v", src, got, want)
		}
	}
	bats := map[string]string{"t/x.bats": "#!/usr/bin/env bats\nload missing_helper\n@test \"t\" { :; }\n"}
	if got := sev(bats, "t/x.bats", DiagOptions{}); len(got) != 1 || got[0] != SevWarning {
		t.Errorf("bats load: %v", got)
	}
	// the path of a source= directive
	ds := ofRule(diagsIn(sh("# shellcheck source=./nope.sh\nsource \"$X\"\n"), "x.sh"), "WB1101")
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "nope.sh") || strings.Contains(ds[0].Message, "computed") || ds[0].Severity != SevWarning {
		t.Errorf("source=: %+v", ds)
	}
	// every workspace folder counts as the workspace
	other := "/other"
	if got := sev(sh("source /other/lib/x.sh\n"), "x.sh", DiagOptions{Roots: []string{other}}); len(got) != 1 || got[0] != SevWarning {
		t.Errorf("second folder: %v", got)
	}
	if got := sev(sh("source /elsewhere/x.sh\n"), "x.sh", DiagOptions{Roots: []string{other}}); len(got) != 1 || got[0] != SevInfo {
		t.Errorf("outside both: %v", got)
	}
	// SC1090 silences only a wholly computed path, SC1091 a missing file or a
	// path with a literal tail (ShellCheck 0.11.0; fix round 3 corrects the
	// split, see TestSourceCodeBoundary)
	expectRule(t, sh("echo a\n# shellcheck disable=SC1090\nsource ./nope.sh\n"), "x.sh", "WB1101", "3:./nope.sh")
	expectRule(t, sh("echo a\n# shellcheck disable=SC1091\nsource \"$X\"\n"), "x.sh", "WB1101", "3:\"$X\"")
	expectRule(t, sh("echo a\n# shellcheck disable=SC1091\nsource ./nope.sh\n"), "x.sh", "WB1101")
	expectRule(t, sh("echo a\n# shellcheck disable=SC1090\nsource \"$X\"\n"), "x.sh", "WB1101")
}

func TestDirectiveTrailingText(t *testing.T) {
	for _, src := range []string{
		"echo a\n# bash-lsp disable=WB1401 # glob intended\nrm $f\n",
		"echo a\n# bash-lsp disable=WB1401 -- reason\nrm $f\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401")
		expectRule(t, sh(src), "x.sh", "WB1008")
	}
	expectRule(t, sh("echo a\n# bash-lsp disable=WB1401 WB9999\nrm $f\n"), "x.sh", "WB1008", "2:# bash-lsp disable=WB1401 WB9999")
}

// Minor: `usage` beside `main "$@"`.
func TestUsageBesideMain(t *testing.T) {
	expectRule(t, sh("#!/bin/bash\nusage(){ echo \"Usage: x [opts]\"; }\nmain(){ echo hi; }\nmain \"$@\"\n"), "x.sh", "WB1201", "2:usage")
}
