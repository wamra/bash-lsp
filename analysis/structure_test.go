package analysis

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

const foldSrc = "# one\n# two\n# three\nf() {\n  if true; then\n    cat <<EOF\nbody\nEOF\n  fi\n}\n# region setup\nx=1\n# endregion\n"

// foldStrs lists folds as (first line, last line, kind), lines 0-based and
// counted by LF, CRLF and a lone CR.
func foldStrs(src string, fs []Fold) string {
	line := func(off int) int {
		n := 0
		for i := 0; i < off; i++ {
			if src[i] == '\n' || src[i] == '\r' && (i+1 == len(src) || src[i+1] != '\n') {
				n++
			}
		}
		return n
	}
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("(%d,%d,%s)", line(f.StartOffset), line(f.EndOffset), f.Kind))
	}
	return strings.Join(out, " ")
}

func TestFolds(t *testing.T) {
	src := foldSrc
	q := buildMem(t, map[string]string{"a.sh": src})
	got := foldStrs(src, q.s.Folds(q.abs("a.sh")))
	want := "(0,2,comment) (3,9,) (4,8,) (5,7,) (10,12,region)"
	if got != want {
		t.Fatalf("folds %s, want %s", got, want)
	}
}

func TestFoldsOtherShapes(t *testing.T) {
	src := "#!/bin/sh\n# only\nx=1\n( echo a\n  echo b )\n{ echo a\n  echo b; }\nwhile true; do\n  :\ndone\ncase a in\n  a) :;;\nesac\nif true; then :; fi\n# lone region\n# endregion\n# stray\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	got := foldStrs(src, q.s.Folds(q.abs("a.sh")))
	want := "(3,4,) (5,6,) (7,9,) (10,12,)"
	if got != want {
		t.Fatalf("folds %s, want %s", got, want)
	}
}

// Folds carry byte offsets, so they do not depend on how lines are counted.
func TestFoldsOnLoneCRText(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"function", "# a\rb\nf() {\n  echo hi\n  echo ho\n}\n", "(0,1,comment) (2,5,)"},
		{"comment run and region", "x=1\r# c1\n# c2\n# region r\nf() {\n  echo hi\n  echo ho\n}\n# endregion\n", "(1,2,comment) (3,8,region) (4,7,)"},
		{"lone CR between comments", "# c1\n\r# c2\n", ""},
		{"one tool line", "f() { :;\r}\n", "(0,1,)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := buildMem(t, map[string]string{"a.sh": tc.src})
			if got := foldStrs(tc.src, q.s.Folds(q.abs("a.sh"))); got != tc.want {
				t.Fatalf("folds %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFoldsUnknownPath(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "x=1\n"})
	if q.s.Folds("/nope") != nil || chainOf(q.s, "/nope", 0) != nil || hlOf(q.s, "/nope", 0) != nil || lkOf(q.s, "/nope") != nil {
		t.Fatal("unknown path must answer nothing")
	}
}

func text(src []byte, r tree.Range) string { return string(src[r.Start.Offset:r.End.Offset]) }

func TestSelectionChain(t *testing.T) {
	src := "f() { echo \"$x\"; }\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	p := q.abs("a.sh")
	chain := chainOf(q.s, p, strings.Index(src, "$x")+1)
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
	chain := chainOf(q.s, q.abs("a.sh"), strings.Index(src, "hi"))
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
	for _, h := range hlOf(q.s, p, off) {
		got = append(got, fmt.Sprintf("%d:%v", h.Range.Start.Line, h.Write))
	}
	if want := "5:true 6:false 7:false"; strings.Join(got, " ") != want {
		t.Fatalf("highlights %v, want %s", got, want)
	}
	// from a read, the same set; the other function's local is a different symbol
	p, off = q.at("main.sh", `build_release "$name"`, 0, 17)
	var again []string
	for _, h := range hlOf(q.s, p, off) {
		again = append(again, fmt.Sprintf("%d:%v", h.Range.Start.Line, h.Write))
	}
	if strings.Join(again, " ") != strings.Join(got, " ") {
		t.Fatalf("from a read: %v", again)
	}
	p, off = q.at("main.sh", `local name=zzz`, 0, 6)
	for _, h := range hlOf(q.s, p, off) {
		if h.Range.Start.Line < 14 {
			t.Fatalf("other(): highlight in deploy at line %d", h.Range.Start.Line)
		}
	}
}

func TestHighlightsFunctionCallsAreReads(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nf\nf\n"})
	p, off := q.at("a.sh", "f() {", 0, 0)
	var got []string
	for _, h := range hlOf(q.s, p, off) {
		got = append(got, fmt.Sprintf("%d:%v", h.Range.Start.Line, h.Write))
	}
	if want := "1:true 2:false 3:false"; strings.Join(got, " ") != want {
		t.Fatalf("highlights %v, want %s", got, want)
	}
	if hs := hlOf(q.s, p, 100000); len(hs) != 0 {
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
	got := linkStrs(q, lkOf(q.s, q.abs("dyn.sh")))
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
	for _, l := range lkOf(q.s, q.abs("dyn.sh")) {
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
	if got := linkStrs(q, lkOf(q.s, q.abs("main.sh"))); len(got) != 0 {
		t.Fatalf("a glob loader got links: %v", got)
	}
}

func TestShebang(t *testing.T) {
	abs := func(p string) string { return "/" + p }
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
		if got := lkOf(q.s, q.abs(f)); len(got) != 0 {
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
	got := linkStrs(q, lkOf(q.s, q.abs("run.sh")))
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
	got := hlOf(q.s, q.abs("a.sh"), off)
	if len(got) != 1 || got[0].Range.Start.Offset != off {
		t.Fatalf("highlights in a.sh: %+v, want only the call at %d (the declaration is in lib.sh, the other calls in b.sh)", got, off)
	}
	// The same symbol from b.sh: its own two uses.
	if got := hlOf(q.s, q.abs("b.sh"), strings.Index(string(q.src("b.sh")), "f\n")); len(got) != 2 {
		t.Fatalf("highlights in b.sh: %+v", got)
	}
}

func TestHighlightsSkipCandidateAndDynamicSites(t *testing.T) {
	src := "f() { :; }\nf\ncmd=f\n$cmd\n\"$cmd\"\neval \"f\"\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	got := hlOf(q.s, q.abs("a.sh"), 0)
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
	if got := linkStrs(q, lkOf(q.s, q.abs("main.sh"))); len(got) != 0 {
		t.Fatalf("dynamic, candidate or unresolved sites got links: %v", got)
	}
}

func TestShebangAndSourceLinkOrder(t *testing.T) {
	abs := "/no/such/sh"
	q := buildMem(t, map[string]string{
		"a.sh":   "#!" + abs + "\n. ./lib.sh\n",
		"lib.sh": "f() { :; }\n",
	})
	sb, ok := q.s.Shebang(q.abs("a.sh"))
	if !ok {
		t.Fatal("no shebang")
	}
	ls := lkOf(q.s, q.abs("a.sh"))
	if len(ls) != 1 || ls[0].Range.Start.Offset <= sb.Range.Start.Offset {
		t.Fatalf("source link %+v does not come after the shebang %+v", ls, sb)
	}
}

func hlOf(s *Snapshot, path string, off int) []Highlight {
	h, _ := s.Highlights(path, off)
	return h
}

func lkOf(s *Snapshot, path string) []Link {
	l, _ := s.Links(path)
	return l
}

// A symbol with more occurrences than maxHighlights is cut to that many, the
// one under the cursor among them wherever it stands, and the cut is reported.
func TestHighlightsCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("f() { :; }\n")
	for i := 0; i < maxHighlights+500; i++ {
		b.WriteString("f\n")
	}
	src := b.String()
	q := buildMem(t, map[string]string{"a.sh": src})
	p := q.abs("a.sh")
	for _, at := range []int{0, len(src) - 2} { // the declaration, the last call
		hs, cut := q.s.Highlights(p, at)
		if len(hs) != maxHighlights || !cut {
			t.Fatalf("cursor at %d: %d highlights, cut %v, want %d and a cut", at, len(hs), cut, maxHighlights)
		}
		found := false
		for i, h := range hs {
			if i > 0 && h.Range.Start.Offset <= hs[i-1].Range.Start.Offset {
				t.Fatalf("not in source order at %d", i)
			}
			found = found || h.Range.Contains(at)
		}
		if !found {
			t.Errorf("cursor at %d: its own occurrence was cut", at)
		}
	}
	// exactly maxHighlights occurrences is not a cut
	q = buildMem(t, map[string]string{"a.sh": "f() { :; }\n" + strings.Repeat("f\n", maxHighlights-1)})
	if hs, cut := q.s.Highlights(q.abs("a.sh"), 0); len(hs) != maxHighlights || cut {
		t.Fatalf("%d highlights, cut %v at the limit", len(hs), cut)
	}
}

// More than maxLinks links are cut to the first maxLinks, in source order.
func TestLinksCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxLinks+500; i++ {
		b.WriteString(". ./lib.sh\n")
	}
	q := buildMem(t, map[string]string{"a.sh": b.String(), "lib.sh": ":\n"})
	ls, cut := q.s.Links(q.abs("a.sh"))
	if len(ls) != maxLinks || !cut {
		t.Fatalf("%d links, cut %v, want %d and a cut", len(ls), cut, maxLinks)
	}
	if ls[0].Range.Start.Line != 1 || ls[maxLinks-1].Range.Start.Line != maxLinks {
		t.Fatalf("not the first %d: lines %d..%d", maxLinks, ls[0].Range.Start.Line, ls[maxLinks-1].Range.Start.Line)
	}
	q = buildMem(t, map[string]string{"a.sh": strings.Repeat(". ./lib.sh\n", maxLinks), "lib.sh": ":\n"})
	if ls, cut := q.s.Links(q.abs("a.sh")); len(ls) != maxLinks || cut {
		t.Fatalf("%d links, cut %v at the limit", len(ls), cut)
	}
}

// The shebang interpreter is a link candidate only when it is a plain
// slash-separated absolute path: no UNC form (two leading separators, either
// kind), no volume, no backslash, no dot segment. The rule is the same on
// every platform, so these cases hold on Linux and on Windows alike.
func TestInterpreterPathRefusesNetworkAndVolumeForms(t *testing.T) {
	for _, p := range []string{"/bin/sh", "/usr/bin/bash", "/opt/homebrew/bin/bash", "/usr/local/bin/my-tool_2.1"} {
		if !interpreterPath(p) {
			t.Errorf("%q is refused", p)
		}
	}
	for _, p := range []string{
		"", "/", "sh", "bin/sh", "./sh", "/usr/bin/", "/usr//bin/sh",
		"//host/share/bash", "//host", `\\host\share\bash`, `\\?\C:\bash`, `\\.\pipe\x`, "\\/host/share/bash", "/\\host/share/bash",
		`C:\Git\bin\bash.exe`, "C:/Git/bin/bash.exe", "c:bash", "/C:/Git/bin/bash", "/c:\\bin\\bash",
		"/usr/bin/../../tmp/x", "/usr/./bin/sh", "/usr/bin/..", "/bin/sh\x00", "/bin\\sh",
	} {
		if interpreterPath(p) {
			t.Errorf("%q is accepted", p)
		}
	}
}

func TestShebangRefusesNetworkForms(t *testing.T) {
	for _, first := range []string{"//host/share/bash", `\\host\share\bash`, "C:/bin/bash", "/usr/bin/../x/sh"} {
		q := buildMem(t, map[string]string{"a.sh": "#!" + first + "\nx=1\n"})
		if l, ok := q.s.Shebang(q.abs("a.sh")); ok {
			t.Errorf("#!%s: link candidate %+v", first, l)
		}
	}
}

func chainOf(s *Snapshot, path string, off int) []tree.Range {
	c, _ := s.SelectionChain(path, off)
	return c
}

// The cut is reported when the nesting is deeper than maxSelectionDepth.
func TestSelectionDepthReportsTheCut(t *testing.T) {
	mk := func(n int) string { return strings.Repeat("( ", n) + "echo hi" + strings.Repeat(" )", n) + "\n" }
	deep := mk(300)
	q := buildMem(t, map[string]string{"a.sh": deep})
	if chain, cut := q.s.SelectionChain(q.abs("a.sh"), strings.Index(deep, "hi")); len(chain) != maxSelectionDepth || !cut {
		t.Fatalf("%d levels, cut %v, want %d and a cut", len(chain), cut, maxSelectionDepth)
	}
	shallow := mk(10)
	q = buildMem(t, map[string]string{"a.sh": shallow})
	if chain, cut := q.s.SelectionChain(q.abs("a.sh"), strings.Index(shallow, "hi")); len(chain) == 0 || cut {
		t.Fatalf("%d levels, cut %v for a shallow file", len(chain), cut)
	}
}
