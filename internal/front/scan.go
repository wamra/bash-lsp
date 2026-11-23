package front

import (
	"errors"
	"io"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"
)

// The parser (mvdan.cc/sh) is recursive descent with no depth limit, and a
// Go stack overflow is fatal: no recover() catches it. Parse therefore
// watches how much stack the parser is using while it parses, and stops it
// long before the stack is gone. docs/m2a-nesting-design.md has the
// measurements and the argument; in short:
//
//   - Every byte the parser sees comes through one io.Reader, at most 1,024
//     bytes per call (Parser.fill, syntax/lexer.go:155 in v3.14.1; the
//     reader here hands out no more than that whatever it is asked for), and
//     the call is made from inside the recursion. guardedReader is that
//     reader: each call is a look at the stack before the parser gets more
//     input.
//   - Every parser run has a goroutine of its own, so its stack starts
//     empty and is given back when the run ends.
//   - The Go runtime does not say how deep a goroutine's stack is. It offers
//     two upper bounds, and a file is refused only when both are over: the
//     stack memory of the whole process (runtime/metrics, a few hundred
//     nanoseconds), and, only when that is high, the number of frames on
//     the run's stack (runtime.Callers, about 60 ns a frame) times the size
//     of the parser's largest frame.
//   - The first bound is about the process, so one parse can push it over
//     for another. Parses therefore run side by side only while it is low.
//     A parse that sees it high gives up without a verdict and is done
//     again alone (lane), after the stacks of the others are gone (settle):
//     what a file gets does not depend on the files parsed next to it.
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
	// without question: 65,536. Real scripts stay under 2,000.
	//
	// What gets a file refused depends on the stacks the process holds for
	// work other than parsing, and on nothing else. While those total less
	// than about 32 MiB (the language server, the CLI): more than 32 MiB of
	// parser stack, which takes nesting some 8,000 levels deep (22,000 for
	// `$(`) or one unbroken block of about 250,000 comment lines (the lexer
	// calls itself once per comment line, 128 bytes each). In a process
	// that holds maxStackMemory or more for other work, every parse runs
	// alone, frames are counted instead of bytes and the count is rationed
	// (maxProbeFrames): the limit is maxFrames frames in the parser, which
	// is nesting some 2,200 levels deep (8,000 for `$(`), and a comment
	// block is refused from 12,000 to 65,000 lines on, sooner the longer
	// its lines. Between the two, the room left under maxStackMemory is
	// what a parse gets before frames are counted.
	maxFrames = maxStackMemory / 2 / maxFrameBytes
	// maxProbeFrames bounds the frames counted for one file, which bounds
	// the guard's cost. Frames are counted only while stackMetric is high;
	// resync stops trying further lines once the file has counted this many,
	// and a parse that still needs counting then is refused.
	maxProbeFrames = 6000000
	// maxRead is the most the parser is given between two looks at its
	// stack. The parser asks for no more (bufSize in v3.14.1); the bound
	// must not depend on that.
	maxRead = 1024
	// settleSlack is how far above the last settled reading stackMetric may
	// be when a parse starts alone before the difference is taken for the
	// stacks of parses that have just ended.
	settleSlack = 1 << 20
	// settleEvery spaces the forced collections of a process whose other
	// stacks total maxStackMemory or more.
	settleEvery = time.Second
	// maxOutgrow is the largest starting stack a parser run still outgrows.
	maxOutgrow = 4 << 20
	// maxFlushStart is the largest starting stack at which settle starts
	// the goroutines that empty the runtime's cache of finished ones.
	maxFlushStart = 64 << 10
)

// refusal is why a file was skipped. It does not say "nesting" alone: one
// unbroken block of comment lines is refused the same way (see maxFrames).
const refusal = "nesting too deep, or comment block too long, to parse safely"

// errTooDeep is what the parser gets instead of more input.
var errTooDeep = errors.New(refusal)

// lane keeps a parse from being judged by another parse's stack. A parse
// holds it shared while stackMetric is low. One that sees stackMetric at
// maxStackMemory cannot tell its own stack from its neighbours': it stops
// without a verdict and is done again holding lane alone.
var lane sync.RWMutex

// floor is the reading of stackMetric the last settle ended on: the stacks
// the process holds when no parse is running. settled is when that was.
// Both are guarded by lane, held alone.
var (
	floor   uint64
	settled time.Time
)

// stackGuard is shared by every parser run over one file.
type stackGuard struct {
	sample    [1]metrics.Sample
	buf       *[]uintptr // scratch for runtime.Callers, borrowed from pcsPool
	frames    int        // frames counted so far, over all runs
	alone     bool       // lane is held alone: no other parse is running
	contended bool       // a run sharing lane saw stackMetric high: no verdict
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

// settle is called before each parser run of a parse that holds lane alone,
// from a goroutine that is not deep. No parser is running then, so what
// stackMetric reads is not parser stack. Three things it can be:
//
//   - The stack of a run that has just ended. It goes back when the run's
//     goroutine exits, a moment after its result arrives, unless the
//     collector is in the middle of a cycle: then when the cycle ends.
//   - Stacks of goroutines that no longer exist. The runtime starts
//     goroutines on a stack as large as the average it found at the last
//     collection (megabytes, after a cycle that met one deep parse), and
//     keeps the stack of a finished goroutine that never outgrew that size
//     for the next one, up to 63 per P, for as long as the process runs.
//     Starting that many goroutines at once takes them out of the cache and
//     frees them (flushDeadStacks).
//   - Stacks the rest of the process is using. Nothing to do about those:
//     the reading is remembered (floor) and nothing is done again while it
//     stays within settleSlack of it.
//
// So: wait briefly; if the reading is still away from floor have the
// collector finish, then empty the cache if there is still enough to
// matter. This is the only forced collection in the package and it needs a
// reading that moved: a file refused again and again (edited while it is
// too deep) does not cause one each time, because its own stack is gone
// when its goroutine is. In a process whose other stacks total
// maxStackMemory or more none of this changes the outcome (frames are
// counted), so there it is done at most once per settleEvery.
func (g *stackGuard) settle() {
	if floor >= maxStackMemory && time.Since(settled) < settleEvery {
		return // the rest of the process held that much a moment ago: frames are counted
	}
	m := g.stackMemory()
	for i := 0; i < 200 && m >= floor+settleSlack; i++ {
		if i < 100 {
			runtime.Gosched()
		} else {
			time.Sleep(20 * time.Microsecond)
		}
		m = g.stackMemory()
	}
	if m < settleSlack || (m < floor+settleSlack && m+settleSlack > floor) {
		return
	}
	runtime.GC()
	m = g.stackMemory()
	// The collection has just set the starting size from the stacks that
	// are left. While that is large there is nothing to flush: the cached
	// stacks are of that size, and the goroutines started to flush them
	// would each get one as large.
	for i := 0; i < 4 && m >= maxStackMemory/8 && startingStack() <= maxFlushStart; i++ {
		flushDeadStacks()
		was := m
		if m = g.stackMemory(); m+settleSlack > was {
			break // nothing more comes out
		}
	}
	floor, settled = m, time.Now()
}

// flushDeadStacks takes finished goroutines out of the runtime's per-P
// caches, which frees the stacks of those whose stack is not the size
// goroutines start with now. Starting a goroutine takes one from the cache
// of the P the starter is running on, so there is one starter per P, and
// each spins until all of them are running: at that moment each has a P of
// its own. The goroutines they start wait until the last starter is done,
// so none is handed out twice. A starter the scheduler moves in between can
// leave a P out: settle repeats the flush while it helps.
func flushDeadStacks() {
	const perP = 70 // a P caches up to 63
	n := runtime.GOMAXPROCS(0)
	var running atomic.Int32
	var started, done sync.WaitGroup
	hold := make(chan struct{})
	began := time.Now()
	started.Add(n)
	done.Add(n * perP)
	for range n {
		go func() {
			defer started.Done()
			running.Add(1)
			for i := 0; int(running.Load()) < n; i++ {
				if i&1023 == 1023 && time.Since(began) > 20*time.Millisecond {
					break // the process is busy elsewhere: do what can be done from here
				}
			}
			for range perP {
				go func() {
					<-hold
					done.Done()
				}()
			}
		}()
	}
	started.Wait()
	close(hold)
	done.Wait()
}

// startMetric is the size of the stack a new goroutine gets.
const startMetric = "/gc/stack/starting-size:bytes"

// outgrow makes the calling goroutine's stack larger than the stack
// goroutines start with, so that the runtime frees it when the goroutine
// exits instead of keeping it for the next one (see settle). Every parser
// run calls it first: otherwise the runs themselves, hundreds in a workspace
// scan, would be what fills that cache after one deep file. Past
// maxOutgrow it does nothing: touching that much stack per run costs more
// than it saves, and settle empties the cache.
func outgrow() {
	if n := startingStack(); n <= maxOutgrow {
		useStack(int(n))
	}
}

// startingStack returns the size of the stack a new goroutine gets.
func startingStack() uint64 {
	s := [1]metrics.Sample{{Name: startMetric}}
	metrics.Read(s[:])
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 1 << 62 // unknown: neither outgrow nor flush
	}
	return s[0].Value.Uint64()
}

//go:noinline
func useStack(n int) byte {
	var pad [1024]byte
	if n > 0 {
		pad[0] = useStack(n - len(pad))
	}
	return pad[len(pad)-1] + pad[n&1023]
}

// tooDeep reports whether the calling goroutine, which is running the
// parser, must stop. It is called from inside the parser.
func (g *stackGuard) tooDeep() bool {
	if g.stackMemory() < maxStackMemory {
		return false
	}
	if !g.alone {
		// This stack, another parse's, or one not given back yet: there is
		// no telling from here. Stop; the file is parsed again alone.
		g.contended = true
		return true
	}
	// No other parse is running and settle has seen the others' stacks go.
	// Either this run's stack is large, or the rest of the process holds
	// large stacks. Count this goroutine's frames: all of them are this
	// run's, and inlined calls count too, so the count is never below the
	// real one.
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
	n := min(len(p), len(r.src)-r.off, maxRead)
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
