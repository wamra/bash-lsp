package analysis

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/tree"
)

func apply(src string, edits []TextEdit) string {
	out := []byte(src)
	for i := len(edits) - 1; i >= 0; i-- { // edits are sorted, so back to front
		e := edits[i]
		out = append(out[:e.Range.Start.Offset:e.Range.Start.Offset], append([]byte(e.NewText), out[e.Range.End.Offset:]...)...)
	}
	return string(out)
}

func TestAddShebang(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"a.sh", "echo hi\n", "#!/usr/bin/env bash\n"},
		{"t.bats", "@test \"x\" { :; }\n", "#!/usr/bin/env bats\n"},
	} {
		q := buildMem(t, map[string]string{c.name: c.src})
		p := filepath.Join(q.root, c.name)
		fx, ok := q.s.AddShebang(p)
		if !ok || fx.Safe || len(fx.Edits) != 1 || fx.Edits[0].NewText != c.want || apply(c.src, fx.Edits) != c.want+c.src {
			t.Errorf("%s: %+v ok %v", c.name, fx, ok)
		}
	}
}

func TestAddShebangDialectText(t *testing.T) {
	for d, want := range map[tree.Dialect]string{tree.Bash: "#!/usr/bin/env bash\n", tree.POSIX: "#!/bin/sh\n", tree.Mksh: "#!/usr/bin/env mksh\n", tree.Bats: "#!/usr/bin/env bats\n"} {
		if got, ok := shebangFor(d); !ok || got != want {
			t.Errorf("%v: %q %v, want %q", d, got, ok, want)
		}
	}
	if _, ok := shebangFor(tree.Zsh); ok {
		t.Error("zsh has no shebang text")
	}
}

func TestAddShebangNotOffered(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh":     "#!/bin/bash\nsource ./lib/util.sh\n",
		"lib/util.sh": "u() { :; }\n",
		"has.sh":      "#!/bin/sh\necho\n",
		"empty.sh":    "",
	})
	if _, ok := q.s.AddShebang(filepath.Join(q.root, "lib/util.sh")); ok {
		t.Error("a sourced file is a library, not offered")
	}
	if _, ok := q.s.AddShebang(filepath.Join(q.root, "has.sh")); ok {
		t.Error("has a shebang")
	}
	if _, ok := q.s.AddShebang(filepath.Join(q.root, "main.sh")); ok {
		t.Error("has a shebang")
	}
	if _, ok := q.s.AddShebang("/nowhere.sh"); ok {
		t.Error("unknown path")
	}
}

func TestAddShebangOnPOSIXFile(t *testing.T) {
	src := "# shellcheck shell=sh\n. ./x\n"
	q := buildMem(t, map[string]string{"p.sh": src})
	fx, ok := q.s.AddShebang(filepath.Join(q.root, "p.sh"))
	if !ok || len(fx.Edits) != 1 || fx.Edits[0].NewText != "#!/bin/sh\n" || apply(src, fx.Edits) != "#!/bin/sh\n"+src {
		t.Fatalf("%+v ok %v", fx, ok)
	}
	if fx.Edits[0].Range.Start.Offset != 0 || fx.Edits[0].Range.End.Offset != 0 {
		t.Fatalf("not an insertion at the start: %+v", fx.Edits[0].Range)
	}
}

func quoteAt(t *testing.T, src string) (Fix, bool) {
	t.Helper()
	off := strings.Index(src, "▮")
	if off < 0 {
		t.Fatal("no cursor mark")
	}
	q := buildMem(t, map[string]string{"a.sh": strings.Replace(src, "▮", "", 1)})
	return q.s.QuoteExpansion(filepath.Join(q.root, "a.sh"), off)
}

func TestQuoteExpansion(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"rm $▮f\n", "rm \"$f\"\n"},
		{"rm ▮$f\n", "rm \"$f\"\n"},
		{"rm $f▮\n", "rm \"$f\"\n"},
		{"echo ${f▮:-x}\n", "echo \"${f:-x}\"\n"},
		{"echo a $▮f b\n", "echo a \"$f\" b\n"},
	} {
		fx, ok := quoteAt(t, c.src)
		if !ok || fx.Safe {
			t.Errorf("%q: ok %v safe %v", c.src, ok, fx.Safe)
			continue
		}
		if got := apply(strings.Replace(c.src, "▮", "", 1), fx.Edits); got != c.want {
			t.Errorf("%q: %q, want %q", c.src, got, c.want)
		}
	}
}

func TestQuoteExpansionNotOffered(t *testing.T) {
	for _, src := range []string{
		"[[ $▮f ]]\n",
		"x=$▮f\n",
		"echo $(( $▮f + 1 ))\n",
		"case $▮f in a) :;; esac\n",
		"echo hi > $▮f\n",
		"echo \"$▮f\"\n",  // already quoted
		"echo '$▮f'\n",    // not an expansion
		"$▮f arg\n",       // the command word
		"echo $▮f.txt\n",  // not the whole word
		"echo ${#▮f}\n",   // a length
		"echo ${▮f:+x}\n", // quoting changes unset into empty
		"echo $▮1\n",      // not a name
		"export X=$▮f\n",
		"echo ▮hi\n",
	} {
		if fx, ok := quoteAt(t, src); ok {
			t.Errorf("%q: offered %+v", src, fx)
		}
	}
	if _, ok := (&Snapshot{}).QuoteExpansion("/nowhere.sh", 0); ok {
		t.Error("unknown path")
	}
}

func fix(safe bool, title string, spans ...[2]int) Fix {
	f := Fix{Title: title, Safe: safe}
	for _, s := range spans {
		f.Edits = append(f.Edits, TextEdit{Range: tree.Range{Start: tree.Pos{Offset: s[0]}, End: tree.Pos{Offset: s[1]}}, NewText: title})
	}
	return f
}

func TestFixAll(t *testing.T) {
	got := FixAll([]Fix{
		fix(true, "a", [2]int{10, 20}),
		fix(false, "unsafe", [2]int{0, 5}),
		fix(true, "b", [2]int{15, 25}), // overlaps a: dropped
		fix(true, "c", [2]int{20, 30}), // touches a: kept
		fix(true, "d", [2]int{2, 4}),
	})
	var titles []string
	for _, e := range got {
		titles = append(titles, e.NewText)
	}
	if !reflect.DeepEqual(titles, []string{"d", "a", "c"}) {
		t.Fatalf("%v", titles)
	}
	if FixAll(nil) != nil || len(FixAll([]Fix{fix(false, "x", [2]int{0, 1})})) != 0 {
		t.Fatal("nothing safe: no edits")
	}
}

func TestFixAllKeepsAFixWhole(t *testing.T) {
	got := FixAll([]Fix{
		fix(true, "a", [2]int{10, 20}),
		fix(true, "b", [2]int{0, 2}, [2]int{12, 14}), // second edit overlaps a: the whole fix goes
	})
	if len(got) != 1 || got[0].NewText != "a" {
		t.Fatalf("%+v", got)
	}
}

func TestFixAllInsertionsAtOnePoint(t *testing.T) {
	got := FixAll([]Fix{fix(true, "a", [2]int{5, 5}), fix(true, "b", [2]int{5, 5}), fix(true, "c", [2]int{4, 8}), fix(true, "d", [2]int{5, 9})})
	if len(got) != 2 || got[0].NewText != "a" || got[1].NewText != "d" {
		t.Fatalf("%+v", got)
	}
}

func TestQuoteExpansionPrefixListAndCRLF(t *testing.T) {
	for _, src := range []string{"echo ${!▮pre*}\n", "echo ${!▮pre@}\n"} {
		if fx, ok := quoteAt(t, src); ok {
			t.Errorf("%q: offered %+v", src, fx)
		}
	}
	fx, ok := quoteAt(t, "echo ${!▮arr[@]}\n")
	if !ok {
		t.Error("the key list of an array is a plain expansion")
	} else if got := apply("echo ${!arr[@]}\n", fx.Edits); got != "echo \"${!arr[@]}\"\n" {
		t.Errorf("%q", got)
	}
	src := "echo $▮x\r\nrm $y\r\n"
	fx, ok = quoteAt(t, src)
	if !ok {
		t.Fatal("a CRLF file gets no offer")
	}
	if got := apply(strings.Replace(src, "▮", "", 1), fx.Edits); got != "echo \"$x\"\r\nrm $y\r\n" {
		t.Errorf("%q", got)
	}
}

func TestAddShebangBOMAndLineEnding(t *testing.T) {
	const bom = "\xEF\xBB\xBF"
	for _, c := range []struct{ name, src, want string }{
		{"a.sh", bom + "echo hi\n", bom + "#!/usr/bin/env bash\necho hi\n"},
		{"b.sh", "echo hi\r\necho\r\n", "#!/usr/bin/env bash\r\necho hi\r\necho\r\n"},
		{"c.sh", bom + "echo hi\r\n", bom + "#!/usr/bin/env bash\r\necho hi\r\n"},
		{"d.sh", "echo hi", "#!/usr/bin/env bash\necho hi"},
	} {
		q := buildMem(t, map[string]string{c.name: c.src})
		fx, ok := q.s.AddShebang(filepath.Join(q.root, c.name))
		if !ok || len(fx.Edits) != 1 {
			t.Errorf("%s: %+v %v", c.name, fx, ok)
			continue
		}
		if got := apply(c.src, fx.Edits); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	for _, src := range []string{bom + "#!/bin/sh\necho\n", bom + "#!/bin/bash\r\n"} {
		q := buildMem(t, map[string]string{"x.sh": src})
		if _, ok := q.s.AddShebang(filepath.Join(q.root, "x.sh")); ok {
			t.Errorf("%q: offered", src)
		}
	}
}

// fixAllNaive is the definition FixAll must agree with: in order, a safe fix
// is taken when none of its edits overlaps another of its own or one taken.
func fixAllNaive(fixes []Fix) []TextEdit {
	over := func(a, b TextEdit) bool {
		as, ae, bs, be := a.Range.Start.Offset, a.Range.End.Offset, b.Range.Start.Offset, b.Range.End.Offset
		if as == ae && bs == be {
			return as == bs
		}
		return as < be && bs < ae
	}
	var out []TextEdit
	for _, fx := range fixes {
		if !fx.Safe || len(fx.Edits) == 0 {
			continue
		}
		ok := true
		for i, e := range fx.Edits {
			for _, o := range append(append([]TextEdit(nil), out...), fx.Edits[:i]...) {
				if over(o, e) {
					ok = false
				}
			}
		}
		if ok {
			out = append(out, fx.Edits...)
		}
	}
	slices.SortStableFunc(out, byPosition)
	return out
}

func TestFixAllAgreesWithTheDefinition(t *testing.T) {
	rnd := rand.New(rand.NewPCG(7, 11))
	for round := 0; round < 3000; round++ {
		var fixes []Fix
		for i, n := 0, rnd.IntN(12); i < n; i++ {
			var spans [][2]int
			for j, m := 0, 1+rnd.IntN(3); j < m; j++ {
				a := rnd.IntN(14)
				b := a
				if rnd.IntN(3) > 0 {
					b = a + rnd.IntN(6)
				}
				spans = append(spans, [2]int{a, b})
			}
			fixes = append(fixes, fix(rnd.IntN(5) > 0, fmt.Sprint(i), spans...))
		}
		got, want := FixAll(fixes), fixAllNaive(fixes)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: %+v\nwant %+v\nfrom %+v", round, got, want, fixes)
		}
	}
}

// Many edits, and wide edits that are taken and rolled back, stay fast.
func TestFixAllScales(t *testing.T) {
	const n = 40000
	var desc, wide []Fix
	for i := n; i > 0; i-- {
		desc = append(desc, fix(true, "x", [2]int{i * 2, i*2 + 1}))
	}
	for i := 0; i < n; i++ {
		wide = append(wide, fix(true, "w", [2]int{0, n * 2}, [2]int{n*2 - 1, n*2 - 1})) // the second edit lands inside the first: the fix is rolled back
	}
	for _, c := range []struct {
		name  string
		fixes []Fix
		want  int
	}{{"descending", desc, n}, {"rolled back", wide, 0}} {
		start := time.Now()
		got := FixAll(c.fixes)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %d fixes took %v", c.name, len(c.fixes), d)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d edits, want %d", c.name, len(got), c.want)
		}
	}
	if got := FixAll(desc); len(got) != n || got[0].Range.Start.Offset != 2 {
		t.Errorf("descending: %d edits, first at %d", len(got), got[0].Range.Start.Offset)
	}
}
