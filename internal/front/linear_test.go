package front

import (
	"runtime"
	"strings"
	"testing"
)

func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// pipeStages re-copied the stages at every level of a pipeline
// (200 KB of `a | a | ...`: 2.3 s, 800 KB: 33 s). Four times the stages must
// cost about four times the bytes, not sixteen.
func TestPipelineIsLinear(t *testing.T) {
	src := func(n int) []byte { return []byte("a" + strings.Repeat(" | a", n) + "\n") }
	small, large := src(5000), src(20000)
	a := allocated(func() { Parse("t.sh", small, Bash) })
	b := allocated(func() { Parse("t.sh", large, Bash) })
	if b > 8*a {
		t.Fatalf("Parse allocated %d bytes for 5,000 stages and %d for 20,000 (%.1fx)", a, b, float64(b)/float64(a))
	}
}
