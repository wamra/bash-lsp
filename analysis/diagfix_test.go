package analysis

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// expectRule runs file in files and requires exactly the listed range texts
// (in order) as findings of rule.
func expectRule(t *testing.T, files map[string]string, file, rule string, texts ...string) {
	t.Helper()
	src := files[file]
	ds := diagsIn(files, file)
	got := ofRule(ds, rule)
	var have []string
	for _, d := range got {
		have = append(have, fmt.Sprintf("%d:%s", d.Range.Start.Line, rangeText(src, d.Range)))
	}
	if strings.Join(have, "|") != strings.Join(texts, "|") {
		t.Fatalf("%s: got %q, want %q:\n%s", rule, have, texts, describe(src, ds))
	}
}

func sh(src string) map[string]string { return map[string]string{"x.sh": src} }

// Fix round 1, item 1: a directive inside a compound body covers the next
// command in that body; one with no command after it in its body covers
// nothing; one before a compound command covers all of it.
func TestSuppressionInCompoundBodies(t *testing.T) {
	d := "# shellcheck disable=SC2086\n"
	cases := map[string]struct {
		src  string
		want []string
	}{
		"if (reviewer w07)":           {"#!/bin/bash\nf=$1\necho start\nif true; then\n  " + d + "  rm $f\nfi\nrm $f\n", []string{"8:$f"}},
		"elif":                        {"f=$1\nif a; then :; elif b; then\n  " + d + "  rm $f\nfi\nrm $f\n", []string{"6:$f"}},
		"else":                        {"f=$1\nif a; then :; else\n  " + d + "  rm $f\nfi\nrm $f\n", []string{"6:$f"}},
		"while":                       {"f=$1\nwhile a; do\n  " + d + "  rm $f\ndone\nrm $f\n", []string{"6:$f"}},
		"until":                       {"f=$1\nuntil a; do\n  " + d + "  rm $f\ndone\nrm $f\n", []string{"6:$f"}},
		"for":                         {"f=$1\nfor x in a; do\n  " + d + "  rm $f\ndone\nrm $f\n", []string{"6:$f"}},
		"select":                      {"f=$1\nselect x in a; do\n  " + d + "  rm $f\ndone\nrm $f\n", []string{"6:$f"}},
		"case arm":                    {"f=$1\ncase $a in\n  x)\n    " + d + "    rm $f ;;\nesac\nrm $f\n", []string{"7:$f"}},
		"brace group":                 {"f=$1\n{\n  " + d + "  rm $f\n}\nrm $f\n", []string{"6:$f"}},
		"subshell":                    {"f=$1\n(\n  " + d + "  rm $f\n)\nrm $f\n", []string{"6:$f"}},
		"function body":               {"g() {\n  " + d + "  rm $f\n}\nrm $f\ng\n", []string{"5:$f"}},
		"nested":                      {"f=$1\nif a; then\n  while b; do\n    " + d + "    rm $f\n  done\n  rm $f\nfi\n", []string{"7:$f"}},
		"before fi, no command after": {"f=$1\nif a; then\n  rm $f\n  " + d + "fi\nrm $f\n", []string{"3:$f", "6:$f"}},
		"before done":                 {"f=$1\nwhile a; do\n  rm $f\n  " + d + "done\nrm $f\n", []string{"3:$f", "6:$f"}},
		"before }":                    {"f=$1\n{\n  rm $f\n  " + d + "}\nrm $f\n", []string{"3:$f", "6:$f"}},
		"before a compound command":   {"f=$1\necho start\n" + d + "if a; then\n  rm $f\nelse\n  rm $f\nfi\nrm $f\n", []string{"9:$f"}},
		"trailing on a command line":  {"f=$1\necho start\nrm $f " + strings.TrimSuffix(d, "\n") + "\nrm $f\n", []string{"3:$f", "4:$f"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) { expectRule(t, sh(c.src), "x.sh", "WB1401", c.want...) })
	}
}

// Item 2 (R67a): a callee's assignment to a caller's local is a return value.
func TestNoUnusedCalleeAssignsCallerLocal(t *testing.T) {
	for _, src := range []string{
		"#!/bin/bash\nset_it(){ result=5; }\nf(){ local result; set_it; echo \"$result\"; }\nf\n",
		"#!/bin/bash\ng(){ ret=1; }\nf(){ local ret; g; return $ret; }\nf\n",
		"#!/bin/bash\ng(){ ret_val=$1; }\nf(){ local ret_val; g x; echo \"$ret_val\"; }\nf\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1202")
	}
	// still fires: a global no function declares local, assigned in a function
	expectRule(t, sh("#!/bin/bash\ng(){ unused_g=1; }\ng\n"), "x.sh", "WB1202", "2:unused_g")
}

// Item 3: bare names in arithmetic contexts are reads.
func TestArithmeticContextsAreReads(t *testing.T) {
	for _, src := range []string{
		"f(){ local lim=3 n=$1; [[ $n -gt lim ]] && echo big; }; f\n",
		"#!/bin/bash\nmax=3\n[[ $1 -le max ]] && echo ok\n",
		"f(){ local lim=3; [[ 4 -eq lim ]] && echo eq; }; f\n",
		"f(){ local lim=3; echo $[lim+1]; }; f\n",
		"f(){ local a=1 b=2 c=3 d=4; [[ a -ne b && c -lt d ]]; }; f\n",
		"f(){ local cap=3; for ((i=0;i<cap;i++)); do :; done; }; f\n",
		"f(){ local k=1; local -a arr=(1 2); echo \"${arr[k]}\"; }; f\n",
		"f(){ local o=1 l=2 s=abc; echo \"${s:o:l}\"; }; f\n",
		"f(){ local m=2; let x=m+1; echo $x; }; f\n",
		"f(){ local m=2; local -i y=m+1; echo $y; }; f\n",
		"f(){ local m=2; (( m > 1 )) && echo; }; f\n",
		"f(){ local m=2; echo $(( m + 1 )); }; f\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1202")
	}
	// still fires: a name that only appears in a string comparison is not read
	expectRule(t, sh("f(){ local lim=3; [[ $1 == lim ]]; }; f\n"), "x.sh", "WB1202", "1:lim")
}

// Item 4: a word equal to a function name is a mention, whatever it holds.
func TestFunctionNameWordsWithPunctuation(t *testing.T) {
	for _, src := range []string{
		"#!/bin/bash\nns::work(){ :; }\nexport -f ns::work\nparallel ns::work ::: a b\n",
		"#!/bin/bash\nlib.cleanup(){ :; }\ntimeout 5 bash -c lib.cleanup\n",
		"#!/bin/bash\nx@y(){ :; }\nretry x@y\n",
		"#!/bin/bash\na:b(){ :; }\nretry a:b\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1201")
	}
	expectRule(t, sh("#!/bin/bash\nns::work(){ :; }\necho hi\n"), "x.sh", "WB1201", "2:ns::work")
}

// Item 5 (R67b): no WB1401 where every value in view is one word without
// glob characters.
func TestQuotingSilentForSingleWordValues(t *testing.T) {
	for _, src := range []string{
		"n=$((1+2)); touch file$n\n",
		"for i in 1 2 3; do touch part$i; done\n",
		"mode=755; chmod $mode a\n",
		"dir=build; rm -rf $dir\n",
		"declare -i k=$1; rm $k\n",
		"(( c++ )); rm $c\n",
		"f(){ local d; d=out; rm -r $d; }; f\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401")
	}
	// still fires: any value that is not provably one word
	for src, text := range map[string]string{
		"count=${#1}; rm $count\n":           "$count",
		"f=$1; rm $f\n":                      "$f",
		"suffix=$(date); rm x$suffix\n":      "$suffix",
		"d=build; d=$2; rm $d\n":             "$d",
		"p='a b'; rm $p\n":                   "$p",
		"g='*.log'; rm $g\n":                 "$g",
		"for i in a 'b c'; do rm $i; done\n": "$i",
		"for i in *; do rm $i; done\n":       "$i",
		"read -r x; rm $x\n":                 "$x",
		"rm $unknown\n":                      "$unknown",
		"e=; rm $e\n":                        "$e",
	} {
		expectRule(t, sh(src), "x.sh", "WB1401", "1:"+text)
	}
}

// Item 6: things that are not files to resolve; bats load.
func TestSourcesThatAreNotFiles(t *testing.T) {
	for _, src := range []string{
		"source /dev/null\n",
		"cat x | source /dev/stdin\n",
		". /proc/self/fd/3\n",
		"source <(kubectl completion bash)\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1101")
	}
	files := map[string]string{
		"t/x.bats":                             "#!/usr/bin/env bats\nload helper\nload test_helper/bats-support/load\nload missing\n@test \"t\" { h; }\n",
		"t/helper.bash":                        "h() { :; }\n",
		"t/test_helper/bats-support/load.bash": "s() { :; }\n",
	}
	expectRule(t, files, "t/x.bats", "WB1101", "4:missing")
	s := diagWorld(files, Options{})
	es := s.SourceEdges(filepath.Join(diagRoot, "t/x.bats"))
	if len(es) != 3 || es[0].Status != SourceResolved || es[0].Targets[0] != filepath.Join(diagRoot, "t/helper.bash") || es[1].Status != SourceResolved {
		t.Fatalf("bats load edges: %+v", es)
	}
}

// Item 7 (R68): severities, the ShellCheck codes and directives.
func TestUnresolvedSourceSeverity(t *testing.T) {
	sev := func(src string) []Severity {
		var out []Severity
		for _, d := range ofRule(diagsIn(sh(src), "x.sh"), "WB1101") {
			out = append(out, d.Severity)
		}
		return out
	}
	cases := map[string][]Severity{
		"source ./nope.sh\n":                              {SevWarning},
		"source " + diagRoot + "/lib/none.sh\n":           {SevWarning},
		". /etc/os-release\n":                             {SevInfo},
		"source /opt/app/lib.sh\n":                        {SevInfo},
		"source ~/.bashrc\n":                              {SevInfo},
		"source \"$HOME/.bashrc\"\n":                      {SevInfo},
		"source \"${HOME}/.x\"\n":                         {SevInfo},
		"source bash-completion.sh\n":                     {SevInfo},
		"[ -f ./local.conf ] && . ./local.conf\n":         {SevInfo},
		"if [[ -r y.sh ]]; then . y.sh; fi\n":             {SevInfo},
		"test -e ./a/b.sh && source ./a/b.sh\n":           {SevInfo},
		"[ -f ./other.conf ] && . ./local.conf\n":         {SevWarning},
		"source \"$X/a.sh\"\n":                            {SevInfo},
		"# shellcheck disable=SC1091\nsource ./nope.sh\n": nil,
		"echo a\n# shellcheck disable=SC1090\nsource \"$X/a.sh\"\nsource ./nope.sh\n": {SevWarning},
		"# shellcheck source=/dev/null\nsource \"$X/a.sh\"\n":                         nil,
	}
	for src, want := range cases {
		if got := sev(src); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: severities %v, want %v", src, got, want)
		}
	}
	// a source= directive names the file ShellCheck should follow
	files := map[string]string{"x.sh": "# shellcheck source=./lib.sh\nsource \"$X/b.sh\"\n", "lib.sh": "l() { :; }\n"}
	expectRule(t, files, "x.sh", "WB1101")
	if es := diagWorld(files, Options{}).SourceEdges(filepath.Join(diagRoot, "x.sh")); len(es) != 1 || es[0].Status != SourceResolved {
		t.Fatalf("%+v", es)
	}
}

// Item 8 (R69): the quote fix is offered, never safe.
func TestQuoteFixNeverSafe(t *testing.T) {
	for _, src := range []string{"rm $f\n", "set -f; IFS=,; list=$1; rm $list\n", "flags=$1; rm $flags x\n"} {
		ds := ofRule(diagsIn(sh(src), "x.sh"), "WB1401")
		if len(ds) != 1 || len(ds[0].Fixes) != 1 || ds[0].Fixes[0].Safe {
			t.Fatalf("%q: %+v", src, ds)
		}
	}
}

// Item 9 (R67c): in a library, a local whose function calls something that
// resolves to nothing in view may be read by that callee.
func TestNoUnusedLocalsInLibraries(t *testing.T) {
	expectRule(t, map[string]string{"lib.sh": "f(){ local opt_quiet=1; run_step; }\n"}, "lib.sh", "WB1202")
	// the reviewer's v05: run_step is defined in a file lib.sh does not see (a name match, not a resolved call)
	expectRule(t, map[string]string{"lib.sh": "f(){ local opt_quiet=1; run_step; }\n", "steps.sh": "run_step(){ [ -z \"$opt_quiet\" ] && echo s; }\n"}, "lib.sh", "WB1202")
	expectRule(t, map[string]string{"lib.sh": "f(){ local verbose=1; log hi; }\n", "steps.sh": "run_step() { echo \"$verbose\"; }\n"}, "lib.sh", "WB1202")
	// still fires: a library function calling only builtins and functions in view
	expectRule(t, map[string]string{"lib.sh": "g(){ :; }\nf(){ local unused_l=1; g; echo hi; }\n"}, "lib.sh", "WB1202", "2:unused_l")
	// and in a script a local is checked as before
	expectRule(t, sh("#!/bin/bash\nf(){ local unused_s=1; run_step; }\nf\n"), "x.sh", "WB1202", "2:unused_s")
}

// Minor: dual-use files and glob loaders are not scripts nothing sources.
func TestNoUnusedFunctionDualUseAndLoaders(t *testing.T) {
	for _, src := range []string{
		"#!/bin/bash\nlib_fn(){ :; }\nif [[ \"${BASH_SOURCE[0]}\" == \"$0\" ]]; then echo run; fi\n",
		"#!/bin/bash\nlib_fn(){ :; }\n[[ \"${BASH_SOURCE[0]}\" = \"${0}\" ]] && echo run\n",
	} {
		expectRule(t, sh(src), "x.sh", "WB1201")
	}
	// the reviewer's f23: a glob loader whose pattern matches nothing resolvable
	expectRule(t, map[string]string{"main.sh": "#!/bin/bash\nfor p in plugins/*.sh; do . \"$p\"; done\n", "a.sh": "#!/bin/bash\nplugin_init(){ :; }\necho x\n"}, "a.sh", "WB1201")
	expectRule(t, map[string]string{"main.sh": "#!/bin/bash\nfor p in plugins/*.sh; do . \"$p\"; done\n", "plugins/a.sh": "#!/bin/bash\nplugin_init(){ :; }\necho x\n"}, "plugins/a.sh", "WB1201")
}

// Minor: directive spellings. ShellCheck 0.11.0 honours disable=2086 and
// refuses disable=sc2086 (SC1072/SC1073, checked with the binary).
func TestDirectiveSpellings(t *testing.T) {
	expectRule(t, sh("echo a\n# shellcheck disable=2086\nrm $f\nrm $f\n"), "x.sh", "WB1401", "4:$f")
	expectRule(t, sh("echo a\n# shellcheck disable=sc2086\nrm $f\nrm $f\n"), "x.sh", "WB1401", "3:$f", "4:$f")
	expectRule(t, sh("echo a\n# bash-lsp disable=WB1202, WB1401\nrm $f\nrm $f\n"), "x.sh", "WB1401", "4:$f")
	expectRule(t, sh("echo a\n# bash-lsp disable=WB1401 WB1202\nrm $f\n"), "x.sh", "WB1401")
	expectRule(t, sh("echo a\n# bash-lsp disable=WB9999,WB1401\nrm $f\n"), "x.sh", "WB1008", "2:# bash-lsp disable=WB9999,WB1401")
	expectRule(t, sh("echo a\n# bash-lsp disable=WB1102\nrm $f\n"), "x.sh", "WB1008") // reserved for a later rule: known
}

// Minor: `xs+=(1)` appends; ShellCheck 0.11.0 is silent on it (checked with
// the binary).
func TestAppendIsNotUnused(t *testing.T) {
	expectRule(t, sh("f(){ local -a xs=(); xs+=(1); }; f\n"), "x.sh", "WB1202")
	expectRule(t, sh("#!/bin/bash\nys=(); ys+=(2)\necho hi\n"), "x.sh", "WB1202")
}

// Minor: WB1401 maps SC2068 (ShellCheck 0.11.0 reports it on `rm ${files[@]}`).
func TestQuotingMapsSC2068(t *testing.T) {
	if !mapsTo(ruleByID["WB1401"].ShellCheck, "SC2068") {
		t.Fatal(ruleByID["WB1401"].ShellCheck)
	}
	expectRule(t, sh("files=(\"$@\")\n# shellcheck disable=SC2068\nrm ${files[@]}\n"), "x.sh", "WB1401")
}

// Minor: WB1003 shows no Go panic text.
func TestInternalErrorMessage(t *testing.T) {
	ds := ofRule(diagsIn(sh("A=$((0 A\nf() { :; }\n"), "x.sh"), "WB1003")
	if len(ds) != 1 || ds[0].Message != "internal parser error; this file is analysed up to line 1" {
		t.Fatalf("%+v", ds)
	}
}

// Minor (R70): at most maxFindings per file, highest severity first, and
// one WB1007 saying how many were cut; bounded in time at 2 MiB.
func TestFindingsCap(t *testing.T) {
	src := "f(){ :; }\nf(){ :; }\n" + strings.Repeat("rm $f\n", 3000)
	ds := diagsIn(sh(src), "x.sh")
	if len(ds) != maxFindings+1 {
		t.Fatalf("%d findings", len(ds))
	}
	cut := ofRule(ds, "WB1007")
	if len(cut) != 1 || cut[0].Range.Start.Line != 1 || cut[0].Message != "1001 more findings not shown" || cut[0].Severity != SevInfo {
		t.Fatalf("%+v", cut)
	}
	if len(ofRule(ds, "WB1103")) != 0 || len(ofRule(ds, "WB1401")) != maxFindings {
		t.Fatalf("the information finding was kept over warnings: %d WB1103, %d WB1401", len(ofRule(ds, "WB1103")), len(ofRule(ds, "WB1401")))
	}
	size, bound := shapeSize()
	big := fillTo(size, "", "rm $f\n", "")
	s := diagWorld(sh(big), Options{})
	var n int
	withinBound(t, "2 MiB of findings", bound, func() { n = len(s.Diagnostics(filepath.Join(diagRoot, "x.sh"), DiagOptions{})) })
	if n != maxFindings+1 {
		t.Fatalf("%d", n)
	}
}

// Minor: disabled rules do not run.
func TestDisabledRulesDoNotRun(t *testing.T) {
	src := "#!/bin/bash\nf(){ local u=1; }\nrm $x\nsource ./nope.sh\n"
	s := diagWorld(sh(src), Options{})
	p := filepath.Join(diagRoot, "x.sh")
	all := map[string]bool{}
	for _, r := range Rules() {
		all[r.ID] = true
	}
	var ran []string
	ruleRunHook = func(id string) { ran = append(ran, id) }
	defer func() { ruleRunHook = nil }()
	if ds := s.Diagnostics(p, DiagOptions{Disabled: all}); len(ds) != 0 {
		t.Fatalf("%+v", ds)
	}
	if len(ran) != 0 {
		t.Fatalf("rules ran while disabled: %v", ran)
	}
	delete(all, "WB1401")
	s.Diagnostics(p, DiagOptions{Disabled: all})
	if strings.Join(ran, ",") != "WB1401" {
		t.Fatalf("ran %v", ran)
	}
}
