package main

import (
	"fmt"
	"io"
	"runtime/metrics"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

type rd struct {
	b      []byte
	s      []metrics.Sample
	max    uint64
	n      int
	spent  time.Duration
	trace  bool
	tracev []uint64
}

func (r *rd) Read(p []byte) (int, error) {
	t0 := time.Now()
	metrics.Read(r.s)
	r.spent += time.Since(t0)
	r.n++
	v := r.s[0].Value.Uint64()
	if v > r.max {
		r.max = v
	}
	if r.trace {
		r.tracev = append(r.tracev, v)
	}
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	k := min(len(p), len(r.b))
	copy(p, r.b[:k])
	r.b = r.b[k:]
	return k, nil
}

func run(src string) *rd {
	r := &rd{b: []byte(src), s: []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		syntax.NewParser(syntax.KeepComments(true)).Parse(r, "x")
	}()
	<-done
	return r
}

func main() {
	for _, c := range []struct {
		name, src string
	}{
		{"flat 2MiB", strings.Repeat("echo a b c\n", 190000)},
		{"$( x 10000 (expect ~15MB)", "echo " + strings.Repeat("$(", 10000) + strings.Repeat(" a", 100000)},
		{"$( x 40000 (expect ~61MB)", "echo " + strings.Repeat("$(", 40000) + strings.Repeat(" a", 100000)},
		{"( x 16000 (expect ~64MB)", strings.Repeat("(", 16000) + strings.Repeat(" a", 100000)},
		{"comments x 100000 (expect ~14MB)", strings.Repeat("#\n", 100000) + strings.Repeat(" a", 100000)},
		{"comments x 1000000", strings.Repeat("#\n", 1000000)},
	} {
		r := run(c.src)
		fmt.Printf("%-34s reads=%d max stacks metric=%.1f MiB, metric read cost=%v each\n", c.name, r.n, float64(r.max)/(1<<20), r.spent/time.Duration(r.n))
	}
}
