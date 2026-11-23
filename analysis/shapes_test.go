package analysis

import (
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
)

// shapeSize and shapeBound are the input size and time bound of the
// bounded-time shape tests: about 2 MiB in 3 s (each shape takes well under a
// second once linear), a quarter of that size in 60 s under -race.
func shapeSize() (int, time.Duration) {
	if raceBuild {
		return (2<<20 - 64) / 4, 60 * time.Second
	}
	return 2<<20 - 64, 3 * time.Second
}

// fillTo repeats unit after pre until the text is about size bytes, then post.
func fillTo(size int, pre, unit, post string) string {
	return pre + strings.Repeat(unit, (size-len(pre)-len(post))/len(unit)) + post
}

// withinBound runs fn and fails t if it does not return within bound; it
// reports whether fn returned.
func withinBound(t *testing.T, what string, bound time.Duration, fn func()) bool {
	t.Helper()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		fn()
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		t.Logf("%s: %v", what, d.Round(time.Millisecond))
		return true
	case <-time.After(bound):
		t.Errorf("%s: not done within %v", what, bound)
		return false
	}
}

// parseClean parses src as Bash and fails t unless it parsed without error.
func parseClean(t *testing.T, name, src string) *FileFacts {
	t.Helper()
	f := front.Parse("/ws/a.sh", []byte(src), front.Bash)
	if f.Skipped != "" || len(f.Errors) > 0 {
		t.Fatalf("%s: not parsed cleanly: %q %v", name, f.Skipped, f.Errors)
	}
	return Bind(f)
}

// m2b shapes found by the merge's shape run: each must bind, build and list
// its edges in bounded time. "$cmd" calls each scanned every declaration of
// the variable's symbol: O(calls x assignments), 9.3 s at 128 KiB. The
// dynamic-scope pass listed the targets of every call of a function once
// per definition: O(definitions^2) for one call, 1.2 s at 176 KB.
func TestM2bShapesBuildInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	cases := map[string]string{
		"assign and call cmd":            fillTo(size, "f() { :; }\n", "cmd=f; \"$cmd\";\n", ""),
		"assign and call cmd, private":   fillTo(size, "f() { :; }\ng() { (", " cmd=f; \"$cmd\";", " ); }\n"),
		"many definitions with reads":    fillTo(size, "", "f() { x=1; echo $y; }\n", "f\n"),
		"many definitions, local caller": fillTo(size, "g() { local y; f; }\n", "f() { echo $y; }\n", ""),
	}
	for name, src := range cases {
		ff := parseClean(t, name, src)
		withinBound(t, name, bound, func() {
			s := Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1)
			s.Edges()
		})
	}
}

// Hover on a function scanned every use and binding of the file once per
// definition (bodyEffects): O(definitions x uses), 0.46 s at 20,000
// definitions of one name and quadratic beyond.
func TestHoverOnManyDefinitionsInBoundedTime(t *testing.T) {
	size, bound := shapeSize()
	src := fillTo(size, "", "f() { x=1; echo $y; }\n", "f\n")
	ff := parseClean(t, "many definitions", src)
	var s *Snapshot
	if !withinBound(t, "build", bound, func() { s = Build(map[string]*FileFacts{"/ws/a.sh": ff}, Options{Root: "/ws"}, 1) }) {
		return
	}
	var md string
	if !withinBound(t, "hover on a call of f", bound, func() { md, _, _ = s.Hover("/ws/a.sh", len(src)-2) }) {
		return
	}
	if !strings.Contains(md, "Reads globals: `y`") || !strings.Contains(md, "Writes globals: `x`") {
		t.Errorf("hover lost the body effects:\n%.300s", md)
	}
}
