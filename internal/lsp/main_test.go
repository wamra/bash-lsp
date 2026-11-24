package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if mode := os.Getenv("WB_FAKE_TOOL"); mode != "" {
		runFakeTool(mode)
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
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			panic(err)
		}
	}
	testBin = bin
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[path]\nprofile = [\""+bin+"\"]\n"), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", cfg)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
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
	body := "[path]\nprofile = [\"" + testBin + "\"]\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
