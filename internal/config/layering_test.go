package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
)

func TestLoadDirectoryIsNotRegular(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".bash-lsp.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Load(root, nil)
	if !hasWarning(s, "not a regular file") || len(s.Files) != 0 {
		t.Fatalf("%q", s.Warnings)
	}
}

func TestRepoLimitsFloor(t *testing.T) {
	user := []byte("[limits]\nmax_files = 500\nmax_file_bytes = 1000\n")
	s := Merge(user, []byte("[limits]\nmax_files = 600\nmax_file_bytes = 2000\n"), nil)
	if s.MaxFiles != 500 || s.MaxFileBytes != 1000 || !hasWarning(s, "limits.max_files", ".bash-lsp.toml:2") || !hasWarning(s, "limits.max_file_bytes") {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
	s = Merge(user, []byte("[limits]\nmax_files = 400\n"), nil)
	if s.MaxFiles != 400 || len(s.Warnings) != 0 {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
	// Equal is not lower.
	s = Merge(user, []byte("[limits]\nmax_files = 500\n"), nil)
	if s.MaxFiles != 500 || !hasWarning(s, "limits.max_files") {
		t.Fatalf("%q", s.Warnings)
	}
	// Trusted init options override the user file (still within the cap).
	s = Merge(user, nil, json.RawMessage(`{"limits":{"max_files":900}}`))
	if s.MaxFiles != 900 {
		t.Fatalf("%+v", s)
	}
}

func TestRepoCannotReEnableShellCheck(t *testing.T) {
	user := []byte("[shellcheck]\nenabled = false\n")
	repo := []byte("[shellcheck]\nenabled = true\n")
	s := Merge(user, repo, nil)
	if s.ShellCheck.Enabled || !hasWarning(s, "shellcheck.enabled", ".bash-lsp.toml:2") {
		t.Fatalf("%+v %q", s.ShellCheck, s.Warnings)
	}
	// With no user value ShellCheck is on by default: a repo value of true is
	// not a re-enable and is not refused.
	if s := Merge(nil, repo, nil); !s.ShellCheck.Enabled || len(s.Warnings) != 0 {
		t.Fatalf("repo enabled with no user value: %+v %q", s.ShellCheck, s.Warnings)
	}
	if s := Merge(nil, []byte("[shellcheck]\nenabled = false\n"), nil); s.ShellCheck.Enabled || len(s.Warnings) != 0 {
		t.Fatal("repo may turn ShellCheck off")
	}
	if s := Merge(user, repo, json.RawMessage(`{"shellcheck":{"enabled":true}}`)); !s.ShellCheck.Enabled {
		t.Fatal("init options override the user file")
	}
}

func TestRepoDisableIsUnioned(t *testing.T) {
	user := []byte("[diagnostics]\ndisable = [\"WB1\", \"WB2\"]\n")
	s := Merge(user, []byte("[diagnostics]\ndisable = [\"WB3\"]\n"), nil)
	if len(s.Disabled) != 3 || !s.Disabled["WB1"] || !s.Disabled["WB2"] || !s.Disabled["WB3"] {
		t.Fatalf("%v", s.Disabled)
	}
	s = Merge(user, []byte("[diagnostics]\ndisable = []\n"), nil)
	if len(s.Disabled) != 2 {
		t.Fatalf("repo cleared the user list: %v", s.Disabled)
	}
	s = Merge(user, nil, json.RawMessage(`{"diagnostics":{"disable":["WB9"]}}`))
	if len(s.Disabled) != 1 || !s.Disabled["WB9"] {
		t.Fatalf("init replaces: %v", s.Disabled)
	}
}

func TestRepoSeverityCannotReEnable(t *testing.T) {
	user := []byte("[diagnostics]\ndisable = [\"WB1\"]\n[severity]\nWB2 = \"info\"\n")
	repo := []byte("[severity]\nWB1 = \"error\"\nWB2 = \"error\"\nWB3 = \"hint\"\n")
	s := Merge(user, repo, nil)
	if _, ok := s.Severity["WB1"]; ok || !hasWarning(s, "severity.WB1", ".bash-lsp.toml:2") {
		t.Fatalf("%v %q", s.Severity, s.Warnings)
	}
	if s.Severity["WB2"] != analysis.SevInfo || !hasWarning(s, "severity.WB2") || s.Severity["WB3"] != analysis.SevHint {
		t.Fatalf("%v", s.Severity)
	}
	// The repo's own disable list does not block its own severity line.
	s = Merge(nil, []byte("[severity]\nWB1 = \"error\"\n[diagnostics]\ndisable = [\"WB1\"]\n"), nil)
	if len(s.Warnings) != 0 {
		t.Fatalf("%q", s.Warnings)
	}
	// Init options may re-enable.
	s = Merge(user, nil, json.RawMessage(`{"severity":{"WB1":"error"}}`))
	if s.Severity["WB1"] != analysis.SevError {
		t.Fatalf("%v", s.Severity)
	}
}

func TestResolvedOnlyKeys(t *testing.T) {
	cases := []struct {
		init     string
		want     bool
		warnings int
	}{
		{`{"references":{"resolved_only":true}}`, true, 0},
		{`{"references":{"resolvedOnly":true}}`, true, 0},
		{`{"references":{"resolved_only":true,"resolvedOnly":true}}`, true, 0},
		{`{"references":{"resolved_only":false,"resolvedOnly":true}}`, false, 1},
		{`{"references":{"resolved_only":true,"resolvedOnly":false}}`, true, 1},
	}
	for _, c := range cases {
		s := Merge(nil, nil, json.RawMessage(c.init))
		if s.ResolvedOnly != c.want || len(s.Warnings) != c.warnings || (c.warnings > 0 && !hasWarning(s, "references.resolved_only")) {
			t.Errorf("%s: %v %q", c.init, s.ResolvedOnly, s.Warnings)
		}
	}
	s := Merge([]byte("[references]\nresolved_only = true\n"), nil, nil)
	if !s.ResolvedOnly {
		t.Fatal("toml canonical key")
	}
}

func TestInitShapeErrors(t *testing.T) {
	s := Merge(nil, nil, json.RawMessage(`{"bash":"x"}`))
	if !hasWarning(s, "bash", "must be an object") || len(s.Warnings) != 1 {
		t.Fatalf("%q", s.Warnings)
	}
	s = Merge(nil, nil, json.RawMessage(`{"bash":{"target":"4.0-5.0"}} {"bash":{"target":"5.0-5.0"}}`))
	if s.Target != analysis.DefaultTarget || !hasWarning(s, "after the first JSON value") {
		t.Fatalf("%+v %q", s.Target, s.Warnings)
	}
	s = Merge(nil, nil, json.RawMessage(`{"bash.target":"4.0-5.0","bash":{"x.y":1}}`))
	if s.Target != analysis.DefaultTarget || !hasWarning(s, "bash.target", "dotted") || !hasWarning(s, "bash.x.y", "dotted") {
		t.Fatalf("%+v %q", s.Target, s.Warnings)
	}
}

func TestCheckRules(t *testing.T) {
	s := Merge([]byte("[diagnostics]\ndisable = [\"WB1001\", \"WB9999\"]\n[severity]\nWB1002 = \"hint\"\nXX = \"info\"\n"), nil, nil)
	registry := map[string]bool{"WB1001": true, "WB1002": true}
	got := s.CheckRules(func(id string) bool { return registry[id] })
	want := []string{
		`diagnostics.disable: unknown rule id "WB9999"`,
		`severity.XX: unknown rule id "XX"`,
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("%q", got)
	}
	if got := s.CheckRules(func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
