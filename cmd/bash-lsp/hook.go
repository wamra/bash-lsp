package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/wamra/bash-lsp/internal/config"
)

// hookInputCap bounds the JSON read from standard input. The edited file's
// path is near the start; an Edit or Write input also carries the text.
const hookInputCap = 8 << 20

// hookShellExts are the files the hook looks at; the same set the workspace
// treats as shell files.
var hookShellExts = map[string]bool{".sh": true, ".bash": true, ".bats": true, ".ksh": true, ".mksh": true}

// cmdHook is `check --hook`: the PostToolUse command of the plugin. It reads
// the hook JSON from stdin, and when the user settings enable it (hook.enabled,
// off by default and never taken from a repository file) checks the edited
// shell file like `check FILE` does: the file alone unless a repository marker
// is above it, never the directories around it. Warnings and errors go to
// stderr and the exit code is 2, which PostToolUse shows to Claude. Anything
// else (disabled, not a shell file, nothing found) exits 0 with no output; a
// check that could not run exits 1 with its message, which is not shown to
// Claude.
func cmdHook(stdin io.Reader, stderr io.Writer) int {
	if !config.Load("", nil).HookEnabled {
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, hookInputCap))
	if err != nil {
		return 0
	}
	var in struct {
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return 0
	}
	file := in.ToolInput.FilePath
	if file == "" || !filepath.IsAbs(file) || !hookShellExts[strings.ToLower(filepath.Ext(file))] {
		return 0
	}
	var out, errb bytes.Buffer
	code := runCheck([]string{"--format", "json", "--", file}, &out, &errb)
	if code == checkRuntime || code == checkUsageErr {
		// A file that is gone is not worth Claude's attention.
		if errb.Len() > 0 && !strings.Contains(errb.String(), "no such file") {
			fmt.Fprint(stderr, errb.String())
			return 1
		}
		return 0
	}
	var res struct {
		Diagnostics []checkDiag `json:"diagnostics"`
	}
	if json.Unmarshal(out.Bytes(), &res) != nil {
		return 0
	}
	n := 0
	for _, d := range res.Diagnostics {
		if d.Severity != "error" && d.Severity != "warning" {
			continue
		}
		fmt.Fprintf(stderr, "%s:%d:%d: %s %s %s\n", d.Path, d.Line, d.Col, d.Severity, d.Rule, oneLine(d.Message))
		n++
	}
	if n > 0 {
		return 2
	}
	return 0
}
