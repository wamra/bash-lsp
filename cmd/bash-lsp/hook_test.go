package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookInput is the JSON a PostToolUse hook receives on standard input.
func hookInput(file string) string {
	return fmt.Sprintf(`{"hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":%q,"old_string":"a","new_string":"b"},"tool_response":{}}`, file)
}

func runHook(t *testing.T, stdin string) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run([]string{"check", "--hook"}, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func TestHookDisabledPrintsNothing(t *testing.T) {
	emptyPath(t)
	t.Setenv("BASH_LSP_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	dir := t.TempDir()
	putFile(t, dir, "e.sh", "#!/bin/bash\nlocal x=1\n")
	code, out, errs := runHook(t, hookInput(filepath.Join(dir, "e.sh")))
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d stdout %q stderr %q; want 0 and nothing", code, out, errs)
	}
}

func TestHookEnabledReportsToStderrWithExit2(t *testing.T) {
	emptyPath(t)
	userConfig(t, "[hook]\nenabled = true\n")
	dir := t.TempDir()
	putFile(t, dir, "e.sh", "#!/bin/bash\nlocal x=1\necho ok\n")
	code, out, errs := runHook(t, hookInput(filepath.Join(dir, "e.sh")))
	if code != 2 || out != "" {
		t.Fatalf("exit %d stdout %q; want 2 and an empty stdout\nstderr: %s", code, out, errs)
	}
	if !strings.Contains(errs, "e.sh:2:") || !strings.Contains(errs, "error WB1105") {
		t.Fatalf("stderr lacks the finding:\n%s", errs)
	}
}

func TestHookCleanFileAndOtherInputsExitZero(t *testing.T) {
	emptyPath(t)
	userConfig(t, "[hook]\nenabled = true\n")
	dir := t.TempDir()
	putFile(t, dir, "ok.sh", "echo ok\n")
	putFile(t, dir, "notes.txt", "#!/bin/bash\nlocal x=1\n")
	for name, in := range map[string]string{
		"clean shell file":  hookInput(filepath.Join(dir, "ok.sh")),
		"not a shell file":  hookInput(filepath.Join(dir, "notes.txt")),
		"missing file":      hookInput(filepath.Join(dir, "gone.sh")),
		"no file_path":      `{"tool_name":"Edit","tool_input":{}}`,
		"not json":          `not json`,
		"empty":             ``,
		"relative path":     hookInput("e.sh"),
		"file_path not str": `{"tool_input":{"file_path":3}}`,
	} {
		if code, out, errs := runHook(t, in); code != 0 || out != "" || errs != "" {
			t.Errorf("%s: exit %d stdout %q stderr %q; want 0 and nothing", name, code, out, errs)
		}
	}
}

func TestHookReportsOnlyWarningsAndErrors(t *testing.T) {
	emptyPath(t)
	userConfig(t, "[hook]\nenabled = true\n")
	dir := t.TempDir()
	putFile(t, dir, "w.sh", "source ./missing.sh\n") // WB1101, a warning
	code, _, errs := runHook(t, hookInput(filepath.Join(dir, "w.sh")))
	if code != 2 || !strings.Contains(errs, "warning WB1101") {
		t.Fatalf("exit %d stderr %q; want 2 and the warning", code, errs)
	}
	for _, l := range strings.Split(strings.TrimSpace(errs), "\n") {
		if !strings.Contains(l, " warning ") && !strings.Contains(l, " error ") {
			t.Errorf("line %q is neither a warning nor an error", l)
		}
	}
}

func TestHookUsageErrors(t *testing.T) {
	var o, e bytes.Buffer
	if code := run([]string{"check", "--hook", "x.sh"}, strings.NewReader(""), &o, &e); code != 2 {
		t.Errorf("--hook with a path: exit %d, want 2", code)
	}
}

// Review Focus 5: the hook analyses the one file it is given. Thousands of
// shell files two levels up, with no repository marker around, are not read.
func TestHookDoesNotWalkParent(t *testing.T) {
	emptyPath(t)
	userConfig(t, "[hook]\nenabled = true\n")
	tmp := t.TempDir()
	for i := 0; i < 3000; i++ {
		putFile(t, tmp, fmt.Sprintf("s%d.sh", i), "#!/bin/bash\nlocal y=1\n")
	}
	sub := filepath.Join(tmp, "x", "y")
	putFile(t, sub, "file.sh", "#!/bin/bash\nlocal z=1\n")
	start := time.Now()
	code, _, errs := runHook(t, hookInput(filepath.Join(sub, "file.sh")))
	if d := time.Since(start); d > time.Second {
		t.Errorf("hook took %v, want under 1s", d)
	}
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, errs)
	}
	if n := strings.Count(errs, "\n"); n != 1 || strings.Contains(errs, "s0.sh") {
		t.Errorf("want exactly one finding, of file.sh:\n%s", errs)
	}
}

// The plugin's hook file has the shape of the Claude Code hooks reference:
// hooks.PostToolUse[].matcher and hooks[]{type, command, args, timeout}.
func TestPluginHooksFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugin", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
				Timeout int      `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	g := f.Hooks["PostToolUse"]
	if len(f.Hooks) != 1 || len(g) != 1 || g[0].Matcher != "Write|Edit" || len(g[0].Hooks) != 1 {
		t.Fatalf("unexpected hook file: %s", raw)
	}
	h := g[0].Hooks[0]
	if h.Type != "command" || h.Command != "${CLAUDE_PLUGIN_ROOT}/bin/bash-lsp" ||
		strings.Join(h.Args, " ") != "check --hook" || h.Timeout <= 0 {
		t.Fatalf("unexpected command hook: %+v", h)
	}
}
