package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "bash-lsp dev" {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"nope"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "usage") {
		t.Fatalf("stderr lacks usage: %q", errb.String())
	}
}

func frame(body string) string { return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body) }

// With no arguments the binary serves LSP on stdio: stdout carries only frames.
func TestNoArgumentsServes(t *testing.T) {
	in := frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`) +
		frame(`{"jsonrpc":"2.0","id":2,"method":"shutdown"}`) + frame(`{"jsonrpc":"2.0","method":"exit"}`)
	for _, args := range [][]string{nil, {"serve"}} {
		var out, errb bytes.Buffer
		if code := run(args, strings.NewReader(in), &out, &errb); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errb.String())
		}
		if !strings.HasPrefix(out.String(), "Content-Length: ") || !strings.Contains(out.String(), `"name":"bash-lsp"`) {
			t.Fatalf("%v: stdout %q", args, out.String())
		}
	}
}

func TestServeEndOfInputExitsNonZero(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errb); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if out.Len() != 0 || !strings.Contains(errb.String(), "input ended") {
		t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
	}
}
