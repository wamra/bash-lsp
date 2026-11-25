package analysis

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/tree"
)

func whole(s *Snapshot, path string) tree.Range {
	return tree.Range{End: tree.Pos{Offset: len(s.Facts(path).File.Src)}}
}

func hintLabels(hs []Hint) string {
	var out []string
	for _, h := range hs {
		out = append(out, fmt.Sprintf("%d:%d %s", h.Pos.Line, h.Pos.Col, h.Label))
	}
	return strings.Join(out, "|")
}

func TestHintsSourceTargets(t *testing.T) {
	q := buildCorpus(t)
	dyn := filepath.Join(q.root, "dyn.sh")
	got := hintsOf(q.s, dyn, whole(q.s, dyn), true, false)
	if len(got) != 1 || got[0].Label != "→ lib/util.sh" || got[0].Param || got[0].Pos.Line != 3 || got[0].Pos.Col != 27 {
		t.Fatalf("dyn.sh: %+v", got)
	}
	main := filepath.Join(q.root, "main.sh")
	if got := hintsOf(q.s, main, whole(q.s, main), true, false); len(got) != 0 {
		t.Fatalf("a literal source gets no hint: %+v", got)
	}
	if got := hintsOf(q.s, dyn, whole(q.s, dyn), false, false); len(got) != 0 {
		t.Fatalf("sources off: %+v", got)
	}
}

func TestHintsScriptDirAndLoopGlob(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh":  "source \"${0%/*}/lib/a.sh\"\nfor f in ./lib/*.sh; do source \"$f\"; done\n",
		"lib/a.sh": "a() { :; }\n",
		"lib/b.sh": "b() { :; }\n",
	})
	p := filepath.Join(q.root, "main.sh")
	var how []string
	for _, e := range q.s.SourceEdges(p) {
		how = append(how, e.How)
	}
	if strings.Join(how, ",") != "script-dir,loop-glob" {
		t.Fatalf("edges resolved by %v", how)
	}
	got := hintsOf(q.s, p, whole(q.s, p), true, false)
	if hintLabels(got) != "1:26 → lib/a.sh|2:36 → 2 files" {
		t.Fatalf("%s", hintLabels(got))
	}
}

func TestHintsDirectiveTarget(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh":  "# shellcheck source=lib/a.sh\nsource \"$X\"\n",
		"lib/a.sh": "a() { :; }\n",
	})
	p := filepath.Join(q.root, "main.sh")
	got := hintsOf(q.s, p, whole(q.s, p), true, false)
	if len(got) != 1 || got[0].Label != "→ lib/a.sh" {
		t.Fatalf("%+v", got)
	}
}

func TestHintsParameterNames(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "cp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 a b c\nf() { echo \"$1\"; }\nf x\n"})
	p := filepath.Join(q.root, "a.sh")
	if got := hintsOf(q.s, p, whole(q.s, p), true, false); len(got) != 0 {
		t.Fatalf("parameter names off: %+v", got)
	}
	got := hintsOf(q.s, p, whole(q.s, p), false, true)
	// `c` has no parameter, and `f`'s only parameter has no name.
	if hintLabels(got) != "2:5 src:|2:7 dst:" {
		t.Fatalf("%s", hintLabels(got))
	}
	for _, h := range got {
		if !h.Param {
			t.Fatalf("not a parameter hint: %+v", h)
		}
	}
}

func TestHintsWithin(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "cp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 a b\ncp2 c d\n"})
	p := filepath.Join(q.root, "a.sh")
	src := string(q.s.Facts(p).File.Src)
	at := strings.Index(src, "cp2 c")
	within := tree.Range{Start: tree.Pos{Offset: at}, End: tree.Pos{Offset: len(src)}}
	if got := hintsOf(q.s, p, within, false, true); hintLabels(got) != "3:5 src:|3:7 dst:" {
		t.Fatalf("%s", hintLabels(got))
	}
	// A window that starts inside the call, after its command word.
	mid := tree.Range{Start: tree.Pos{Offset: at + 6}, End: tree.Pos{Offset: len(src)}}
	if got := hintsOf(q.s, p, mid, false, true); hintLabels(got) != "3:7 dst:" {
		t.Fatalf("%s", hintLabels(got))
	}
	inverted := tree.Range{Start: tree.Pos{Offset: len(src)}, End: tree.Pos{Offset: 0}}
	if got := hintsOf(q.s, p, inverted, true, true); len(got) != 0 {
		t.Fatalf("inverted range: %+v", got)
	}
	if got := hintsOf(q.s, "/nowhere.sh", within, true, true); got != nil {
		t.Fatalf("unknown path: %+v", got)
	}
}

func TestHintsCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("f() { local x=\"$1\"; }\n")
	for i := 0; i < maxHints+500; i++ {
		b.WriteString("f a\n")
	}
	q := buildMem(t, map[string]string{"a.sh": b.String()})
	p := filepath.Join(q.root, "a.sh")
	got := hintsOf(q.s, p, whole(q.s, p), true, true)
	if len(got) != maxHints {
		t.Fatalf("%d hints, want %d", len(got), maxHints)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Pos.Offset <= got[i-1].Pos.Offset {
			t.Fatalf("not in position order at %d", i)
		}
	}
	if got[0].Pos.Line != 2 || got[maxHints-1].Pos.Line != maxHints+1 {
		t.Fatalf("not the first %d: lines %d..%d", maxHints, got[0].Pos.Line, got[maxHints-1].Pos.Line)
	}
}

// A function that reads `${N}` for a huge N names at most maxParams parameters.
func TestHintsHugeParameterIndex(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { echo \"${999999999}\"; }\nf a b c\n"})
	p := filepath.Join(q.root, "a.sh")
	if got := hintsOf(q.s, p, whole(q.s, p), false, true); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	sig, _, ok := sigAt(t, "f() { echo \"${999999999}\"; }\nf ▮")
	if !ok || len(sig.Params) != maxParams {
		t.Fatalf("declared parameters %d, want the cap %d (ok %v)", len(sig.Params), maxParams, ok)
	}
}

// A word that contains a spread expansion anywhere is a spread: no hint on
// it and none after it; hints before it stay. A count (`${#arr[@]}`) and a
// single-quoted `'$@'` are one word each.
func TestHintsSpreadInsideWord(t *testing.T) {
	const pre = "cp2() { local src=\"$1\" dst=\"$2\"; }\n"
	names := func(call string) string {
		q := buildMem(t, map[string]string{"a.sh": pre + call + "\n"})
		p := filepath.Join(q.root, "a.sh")
		var out []string
		for _, h := range hintsOf(q.s, p, whole(q.s, p), false, true) {
			out = append(out, h.Label)
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ arg, before, after string }{
		{`pre"$@"`, "src:", ""}, {`a$@`, "src:", ""}, {`"${arr[@]:1}"`, "src:", ""},
		{`"${@:2}"`, "src:", ""}, {`${!arr[@]}`, "src:", ""}, {`"${arr[*]}"`, "src:", ""},
		{`"x${*}y"`, "src:", ""}, {`"${#arr[@]}"`, "src: dst:", "src: dst:"}, {`'$@'`, "src: dst:", "src: dst:"},
	} {
		if got := names("cp2 a " + c.arg + " c"); got != c.before {
			t.Errorf("a %s c: %q, want %q", c.arg, got, c.before)
		}
		if got := names("cp2 " + c.arg + " c"); got != c.after {
			t.Errorf("%s c: %q, want %q", c.arg, got, c.after)
		}
	}
}

// A call through an alias may add arguments of its own, and the alias
// definition itself is not a call: no hints for either.
func TestHintsAliasCallGetsNone(t *testing.T) {
	const pre = "cp2() { local src=\"$1\" dst=\"$2\"; }\nshopt -s expand_aliases\n"
	for name, c := range map[string]struct{ src, want string }{
		"plain alias":        {pre + "alias cpx=cp2\ncpx a b\ncp2 a b\n", "5:5 src:|5:7 dst:"},
		"alias same name":    {pre + "alias cp2='cp2 x'\ncp2 a b\n", ""},
		"alias definition":   {pre + "alias cpx=cp2\n", ""},
		"alias with a value": {pre + "alias cpx='cp2 one'\ncpx two\n", ""},
	} {
		q := buildMem(t, map[string]string{"a.sh": c.src})
		p := filepath.Join(q.root, "a.sh")
		if got := hintLabels(hintsOf(q.s, p, whole(q.s, p), false, true)); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// "$@" and "$*" stand for any number of words: no hint on them, and none
// after them, where the position of an argument is unknown.
func TestHintsSkipSpreadArguments(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "cp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 \"$@\" b\ncp2 $* b\ncp2 \"$*\" b\ncp2 $@ b\ncp2 a \"$@\"\n"})
	p := filepath.Join(q.root, "a.sh")
	got := hintsOf(q.s, p, whole(q.s, p), false, true)
	if hintLabels(got) != "6:5 src:" {
		t.Fatalf("%s", hintLabels(got))
	}
}

func hintsOf(s *Snapshot, path string, within tree.Range, sources, params bool) []Hint {
	h, _ := s.Hints(path, within, sources, params)
	return h
}

// The cut is reported when there are more than maxHints hints, and only then.
func TestHintsReportTheCut(t *testing.T) {
	build := func(calls int) (*Snapshot, string) {
		var b strings.Builder
		b.WriteString("f() { local x=\"$1\"; }\n")
		for i := 0; i < calls; i++ {
			b.WriteString("f a\n")
		}
		q := buildMem(t, map[string]string{"a.sh": b.String()})
		return q.s, filepath.Join(q.root, "a.sh")
	}
	s, p := build(maxHints + 1)
	if got, cut := s.Hints(p, whole(s, p), true, true); len(got) != maxHints || !cut {
		t.Fatalf("%d hints, cut %v, want %d and a cut", len(got), cut, maxHints)
	}
	s, p = build(maxHints)
	if got, cut := s.Hints(p, whole(s, p), true, true); len(got) != maxHints || cut {
		t.Fatalf("%d hints, cut %v at the limit", len(got), cut)
	}
}
