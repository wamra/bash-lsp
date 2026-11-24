package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/analysis"
)

func TestRepoTimeoutFloor(t *testing.T) {
	user := []byte("[shellcheck]\ntimeout_seconds = 2\n")
	s := Merge(user, []byte("[shellcheck]\ntimeout_seconds = 120\n"), nil)
	if s.ShellCheck.Timeout != 2*time.Second || !hasWarning(s, "shellcheck.timeout_seconds", ".bash-lsp.toml:2") {
		t.Fatalf("%v %q", s.ShellCheck.Timeout, s.Warnings)
	}
	// Equal is not lower, as for the limits.
	s = Merge(user, []byte("[shellcheck]\ntimeout_seconds = 2\n"), nil)
	if s.ShellCheck.Timeout != 2*time.Second || !hasWarning(s, "shellcheck.timeout_seconds") {
		t.Fatalf("%q", s.Warnings)
	}
	s = Merge(user, []byte("[shellcheck]\ntimeout_seconds = 1\n"), nil)
	if s.ShellCheck.Timeout != time.Second || len(s.Warnings) != 0 {
		t.Fatalf("%v %q", s.ShellCheck.Timeout, s.Warnings)
	}
	// Without a user value the built-in 10 s is the floor.
	s = Merge(nil, []byte("[shellcheck]\ntimeout_seconds = 60\n"), nil)
	if s.ShellCheck.Timeout != 10*time.Second || len(s.Warnings) != 1 {
		t.Fatalf("%v %q", s.ShellCheck.Timeout, s.Warnings)
	}
	s = Merge(nil, []byte("[shellcheck]\ntimeout_seconds = 5\n"), nil)
	if s.ShellCheck.Timeout != 5*time.Second {
		t.Fatal(s.ShellCheck.Timeout)
	}
	// Init options override.
	s = Merge(user, nil, json.RawMessage(`{"shellcheck":{"timeout_seconds":90}}`))
	if s.ShellCheck.Timeout != 90*time.Second {
		t.Fatal(s.ShellCheck.Timeout)
	}
}

func TestRepoSeverityKeepsUserValue(t *testing.T) {
	user := []byte("[severity]\nWB1 = \"hint\"\n")
	repo := []byte("[severity]\nWB1 = \"error\"\nWB2 = \"error\"\n")
	s := Merge(user, repo, nil)
	if s.Severity["WB1"] != analysis.SevHint || s.Severity["WB2"] != analysis.SevError ||
		!hasWarning(s, "severity.WB1", ".bash-lsp.toml:2") || len(s.Warnings) != 1 {
		t.Fatalf("%v %q", s.Severity, s.Warnings)
	}
	s = Merge(user, nil, json.RawMessage(`{"severity":{"WB1":"error"}}`))
	if s.Severity["WB1"] != analysis.SevError {
		t.Fatalf("%v", s.Severity)
	}
}

func TestParseTOMLTrailingCR(t *testing.T) {
	if _, err := ParseTOML([]byte("x = 1\na = 1\r")); err == nil || !contains(err.Error(), "line 2") {
		t.Fatal(err)
	}
	if _, err := ParseTOML([]byte("a = 1\r\n")); err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
