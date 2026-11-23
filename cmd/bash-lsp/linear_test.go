package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/analysis"
)

// The CLI converted every position it printed with File.ToUTF16, which scans
// the line from its start: O(records x line length) on a long line (extract
// of 1 MiB on one line: minutes). extract also found the function of each
// alias with CallerAt, a walk down and up the tree: O(aliases x depth) (an
// alias run 3,900 subshells deep: 7 min 25 s at 1 MiB, with the first cost).
// Each command must finish in bounded time on these shapes.
func TestCLIOutputInBoundedTime(t *testing.T) {
	size, bound := 2<<20-64, 5*time.Second
	if raceBuild {
		size, bound = size/4, 60*time.Second
	}
	fill := func(pre, unit, post string) string {
		return pre + strings.Repeat(unit, (size-len(pre)-len(post))/len(unit)) + post
	}
	var vars strings.Builder
	for i := 0; vars.Len() < size-20; i++ {
		fmt.Fprintf(&vars, "v%d=1; ", i)
	}
	vars.WriteString("\n")
	deep := strings.Repeat("( ", 3900)
	undeep := strings.Repeat(" )", 3900)
	for _, c := range []struct {
		name, src string
		args      []string
	}{
		{"extract, calls on one line", fill("f() { :; }; ", "f; ", "\n"), []string{"extract", "--root"}},
		{"extract, deep aliases", fill("f() { :; }\n"+deep, "alias a=f;", undeep+"\n"), []string{"extract", "--root"}},
		{"symbols, one line", vars.String(), []string{"symbols"}},
		{"callers, calls on one line", "f() { :; }; " + strings.Repeat("f; ", size/6) + "\n", []string{"callers", "a.sh:1:1", "--json", "--root"}},
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.sh")
		if err := os.WriteFile(p, []byte(c.src), 0o644); err != nil {
			t.Fatal(err)
		}
		args := append(append([]string(nil), c.args...), dir)
		if c.args[0] == "symbols" {
			args = []string{"symbols", p}
		}
		done := make(chan [2]any, 1)
		go func() {
			var out, errb bytes.Buffer
			start := time.Now()
			code := run(args, strings.NewReader(""), &out, &errb)
			done <- [2]any{time.Since(start), fmt.Sprintf("exit %d, %d bytes out, stderr %.200s", code, out.Len(), errb.String())}
		}()
		select {
		case r := <-done:
			t.Logf("%s: %v (%s)", c.name, r[0].(time.Duration).Round(time.Millisecond), r[1])
			if !strings.HasPrefix(r[1].(string), "exit 0,") {
				t.Errorf("%s: %s", c.name, r[1])
			}
		case <-time.After(bound):
			t.Errorf("%s: not done within %v", c.name, bound)
		}
	}
}

// funcsAround must name the function CallerAt names, at every declaration
// offset of the corpus and of random nested programs (named, unnamed bats
// tests, subshells, siblings).
func TestFuncsAroundMatchesCallerAt(t *testing.T) {
	files := map[string][]byte{}
	for _, dir := range []string{"../../testdata/corpus", "../../testdata/corpus-annotated", "../../testdata/broken"} {
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				b, _ := os.ReadFile(p)
				abs, _ := filepath.Abs(p)
				files[abs] = b
			}
			return nil
		})
	}
	r := rand.New(rand.NewSource(1))
	var stmt func(d int) string
	stmt = func(d int) string {
		k := r.Intn(8)
		if d > 4 {
			k = r.Intn(3)
		}
		switch k {
		case 0:
			return "alias a=f"
		case 1:
			return "x=1"
		case 2:
			return "alias b=g c=h"
		case 3:
			return fmt.Sprintf("f%d() { %s; %s; }", r.Intn(3), stmt(d+1), stmt(d+1))
		case 4:
			return "( " + stmt(d+1) + " )"
		case 5:
			return `@test "" { ` + stmt(d+1) + "; }"
		case 6:
			return "function g { " + stmt(d+1) + "; }"
		default:
			return "if t; then " + stmt(d+1) + "; fi"
		}
	}
	for i := range 2000 {
		var sb strings.Builder
		for range 1 + r.Intn(5) {
			sb.WriteString(stmt(0) + "\n")
		}
		ext := ".sh"
		if i%4 == 0 {
			ext = ".bats"
		}
		files[fmt.Sprintf("/ws/r%d%s", i, ext)] = []byte(sb.String())
	}
	s := analysis.Analyze(files, analysis.Options{Root: "/"})
	checked := 0
	for _, p := range s.Paths() {
		ff := s.Facts(p)
		if ff == nil || ff.File == nil {
			continue
		}
		at := funcsAround(ff)
		for _, d := range ff.Decls {
			off := d.NameRange.Start.Offset
			want := ""
			if c := s.CallerAt(p, off); c.Func != nil && c.Func.Kind == analysis.DeclFunction {
				want = c.Func.Name
			}
			if d.Kind == analysis.DeclFunction { // CallerAt counts a function's own name as inside it
				continue
			}
			if got := at(off); got != want {
				t.Fatalf("%s offset %d (kind %d %q): funcsAround %q, CallerAt %q\n%s", p, off, d.Kind, d.Name, got, want, ff.File.Src)
			}
			checked++
		}
	}
	if checked < 2000 {
		t.Fatalf("only %d declarations checked", checked)
	}
}
