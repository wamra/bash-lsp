package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

var pool = sync.Pool{New: func() any { return make([]uintptr, 1<<22) }}

type unit struct{ name, pre, rep, post string }

var units = []unit{
	{"paren", "", "(", "\n"},
	{"cmdsubst", "echo ", "$(", "\n"},
	{"procsubst", "cat ", "<(", "\n"},
	{"dq-cmdsubst", "echo ", `"$(`, "\n"},
	{"dollar-brk", "echo ", "$[", "\n"},
	{"param", "echo ", "${a:-", "\n"},
	{"param-index", "echo ", "${x[", "\n"},
	{"brace", "", "{ ", "\n"},
	{"arith-paren", "echo $((", "(", "1\n"},
	{"ternary", "echo $((", "a?", "1))\n"},
	{"arith-assign", "echo $((", "a=", "1))\n"},
	{"arith-power", "echo $((", "2**", "2))\n"},
	{"arith-minus", "echo $((", "- ", "1))\n"},
	{"arith-not", "echo $((", "!", "1))\n"},
	{"arith-plus", "echo $((", "1+", "1))\n"},
	{"test-not", "[[ ", "! ", "a ]]\n"},
	{"test-paren", "[[ ", "( ", "a ]]\n"},
	{"test-and", "[[ ", "a && ", "a ]]\n"},
	{"if", "", "if a;then\n", ""},
	{"if1", "", "if a;then ", "\n"},
	{"while", "", "while a;do\n", ""},
	{"for", "", "for a in b;do\n", ""},
	{"case", "", "case a in b)\n", ""},
	{"elif", "if a; then :\n", "elif a; then :\n", "fi\n"},
	{"time", "", "time ", "a\n"},
	{"coproc", "", "coproc ", "a\n"},
	{"bang", "", "! ", "a\n"},
	{"funcs", "", "f() ", "{ :; }\n"},
	{"function-kw", "", "function f\n", "{ :; }\n"},
	{"andor", "a ", "&& a ", "\n"},
	{"pipes", "a ", "| a ", "\n"},
	{"subshell-sp", "", "( ", "\n"},
	{"bq-in-dq", "echo ", "\"`", "\n"},
	{"array-cmdsubst", "", "a=($(", "\n"},
	{"semi-brace", "", "{\n", "\n"},
}

func src(u unit, n int) []byte { return []byte(u.pre + strings.Repeat(u.rep, n) + u.post) }

type rd struct {
	b      []byte
	chunk  int
	maxF   int
	pcs    []uintptr
	checks int
	spent  time.Duration
	timeit bool
}

func (r *rd) Read(p []byte) (int, error) {
	var t0 time.Time
	if r.timeit {
		t0 = time.Now()
	}
	n := runtime.Callers(0, r.pcs)
	if r.timeit {
		r.spent += time.Since(t0)
	}
	r.checks++
	if n > r.maxF {
		r.maxF = n
	}
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	k := min(len(p), r.chunk, len(r.b))
	copy(p, r.b[:k])
	r.b = r.b[k:]
	return k, nil
}

func parse(b []byte, chunk int, timeit bool) *rd {
	r := &rd{b: b, chunk: chunk, pcs: pool.Get().([]uintptr), timeit: timeit}
	defer pool.Put(r.pcs)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		p := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50), syntax.Variant(lang()))
		p.Parse(r, "x")
	}()
	<-done
	return r
}

func lang() syntax.LangVariant {
	switch os.Getenv("SPIKE_LANG") {
	case "posix":
		return syntax.LangPOSIX
	case "mksh":
		return syntax.LangMirBSDKorn
	case "bats":
		return syntax.LangBats
	}
	return syntax.LangBash
}

func find(name string) unit {
	for _, u := range units {
		if u.name == name {
			return u
		}
	}
	panic(name)
}

func main() {
	switch os.Args[1] {
	case "child": // child NAME N MAXSTACK : plain parse
		n, _ := strconv.Atoi(os.Args[3])
		ms, _ := strconv.Atoi(os.Args[4])
		debug.SetMaxStack(ms)
		p := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50), syntax.Variant(lang()))
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = recover() }()
			p.Parse(bytes.NewReader(src(find(os.Args[2]), n)), "x")
		}()
		<-done
	case "frames": // frames per level with 1-byte feed
		for _, u := range units {
			a := parse(src(u, 100), 1, false).maxF
			b := parse(src(u, 1100), 1, false).maxF
			fpl := float64(b-a) / 1000
			fmt.Printf("%-16s unit=%d bytes  frames/level=%.2f frames/byte=%.2f base=%d\n", u.name, len(u.rep), fpl, fpl/float64(len(u.rep)), a)
		}
	case "bytes": // bisect levels fitting in 64 MiB
		const ms = 64 << 20
		for _, u := range units {
			ok := func(n int) bool {
				return exec.Command(os.Args[0], "child", u.name, strconv.Itoa(n), strconv.Itoa(ms)).Run() == nil
			}
			hi := (2 << 20) / len(u.rep)
			if ok(hi) {
				fmt.Printf("%-16s no overflow at 2MiB (%d levels) within 64MiB\n", u.name, hi)
				continue
			}
			lo := 0
			for hi-lo > max(1, lo/100) {
				mid := (lo + hi) / 2
				if ok(mid) {
					lo = mid
				} else {
					hi = mid
				}
			}
			bpl := float64(ms) / float64(lo)
			fmt.Printf("%-16s max levels in 64MiB=%d  bytes/level>=%.0f(32MiB) <=%.0f(64MiB) stack bytes per input byte<=%.0f\n", u.name, lo, bpl/2, bpl, bpl/float64(len(u.rep)))
		}
	case "backquotes":
		backquotes()
	case "search":
		search()
	case "cost": // cost of Callers at depth
		for _, n := range []int{0, 100, 1000, 5000, 20000} {
			u := find("cmdsubst")
			b := append(src(unit{"", u.pre, u.rep, ""}, n), bytes.Repeat([]byte("a "), 500000)...)
			r := parse(b, 1024, true)
			fmt.Printf("levels=%d maxframes=%d checks=%d callers-time=%v per-check=%v per-frame=%.1fns\n", n, r.maxF, r.checks, r.spent, r.spent/time.Duration(r.checks), float64(r.spent.Nanoseconds())/float64(r.checks)/float64(r.maxF))
		}
	}
}
