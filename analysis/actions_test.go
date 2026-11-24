package analysis

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
