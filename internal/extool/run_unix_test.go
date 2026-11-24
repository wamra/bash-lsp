//go:build !windows

package extool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	// spawn: start a grandchild that sleeps, print its pid, then sleep too.
	fakeModes["spawn"] = func() {
		c := exec.Command(os.Args[0])
		c.Env = []string{"WB_FAKE_TOOL=sleep"}
		if err := c.Start(); err != nil {
			os.Exit(65)
		}
		fmt.Println(c.Process.Pid)
		time.Sleep(time.Hour)
	}
	// holder: start a grandchild that keeps stdout open, print its pid, exit 0.
	fakeModes["holder"] = func() {
		c := exec.Command(os.Args[0])
		c.Env = []string{"WB_FAKE_TOOL=sleep"}
		c.Stdout = os.Stdout
		if err := c.Start(); err != nil {
			os.Exit(65)
		}
		fmt.Println(c.Process.Pid)
	}
	fakeModes["crash"] = func() { syscall.Kill(os.Getpid(), syscall.SIGKILL); time.Sleep(time.Minute) }
}

func gone(pid int) bool {
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func firstInt(t *testing.T, b []byte) int {
	t.Helper()
	line, _ := bufio.NewReader(bytes.NewReader(b)).ReadString('\n')
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("no pid in output %q", b)
	}
	return pid
}

// A grandchild of a child killed by the timeout goes with it (the process
// group is killed).
func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("spawn"), Timeout: time.Second})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if pid := firstInt(t, res.Stdout); !gone(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild %d outlived the timeout", pid)
	}
}

func TestRunCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	res, err := Run(ctx, fake(t), nil, RunOptions{Env: mode("spawn"), Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if pid := firstInt(t, res.Stdout); !gone(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild %d outlived the cancel", pid)
	}
}

// A child that exits but leaves a descendant holding its output does not hang
// Run, and the descendant is killed.
func TestRunDescendantHoldingPipe(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("holder"), Timeout: 30 * time.Second})
	if err == nil {
		t.Error("no error for a descendant that holds the output open")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if pid := firstInt(t, res.Stdout); !gone(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("descendant %d was left running", pid)
	}
}

func TestRunCrashIsAnError(t *testing.T) {
	_, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("crash"), Timeout: 10 * time.Second})
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want a signal error", err)
	}
}

func init() {
	// detach: a grandchild in the same group that holds no pipe; the child exits 0.
	fakeModes["detach"] = func() {
		c := exec.Command(os.Args[0])
		c.Env = []string{"WB_FAKE_TOOL=sleep"}
		if err := c.Start(); err != nil {
			os.Exit(65)
		}
		fmt.Println(c.Process.Pid)
	}
}

// After a normal exit the group is killed too: a grandchild that holds no
// pipe does not survive.
func TestRunKillsGroupAfterNormalExit(t *testing.T) {
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("detach"), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if pid := firstInt(t, res.Stdout); !gone(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild %d outlived a normal exit", pid)
	}
}
