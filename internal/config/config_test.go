package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

// Every test runs without a user config unless it sets one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wb-config-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", filepath.Join(dir, "none.toml"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func hasWarning(s Settings, parts ...string) bool {
	for _, w := range s.Warnings {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(w, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestMergeDefaults(t *testing.T) {
	s := Merge(nil, nil, nil)
	if s.Target != analysis.DefaultTarget || !s.ShellCheck.Enabled || s.ShellCheck.Timeout != 10*time.Second ||
		s.HookEnabled || s.Dialect != 0 || s.MaxFiles != 0 || s.MaxFileBytes != 0 || s.PathProfile != nil || len(s.Warnings) != 0 || len(s.Refused) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestMergePrecedence(t *testing.T) {
	user := []byte("[bash]\ntarget = \"4.0-5.3\"\n")
	repo := []byte("[bash]\ntarget = \"4.2-5.3\"\n")
	init := json.RawMessage(`{"bash":{"target":"5.0-5.3"}}`)
	if got := Merge(user, repo, init).Target.Min; got != (analysis.BashVersion{Major: 5, Minor: 0}) {
		t.Fatalf("with init: %v", got)
	}
	if got := Merge(user, repo, nil).Target.Min; got != (analysis.BashVersion{Major: 4, Minor: 2}) {
		t.Fatalf("without init: %v", got)
	}
	if got := Merge(user, nil, nil).Target.Min; got != (analysis.BashVersion{Major: 4, Minor: 0}) {
		t.Fatalf("user only: %v", got)
	}
}

func TestRepoCannotNameExecutable(t *testing.T) {
	repo := []byte("[shellcheck]\npath = \"./shellcheck\"\n[codeindex]\npath = \"x\"\n[path]\nprofile = [\"/a\"]\n[hook]\nenabled = true\n")
	s := Merge(nil, repo, nil)
	if s.ShellCheck.Path != "" || s.CodeIndexPath != "" || s.PathProfile != nil || s.HookEnabled {
		t.Fatalf("repo executable accepted: %+v", s)
	}
	if len(s.Refused) != 4 {
		t.Fatalf("refusals: %+v", s.Refused)
	}
	r := s.Refused[0]
	if r.File != ".bash-lsp.toml" || r.Key != "shellcheck.path" || r.Reason != "executables and hooks are accepted only from user settings" {
		t.Fatalf("%+v", r)
	}
	for i, k := range []string{"shellcheck.path", "codeindex.path", "path.profile", "hook.enabled"} {
		if s.Refused[i].Key != k {
			t.Fatalf("refusal %d: %+v", i, s.Refused[i])
		}
	}
	// A repo file cannot override a user-level executable either.
	user := []byte("[shellcheck]\npath = \"/opt/sc\"\n")
	if got := Merge(user, repo, nil).ShellCheck.Path; got != "/opt/sc" {
		t.Fatal(got)
	}
}

func TestUserAndInitMayNameExecutables(t *testing.T) {
	user := []byte("[shellcheck]\npath = \"/opt/sc\"\ntimeout_seconds = 5\n[codeindex]\npath = \"/opt/ci\"\n[path]\nprofile = [\"/usr/bin\"]\n[hook]\nenabled = true\n")
	s := Merge(user, nil, nil)
	if s.ShellCheck.Path != "/opt/sc" || s.ShellCheck.Timeout != 5*time.Second || s.CodeIndexPath != "/opt/ci" ||
		len(s.PathProfile) != 1 || !s.HookEnabled || len(s.Refused) != 0 || len(s.Warnings) != 0 {
		t.Fatalf("%+v", s)
	}
	s = Merge(nil, nil, json.RawMessage(`{"shellcheck":{"path":"/i/sc","enabled":false},"hook":{"enabled":true},"path":{"profile":[]}}`))
	if s.ShellCheck.Path != "/i/sc" || s.ShellCheck.Enabled || !s.HookEnabled || s.PathProfile == nil || len(s.PathProfile) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestUnknownKeyWarned(t *testing.T) {
	s := Merge(nil, []byte("[files]\nexclud=[\"x\"]\n"), nil)
	if !hasWarning(s, "files.exclud", ".bash-lsp.toml:2") {
		t.Fatalf("%q", s.Warnings)
	}
	s = Merge(nil, []byte("nokey = 1\n[nosuch]\nk = 1\n"), nil)
	if !hasWarning(s, "nokey", ":1") || !hasWarning(s, "nosuch.k", ":3") {
		t.Fatalf("%q", s.Warnings)
	}
}

func TestLimitsOnlyLower(t *testing.T) {
	s := Merge(nil, []byte("[limits]\nmax_files = 100\nmax_file_bytes = 1000\n"), nil)
	if s.MaxFiles != 100 || s.MaxFileBytes != 1000 || len(s.Warnings) != 0 {
		t.Fatalf("%+v", s)
	}
	s = Merge(nil, []byte("[limits]\nmax_files = 50000\nmax_file_bytes = 4194304\n"), nil)
	if s.MaxFiles != 0 || s.MaxFileBytes != 0 || !hasWarning(s, "limits.max_files", ":2") || !hasWarning(s, "limits.max_file_bytes", ":3") {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
	s = Merge(nil, []byte("[limits]\nmax_files = 0\nmax_file_bytes = -5\n"), nil)
	if s.MaxFiles != 0 || s.MaxFileBytes != 0 || len(s.Warnings) != 2 {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
	// From init options too, and a rejected later value keeps the earlier one.
	s = Merge([]byte("[limits]\nmax_files = 300\n"), nil, json.RawMessage(`{"limits":{"max_files":99999}}`))
	if s.MaxFiles != 300 || !hasWarning(s, "limits.max_files") {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
}

func TestTOMLErrorsIgnoreFile(t *testing.T) {
	if _, err := ParseTOML([]byte("a = {x=1}")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatal(err)
	}
	user := []byte("[bash]\ntarget = \"4.0-5.3\"\n")
	s := Merge(user, []byte("a = {x=1}\n[bash]\ntarget = \"5.0-5.3\"\n"), nil)
	if s.Target.Min.Major != 4 || !hasWarning(s, ".bash-lsp.toml:1") {
		t.Fatalf("%+v %q", s.Target, s.Warnings)
	}
	s = Merge([]byte("[bash\n"), nil, nil)
	if s.Target != analysis.DefaultTarget || len(s.Warnings) != 1 {
		t.Fatalf("%q", s.Warnings)
	}
}

func TestValueErrorsWarn(t *testing.T) {
	repo := []byte("[bash]\ntarget = \"5.3-3.2\"\ndialect = \"zsh\"\n[shellcheck]\nenabled = \"yes\"\ntimeout_seconds = 0\n[format]\nindent = -1\n[severity]\nWB1001 = \"fatal\"\nWB1002 = \"hint\"\n")
	s := Merge(nil, repo, nil)
	if s.Target != analysis.DefaultTarget || s.Dialect != 0 || !s.ShellCheck.Enabled || s.ShellCheck.Timeout != 10*time.Second || s.Format.Indent != nil {
		t.Fatalf("%+v", s)
	}
	for _, k := range []string{"bash.target", "bash.dialect", "shellcheck.enabled", "shellcheck.timeout_seconds", "format.indent", "severity.WB1001"} {
		if !hasWarning(s, k) {
			t.Errorf("no warning for %s: %q", k, s.Warnings)
		}
	}
	if len(s.Severity) != 1 || s.Severity["WB1002"] != analysis.SevHint {
		t.Fatalf("%v", s.Severity)
	}
}

func TestDialectKeys(t *testing.T) {
	for name, want := range map[string]tree.Dialect{"bash": tree.Bash, "sh": tree.POSIX, "mksh": tree.Mksh, "bats": tree.Bats} {
		s := Merge(nil, []byte("[bash]\ndialect = \""+name+"\"\n"), nil)
		if s.Dialect != want || len(s.Warnings) != 0 {
			t.Errorf("%s: %v %q", name, s.Dialect, s.Warnings)
		}
	}
}

func TestListsAndSections(t *testing.T) {
	repo := []byte("[diagnostics]\ndisable = [\"WB1401\", \"WB1301\"]\n[files]\ninclude = [\"bin/**\"]\nexclude = [\"vendor/\"]\n[sources]\npaths = [\"lib\", \"My Docs/x\"]\n" +
		"[format]\nindent = 2\nbinary_next_line = true\nswitch_case_indent = false\nspace_redirects = true\nfunction_next_line = false\nsimplify = true\n[references]\nresolvedOnly = true\n[severity]\nWB1401 = \"error\"\n")
	s := Merge(nil, repo, nil)
	if !s.Disabled["WB1401"] || !s.Disabled["WB1301"] || len(s.Disabled) != 2 ||
		len(s.Include) != 1 || s.Exclude[0] != "vendor/" || len(s.SourcePaths) != 2 || s.SourcePaths[1] != "My Docs/x" ||
		s.Format.Indent == nil || *s.Format.Indent != 2 || *s.Format.BinaryNextLine != true || *s.Format.SwitchCaseIndent != false ||
		*s.Format.SpaceRedirects != true || *s.Format.FunctionNextLine != false || *s.Format.Simplify != true ||
		!s.ResolvedOnly || s.Severity["WB1401"] != analysis.SevError || len(s.Warnings) != 0 {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
	// Later layers win per format field; severity merges per rule.
	s = Merge([]byte("[format]\nindent = 4\nsimplify = true\n[severity]\nWB1001 = \"info\"\nWB1401 = \"info\"\n"), repo, json.RawMessage(`{"format":{"indent":8},"severity":{"WB1001":"warning"}}`))
	if *s.Format.Indent != 8 || !*s.Format.Simplify || s.Severity["WB1001"] != analysis.SevWarning || s.Severity["WB1401"] != analysis.SevError {
		t.Fatalf("%+v", s)
	}
}

func TestInitOptionsErrors(t *testing.T) {
	s := Merge(nil, nil, json.RawMessage(`{"bash":{"target":5,"nosuch":1},"files":{"include":[1]},"other":3,"extra":{"a":{"b":1}},"limits":{"max_files":1.5}}`))
	for _, k := range []string{"bash.target", "bash.nosuch", "files.include", "other", "extra.a", "limits.max_files"} {
		if !hasWarning(s, k) {
			t.Errorf("no warning for %s: %q", k, s.Warnings)
		}
	}
	s = Merge(nil, nil, json.RawMessage(`"yes"`))
	if len(s.Warnings) != 1 {
		t.Fatalf("%q", s.Warnings)
	}
	s = Merge(nil, nil, json.RawMessage(`null`))
	if len(s.Warnings) != 0 {
		t.Fatalf("%q", s.Warnings)
	}
}

func TestUserPathOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(cfg, []byte("[bash]\ntarget = \"4.0-5.0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_LSP_CONFIG", cfg)
	if p, err := UserPath(); err != nil || p != cfg {
		t.Fatal(p, err)
	}
	root := t.TempDir()
	s := Load(root, nil)
	if s.Target.Min.Major != 4 || len(s.Files) != 1 || s.Files[0] != cfg {
		t.Fatalf("%+v", s)
	}
	t.Setenv("BASH_LSP_CONFIG", "")
	if p, err := UserPath(); err != nil || p != "" {
		t.Fatal(p, err)
	}
	s = Load(root, nil)
	if s.Target != analysis.DefaultTarget || len(s.Files) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestUserPathDefault(t *testing.T) {
	// With the variable unset the path is under the user config dir; nothing is read here.
	old, had := os.LookupEnv("BASH_LSP_CONFIG")
	os.Unsetenv("BASH_LSP_CONFIG")
	defer func() {
		if had {
			os.Setenv("BASH_LSP_CONFIG", old)
		}
	}()
	p, err := UserPath()
	if err != nil {
		t.Skip(err)
	}
	if filepath.Base(p) != "config.toml" || filepath.Base(filepath.Dir(p)) != "bash-lsp" {
		t.Fatal(p)
	}
}

func TestLoadRepoFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".bash-lsp.toml"), []byte("[bash]\ntarget = \"4.2-5.3\"\n[shellcheck]\npath = \"./sc\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Load(root, nil)
	if s.Target.Min.Minor != 2 || s.ShellCheck.Path != "" || len(s.Refused) != 1 || len(s.Files) != 1 {
		t.Fatalf("%+v", s)
	}
	// Over 64 KiB: ignored with a warning.
	big := "# " + strings.Repeat("x", 64<<10) + "\n[bash]\ntarget = \"4.2-5.3\"\n"
	if err := os.WriteFile(filepath.Join(root, ".bash-lsp.toml"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	s = Load(root, nil)
	if s.Target != analysis.DefaultTarget || !hasWarning(s, ".bash-lsp.toml", "64 KiB") || len(s.Files) != 0 {
		t.Fatalf("%+v %q", s, s.Warnings)
	}
	// A symlink that leaves the root is not followed.
	outside := filepath.Join(t.TempDir(), "evil.toml")
	if err := os.WriteFile(outside, []byte("[bash]\ntarget = \"5.0-5.3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".bash-lsp.toml")
	os.Remove(link)
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	s = Load(root, nil)
	if s.Target != analysis.DefaultTarget || len(s.Files) != 0 {
		t.Fatalf("%+v", s)
	}
}
