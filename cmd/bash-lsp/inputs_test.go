package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putFile(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// userConfig points BASH_LSP_CONFIG at a file with text for one test.
func userConfig(t *testing.T, text string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_LSP_CONFIG", p)
}

// Calls of external commands are not part of the graph: extract writes the
// same records whatever the PATH profile holds.
func TestExtractLeavesOutCommandEdges(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, "a.sh", "#!/bin/bash\nf() { ls; }\nf\ndeplyo app\nls\n")
	bin := t.TempDir()
	putFile(t, bin, "ls", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(bin, "ls"), 0o755); err != nil {
		t.Fatal(err)
	}
	userConfig(t, "[path]\nprofile = [\""+bin+"\"]\n")
	with := runOK(t, "extract", "--root", root)
	userConfig(t, "[path]\nprofile = []\n")
	without := runOK(t, "extract", "--root", root)
	if with != without || strings.Contains(with, "deplyo") || strings.Contains(with, bin) {
		t.Errorf("extract depends on the profile:\n%s\n---\n%s", with, without)
	}
	if !strings.Contains(with, `"name":"f"`) {
		t.Errorf("the call of f is missing:\n%s", with)
	}
}

func TestSettingsReachTheCLI(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, ".bash-lsp.toml", "[files]\nexclude = [\"vendor/\"]\n[sources]\npaths = [\"lib\", \"../x\"]\n[limits]\nmax_files = 3\n")
	putFile(t, root, "vendor/v.sh", "vend() { :; }\n")
	putFile(t, root, "lib/util.sh", "util() { :; }\n")
	putFile(t, root, "bin/run.sh", "#!/bin/bash\nsource util.sh\nutil\n")
	userConfig(t, "")
	out := runOK(t, "doctor", root)
	wantLines(t, out, "files: 2", "unresolved sources: 0", `notice: sources.paths "../x"`)
	if strings.Contains(runOK(t, "symbols", root), "vend") {
		t.Error("symbols lists an excluded file")
	}
}

func TestSettingsWarningsGoToStderr(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, ".bash-lsp.toml", "[files]\nbogus = 1\n")
	putFile(t, root, "a.sh", "echo\n")
	userConfig(t, "")
	code, _, errs := runCode(t, "doctor", root)
	if code != 0 || !strings.Contains(errs, "unknown key files.bogus") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}
