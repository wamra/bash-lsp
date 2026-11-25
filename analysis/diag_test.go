package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

const diagRoot = "/w"

// diagWorld analyses files (root-relative names) under diagRoot.
func diagWorld(files map[string]string, opt Options) *Snapshot {
	m := map[string][]byte{}
	for name, src := range files {
		m[filepath.Join(diagRoot, name)] = []byte(src)
	}
	opt.Root = diagRoot
	return Analyze(m, opt)
}

// diagsIn returns the findings of file in a workspace of files.
func diagsIn(files map[string]string, file string) []Diagnostic {
	s := diagWorld(files, Options{})
	return s.Diagnostics(filepath.Join(diagRoot, file), DiagOptions{})
}

func ofRule(ds []Diagnostic, rule string) []Diagnostic {
	var out []Diagnostic
	for _, d := range ds {
		if d.Rule == rule {
			out = append(out, d)
		}
	}
	return out
}

func rangeText(src string, r tree.Range) string {
	if r.Start.Offset < 0 || r.End.Offset > len(src) || r.Start.Offset > r.End.Offset {
		return fmt.Sprintf("<bad range %+v>", r)
	}
	return src[r.Start.Offset:r.End.Offset]
}

func describe(src string, ds []Diagnostic) string {
	var sb strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&sb, "  %s %d:%d %q %s\n", d.Rule, d.Range.Start.Line, d.Range.Start.Col, rangeText(src, d.Range), d.Message)
	}
	return sb.String()
}

// ruleCase is one row example: files (x.sh is the file checked when the
// map has more than one entry and file is empty), and for a trigger the
// expected range text of the one finding.
type ruleCase struct {
	files map[string]string
	file  string
	text  string
}

func one(src, text string) ruleCase {
	return ruleCase{files: map[string]string{"x.sh": src}, text: text}
}

func (c ruleCase) run() (string, []Diagnostic) {
	file := c.file
	if file == "" {
		file = "x.sh"
	}
	return c.files[file], diagsIn(c.files, file)
}

const pad3MiB = 3 << 20

func bigComment(n int) string {
	unit := "# pad\n"
	return strings.Repeat(unit, n/len(unit)) + strings.Repeat("#", n%len(unit))
}

// The native rule table: each "Triggers" example yields exactly one finding
// of its rule with the asserted range text; each "Must not trigger"
// example yields none.
var ruleTable = []struct {
	rule     string
	triggers []ruleCase
	silent   []ruleCase
}{
	{"WB1001",
		[]ruleCase{one("echo \"abc\nf() { :; }\n", "echo \"abc")},
		[]ruleCase{one("echo hi\n", "")}},
	{"WB1002",
		[]ruleCase{one(bigComment(pad3MiB), "")},
		[]ruleCase{one(bigComment(2<<20-1), "")}},
	{"WB1003",
		[]ruleCase{one("A=$((0 A\nf() { :; }\n", "A=$((0 A")},
		[]ruleCase{one("f() { :; }\nf\n", "")}},
	{"WB1005",
		[]ruleCase{one(resyncBudgetInput(), ") ) fi done")},
		[]ruleCase{one("a() { :; }\necho ${x\nb() { :; }\n", "")}},
	{"WB1101",
		[]ruleCase{one("source ./nope.sh\n", "./nope.sh"), one("source \"$X/a.sh\"\n", "\"$X/a.sh\"")},
		[]ruleCase{
			one("# shellcheck source=/dev/null\nsource ./nope.sh\n", ""),
			{files: map[string]string{"x.sh": "HERE=\"$(dirname \"$0\")\"\nsource \"$HERE/lib.sh\"\n", "lib.sh": "l() { :; }\n"}},
		}},
	{"WB1103",
		[]ruleCase{one("f(){ :; }\nf(){ :; }\n", "f")},
		[]ruleCase{
			one("if x; then f(){ :; }; else f(){ :; }; fi\n", ""),
			{files: map[string]string{"x.sh": "f(){ :; }\n", "y.sh": "f(){ :; }\n"}},
		}},
	{"WB1104",
		[]ruleCase{one("test() { :; }\n", "test")},
		[]ruleCase{
			one("mytest() { :; }\n", ""),
			one("cd() { builtin cd \"$@\" && pwd; }\n", ""),       // a deliberate wrapper
			one("echo() { command echo \"$@\"; }\necho hi\n", ""), // a deliberate wrapper
			one("help() { echo usage; }\nhelp\n", ""),             // an interactive-only builtin
		}},
	{"WB1105",
		[]ruleCase{one("#!/bin/bash\nlocal x=1\n", "local")},
		[]ruleCase{
			one("#!/bin/bash\nf(){ local x; }\nf\n", ""),
			{files: map[string]string{"x.sh": "local x=1\necho \"$x\"\n", "m.sh": "#!/bin/bash\nf() { source ./x.sh; }\nf\n"}},
			{files: map[string]string{"x.sh": "#!/bin/bash\nlocal x=1\necho \"$x\"\n", "m.sh": "#!/bin/bash\nf() { source ./x.sh; }\nf\n"}},
			{files: map[string]string{"x.bats": "#!/usr/bin/env bats\n@test \"t\" { local x=1; echo \"$x\"; }\n"}, file: "x.bats"},
		}},
	{"WB1106",
		[]ruleCase{one("echo hi | read y; echo \"$y\"\n", "$y"), one("( y=2 ); echo \"$y\"\n", "$y")},
		[]ruleCase{
			one("( y=2; echo \"$y\" )\n", ""),
			one("cmd | while read x; do echo \"$x\"; done\n", ""),
			one("v=$(w=3; echo \"$w\")\necho \"$v\"\n", ""),
			one("y=1\n( y=2 )\necho \"$y\"\n", ""),              // the read binds to the outer y
			one("( PATH=/x; cmd )\necho \"$PATH\"\n", ""),       // an environment variable
			one("f() { ( y=2 ); }\ng() { echo \"$y\"; }\n", ""), // another function
			one("shopt -s lastpipe\necho hi | read y; echo \"$y\"\n", ""),
		}},
	{"WB1201",
		[]ruleCase{one("#!/bin/bash\nf(){ :; }\necho hi\n", "f")},
		[]ruleCase{
			{files: map[string]string{"x.sh": "#!/bin/bash\nf(){ :; }\necho hi\n", "m.sh": "#!/bin/bash\nsource ./x.sh\n"}},
			one("#!/bin/bash\nf(){ :; }\nhandler=$1\n\"$handler\" x\n", ""),
			one("#!/bin/bash\nf(){ :; }\ntrap f EXIT\necho hi\n", ""),
			one("#!/bin/bash\nf(){ :; }\nretry 3 f\n", ""),                        // a name passed as an argument
			one("#!/bin/bash\nf(){ :; }\ncb=f\nrun_later\n", ""),                  // a name kept as a value
			one("#!/bin/bash\nf(){ :; }\nexport -f f\nbash -c x\n", ""),           // exported
			one("#!/bin/bash\n_f(){ :; }\ncomplete -F _f cmd\n", ""),              // a completion function
			one("#!/bin/bash\nf(){ :; }\neval \"$code\"\n", ""),                   // eval of unknown code
			one("#!/bin/bash\nf(){ :; }\nsu -c 'f' root\n", ""),                   // named inside a string
			one("#!/bin/bash\nf(){ :; }\necho ${x\n", ""),                         // code not analysed
			one("#!/bin/bash\nf(){ :; }\n", ""),                                   // functions only: a library
			one("f(){ :; }\necho hi\n", ""),                                       // no shebang
			one("#!/bin/bash\nf(){ :; }\nsource \"$X/plugin.sh\"\necho hi\n", ""), // graph incomplete
			one("#!/bin/bash\ncommand_not_found_handle(){ :; }\necho hi\n", ""),
			one("#!/bin/bash\n[[ -n ${_LIB:-} ]] && return\n_LIB=1\nf() { :; }\n", ""), // an include guard: meant to be sourced
			{files: map[string]string{"x.bats": "#!/usr/bin/env bats\nsetup() { :; }\n@test \"t\" { :; }\n"}, file: "x.bats"},
		}},
	{"WB1202",
		[]ruleCase{one("f(){ local u=1; }; f\n", "u"), one("#!/bin/bash\nx=1\necho hi\n", "x")},
		[]ruleCase{
			one("f(){ local u=1; echo \"$u\"; }; f\n", ""),
			one("#!/bin/bash\nexport X=1\necho hi\n", ""),
			one("#!/bin/bash\nX=1\nexport X\necho hi\n", ""),
			one("#!/bin/bash\nset -a\nX=1\necho hi\n", ""),
			one("X=1\n", ""), // a library global
			one("x=1\n", ""), // a library global
			one("#!/bin/bash\nTMPDIR=/x\nTZ=UTC\nBASH_XTRACEFD=3\nmktemp\n", ""), // environment names by convention
			one("#!/bin/bash\nf() { local u=1; }\nf\n\"$handler\" x\n", ""),      // a dynamic call in view
			one("#!/bin/bash\n[[ -n ${_LIB:-} ]] && return\n_lib=1\n", ""),       // an include guard: meant to be sourced
			one("#!/bin/bash\ntmp=$(mktemp)\ntrap 'rm -f \"$tmp\"' EXIT\necho hi\n", ""),
			one("f(){ local u=1; eval 'echo $u'; }; f\n", ""),
			one("f(){ local u=1; eval \"echo \\$u\"; }; f\n", ""),
			one("f(){ local u=1; cat <<EOF\n$u\nEOF\n}; f\n", ""),
			one("f(){ local u=1; g; }\ng(){ h; }\nh(){ echo \"$u\"; }\nf\n", ""),                                 // dynamic scope, two calls down
			{files: map[string]string{"x.sh": "f(){ local u=1; source ./l.sh; }\nf\n", "l.sh": "echo \"$u\"\n"}}, // read by a sourced file
			one("f(){ local cmd=g; \"$cmd\"; }\ng(){ :; }\nf\n", ""),
			one("f(){ local a=1 n=a; echo \"${!n}\"; }; f\n", ""),
			one("f(){ local a=1; local -n r=a; echo \"$r\"; }; f\n", ""),
			one("f(){ local IFS=,; read -r a b <<< \"$1\"; echo \"$a$b\"; }; f x\n", ""),
			one("f(){ local _=1; }; f\n", ""),
			one("f(){ read -r a b; echo \"$a\"; }; f\n", ""), // read's names are not flagged
			one("f(){ for i in 1 2; do :; done; }; f\n", ""),
			one("f(){ local u=1; [[ -v u ]] && echo set; }; f\n", ""),
			one("f(){ local u=1; declare -p u; }; f\n", ""),
			one("f(){ local u=1; \"$handler\"; }\nhandler=$1\nf\n", ""), // a dynamic call may read it
			one("#!/bin/bash\necho hi\n# shellcheck disable=SC2034\nY=2\n", ""),
		}},
	{"WB1301",
		[]ruleCase{one("declare -A m\n", "declare -A"), one("echo \"${v,,}\"\n", "${v,,}")},
		[]ruleCase{
			one("if ((BASH_VERSINFO[0] >= 4)); then declare -A m; fi\n", ""),
			one("#!/bin/sh\ndeclare -A m\n", ""),
			one("echo 'declare -A m'\n", ""),
			{files: map[string]string{"x.sh": "declare -A m\n", "m.sh": "#!/bin/bash\nsource ./x.sh\n"}},
		}},
	{"WB1302",
		[]ruleCase{one("#!/bin/sh\n[[ -n $x ]]\n", "[["), one("#!/bin/sh\nfunction f { :; }\n", "function f")},
		[]ruleCase{
			one("#!/bin/bash\n[[ -n $x ]]\n", ""),
			one("#!/bin/sh\n[ -n \"$x\" ]\n", ""),
			one("#!/bin/sh\necho '[[ x ]]'\n", ""),
		}},
	{"WB1401",
		[]ruleCase{one("rm $f\n", "$f"), one("rm \"$a\"$b\n", "$b"), one("sudo rm $f\n", "$f"), one("command mv ${f} /tmp\n", "${f}"), one("rm $d/*.log\n", "$d")},
		[]ruleCase{
			one("rm \"$f\"\n", ""),
			one("rm -- \"${f}\"\n", ""),
			one("echo $f\n", ""),
			one("rm $#\n", ""),
			one("rm $1\n", ""),
			one("rm ${#x}\n", ""),
			one("rm ${force:+-f} x\n", ""),
			one("declare -i n=1\nrm $n\n", ""),
			one("rm $(( n + 1 ))\n", ""),
			one("rm \"$(ls $d)\"\n", ""),
		}},
}

func resyncBudgetInput() string {
	return "a() { echo hi; }\n) ) fi done\n" + strings.Repeat("echo ${x\n", 80) + "z() { :; }\n"
}

func TestRuleTable(t *testing.T) {
	for _, row := range ruleTable {
		for i, c := range row.triggers {
			t.Run(fmt.Sprintf("%s/trigger%d", row.rule, i), func(t *testing.T) {
				src, ds := c.run()
				got := ofRule(ds, row.rule)
				if len(got) != 1 {
					t.Fatalf("%d %s findings, want 1:\n%s", len(got), row.rule, describe(src, ds))
				}
				if txt := rangeText(src, got[0].Range); txt != c.text {
					t.Fatalf("range text %q, want %q:\n%s", txt, c.text, describe(src, ds))
				}
				if got[0].Origin != "bash-lsp" || got[0].Message == "" {
					t.Fatalf("%+v", got[0])
				}
			})
		}
		for i, c := range row.silent {
			t.Run(fmt.Sprintf("%s/silent%d", row.rule, i), func(t *testing.T) {
				src, ds := c.run()
				if got := ofRule(ds, row.rule); len(got) != 0 {
					t.Fatalf("%d %s findings, want none:\n%s", len(got), row.rule, describe(src, ds))
				}
			})
		}
	}
}

// Every rule the table covers is registered with its default severity and
// ShellCheck map, and every registered rule is in the table.
func TestRulesRegistry(t *testing.T) {
	want := map[string]struct {
		sev Severity
		sc  []string
	}{
		"WB1001": {SevError, nil}, "WB1002": {SevInfo, nil}, "WB1003": {SevInfo, nil}, "WB1005": {SevInfo, nil},
		"WB1006": {SevInfo, nil}, "WB1007": {SevInfo, nil}, "WB1008": {SevInfo, nil},
		"WB1101": {SevWarning, []string{"SC1090", "SC1091"}}, "WB1102": {SevInfo, nil}, "WB1103": {SevInfo, nil}, "WB1104": {SevInfo, nil},
		"WB1105": {SevError, []string{"SC2168"}}, "WB1106": {SevWarning, []string{"SC2030", "SC2031"}},
		"WB1201": {SevHint, []string{"SC2329"}}, "WB1202": {SevHint, []string{"SC2034"}},
		"WB1301": {SevWarning, nil}, "WB1302": {SevWarning, []string{"SC3*", "SC2113"}},
		"WB1401": {SevWarning, []string{"SC2086", "SC2068"}},
	}
	rules := Rules()
	if len(rules) != len(want) {
		t.Fatalf("%d rules: %+v", len(rules), rules)
	}
	for _, r := range rules {
		w, ok := want[r.ID]
		if !ok || r.Default != w.sev || !slices.Equal(r.ShellCheck, w.sc) || r.Name == "" || r.Summary == "" {
			t.Fatalf("%+v", r)
		}
	}
	for _, row := range ruleTable {
		if _, ok := want[row.rule]; !ok {
			t.Fatalf("table row %s not registered", row.rule)
		}
	}
}

// Review Focus 1: unused findings stay silent in a library nothing sources,
// near a dynamic call, and in a file whose graph is incomplete.
func TestNoUnusedInLibraries(t *testing.T) {
	cases := map[string]map[string]string{
		"library":    {"x.sh": "f() { :; }\nx=1\n"},
		"handler":    {"x.sh": "#!/bin/bash\nf() { local u=1; }\nx=1\ng() { :; }\nf\n\"$handler\" x\n"},
		"incomplete": {"x.sh": "#!/bin/bash\nsource \"$LIB\"\nf() { :; }\nx=1\necho hi\n"},
		"lib sourced by a script": {
			"x.sh": "#!/bin/bash\nf() { :; }\nx=1\necho hi\n",
			"m.sh": "#!/bin/bash\n. ./x.sh\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			ds := diagsIn(files, "x.sh")
			if got := append(ofRule(ds, "WB1201"), ofRule(ds, "WB1202")...); len(got) != 0 {
				t.Fatalf("%s", describe(files["x.sh"], got))
			}
		})
	}
}

func TestWB1401Fix(t *testing.T) {
	src := "rm $f\n"
	ds := ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401")
	if len(ds) != 1 || len(ds[0].Fixes) != 1 {
		t.Fatalf("%+v", ds)
	}
	fx := ds[0].Fixes[0]
	if fx.Safe || fx.Title == "" { // offered, never safe: quoting changes intended splitting
		t.Fatalf("%+v", fx)
	}
	if got := applyEdits(src, fx.Edits); got != "rm \"$f\"\n" {
		t.Fatalf("fixed %q", got)
	}
	// an expansion with quotes inside is offered, not marked safe
	src = "rm ${f:-\"x y\"}\n"
	ds = ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401")
	if len(ds) != 1 || len(ds[0].Fixes) != 1 || ds[0].Fixes[0].Safe {
		t.Fatalf("%+v", ds)
	}
}

func applyEdits(src string, edits []TextEdit) string {
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(a, b TextEdit) int { return b.Range.Start.Offset - a.Range.Start.Offset })
	for _, e := range edits {
		src = src[:e.Range.Start.Offset] + e.NewText + src[e.Range.End.Offset:]
	}
	return src
}

func TestSuppressionScope(t *testing.T) {
	// a directive before the first command applies to the whole file
	src := "#!/bin/bash\n# bash-lsp disable=WB1401\nrm $a\nf() { rm $b; }\nf\n"
	if ds := ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401"); len(ds) != 0 {
		t.Fatalf("file-wide:\n%s", describe(src, ds))
	}
	// later, a directive covers the next command (here a whole function) only
	src = "echo start\n# shellcheck disable=SC2086\nf() {\n  rm $a\n  rm $b\n}\nrm $c\nf\n"
	ds := ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "$c" {
		t.Fatalf("per-command:\n%s", describe(src, ds))
	}
	src = "echo start\n# bash-lsp disable=WB1401,WB1202\nrm $a\nrm $b\n"
	ds = ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "$b" {
		t.Fatalf("next command:\n%s", describe(src, ds))
	}
	// an unmapped ShellCheck code does not silence a native rule; a range does
	src = "echo start\n# shellcheck disable=SC2035\nrm $a\n# shellcheck disable=SC2000-SC2100\nrm $b\n"
	ds = ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "$a" {
		t.Fatalf("codes:\n%s", describe(src, ds))
	}
	// a directive inside a function body covers the next command there
	src = "f() {\n  # shellcheck disable=SC2086\n  rm $a\n  rm $b\n}\nf\n"
	ds = ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1401")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "$b" {
		t.Fatalf("in a body:\n%s", describe(src, ds))
	}
}

// T4-2: a statement straddling the failing line is inside the gap, and the
// syntax finding covers both lines.
func TestStraddlingGap(t *testing.T) {
	src := "x=$(foo \\\n\"bar\nf() { :; }\n"
	s := diagWorld(map[string]string{"x.sh": src}, Options{})
	p := filepath.Join(diagRoot, "x.sh")
	ds := ofRule(s.Diagnostics(p, DiagOptions{}), "WB1001")
	if len(ds) != 1 || ds[0].Range.Start.Line != 1 || ds[0].Range.End.Line < 2 {
		t.Fatalf("%s", describe(src, ds))
	}
	if sym, ok := s.SymbolAt(p, strings.Index(src, "f()")); !ok || sym.Name != "f" {
		t.Fatal("f is not a symbol")
	}
}

// T3-5, T4-3: a recovered parser panic is WB1003, not also WB1001.
func TestRecoveredPanicIsDiagnostic(t *testing.T) {
	src := "A=$((0 A\nf() { :; }\n"
	ds := diagsIn(map[string]string{"x.sh": src}, "x.sh")
	if len(ofRule(ds, "WB1003")) != 1 || len(ofRule(ds, "WB1001")) != 0 {
		t.Fatalf("%s", describe(src, ds))
	}
}

func TestGapWithoutErrorIsWB1001(t *testing.T) {
	src := "echo ${x\nb() { :; }\n"
	p := filepath.Join(diagRoot, "x.sh")
	f := parse.File(p, []byte(src))
	if len(f.Errors) != 1 {
		t.Fatalf("errors %+v", f.Errors)
	}
	with := Build(map[string]*FileFacts{p: Bind(f)}, Options{Root: diagRoot}, 1).Diagnostics(p, DiagOptions{})
	if ds := ofRule(with, "WB1001"); len(ds) != 1 || ds[0].Message == "code not analysed (error gap)" {
		t.Fatalf("with its error:\n%s", describe(src, ds))
	}
	g := parse.File(p, []byte(src))
	g.Errors = nil
	without := Build(map[string]*FileFacts{p: Bind(g)}, Options{Root: diagRoot}, 1).Diagnostics(p, DiagOptions{})
	ds := ofRule(without, "WB1001")
	if len(ds) != 1 || ds[0].Message != "code not analysed (error gap)" || rangeText(src, ds[0].Range) != "echo ${x\n" {
		t.Fatalf("without:\n%s", describe(src, ds))
	}
}

func TestMergeDedup(t *testing.T) {
	r := func(a, b int) tree.Range {
		return tree.Range{Start: tree.Pos{Offset: a, Line: 1, Col: a + 1}, End: tree.Pos{Offset: b, Line: 1, Col: b + 1}}
	}
	native := []Diagnostic{{Rule: "WB1401", Severity: SevWarning, Range: r(3, 5), Origin: "bash-lsp"}}
	sc := []Diagnostic{
		{Rule: "SC2086", Severity: SevInfo, Range: r(3, 5), Origin: "shellcheck"},
		{Rule: "SC2154", Severity: SevWarning, Range: r(10, 12), Origin: "shellcheck"},
	}
	got := Merge(native, sc)
	if len(got) != 2 || got[0].Rule != "SC2086" || got[1].Rule != "SC2154" {
		t.Fatalf("%+v", got)
	}
	// a mapped code elsewhere does not drop it; an unmapped code at the same range does not either
	got = Merge(native, []Diagnostic{{Rule: "SC2086", Range: r(20, 22)}, {Rule: "SC2035", Range: r(3, 5)}})
	if len(got) != 3 || got[0].Rule != "SC2035" && got[0].Rule != "WB1401" {
		t.Fatalf("%+v", got)
	}
	if Merge(nil, nil) == nil {
		t.Fatal("nil")
	}
}

func TestWB1005(t *testing.T) {
	src := resyncBudgetInput()
	ds := ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1005")
	if len(ds) != 1 || ds[0].Range.Start.Line != 2 || !strings.Contains(ds[0].Message, "resync stopped after 50 attempts") {
		t.Fatalf("%s", describe(src, ds))
	}
}

func TestWB1101Reasons(t *testing.T) {
	big := filepath.Join(diagRoot, "big.sh")
	main := filepath.Join(diagRoot, "m.sh")
	src := "source ./big.sh\nsource ./none.sh\nsource \"$X/a.sh\"\n"
	s := Analyze(map[string][]byte{main: []byte(src)}, Options{Root: diagRoot, Oversize: func(p string) bool { return p == big }})
	ds := ofRule(s.Diagnostics(main, DiagOptions{}), "WB1101")
	if len(ds) != 3 {
		t.Fatalf("%s", describe(src, ds))
	}
	if !strings.Contains(ds[0].Message, "skipped-for-size") || !strings.Contains(ds[0].Message, "big.sh") ||
		!strings.Contains(ds[0].Message, "over the 2 MiB analysis limit") || ds[0].Severity != SevWarning {
		t.Fatalf("first: %+v", ds[0])
	}
	if !strings.Contains(ds[1].Message, "not-found") || ds[1].Severity != SevWarning {
		t.Fatalf("second: %+v", ds[1])
	}
	if ds[2].Severity != SevInfo {
		t.Fatalf("computed: %+v", ds[2])
	}
	src = "# shellcheck source=/dev/null\nsource ./big.sh\n# shellcheck source=/dev/null\nsource ./none.sh\n"
	s = Analyze(map[string][]byte{main: []byte(src)}, Options{Root: diagRoot, Oversize: func(p string) bool { return p == big }})
	if ds := ofRule(s.Diagnostics(main, DiagOptions{}), "WB1101"); len(ds) != 0 {
		t.Fatalf("%s", describe(src, ds))
	}
}

func TestDiagOptions(t *testing.T) {
	src := "rm $f\nsource ./nope.sh\n"
	s := diagWorld(map[string]string{"x.sh": src}, Options{})
	p := filepath.Join(diagRoot, "x.sh")
	all := s.Diagnostics(p, DiagOptions{})
	if len(ofRule(all, "WB1401")) != 1 || len(ofRule(all, "WB1101")) != 1 {
		t.Fatalf("%s", describe(src, all))
	}
	got := s.Diagnostics(p, DiagOptions{Disabled: map[string]bool{"WB1401": true}, Severity: map[string]Severity{"WB1101": SevHint}})
	if len(ofRule(got, "WB1401")) != 0 || ofRule(got, "WB1101")[0].Severity != SevHint {
		t.Fatalf("%s", describe(src, got))
	}
	if ds := s.Diagnostics("/elsewhere.sh", DiagOptions{}); ds == nil || len(ds) != 0 {
		t.Fatalf("unknown path: %#v", ds)
	}
}

// Findings come sorted by offset, then rule.
func TestDiagnosticsSorted(t *testing.T) {
	src := "#!/bin/bash\nf(){ local u=$1; rm $u; }\nf(){ :; }\nrm $x\nsource ./nope.sh\n"
	ds := diagsIn(map[string]string{"x.sh": src}, "x.sh")
	if len(ds) < 4 {
		t.Fatalf("%s", describe(src, ds))
	}
	for i := 1; i < len(ds); i++ {
		a, b := ds[i-1], ds[i]
		if a.Range.Start.Offset > b.Range.Start.Offset || a.Range.Start.Offset == b.Range.Start.Offset && a.Rule > b.Rule {
			t.Fatalf("not sorted:\n%s", describe(src, ds))
		}
	}
}

// Every rule over broken input: no panic, every range inside the file.
func TestRulesOnBrokenInput(t *testing.T) {
	for _, src := range []string{
		"rm $\n", "rm ${\n", "f() {\nlocal\n", "( y=\n", "echo hi | read\n", "source\n", "source \"\n",
		"test() {", "x=$(\n", "rm \"$a\"$", "local -n\n", "\x00rm $f\x00\n", "cd() { builtin\n",
	} {
		ds := diagsIn(map[string]string{"x.sh": src}, "x.sh")
		for _, d := range ds {
			if d.Range.Start.Offset < 0 || d.Range.End.Offset > len(src) || d.Range.Start.Offset > d.Range.End.Offset {
				t.Fatalf("%q: bad range %+v", src, d)
			}
		}
	}
}

// testdata/diag holds one file per native rule row, named after the rule,
// with the row's "Triggers" example: each yields exactly one finding of that
// rule. WB1002 (a 3 MiB file) is the table's generated case instead.
func TestDiagTestdata(t *testing.T) {
	paths, err := filepath.Glob("../testdata/diag/WB*.sh")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no files: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		rule := strings.TrimSuffix(filepath.Base(p), ".sh")
		seen[rule] = true
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		// With a PATH profile, so that WB1102 has something to say.
		ds := diagWorld(map[string]string{"x.sh": string(src)}, Options{Commands: fakeProfile("ls")}).Diagnostics(filepath.Join(diagRoot, "x.sh"), DiagOptions{})
		if got := ofRule(ds, rule); len(got) != 1 {
			t.Errorf("%s: %d findings of %s:\n%s", p, len(got), rule, describe(string(src), ds))
		}
	}
	for _, r := range Rules() {
		if !seen[r.ID] && r.ID != "WB1002" && r.ID != "WB1006" && r.ID != "WB1007" { // a 3 MiB file, a 2 MiB string, 2,000 findings: generated in TestRuleTable, TestStringBudgetReason, TestFindingsCap
			t.Errorf("no testdata/diag file for %s", r.ID)
		}
	}
}

// Rules read facts and bindings once each: about 2 MiB of the shapes that
// would multiply (a dynamic call next to every unused local, reads after
// every subshell, thousands of nested directives over thousands of findings,
// one global assigned thousands of times, many distinct unread locals) gives
// its findings in bounded time.
func TestDiagnosticsInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	nest := 3000
	if raceBuild {
		nest = 750
	}
	var locals strings.Builder
	locals.WriteString("f() {")
	for i := 0; locals.Len() < size-20; i++ {
		fmt.Fprintf(&locals, " local v%d=1;", i)
	}
	locals.WriteString(" }\nf\n")
	cases := map[string]string{
		"unused locals beside dynamic calls": fillTo(size, "", "f() { local u=1; \"$h\"; }\n", ""),
		"reads after subshells":              fillTo(size, "", "( y=1 ); echo \"$y\"\n", ""),
		"nested directives, other rule":      fillTo(size, strings.Repeat("{\n# shellcheck disable=SC2034\n", nest), "rm $f\n", strings.Repeat("}\n", nest)),
		"nested directives, same rule":       fillTo(size, strings.Repeat("{\n# shellcheck disable=SC2086\n", nest), "rm $f\n", strings.Repeat("}\n", nest)),
		"directive per command":              fillTo(size, "echo start\n", "# shellcheck disable=SC2086\nrm $f\n", ""),
		"one global assigned many times":     fillTo(size, "#!/bin/bash\necho hi\n", "x=1\n", ""),
		"unused and duplicate functions":     fillTo(size, "#!/bin/bash\necho hi\n", "f() { :; }\n", ""),
		"many unquoted arguments":            fillTo(size, "rm", " $f", "\n"),
		"reads inside strings":               fillTo(size, "f() { local u=1; ", "eval 'echo $u';", " }\nf\n"),
		"many distinct unread locals":        locals.String(),
		// distinct names: one name defined many times and called many times is
		// the calls x definitions product of Build, not a rule's cost
		"caller locals and global reads": numbered(size, "dir=build\n", "f%[1]d(){ local dir=$1; g%[1]d; }\ng%[1]d(){ rm -rf $dir; }\n"),
		"local IFS and global reads":     numbered(size, "dir=build\n", "f%[1]d(){ local IFS=,; g%[1]d; }\ng%[1]d(){ rm -rf $dir; }\n"),
		"heredocs with apostrophes":      fillTo(size, "f(){ local x=1; }\n", "cat <<EOF\ndon't $y\nEOF\necho '$x'\n", "f\n"),
		// The version rules: one finding per feature, a lookup per negative subscript.
		"many case modifications":         fillTo(size, "echo", " ${v,,}", "\n"),
		"many associative declarations":   fillTo(size, "#!/bin/bash\n", "declare -A m\n", ""),
		"one declaration with many names": fillTo(size, "declare -A", " m", "\n"),
		"many negative subscripts":        fillTo(size, "declare -a a\n", "echo \"${a[-1]}\"\n", ""),
		"many negative assoc subscripts":  fillTo(size, "declare -A a\n", "echo \"${a[-1]}\"\n", ""),
		"many epoch reads":                fillTo(size, "echo", " $EPOCHSECONDS", "\n"),
		"many shopt and wait calls":       fillTo(size, "", "shopt -s globstar; sleep 0 & wait -n -p v\n", ""),
		"many bash-only forms in sh":      fillTo(size, "#!/bin/sh\n", "[[ -n $x ]] && cat <<< hi <(a) $'x'\n", ""),
		"many guarded features":           fillTo(size, "(( BASH_VERSINFO[0] >= 4 ))\n", "declare -A m\n", ""),
	}
	for name, src := range cases {
		f := parse.File("/ws/a.sh", []byte(src))
		if f.Skipped != "" {
			t.Fatalf("%s: skipped: %s", name, f.Skipped)
		}
		s := Build(map[string]*FileFacts{"/ws/a.sh": Bind(f)}, Options{Root: "/ws"}, 1)
		var n int
		withinBound(t, name, bound, func() { n = len(s.Diagnostics("/ws/a.sh", DiagOptions{})) })
		t.Logf("%s: %d findings", name, n)
	}
}

// A build from the previous snapshot reuses the scans and mentions of the
// files whose facts it shares; the edited file is scanned afresh.
func TestDiagScansCarryOver(t *testing.T) {
	a, b := "/w/a.sh", "/w/b.sh"
	fa := Bind(parse.File(a, []byte("#!/bin/bash\nsource ./b.sh\nf() { local u=1; }\nf\n")))
	fb := Bind(parse.File(b, []byte("g() { echo \"$u\"; }\n")))
	s1 := Build(map[string]*FileFacts{a: fa, b: fb}, Options{Root: "/w"}, 1)
	s1.Diagnostics(a, DiagOptions{})
	if s1.scans[fb] == nil || s1.mentions[fb] == nil {
		t.Fatal("the visible file was not scanned")
	}
	fa2 := Bind(parse.File(a, []byte("#!/bin/bash\nsource ./b.sh\nf() { local u=2; }\nf\n")))
	s2 := Build(map[string]*FileFacts{a: fa2, b: fb}, Options{Root: "/w", Prev: s1}, 2)
	if s2.scans[fb] != s1.scans[fb] || s2.mentions[fb] != s1.mentions[fb] {
		t.Fatal("the unchanged file's scan was not carried over")
	}
	if s2.scans[fa] != nil || s2.scans[fa2] != nil {
		t.Fatal("a stale or premature scan of the edited file")
	}
	s2.Diagnostics(a, DiagOptions{})
	if s2.scans[fa2] == nil {
		t.Fatal("the edited file was not scanned")
	}
}

// numbered is about size bytes: pre, then unit formatted with 0, 1, 2, ...
func numbered(size int, pre, unit string) string {
	var sb strings.Builder
	sb.WriteString(pre)
	for i := 0; sb.Len() < size-len(unit)-16; i++ {
		fmt.Fprintf(&sb, unit, i)
	}
	return sb.String()
}

// bash -n exits 0 for a here-document that runs to the end of the file (bash
// warns when it runs it), so the finding is a warning, not an error.
func TestUnclosedHeredocIsAWarning(t *testing.T) {
	src := "cat <<EOT\nnever closed\n"
	ds := ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1001")
	if len(ds) != 1 || ds[0].Severity != SevWarning || !strings.Contains(ds[0].Message, "unclosed here-document") {
		t.Fatalf("%s", describe(src, ds))
	}
	// another syntax error in the same file stays an error
	src = "echo \"abc\nf() { :; }\n"
	if ds := ofRule(diagsIn(map[string]string{"y.sh": src}, "y.sh"), "WB1001"); len(ds) != 1 || ds[0].Severity != SevError {
		t.Fatalf("%s", describe(src, ds))
	}
}

// The here-document is a warning only when nothing is left open at it: each
// source below is as `bash -n` judges it (3.2 and 5.3 agree on all of them).
func TestUnclosedHeredocSeverityFollowsBash(t *testing.T) {
	for src, want := range map[string]Severity{
		"cat <<EOF\nx\n":                     SevWarning,
		"cat <<-EOF\nx\n":                    SevWarning,
		"cat <<'EOF'\nx\n":                   SevWarning,
		"cat <<A <<B\nx\n":                   SevWarning,
		"cat <<EOF; echo ok\nx\n":            SevWarning,
		"cat <<EOF\nx":                       SevWarning,
		"echo a\nf() { :; }\ncat <<EOF\nx\n": SevWarning,
		"f() {\n cat <<EOF\nx\n":             SevError,
		"if a; then cat <<EOF\nx\n":          SevError,
		"( cat <<EOF\nx\n":                   SevError,
		"{ cat <<EOF\nx\n":                   SevError,
		"while a; do cat <<EOF\nx\n":         SevError,
		"for i in 1; do cat <<EOF\nx\n":      SevError,
		"case x in a) cat <<EOF\nx\n":        SevError,
		"x=$(cat <<EOF\nx\n":                 SevError,
	} {
		ds := ofRule(diagsIn(map[string]string{"x.sh": src}, "x.sh"), "WB1001")
		var got []Severity
		for _, d := range ds {
			got = append(got, d.Severity)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%q: severities %v, want [%d]", src, got, want)
		}
	}
}
