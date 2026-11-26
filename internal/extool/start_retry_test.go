package extool

import (
	"context"
	"io/fs"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// swap replaces the starter and the system the errors are read as, for one test.
func swap(t *testing.T, windows bool, start func(*exec.Cmd) error) {
	t.Helper()
	oldStart, oldWin := startCmd, onWindows
	startCmd, onWindows = start, windows
	t.Cleanup(func() { startCmd, onWindows = oldStart, oldWin })
}

func sharingViolation() error {
	return &fs.PathError{Op: "fork/exec", Path: "shellcheck.exe", Err: syscall.Errno(32)}
}

func TestRunRetriesAStartBlockedBySharingViolation(t *testing.T) {
	calls := 0
	swap(t, true, func(c *exec.Cmd) error {
		calls++
		if calls <= 2 {
			return sharingViolation()
		}
		return c.Start()
	})
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("json"), Timeout: 10 * time.Second})
	if err != nil || res.Exit != 1 || calls != 3 {
		t.Fatalf("err %v, exit %d, %d start attempts (want a result after 3)", err, res.Exit, calls)
	}
}

func TestRunGivesUpOnAStartThatKeepsFailing(t *testing.T) {
	calls := 0
	swap(t, true, func(*exec.Cmd) error { calls++; return sharingViolation() })
	_, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("json"), Timeout: 10 * time.Second})
	if err == nil || calls != startRetries+1 {
		t.Fatalf("err %v after %d attempts, want an error after %d", err, calls, startRetries+1)
	}
}

func TestRunDoesNotRetryOtherStartErrorsOrOtherSystems(t *testing.T) {
	for name, c := range map[string]struct {
		windows bool
		err     error
	}{
		"access denied on windows":   {true, &fs.PathError{Op: "fork/exec", Err: syscall.Errno(5)}},
		"errno 32 away from windows": {false, sharingViolation()},
	} {
		calls := 0
		swap(t, c.windows, func(*exec.Cmd) error { calls++; return c.err })
		if _, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("json")}); err == nil || calls != 1 {
			t.Errorf("%s: err %v after %d attempts, want one", name, err, calls)
		}
	}
}

func TestRunStopsRetryingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	swap(t, true, func(*exec.Cmd) error { calls++; cancel(); return sharingViolation() })
	if _, err := Run(ctx, fake(t), nil, RunOptions{Env: mode("json")}); err == nil || calls != 1 {
		t.Fatalf("err %v after %d attempts, want one", err, calls)
	}
}
