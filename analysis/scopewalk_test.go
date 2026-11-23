package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
)

// deepCases hold about 2 MiB of names 3,900 subshells deep (the lowerer
// accepts 4,000). Before Bind numbered its scopes, each lookup below walked the
// scope chain: an integer-attribute check per assignment and a declaration
// check per arithmetic write in Bind; a private-scope and a function-body walk
// per use, and a walk up the tree per `. $f`, in Build. Each cost O(depth) per
// name: 23 s, 6 s, 45 s and 4 s at af4a1a8.
func deepCases(size int) map[string]string {
	const depth = 3900
	fill := func(pre, unit, post string) string {
		n := (size - len(pre) - len(post) - 4*depth) / len(unit)
		return pre + strings.Repeat("( ", depth) + strings.Repeat(unit, n) + strings.Repeat(" )", depth) + post
	}
	expr := "x=$(( a[" + strings.Repeat("b+", 4000) + "1] ))\n"
	return map[string]string{
		"integer assignments":           fill("declare -i q\n", "x=1;", "\n"),
		"arithmetic writes, function":   fill("f() { y=0; ", "((y=1));", " }\n"),
		"reads under private names, fn": fill("f() { ", expr, " }\n"),
		"loop sources":                  fill("", ". $f;", "\n"),
	}
}

func TestDeepScopesBindAndBuildInBoundedTime(t *testing.T) {
	bound := 3 * time.Second
	size := 2<<20 - 64
	if raceBuild {
		size /= 4
		bound = 60 * time.Second
	}
	for name, src := range deepCases(size) {
		f := front.Parse("/ws/a.sh", []byte(src), front.Bash)
		if f.Skipped != "" || len(f.Errors) > 0 {
			t.Fatalf("%s: not parsed cleanly: %q %v", name, f.Skipped, f.Errors)
		}
		done := make(chan [2]time.Duration, 1)
		go func() {
			start := time.Now()
			ff := Bind(f)
			mid := time.Now()
			Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1)
			done <- [2]time.Duration{mid.Sub(start), time.Since(mid)}
		}()
		select {
		case d := <-done:
			t.Logf("%s: %d bytes, Bind %v, Build %v", name, len(src), d[0].Round(time.Millisecond), d[1].Round(time.Millisecond))
		case <-time.After(bound):
			t.Errorf("%s: %d bytes not bound and built within %v", name, len(src), bound)
		}
	}
}
