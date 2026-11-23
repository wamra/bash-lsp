package front

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// funcNames returns the Name of every KFuncDecl in source order.
func funcNames(f *tree.File) []string {
	var out []string
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KFuncDecl {
			out = append(out, n.Name)
		}
		return true
	})
	return out
}

// find returns the first node of kind k with Name name.
func find(f *tree.File, k tree.Kind, name string) *tree.Node {
	var got *tree.Node
	f.Root.Walk(func(n *tree.Node) bool {
		if got == nil && n.Kind == k && n.Name == name {
			got = n
		}
		return got == nil
	})
	return got
}

func TestDetect(t *testing.T) {
	cases := []struct {
		path, src string
		want      Dialect
	}{
		{"a.sh", "#!/usr/bin/env bash\n", Bash}, {"a.sh", "#!/bin/sh\n", POSIX}, {"a", "#!/bin/dash\n", POSIX},
		{"a.sh", "#!/bin/zsh\n", Zsh}, {"a.zsh", "", Zsh}, {"t.bats", "", Bats}, {"a.ksh", "", Mksh},
		{"a.sh", "#!/bin/sh\n# shellcheck shell=bash\n", Bash}, {"noext", "echo hi\n", Bash},
		// extra pins: env unwrapping with flags, ksh/mksh/ash/bats shebangs, extension fallbacks
		{"a", "#!/usr/bin/env -S bash -e\n", Bash}, {"a", "#!/usr/bin/env ksh\n", Mksh},
		{"a", "#!/bin/mksh\n", Mksh}, {"a", "#!/bin/ash\n", POSIX}, {"a", "#!/usr/bin/env bats\n", Bats},
		{"a.bash", "", Bash}, {"a.mksh", "", Mksh}, {"a.SH", "", Bash},
		{"a", "#!/bin/sh\n# shellcheck shell=dash\n", POSIX},
		{"a", "#!/bin/bash\n\n\n\n\n# shellcheck shell=sh\n", Bash}, // 6th line: ignored
		{"a", "#!/usr/bin/python\n", Bash},
	}
	for _, c := range cases {
		if got := Detect(c.path, []byte(c.src)); got != c.want {
			t.Errorf("%s %q: %v want %v", c.path, c.src, got, c.want)
		}
	}
}

func TestDialectString(t *testing.T) {
	want := map[Dialect]string{Bash: "bash", POSIX: "sh", Mksh: "mksh", Bats: "bats", Zsh: "zsh"}
	for d, s := range want {
		if d.String() != s {
			t.Errorf("%d: %q want %q", d, d.String(), s)
		}
	}
}

func TestLowerCorpusMain(t *testing.T) {
	src := mustRead(t, "../../testdata/corpus/main.sh")
	f := Parse("main.sh", src, Bash)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	fns := funcNames(f)
	if !reflect.DeepEqual(fns, []string{"deploy", "other", "unquoted"}) {
		t.Fatal(fns)
	}
	d := find(f, tree.KFuncDecl, "deploy")
	if got := string(src[d.NameRange.Start.Offset:d.NameRange.End.Offset]); got != "deploy" {
		t.Fatal(got)
	}
	if d.NameRange.Start.Line != 4 || d.NameRange.Start.Col != 1 {
		t.Fatalf("%+v", d.NameRange.Start)
	}
	if len(d.Children) != 1 {
		t.Fatalf("FuncDecl must have exactly one child (the body), got %d", len(d.Children))
	}
	var locals []string
	d.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KDecl && n.Name == "local" {
			for _, a := range n.Children {
				locals = append(locals, a.Name)
			}
		}
		return true
	})
	if !reflect.DeepEqual(locals, []string{"name", "cmd"}) {
		t.Fatal(locals)
	}
}

func TestLowerShapes(t *testing.T) {
	src := "a=1 b=2 cmd x \"$y\"\n" +
		"for x in 1 2; do echo $x; done\n" +
		"(echo sub)\n" +
		"a | b | c\n" +
		"a && b || c\n" +
		"export FOO=1 BAR\n"
	f := Parse("s.sh", []byte(src), Bash)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	kinds := func(n *tree.Node) []tree.Kind {
		var ks []tree.Kind
		for _, c := range n.Children {
			ks = append(ks, c.Kind)
		}
		return ks
	}
	only := func(k tree.Kind) []*tree.Node {
		var out []*tree.Node
		f.Root.Walk(func(n *tree.Node) bool {
			if n.Kind == k {
				out = append(out, n)
			}
			return true
		})
		return out
	}
	call := only(tree.KCall)[0]
	if got := kinds(call); !reflect.DeepEqual(got, []tree.Kind{tree.KAssign, tree.KAssign, tree.KWord, tree.KWord, tree.KWord}) {
		t.Fatalf("call children %v", got)
	}
	if call.Children[0].Name != "a" || call.Children[1].Name != "b" {
		t.Fatal("assign names")
	}
	fr := only(tree.KFor)[0]
	if fr.Name != "x" || string(src[fr.NameRange.Start.Offset:fr.NameRange.End.Offset]) != "x" {
		t.Fatalf("for name %q %+v", fr.Name, fr.NameRange)
	}
	if got := kinds(fr); !reflect.DeepEqual(got, []tree.Kind{tree.KWord, tree.KWord, tree.KBlock}) {
		t.Fatalf("for children %v", got)
	}
	if len(only(tree.KSubshell)) != 1 {
		t.Fatal("subshell")
	}
	pl := only(tree.KPipeline)
	if len(pl) != 1 || len(pl[0].Children) != 3 {
		t.Fatalf("pipeline stages: %d pipelines", len(pl))
	}
	if len(only(tree.KAndOr)) != 2 {
		t.Fatalf("andor count %d", len(only(tree.KAndOr)))
	}
	dc := only(tree.KDecl)[0]
	if dc.Name != "export" || len(dc.Children) != 2 || dc.Children[0].Name != "FOO" || dc.Children[1].Name != "BAR" {
		t.Fatalf("decl %+v", dc)
	}
	if dc.Children[1].Kind != tree.KAssign || len(dc.Children[1].Children) != 0 {
		t.Fatal("naked decl arg must be a childless KAssign")
	}
}

func TestParentLinksAndRanges(t *testing.T) {
	for _, p := range []string{"main.sh", "app.sh", "lib/util.sh", "lib/hooks.sh", "dyn.sh", "broken_fi.sh", "broken_exp.sh", "orphan.sh"} {
		f := Parse(p, mustRead(t, "../../testdata/corpus/"+p), Bash)
		count := 0
		f.Root.Walk(func(n *tree.Node) bool {
			count++
			if n.Range.Start.Offset > n.Range.End.Offset {
				t.Errorf("%s: %v start %d > end %d", p, n.Kind, n.Range.Start.Offset, n.Range.End.Offset)
			}
			if n == f.Root {
				if n.Parent != nil {
					t.Errorf("%s: root has parent", p)
				}
				return true
			}
			if n.Parent == nil {
				t.Errorf("%s: %v at %d has nil Parent", p, n.Kind, n.Range.Start.Offset)
				return true
			}
			pr := n.Parent.Range
			if n.Range.Start.Offset < pr.Start.Offset || n.Range.End.Offset > pr.End.Offset {
				t.Errorf("%s: %v %v not inside parent %v %v", p, n.Kind, n.Range, n.Parent.Kind, pr)
			}
			found := false
			for _, c := range n.Parent.Children {
				if c == n {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %v not among its parent's children", p, n.Kind)
			}
			return true
		})
		if count < 2 {
			t.Errorf("%s: tree is empty", p)
		}
	}
}

func TestParentCoversHeredocInPipeline(t *testing.T) {
	f := Parse("h.sh", []byte("cat <<EOF | grep x\nbody $v\nEOF\necho after\n"), Bash)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Parent != nil && (n.Range.Start.Offset < n.Parent.Range.Start.Offset || n.Range.End.Offset > n.Parent.Range.End.Offset) {
			t.Errorf("%v %v escapes parent %v %v", n.Kind, n.Range, n.Parent.Kind, n.Parent.Range)
		}
		return true
	})
}

func TestCommentsKept(t *testing.T) {
	f := Parse("util.sh", mustRead(t, "../../testdata/corpus/lib/util.sh"), Bash)
	if len(f.Comments) == 0 || f.Comments[1].Value != "Log a message to stderr." || f.Comments[1].Range.Start.Line != 2 {
		t.Fatalf("%+v", f.Comments)
	}
	if f.Comments[1].Kind != tree.KComment {
		t.Fatal("kind")
	}
}

func TestMissingFiIsReported(t *testing.T) {
	f := Parse("b.sh", mustRead(t, "../../testdata/corpus/broken_fi.sh"), Bash)
	if len(f.Errors) != 1 || !strings.Contains(f.Errors[0].Msg, "fi") {
		t.Fatal(f.Errors)
	}
	if f.Errors[0].Msg != "missing fi" {
		t.Fatalf("msg %q", f.Errors[0].Msg)
	}
	if got := funcNames(f); !reflect.DeepEqual(got, []string{"before_break", "after_fi"}) {
		t.Fatal(got)
	}
	ifn := find(f, tree.KIf, "")
	if ifn == nil || !ifn.Recovered {
		t.Fatalf("KIf must be Recovered: %+v", ifn)
	}
}

func TestHardErrorKeepsStatementsBefore(t *testing.T) {
	src := "ok() { echo hi; }\nfi\n"
	f := Parse("h.sh", []byte(src), Bash)
	if len(f.Errors) != 1 {
		t.Fatalf("errors %+v", f.Errors)
	}
	if got := funcNames(f); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Fatal(got)
	}
	if f.Errors[0].Range.Start.Line != 2 {
		t.Fatalf("error position %+v", f.Errors[0].Range)
	}
}

func TestZshSkipped(t *testing.T) {
	f := Parse("a.zsh", []byte("echo ${foo[(r)b]}\n"), Zsh)
	if f.Skipped == "" || len(f.Errors) != 0 || len(f.Root.Children) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestCRLF(t *testing.T) {
	f := Parse("a.sh", []byte("foo() {\r\n  echo hi\r\n}\r\nfoo\r\n"), Bash)
	if got := funcNames(f); !reflect.DeepEqual(got, []string{"foo"}) {
		t.Fatal(got)
	}
	var last *tree.Node
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KLit && n.Value != "" {
			last = n
		}
		return true
	})
	if last.Value != "foo" {
		t.Fatalf("%q", last.Value)
	}
}

func TestHeredocAndExpansions(t *testing.T) {
	src := "cat <<EOF\nhello $name ${x:-d} $(date)\nEOF\necho `id` $((1+n)) <(ls) 'q' \"d$z\"\n"
	f := Parse("h.sh", []byte(src), Bash)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	hd := find(f, tree.KHeredoc, "EOF")
	if hd == nil {
		t.Fatal("no heredoc node with tag EOF")
	}
	have := map[tree.Kind]bool{}
	f.Root.Walk(func(n *tree.Node) bool { have[n.Kind] = true; return true })
	for _, k := range []tree.Kind{tree.KParamExp, tree.KCmdSubst, tree.KArithExp, tree.KProcSubst, tree.KSglQuoted, tree.KDblQuoted, tree.KRedirect} {
		if !have[k] {
			t.Errorf("missing kind %v", k)
		}
	}
	if find(f, tree.KParamExp, "name") == nil || find(f, tree.KParamExp, "x") == nil {
		t.Error("ParamExp names not set")
	}
}
