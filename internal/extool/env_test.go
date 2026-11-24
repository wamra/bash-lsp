package extool

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// The child's PATH keeps only absolute entries outside the workspace roots.
func TestChildEnvFiltersPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)
	outside := t.TempDir()
	sep := string(os.PathListSeparator)
	entries := []string{"", ".", "./x", "../x", "rel/bin", root, sub, outside}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(outside, "alias")
		if err := os.Symlink(sub, alias); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, alias, filepath.Join(sub, "not-yet"))
	}
	t.Setenv("PATH", strings.Join(entries, sep))
	got, _ := envValue(ChildEnv([]string{root}), "PATH")
	if got != outside {
		t.Errorf("PATH = %q, want only %q", got, outside)
	}
	// With no roots only the non-absolute entries go.
	got, _ = envValue(ChildEnv(nil), "PATH")
	if strings.Contains(got, "rel/bin") || strings.HasPrefix(got, sep) || strings.Contains(got, sep+sep) || !strings.Contains(got, root) {
		t.Errorf("PATH with no roots = %q", got)
	}
}

func TestChildEnvAllowlist(t *testing.T) {
	t.Setenv("GHCRTS", "-M1m")
	t.Setenv("SHELLCHECK_OPTS", "-e SC2086")
	t.Setenv("WB_SECRET", "s")
	t.Setenv("HOME", "/home/x")
	t.Setenv("LC_ALL", "C")
	env := ChildEnv(nil, "EXTRA=1")
	for _, n := range []string{"GHCRTS", "SHELLCHECK_OPTS", "WB_SECRET"} {
		if _, ok := envValue(env, n); ok {
			t.Errorf("%s was passed", n)
		}
	}
	for _, kv := range []string{"HOME=/home/x", "LC_ALL=C", "EXTRA=1"} {
		if !slices.Contains(env, kv) {
			t.Errorf("%s missing from %q", kv, env)
		}
	}
}

func TestNeutralDir(t *testing.T) {
	d := NeutralDir()
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		t.Fatalf("NeutralDir %q is not a directory (%v)", d, err)
	}
	if insideRoot(d, t.TempDir()) {
		t.Errorf("NeutralDir %q is inside a temp directory", d)
	}
	if runtime.GOOS != "windows" && d != "/" {
		t.Errorf("NeutralDir = %q, want /", d)
	}
}
