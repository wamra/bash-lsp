package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testBin is the PATH profile directory of the tests (see TestMain).
var testBin string

// TestMain points the user settings file at a file of its own, so no test
// reads the real user configuration or the real $PATH: the file names a PATH
// profile directory holding a few programs.
//
// The test binary is also the fake ShellCheck of the tests that need one
// (fakeShellCheck): started with WB_FAKE_TOOL set it plays that part and exits.
func TestMain(m *testing.M) {
	if mode := fakeToolMode(); mode != "" {
		fakeLog("mode %s, args %q", mode, os.Args[1:])
		runFakeTool(mode)
		fakeLog("answered")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "bash-lsp-lsp-test")
	if err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		panic(err)
	}
	for _, n := range []string{"ls", "cat", "grep", "rm", "mkdir"} {
		// On Windows a program needs an executable extension to count as one.
		if err := os.WriteFile(filepath.Join(bin, n+exeExt()), []byte("#!/bin/sh\n"), 0o755); err != nil {
			panic(err)
		}
	}
	testBin = bin
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[path]\nprofile = ["+tomlLiteral(bin)+"]\n"), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", cfg)
	code := m.Run()
	os.RemoveAll(dir)
	if fakeBase != "" {
		os.RemoveAll(filepath.Dir(fakeBase))
	}
	os.Exit(code)
}

// absPath is p, written as a Unix absolute path ("/x/a.sh"), as an absolute
// path of the host: unchanged on Unix; on Windows it takes the current volume
// and uses backslashes.
func absPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	a, err := filepath.Abs(filepath.FromSlash(p))
	if err != nil {
		return p
	}
	return a
}

// tomlLiteral is p as a TOML literal string: nothing in it is an escape, so a
// Windows path ("C:\Users\x") reads back as it is written.
func tomlLiteral(p string) string { return "'" + p + "'" }

// exeExt is the extension a program needs on this platform.
func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// fakeLog appends a line to the record of this fake run (NAME.log beside the
// program), when it plays the fake as a program of its own; fakeShellCheck's
// cleanup prints the record of a failed test, so a run that never answered
// can be told from one whose answer was lost.
func fakeLog(format string, args ...any) {
	exe, err := os.Executable()
	if err != nil || os.Getenv("WB_FAKE_TOOL") != "" {
		return
	}
	if f, err := os.OpenFile(exe+".log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s pid %d: %s\n", time.Now().Format("15:04:05.000"), os.Getpid(), fmt.Sprintf(format, args...))
		f.Close()
	}
}

// fakeBase is the one copy of the test binary the Windows fakes link to: a
// new program is checked by the virus scanner when it first runs, and with a
// fresh copy per test that cost fell on every test.
var (
	fakeBase     string
	fakeBaseOnce sync.Once
)

// fakeToolMode is the mode this process plays the fake ShellCheck in, or "".
// On Unix the fake is a script that sets WB_FAKE_TOOL. On Windows it is a link
// to a copy of the test binary named shellcheck.exe, and the mode and the extra
// environment are in the file beside it, NAME.fake (see fakeShellCheck): the
// runner passes the fake only a short list of environment variables.
func fakeToolMode() string {
	if mode := os.Getenv("WB_FAKE_TOOL"); mode != "" {
		return mode
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(exe + ".fake")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, kv := range lines[1:] {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			os.Setenv(k, v)
		}
	}
	return strings.TrimSpace(lines[0])
}

// runFakeTool is the fake ShellCheck. It reads the script on standard input
// and answers by mode:
//
//	hang   appends "start <pid>" to $WB_RUNLOG, then never answers
//	mark   one finding "mark:<the line of the script containing MARK>"; a
//	       script containing SLOW takes 500 ms; every process appends "start"
//	       to $WB_RUNLOG when it starts and "run" once it has read the script
//	clean  no findings
//	sleep  never answers (the runner's timeout kills it)
//	flood  writes without end (the runner's output cap kills it)
//	exit2  fails with status 2 and a message on standard error
func runFakeTool(mode string) {
	switch mode {
	case "hang":
		runLog("start %d", os.Getpid())
		time.Sleep(time.Hour)
	case "mark":
		runLog("start")
		src, _ := io.ReadAll(os.Stdin)
		runLog("run")
		if bytes.Contains(src, []byte("SLOW")) {
			time.Sleep(500 * time.Millisecond)
		}
		msg := ""
		for _, l := range strings.Split(string(src), "\n") {
			if strings.Contains(l, "MARK") {
				msg = "mark:" + strings.TrimSpace(l)
			}
		}
		b, _ := json.Marshal(msg)
		fmt.Printf(`{"comments":[{"file":"-","line":1,"endLine":1,"column":1,"endColumn":2,"level":"info","code":9999,"message":%s,"fix":null}]}`, b)
	case "clean":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"comments":[]}`)
	case "sleep":
		time.Sleep(time.Hour)
	case "flood":
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(9)
			}
		}
	case "exit2":
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprint(os.Stderr, "\nfake failure: first line\nsecond line")
		fakeLog("answered with exit 2")
		os.Exit(2)
	default:
		os.Exit(64)
	}
}

// runLog appends a line to $WB_RUNLOG, when it is set.
func runLog(format string, args ...any) {
	p := os.Getenv("WB_RUNLOG")
	if p == "" {
		return
	}
	if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, format+"\n", args...)
		f.Close()
	}
}

// fakeShellCheck writes a program, outside every workspace, that runs the test
// binary as the fake ShellCheck in mode (the runner passes only a short list of
// environment variables, so the mode travels in the script). env are extra
// NAME=value pairs for the fake. It returns the path to configure as
// shellcheck.path.
func fakeShellCheck(t *testing.T, mode string, env ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Not a link to the test binary itself: that is the file whose image is
		// mapped, and Windows does not delete any name of such a file.
		// The directory is removed with retries, as the copy's process may
		// still be going away.
		dir, err := os.MkdirTemp("", "bash-lsp-fake-shellcheck")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			var rerr error
			for i := 0; i < 50; i++ {
				if rerr = os.RemoveAll(dir); rerr == nil {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Errorf("removing %s: %v", dir, rerr)
		})
		p := filepath.Join(dir, "shellcheck.exe")
		fakeBaseOnce.Do(func() {
			bd, err := os.MkdirTemp("", "bash-lsp-fake-base")
			if err != nil {
				return
			}
			b, err := os.ReadFile(exe)
			if err != nil {
				return
			}
			base := filepath.Join(bd, "base.exe")
			if os.WriteFile(base, b, 0o755) == nil {
				fakeBase = base
			}
		})
		// Linked to the copy, never to the test binary itself.
		if fakeBase == "" || os.Link(fakeBase, p) != nil {
			b, rerr := os.ReadFile(exe)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if err := os.WriteFile(p, b, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			if t.Failed() {
				rec, _ := os.ReadFile(p + ".log")
				t.Logf("record of the fake ShellCheck runs (%s):\n%s", mode, rec)
			}
		})
		if err := os.WriteFile(p+".fake", []byte(mode+"\n"+strings.Join(env, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Start the program once now, in a mode that writes no record, so that
		// whatever Windows does the first time a new program runs (a scan, a
		// slow image load) happens before the test's own timers; its duration
		// is kept for a failed test's output.
		began := time.Now()
		warm := exec.Command(p)
		warm.Env = append(os.Environ(), "WB_FAKE_TOOL=clean")
		werr := warm.Run()
		took := time.Since(began)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("first start of the fake took %s (error: %v)", took, werr)
			}
		})
		return p
	}
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\nWB_FAKE_TOOL=" + mode)
	for _, kv := range env {
		sb.WriteString(" " + kv)
	}
	sb.WriteString(" exec '" + exe + "' \"$@\"\n")
	p := filepath.Join(t.TempDir(), "shellcheck")
	if err := os.WriteFile(p, []byte(sb.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// userConfig makes BASH_LSP_CONFIG name a new file that holds the tests' PATH
// profile (so no real program is found) followed by extra; it returns the path.
func userConfig(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	writeUserConfig(t, p, extra)
	t.Setenv("BASH_LSP_CONFIG", p)
	return p
}

func writeUserConfig(t *testing.T, p, extra string) {
	t.Helper()
	body := "[path]\nprofile = [" + tomlLiteral(testBin) + "]\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
