package analysis

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProfile is a PATH profile of the given names, each at /usr/bin/NAME.
func fakeProfile(names ...string) func(string) (string, bool) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(name string) (string, bool) {
		if set[name] {
			return "/usr/bin/" + name, true
		}
		return "", false
	}
}

func commandEdges(s *Snapshot, path string) []Edge {
	var out []Edge
	for _, e := range s.Edges() {
		if e.Site.Path == path && e.IsCommand() {
			out = append(out, e)
		}
	}
	return out
}

func TestCommandEdges(t *testing.T) {
	src := "#!/bin/bash\nf() { :; }\nls -l\ndeplyo app\ncd /tmp\nf\necho hi\n./local.sh\n\"$x\" y\neval 'ls'\n"
	s := diagWorld(map[string]string{"x.sh": src}, Options{Commands: fakeProfile("ls", "echo", "cd")})
	p := filepath.Join(diagRoot, "x.sh")
	got := commandEdges(s, p)
	if len(got) != 2 {
		t.Fatalf("command edges = %d, want 2 (ls, deplyo): %+v", len(got), got)
	}
	ls, dep := got[0], got[1]
	if ls.Text != "ls" || ls.Rel != RelCalls || ls.Res != Candidate || ls.Reason != "path-lookup" || ls.ToFile != "/usr/bin/ls" || ls.To != nil || ls.ToDecl != nil {
		t.Errorf("ls edge = %+v", ls)
	}
	if dep.Text != "deplyo" || dep.Rel != RelCalls || dep.Res != Unresolved || dep.Reason != "not-found" || dep.ToFile != "" || dep.To != nil {
		t.Errorf("deplyo edge = %+v", dep)
	}
	if ls.Site.Range.Start.Line != 3 || dep.Site.Range.Start.Line != 4 || ls.Site.Path != p {
		t.Errorf("sites: ls %+v, deplyo %+v", ls.Site, dep.Site)
	}
	if ls.From.Func != nil || ls.From.Path != p {
		t.Errorf("ls caller = %+v, want the top level of x.sh", ls.From)
	}
	// builtins, functions, paths, expansions and strings give no command edge; f keeps its call edge.
	calls := 0
	for _, e := range s.Edges() {
		if !e.IsCommand() && e.Rel == RelCalls && e.To != nil && e.To.Name == "f" {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("call edges to f = %d, want 1", calls)
	}
	ds := ofRule(s.Diagnostics(p, DiagOptions{}), "WB1102")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "deplyo" || ds[0].Severity != SevInfo {
		t.Fatalf("WB1102 = %+v", ds)
	}
	if !strings.Contains(ds[0].Message, "deplyo") || !strings.Contains(ds[0].Message, ReasonNotFound) {
		t.Errorf("message = %q", ds[0].Message)
	}
}

func TestCommandCallerFunction(t *testing.T) {
	s := diagWorld(map[string]string{"x.sh": "g() {\n  deplyo\n}\n"}, Options{Commands: fakeProfile()})
	got := commandEdges(s, filepath.Join(diagRoot, "x.sh"))
	if len(got) != 1 || got[0].From.Func == nil || got[0].From.Func.Name != "g" {
		t.Fatalf("edges = %+v", got)
	}
}

func TestReasonPathLookupRegistered(t *testing.T) {
	_ = reasonCode(ReasonPathLookup) // panics when not registered
	s := diagWorld(map[string]string{"x.sh": "ls\n"}, Options{Commands: fakeProfile("ls")})
	for _, e := range s.Edges() {
		if e.Reason == "path-lookup" {
			return
		}
	}
	t.Fatalf("no path-lookup edge: %+v", s.Edges())
}

func TestNoMissingCommandWithoutProfile(t *testing.T) {
	files := map[string]string{"x.sh": "deplyo app\nls\n"}
	s := diagWorld(files, Options{})
	p := filepath.Join(diagRoot, "x.sh")
	if got := commandEdges(s, p); len(got) != 0 {
		t.Errorf("command edges without a profile: %+v", got)
	}
	if ds := ofRule(s.Diagnostics(p, DiagOptions{}), "WB1102"); len(ds) != 0 {
		t.Errorf("WB1102 without a profile: %+v", ds)
	}
}

// A name defined as a function or alias anywhere in the workspace is a
// candidate call, never a missing command.
func TestMissingCommandSilentCases(t *testing.T) {
	cases := map[string]map[string]string{
		"function in view":         {"x.sh": "f() { :; }\nf\n"},
		"function in another file": {"x.sh": "helper\n", "y.sh": "helper() { :; }\n"},
		"alias":                    {"x.sh": "shopt -s expand_aliases\nalias ll='ls -l'\nll\n"},
		"command -v guard":         {"x.sh": "if command -v fancy >/dev/null; then fancy; fi\n"},
		"type guard":               {"x.sh": "type fancy >/dev/null 2>&1 && fancy\n"},
		"hash guard":               {"x.sh": "hash fancy 2>/dev/null || exit 0\nfancy\n"},
		"graph incomplete":         {"x.sh": "source \"$LIB\"\nfancy\n"},
		"bats":                     {"x.bats": "#!/usr/bin/env bats\n@test \"t\" {\n  run fancy\n  assert_success\n}\n"},
		"command inside a string":  {"x.sh": "eval 'fancy'\ntrap 'fancy' EXIT\n"},
		"variable command":         {"x.sh": "cmd=fancy\n\"$cmd\" x\n"},
		"path word":                {"x.sh": "./fancy\n/opt/x/fancy\n"},
		"alias value":              {"x.sh": "alias a='fancy'\n"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			file := "x.sh"
			if _, ok := files["x.bats"]; ok {
				file = "x.bats"
			}
			s := diagWorld(files, Options{Commands: fakeProfile()})
			if ds := ofRule(s.Diagnostics(filepath.Join(diagRoot, file), DiagOptions{}), "WB1102"); len(ds) != 0 {
				t.Errorf("WB1102 = %+v", ds)
			}
		})
	}
}

func TestMissingCommandSuppression(t *testing.T) {
	src := "echo start\n# bash-lsp disable=WB1102\nfancy\nother\n"
	s := diagWorld(map[string]string{"x.sh": src}, Options{Commands: fakeProfile()})
	ds := ofRule(s.Diagnostics(filepath.Join(diagRoot, "x.sh"), DiagOptions{}), "WB1102")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "other" {
		t.Fatalf("WB1102 = %+v, want only `other` (the directive covers the next command)", ds)
	}
	off := s.Diagnostics(filepath.Join(diagRoot, "x.sh"), DiagOptions{Disabled: map[string]bool{"WB1102": true}})
	if len(ofRule(off, "WB1102")) != 0 {
		t.Error("a disabled rule still reports")
	}
}

// Command edges are not part of the graph: Callers, Callees, Impact and
// Unresolved of the corpus are the same with and without a profile.
func TestCommandEdgesDoNotChangeGraphOutput(t *testing.T) {
	files := map[string][]byte{}
	root, _ := filepath.Abs("../testdata/corpus")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".sh") || strings.HasSuffix(p, ".bash") {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			files[p] = b
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("corpus: %v (%d files)", err, len(files))
	}
	plain := Analyze(files, Options{Root: root})
	prof := Analyze(files, Options{Root: root, Commands: fakeProfile("echo", "ls", "cat", "grep", "rm")})
	if len(prof.Edges()) <= len(plain.Edges()) {
		t.Fatalf("the profile added no edge to the corpus (%d vs %d): the test checks nothing", len(prof.Edges()), len(plain.Edges()))
	}
	node := func(n Node) string {
		if n.Func == nil {
			return n.Path
		}
		return n.Path + "#" + n.Func.Name
	}
	step := func(st Step) string {
		e := st.Edge
		return fmt.Sprintf("%s -> %s %s/%s/%s @%s:%d", node(st.From), node(st.To), e.Rel, e.Res, e.Reason, e.Site.Path, e.Site.Range.Start.Offset)
	}
	summarize := func(s *Snapshot) string {
		ns, err := s.Lookup("build_release")
		if err != nil || len(ns) == 0 {
			t.Fatalf("Lookup(build_release): %v, %d", err, len(ns))
		}
		var sb strings.Builder
		for _, n := range ns {
			fmt.Fprintf(&sb, "node %s\n", node(n))
			for _, st := range s.Callers(n, 3) {
				fmt.Fprintf(&sb, "caller %s\n", step(st))
			}
			for _, st := range s.Callees(n, 3) {
				fmt.Fprintf(&sb, "callee %s\n", step(st))
			}
			for _, m := range s.Impact(n, 3) {
				fmt.Fprintf(&sb, "impact %s\n", node(m))
			}
		}
		for _, e := range s.Unresolved(nil) {
			fmt.Fprintf(&sb, "unresolved %s %s/%s @%s:%d %q\n", e.Rel, e.Res, e.Reason, e.Site.Path, e.Site.Range.Start.Offset, e.Text)
		}
		return sb.String()
	}
	if a, b := summarize(plain), summarize(prof); a != b {
		t.Errorf("graph output differs with a profile:\n--- without\n%s\n--- with\n%s", a, b)
	}
}

func missingIn(files map[string]string, file string) []Diagnostic {
	s := diagWorld(files, Options{Commands: fakeProfile("echo", "export")})
	return ofRule(s.Diagnostics(filepath.Join(diagRoot, file), DiagOptions{}), "WB1102")
}

// A file that sets PATH may find commands the profile does not know.
func TestMissingCommandSilentWhenPathIsSet(t *testing.T) {
	silent := map[string]string{
		"assign":     "PATH=\"$PWD/bin:$PATH\"\ndeploy\n",
		"export":     "export PATH=\"$PWD/bin:$PATH\"\ndeploy\n",
		"append":     "PATH+=:/opt/x\ndeploy\n",
		"declare -x": "declare -x PATH=/opt/x\ndeploy\n",
		"local":      "f() {\n  local PATH=/opt/x\n  deploy\n}\n",
		"later":      "deploy\nPATH=/opt/x\n",
	}
	for name, src := range silent {
		t.Run(name, func(t *testing.T) {
			if ds := missingIn(map[string]string{"x.sh": src}, "x.sh"); len(ds) != 0 {
				t.Errorf("WB1102 = %+v", ds)
			}
		})
	}
	reports := map[string]string{
		"read only":  "echo $PATH\ndeploy\n",
		"MANPATH":    "MANPATH=/opt/x\ndeploy\n",
		"other name": "MYPATH=/opt/x\ndeploy\n",
		"path word":  "./deploy\nbin/deploy\n",
	}
	for name, src := range reports {
		t.Run("control "+name, func(t *testing.T) {
			ds := missingIn(map[string]string{"x.sh": src}, "x.sh")
			if name == "path word" {
				if len(ds) != 0 {
					t.Errorf("a path is no lookup: %+v", ds)
				}
			} else if len(ds) != 1 {
				t.Errorf("WB1102 = %+v, want one", ds)
			}
		})
	}
}

func TestMissingCommandPrefixAssignmentSilencesOnlyItsCommand(t *testing.T) {
	src := "PATH=/opt/x deploy\nother\n"
	ds := missingIn(map[string]string{"x.sh": src}, "x.sh")
	if len(ds) != 1 || rangeText(src, ds[0].Range) != "other" {
		t.Fatalf("WB1102 = %+v, want only `other`", ds)
	}
}

func TestMissingCommandPathSetInSourcedFile(t *testing.T) {
	files := map[string]string{
		"main.sh":    "source ./lib/env.sh\ndeploy\n",
		"lib/env.sh": "export PATH=\"$PWD/bin:$PATH\"\n",
		"other.sh":   "deploy\n",
	}
	if ds := missingIn(files, "main.sh"); len(ds) != 0 {
		t.Errorf("sourcing file: %+v", ds)
	}
	if ds := missingIn(files, "other.sh"); len(ds) != 1 {
		t.Errorf("unrelated file: %+v", ds)
	}
	// and the other way: the sourced library is silenced by its sourcer
	files2 := map[string]string{
		"main.sh":  "PATH=/opt/x\nsource ./lib/f.sh\n",
		"lib/f.sh": "deploy\n",
	}
	if ds := missingIn(files2, "lib/f.sh"); len(ds) != 0 {
		t.Errorf("sourced file: %+v", ds)
	}
}
