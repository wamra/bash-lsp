package front

import (
	"errors"
	"io"
	"runtime"
	"runtime/metrics"
	"sync"
)

// The parser (mvdan.cc/sh) is recursive descent with no depth limit, and a
// Go stack overflow is fatal: no recover() catches it. Parse therefore
// watches how much stack the parser is using while it parses, and stops it
// long before the stack is gone. docs/m2a-nesting-design.md has the
// measurements and the argument; in short:
//
//   - Every byte the parser sees comes through one io.Reader, at most 1,024
//     bytes per call (Parser.fill, syntax/lexer.go:155 in v3.14.1), and the
//     call is made from inside the recursion. guardedReader is that reader:
//     each call is a look at the stack before the parser gets more input.
//   - The Go runtime does not say how deep a goroutine's stack is. It offers
//     two upper bounds, and the parser is stopped only when both are over:
//     the stack memory of the whole process (runtime/metrics, a few hundred
//     nanoseconds), and, only when that is high, the number of frames on
//     this goroutine's stack (runtime.Callers, about 60 ns a frame) times
//     the size of the parser's largest frame.
//   - Between two looks the parser can only recurse on the 1,024 bytes it was
//     given. The most it has been measured to use is 4.0 KB of stack per
//     input byte (nested arithmetic parentheses). So the stack stays below
//     about 32 MiB + 4 MiB, against the 512 MiB a goroutine stack can reach
//     under the runtime's default limit. That last step is measured, not
//     proven: it holds while the parser uses less than about 480 KB of stack
//     per input byte, 120 times the worst seen.
//
// Nothing here reads the source: there is no second lexer to disagree with
// the parser about quotes, comments or heredocs, and every range handed to
// the parser, resync's included, goes through the same reader.
//
// The guard does not bound the tree: chains the parser builds in a loop
// (&&, ||, |, elif, 1+1+...) use no stack there. The lowerer flattens them
// or stops at maxDepth (lower.go).
const (
	// stackMetric is all the memory the process holds for goroutine stacks.
	// A goroutine's stack is one block of it, so it is never less than the
	// stack of the goroutine that is parsing.
	stackMetric = "/memory/classes/heap/stacks:bytes"
	// maxStackMemory is the reading of stackMetric below which the parser
	// is not looked at any closer. Stacks double, so a parse alone in the
	// process reaches it when it uses more than half of it: 32 MiB.
	maxStackMemory = 64 << 20
	// maxFrameBytes is more than any frame of the parser: the largest in
	// v3.14.1 has 376 bytes of locals (Parser.wordPart, from go build
	// -gcflags=-S).
	maxFrameBytes = 512
	// maxFrames frames of the parser fit in the 32 MiB a parse may use
	// without question. Real scripts stay under 2,000. What gets a file
	// refused in practice is nesting some 8,000 levels deep or more, or one
	// unbroken block of more than about 250,000 comment lines (the lexer
	// calls itself once per comment line, about 128 bytes each).
	maxFrames = maxStackMemory / 2 / maxFrameBytes
	// maxProbeFrames bounds the frames counted for one file, which bounds
	// the guard's cost. Frames are counted only while stackMetric is high;
	// resync stops trying further lines once the file has counted this many,
	// and a parse that still needs counting then is refused.
	maxProbeFrames = 6000000
)

const refusal = "nesting too deep to parse safely"

// errTooDeep is what the parser gets instead of more input.
var errTooDeep = errors.New(refusal)

// stackGuard is shared by every parser run over one file.
type stackGuard struct {
	sample [1]metrics.Sample
	buf    *[]uintptr // scratch for runtime.Callers, borrowed from pcsPool
	frames int        // frames counted so far, over all runs
}

var pcsPool = sync.Pool{New: func() any {
	s := make([]uintptr, maxFrames+1)
	return &s
}}

// release returns the scratch buffer. The guard must not be used afterwards.
func (g *stackGuard) release() {
	if g.buf != nil {
		pcsPool.Put(g.buf)
		g.buf = nil
	}
}

// spent reports whether the file has used up its frame-counting budget.
func (g *stackGuard) spent() bool { return g.frames > maxProbeFrames }

// stackMemory returns the stack memory of the whole process.
func (g *stackGuard) stackMemory() uint64 {
	g.sample[0].Name = stackMetric
	metrics.Read(g.sample[:])
	if g.sample[0].Value.Kind() != metrics.KindUint64 {
		return 1 << 62 // the runtime dropped the metric: count frames every time
	}
	return g.sample[0].Value.Uint64()
}

// tooDeep reports whether the calling goroutine may be using more stack
// than a parse is allowed. It is called from inside the parser.
func (g *stackGuard) tooDeep() bool {
	if g.stackMemory() < maxStackMemory {
		return false
	}
	// Some stack in the process is large: this one, another goroutine's, or
	// one the runtime has not released yet. Count this goroutine's frames;
	// inlined calls count too, so the count is never below the real one.
	if g.spent() {
		return true
	}
	if g.buf == nil {
		g.buf = pcsPool.Get().(*[]uintptr)
	}
	n := runtime.Callers(0, *g.buf)
	g.frames += n
	return n > maxFrames
}

// guardedReader hands one byte range to the parser and stops it when its
// stack has grown too deep.
type guardedReader struct {
	g       *stackGuard
	src     []byte
	off     int  // bytes handed out
	tripped bool // the run was stopped; off is where
}

func (r *guardedReader) Read(p []byte) (int, error) {
	if r.tripped {
		return 0, errTooDeep
	}
	n := min(len(p), len(r.src)-r.off)
	if n == 0 {
		return 0, io.EOF
	}
	if r.g.tooDeep() {
		r.tripped = true
		return 0, errTooDeep
	}
	copy(p, r.src[r.off:r.off+n])
	r.off += n
	return n, nil
}
