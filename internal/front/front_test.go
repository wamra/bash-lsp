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

func TestNodeAtHeredocPipeline(t *testing.T) {
	src := "cat <<EOF | grep x\nbody\nEOF\n"
	f := Parse("h.sh", []byte(src), Bash)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	callWord := func(n *tree.Node) string {
		c := n.Enclosing(tree.KCall)
		if n.Kind == tree.KCall {
			c = n
		}
		if c == nil {
			return ""
		}
		return string(src[c.Children[0].Range.Start.Offset:c.Children[0].Range.End.Offset])
	}
	if got := callWord(f.NodeAt(strings.Index(src, "grep"))); got != "grep" {
		t.Errorf("NodeAt(grep) call = %q", got)
	}
	body := f.NodeAt(strings.Index(src, "body"))
	hd := body.Enclosing(tree.KHeredoc)
	if body.Kind != tree.KHeredoc && hd == nil {
		t.Fatalf("NodeAt(body) = %v, not under a heredoc", body.Kind)
	}
	if got := callWord(hd0(body, hd)); got != "" {
		t.Errorf("heredoc body should not be inside a call, got %q", got)
	}
	if st := body.Enclosing(tree.KPipeline); st == nil {
		t.Fatal("body not under the pipeline")
	}
	var cat *tree.Node
	for p := body; p != nil; p = p.Parent {
		if p.Kind == tree.KStmt && p.Parent != nil && p.Parent.Kind == tree.KPipeline {
			cat = p
			break
		}
	}
	if cat == nil || cat != cat.Parent.Children[0] {
		t.Fatal("body must land under the first (cat) stage")
	}
}

func hd0(body, hd *tree.Node) *tree.Node {
	if body.Kind == tree.KHeredoc {
		return body
	}
	return hd
}

func TestDeclOutsideBashMatchesBash(t *testing.T) {
	src := []byte("#!/bin/sh\nf() { local a=1 b; }\n")
	if Detect("x.sh", src) != POSIX {
		t.Fatal("expected POSIX detection")
	}
	shape := func(d Dialect) (name string, assigns []string, vals int) {
		f := Parse("x.sh", src, d)
		if len(f.Errors) != 0 {
			t.Fatal(d, f.Errors)
		}
		dc := find(f, tree.KDecl, "local")
		if dc == nil {
			t.Fatalf("%v: no KDecl", d)
		}
		for _, a := range dc.Children {
			if a.Kind != tree.KAssign {
				t.Fatalf("%v: child %v", d, a.Kind)
			}
			assigns = append(assigns, a.Name)
			vals += len(a.Children)
			if a.Name != "" && string(src[a.NameRange.Start.Offset:a.NameRange.End.Offset]) != a.Name {
				t.Errorf("%v: NameRange for %q", d, a.Name)
			}
		}
		return dc.Name, assigns, vals
	}
	pn, pa, pv := shape(POSIX)
	bn, ba, bv := shape(Bash)
	if pn != "local" || bn != "local" || !reflect.DeepEqual(pa, []string{"a", "b"}) || !reflect.DeepEqual(ba, pa) || pv != bv || pv != 1 {
		t.Fatalf("posix %q %v %d, bash %q %v %d", pn, pa, pv, bn, ba, bv)
	}
}

func TestDeclOutsideBashEdges(t *testing.T) {
	f := Parse("x.sh", []byte("export -r X=\"$y\" Z\nX=1 local y\nlocal\n"), POSIX)
	if len(f.Errors) != 0 {
		t.Fatal(f.Errors)
	}
	ex := find(f, tree.KDecl, "export")
	if ex == nil || len(ex.Children) != 3 || ex.Children[0].Name != "" || ex.Children[1].Name != "X" || ex.Children[2].Name != "Z" {
		t.Fatalf("export: %+v", ex)
	}
	if ex.Children[0].Children[0].Kind != tree.KWord || len(ex.Children[1].Children) != 1 {
		t.Fatal("option word / value child shape")
	}
	if find(f, tree.KDecl, "local") == nil {
		t.Fatal("bare `local` should be a KDecl")
	}
	n := 0
	f.Root.Walk(func(x *tree.Node) bool {
		if x.Kind == tree.KDecl && x.Name == "local" {
			n++
		}
		return true
	})
	if n != 1 {
		t.Fatalf("`X=1 local y` must stay a call; %d local decls", n)
	}
}

// Nodes and child arrays come from shared chunks. Growing one node's Children
// must copy, never write into the array of the node next to it, and every node
// must be its own object with the parent links intact.
func TestChunkedNodesDoNotAlias(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ { // more nodes than the largest chunk
		b.WriteString("f() {\n  if [[ -n \"$a\" ]]; then echo \"${x:-1}\" | cat <<EOF\nbody\nEOF\n  fi\n  case $a in s) :;; esac\n}\n")
	}
	checkNoAliasing(t, Parse("a.sh", []byte(b.String()), Bash), false)
}

// The same after a resync: the broken line is a gap and the tail is lowered by
// a second lowerer with its own chunks, several times.
func TestChunkedNodesDoNotAliasAfterResync(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 150; i++ {
		b.WriteString("f() {\n  if [[ -n \"$a\" ]]; then echo \"${x:-1}\" | cat; fi\n  case $a in s) :;; esac\n}\n")
		if i%50 == 25 {
			b.WriteString("echo \"unterminated\n")
		}
		b.WriteString("g() { echo ok; }\n")
	}
	f := Parse("a.sh", []byte(b.String()), Bash)
	gaps := 0
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KErrorGap {
			gaps++
		}
		return true
	})
	if gaps == 0 || len(f.Errors) == 0 {
		t.Fatalf("input did not resync: %d gaps, %d errors", gaps, len(f.Errors))
	}
	checkNoAliasing(t, f, true)
}

func checkNoAliasing(t *testing.T, f *tree.File, resynced bool) {
	t.Helper()
	var all []*tree.Node
	f.Root.Walk(func(n *tree.Node) bool { all = append(all, n); return true })
	if len(all) < 3*maxChunk {
		t.Fatalf("only %d nodes, the test must span several chunks", len(all))
	}
	seen := map[*tree.Node]bool{}
	before := make([][]*tree.Node, len(all))
	for i, n := range all {
		if seen[n] {
			t.Fatal("a node was returned twice")
		}
		seen[n] = true
		before[i] = append([]*tree.Node(nil), n.Children...)
		for _, c := range n.Children {
			if c.Parent != n {
				t.Fatalf("child %v of %v has parent %v", c.Kind, n.Kind, c.Parent)
			}
		}
	}
	sentinel := &tree.Node{}
	for _, n := range all {
		_ = append(n.Children, sentinel)
	}
	for i, n := range all {
		if !reflect.DeepEqual(n.Children, before[i]) {
			t.Fatalf("appending to a node's children changed another node's array (%v, resynced=%v)", n.Kind, resynced)
		}
	}
}
