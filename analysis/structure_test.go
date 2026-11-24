package analysis

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

const foldSrc = "# one\n# two\n# three\nf() {\n  if true; then\n    cat <<EOF\nbody\nEOF\n  fi\n}\n# region setup\nx=1\n# endregion\n"

func foldStrs(fs []Fold) string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("(%d,%d,%s)", f.StartLine, f.EndLine, f.Kind))
	}
	return strings.Join(out, " ")
}

func TestFolds(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": foldSrc})
	got := foldStrs(q.s.Folds(q.abs("a.sh")))
	want := "(0,2,comment) (3,9,) (4,8,) (5,7,) (10,12,region)"
	if got != want {
		t.Fatalf("folds %s, want %s", got, want)
	}
}

func TestFoldsOtherShapes(t *testing.T) {
	src := "#!/bin/sh\n# only\nx=1\n( echo a\n  echo b )\n{ echo a\n  echo b; }\nwhile true; do\n  :\ndone\ncase a in\n  a) :;;\nesac\nif true; then :; fi\n# lone region\n# endregion\n# stray\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	got := foldStrs(q.s.Folds(q.abs("a.sh")))
	want := "(3,4,) (5,6,) (7,9,) (10,12,)"
	if got != want {
		t.Fatalf("folds %s, want %s", got, want)
	}
}

func TestFoldsUnknownPath(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "x=1\n"})
	if q.s.Folds("/nope") != nil || q.s.SelectionChain("/nope", 0) != nil || q.s.Highlights("/nope", 0) != nil || q.s.Links("/nope") != nil {
		t.Fatal("unknown path must answer nothing")
	}
}

func text(src []byte, r tree.Range) string { return string(src[r.Start.Offset:r.End.Offset]) }

func TestSelectionChain(t *testing.T) {
	src := "f() { echo \"$x\"; }\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	p := q.abs("a.sh")
	chain := q.s.SelectionChain(p, strings.Index(src, "$x")+1)
	var got []string
	for _, r := range chain {
		got = append(got, text([]byte(src), r))
	}
	want := []string{"x", "$x", "\"$x\"", "echo \"$x\"", "{ echo \"$x\"; }", "f() { echo \"$x\"; }", src}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("chain %q, want %q", got, want)
	}
	for i := 1; i < len(chain); i++ {
		a, b := chain[i-1], chain[i]
		if b.Start.Offset > a.Start.Offset || b.End.Offset < a.End.Offset || a == b {
			t.Fatalf("level %d does not strictly contain level %d: %v %v", i, i-1, b, a)
		}
	}
}

func TestSelectionDepthCapped(t *testing.T) {
	// The parser accepts about 2,500 nested subshells; the cap applies below
	// that.
	mk := func(n int) string { return strings.Repeat("( ", n) + "echo hi" + strings.Repeat(" )", n) + "\n" }
	src := mk(2000)
	q := buildMem(t, map[string]string{"a.sh": src})
	chain := q.s.SelectionChain(q.abs("a.sh"), strings.Index(src, "hi"))
	if len(chain) != maxSelectionDepth {
		t.Fatalf("%d levels, want %d", len(chain), maxSelectionDepth)
	}
	if got := text([]byte(src), chain[0]); got != "hi" {
		t.Fatalf("innermost %q", got)
	}
	last := chain[len(chain)-1]
	if last.End.Offset-last.Start.Offset >= len(src)-1 {
		t.Fatal("the outermost levels were not the ones dropped")
	}
}

func TestHighlightsLocalName(t *testing.T) {
	q := buildCorpus(t)
	p, off := q.at("main.sh", `local name="$1"`, 0, 6)
	var got []string
	for _, h := range q.s.Highlights(p, off) {
		got = append(got, fmt.Sprintf("%d:%v", h.Range.Start.Line, h.Write))
	}
	if want := "5:true 6:false 7:false"; strings.Join(got, " ") != want {
		t.Fatalf("highlights %v, want %s", got, want)
	}
	// from a read, the same set; the other function's local is a different symbol
	p, off = q.at("main.sh", `build_release "$name"`, 0, 17)
	var again []string
	for _, h := range q.s.Highlights(p, off) {
		again = append(again, fmt.Sprintf("%d:%v", h.Range.Start.Line, h.Write))
	}
	if strings.Join(again, " ") != strings.Join(got, " ") {
		t.Fatalf("from a read: %v", again)
	}
	p, off = q.at("main.sh", `local name=zzz`, 0, 6)
	for _, h := range q.s.Highlights(p, off) {
		if h.Range.Start.Line < 14 {
			t.Fatalf("other(): highlight in deploy at line %d", h.Range.Start.Line)
		}
	}
}

func TestHighlightsFunctionCallsAreReads(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nf\nf\n"})
	p, off := q.at("a.sh", "f() {", 0, 0)
	var got []string
	for _, h := range q.s.Highlights(p, off) {
		got = append(got, fmt.Sprintf("%d:%v", h.Range.Start.Line, h.Write))
	}
	if want := "1:true 2:false 3:false"; strings.Join(got, " ") != want {
		t.Fatalf("highlights %v, want %s", got, want)
	}
	if hs := q.s.Highlights(p, 100000); len(hs) != 0 {
		t.Fatalf("nothing under the cursor: %v", hs)
	}
}

func linkStrs(q *qworld, ls []Link) []string {
	var out []string
	for _, l := range ls {
		out = append(out, fmt.Sprintf("%d:%s|%s", l.Range.Start.Line, q.rel(l.Target), l.Tooltip))
	}
	return out
}

func TestAC09DocumentLink(t *testing.T) {
	q := buildCorpus(t)
	got := linkStrs(q, q.s.Links(q.abs("dyn.sh")))
	found := false
	for _, g := range got {
		if g == "3:lib/util.sh|source → lib/util.sh" {
			found = true
		}
	}
	if !found {
		t.Fatalf("links of dyn.sh: %v", got)
	}
	// the link covers the path word
	for _, l := range q.s.Links(q.abs("dyn.sh")) {
		if l.Range.Start.Line == 3 && text(q.src("dyn.sh"), l.Range) != `"$HERE/lib/util.sh"` {
			t.Fatalf("link range %q", text(q.src("dyn.sh"), l.Range))
		}
	}
}

func TestLinksGlobLoaderGivesNone(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh":   "for f in mods/*.sh; do\n  source \"$f\"\ndone\n",
		"mods/a.sh": "a() { :; }\n",
		"mods/b.sh": "b() { :; }\n",
	})
	if got := q.s.SourceEdges(q.abs("main.sh")); len(got) != 1 || len(got[0].Targets) != 2 {
		t.Fatalf("setup: the loader should have two targets: %+v", got)
	}
	if got := linkStrs(q, q.s.Links(q.abs("main.sh"))); len(got) != 0 {
		t.Fatalf("a glob loader got links: %v", got)
	}
}

func TestShebang(t *testing.T) {
	abs := func(p string) string { return filepath.Join(string(filepath.Separator), filepath.FromSlash(p)) }
	q := buildMem(t, map[string]string{
		"a.sh": "#!" + abs("no/such/sh") + "\nx=1\n",
		"b.sh": "#! " + abs("no/such/env") + " bash\n",
		"c.sh": "#!sh\n",
		"d.sh": "x=1\n#!" + abs("no/such/sh") + "\n",
		"e.sh": "#!" + abs("no/such/sh"),
	})
	for _, f := range []string{"a.sh", "b.sh", "e.sh"} {
		l, ok := q.s.Shebang(q.abs(f))
		want := abs(map[string]string{"a.sh": "no/such/sh", "b.sh": "no/such/env", "e.sh": "no/such/sh"}[f])
		if !ok || l.Target != want || text(q.src(f), l.Range) != want || l.Range.Start.Line != 1 {
			t.Fatalf("%s: %+v %v", f, l, ok)
		}
		if got := q.s.Links(q.abs(f)); len(got) != 0 {
			t.Fatalf("%s: Links holds the shebang: %+v", f, got)
		}
	}
	for _, f := range []string{"c.sh", "d.sh"} {
		if l, ok := q.s.Shebang(q.abs(f)); ok {
			t.Fatalf("%s: %+v", f, l)
		}
	}
	if _, ok := q.s.Shebang("/nope"); ok {
		t.Fatal("unknown path")
	}
}

func TestLinksInvokedFile(t *testing.T) {
	q := buildMem(t, map[string]string{"run.sh": "./tool.sh arg\n", "tool.sh": "echo hi\n"})
	got := linkStrs(q, q.s.Links(q.abs("run.sh")))
	if len(got) != 1 || got[0] != "1:tool.sh|invokes → tool.sh" {
		t.Fatalf("links: %v (%s)", got, filepath.Base(q.root))
	}
}

func TestHighlightsStayInTheFile(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh": "f() { :; }\n",
		"a.sh":   ". ./lib.sh\nf\n",
		"b.sh":   ". ./lib.sh\nf\nf\n",
	})
	src := string(q.src("a.sh"))
	off := strings.LastIndex(src, "f")
	got := q.s.Highlights(q.abs("a.sh"), off)
	if len(got) != 1 || got[0].Range.Start.Offset != off {
		t.Fatalf("highlights in a.sh: %+v, want only the call at %d (the declaration is in lib.sh, the other calls in b.sh)", got, off)
	}
	// The same symbol from b.sh: its own two uses.
	if got := q.s.Highlights(q.abs("b.sh"), strings.Index(string(q.src("b.sh")), "f\n")); len(got) != 2 {
		t.Fatalf("highlights in b.sh: %+v", got)
	}
}

func TestHighlightsSkipCandidateAndDynamicSites(t *testing.T) {
	src := "f() { :; }\nf\ncmd=f\n$cmd\n\"$cmd\"\neval \"f\"\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	got := q.s.Highlights(q.abs("a.sh"), 0)
	var lines []int
	for _, h := range got {
		lines = append(lines, h.Range.Start.Line)
	}
	for _, l := range lines {
		if l >= 4 {
			t.Fatalf("a candidate or dynamic site is highlighted (lines %v): %+v", lines, got)
		}
	}
	if len(got) < 2 {
		t.Fatalf("the declaration and the direct call are missing: %+v", got)
	}
}

func TestLinksSkipDynamicSites(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh": "x=./t.sh\nsource \"$x\"\ncmd=./t.sh\n$cmd\nsource ./missing.sh\n",
		"t.sh":    "echo hi\n",
	})
	if got := linkStrs(q, q.s.Links(q.abs("main.sh"))); len(got) != 0 {
		t.Fatalf("dynamic, candidate or unresolved sites got links: %v", got)
	}
}

func TestShebangAndSourceLinkOrder(t *testing.T) {
	abs := filepath.Join(string(filepath.Separator), "no", "such", "sh")
	q := buildMem(t, map[string]string{
		"a.sh":   "#!" + abs + "\n. ./lib.sh\n",
		"lib.sh": "f() { :; }\n",
	})
	sb, ok := q.s.Shebang(q.abs("a.sh"))
	if !ok {
		t.Fatal("no shebang")
	}
	ls := q.s.Links(q.abs("a.sh"))
	if len(ls) != 1 || ls[0].Range.Start.Offset <= sb.Range.Start.Offset {
		t.Fatalf("source link %+v does not come after the shebang %+v", ls, sb)
	}
}
