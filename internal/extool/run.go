package extool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Tool is a program the server may start: Name is what it is called ("shellcheck"),
// Path the absolute, symlink-resolved file Find chose.
type Tool struct{ Name, Path string }

// MaxStderr is the most of a child's standard error that Result keeps.
const MaxStderr = 64 << 10

const (
	defaultTimeout   = 30 * time.Second
	defaultMaxOutput = 4 << 20
	waitDelay        = 2 * time.Second // after the child is gone: how long its output pipes may stay open
)

// RunOptions control one run. Dir is the child's working directory. Stdin is
// its standard input. Env is the child's whole environment: nil means the
// server's own, a non-nil empty list means none (the caller picks; the
// ShellCheck bridge passes a short list). A Timeout or MaxOutput of zero or
// less means 30 s or 4 MiB.
type RunOptions struct {
	Dir       string
	Stdin     []byte
	Env       []string
	Timeout   time.Duration
	MaxOutput int
}

// Result is what a child wrote and how it ended. Stderr is cut at MaxStderr
// bytes; Stdout is cut at RunOptions.MaxOutput (and then Run fails with
// ErrOutputCap). Exit is -1 when no exit status exists (killed, never started).
type Result struct {
	Stdout, Stderr []byte
	Exit           int
}

// ErrTimeout and ErrOutputCap are what Run returns when it killed the child
// for running too long or for writing more than MaxOutput bytes to stdout.
var (
	ErrTimeout   = errors.New("external tool timed out")
	ErrOutputCap = errors.New("external tool wrote more output than allowed")
)

// Run starts t with args and waits for it. The program is started directly
// (exec.CommandContext): no shell reads the arguments, so text in them is never
// interpreted; callers still must not put analysed text in an argument that a
// program could read as an option, and send file content on Stdin or by a path
// the server owns. Exit status is not an error here; the caller judges it.
//
// What is bounded: wall time (Timeout, and the caller's context: a cancelled
// request kills the child); stdout (MaxOutput, past which the child is killed
// and the rest never read); stderr (the first MaxStderr bytes are kept, the
// rest is discarded); lingering processes (on Unix the child leads its own
// process group and the whole group is killed on timeout, cancel or output
// cap, and once more after every run, normal exit included, so a descendant
// that stays in the group dies even when it holds no pipe; a descendant that
// calls setsid or setpgid leaves the group and escapes, and on Windows only
// the child itself is killed). When Run returns no goroutine of it is left.
// What is not bounded: the child's memory and CPU while it runs (the standard
// library cannot set a resource limit on a child alone, and setting one on
// the server would limit the server), its writes to files, and its use of the
// network. Nothing limits those, so the tool must be one the user chose and
// its working directory and environment (see ChildEnv, NeutralDir) must not
// let a workspace steer it.
//
// The error is ErrTimeout, ErrOutputCap, the caller's context error, a start
// failure, or a description of a death by signal; Result is filled as far as
// the child got.
func Run(ctx context.Context, t Tool, args []string, o RunOptions) (Result, error) {
	if t.Path == "" || !filepath.IsAbs(t.Path) {
		return Result{Exit: -1}, fmt.Errorf("external tool %q: the path %q is not absolute", t.Name, t.Path)
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.MaxOutput <= 0 {
		o.MaxOutput = defaultMaxOutput
	}
	runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	var capped atomic.Bool
	out := &capWriter{max: o.MaxOutput, over: func() { capped.Store(true); cancel() }}
	errw := &capWriter{max: MaxStderr}
	var cmd *exec.Cmd
	var group procGroup
	var werr error
	for attempt := 0; ; attempt++ {
		// A Cmd cannot start twice, so each attempt builds its own.
		cmd = exec.CommandContext(runCtx, t.Path, args...)
		cmd.Dir = o.Dir
		cmd.Env = o.Env
		if o.Stdin != nil {
			cmd.Stdin = bytes.NewReader(o.Stdin)
		}
		cmd.Stdout, cmd.Stderr = out, errw
		cmd.WaitDelay = waitDelay
		group = setGroup(cmd)
		if werr = startCmd(cmd); werr == nil {
			werr = cmd.Wait()
			break
		}
		if attempt >= startRetries || !transientStart(werr, onWindows) || sleepCtx(runCtx, startBackoff<<attempt) != nil {
			break
		}
	}
	res := Result{Stdout: out.bytes(), Stderr: errw.bytes(), Exit: -1}
	if cmd.ProcessState != nil {
		res.Exit = cmd.ProcessState.ExitCode()
	}
	group.killAll() // the child is gone; kill what it left in its group
	switch {
	case capped.Load():
		return res, ErrOutputCap
	case runCtx.Err() != nil && werr != nil: // killed by the deadline or the caller
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, ErrTimeout
	case errors.Is(werr, exec.ErrWaitDelay):
		return res, fmt.Errorf("external tool %q left a process holding its output open", t.Name)
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if res.Exit == -1 { // no status: ended by a signal
			return res, fmt.Errorf("external tool %q: %s", t.Name, ee.ProcessState)
		}
		return res, nil
	}
	if werr != nil {
		return res, fmt.Errorf("external tool %q: %w", t.Name, werr)
	}
	return res, nil
}

// capWriter keeps the first max bytes written to it and drops the rest, so a
// child never blocks on a full pipe; over, when set, is called once on the
// first byte past max.
type capWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	full bool
	over func()
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.max - w.buf.Len(); len(p) <= room {
		w.buf.Write(p)
	} else {
		if room > 0 {
			w.buf.Write(p[:room])
		}
		if !w.full {
			w.full = true
			if w.over != nil {
				w.over()
			}
		}
	}
	return len(p), nil
}

func (w *capWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// onWindows tells transientStart which system the start errors come from; a
// test sets it.
var onWindows = runtime.GOOS == "windows"

// startCmd starts a prepared command; a test replaces it to make a start fail.
var startCmd = func(c *exec.Cmd) error { return c.Start() }

// A start that fails because another process still holds the program's file
// (on Windows, ERROR_SHARING_VIOLATION: a virus scanner or the writer of a
// program that was just created) is tried again after startBackoff, twice that,
// and so on, startRetries times in all.
const (
	startRetries = 4
	startBackoff = 50 * time.Millisecond
)

// transientStart reports whether err from starting a program is the sharing
// violation of Windows. Errno 32 means something else on other systems, so
// windows says which one the error comes from.
func transientStart(err error, windows bool) bool {
	const errorSharingViolation = syscall.Errno(32)
	return windows && errors.Is(err, errorSharingViolation)
}

// sleepCtx waits d or until ctx ends, and returns ctx's error in that case.
func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
