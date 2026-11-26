package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// profileDir holds executables named names; the path.profile of a test.
func profileDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n+exeExt()), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A command in no function, alias, builtin or PATH-profile entry is
// published as WB1102 (information) on its name; one in the profile is not.
func TestMissingCommandPublished(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "m.sh"), "ls\ndeplyo app\n")
	c := clientWith(t, clientCfg{root: root, initOpts: map[string]any{"path": map[string]any{"profile": []string{profileDir(t, "ls")}}}})
	c.open("m.sh")
	d := c.waitDiag("m.sh")
	if len(d) != 1 || d[0].Code != "WB1102" || d[0].Severity != 3 || d[0].Range != (Range{Position{1, 0}, Position{1, 6}}) {
		t.Fatalf("%+v", d)
	}
	if !strings.Contains(d[0].Message, "deplyo") {
		t.Errorf("message %q", d[0].Message)
	}
}

// files.exclude, sources.paths and bash.dialect of the repo file reach the
// folder's workspace.
func TestRepoFileKeysApplied(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r")
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[files]\nexclude = [\"vendor/\"]\n[sources]\npaths = [\"lib\", \"../x\"]\n")
	put(t, filepath.Join(root, "lib", "util.sh"), "util() { :; }\n")
	put(t, filepath.Join(root, "vendor", "v.sh"), "vend() { :; }\n")
	put(t, filepath.Join(root, "bin", "run.sh"), "source util.sh\nutil\n")
	c := clientWith(t, clientCfg{folders: []string{root}})
	c.root = root
	if got := c.symbols("vend"); len(got) != 0 {
		t.Errorf("an excluded file is indexed: %v", got)
	}
	if got := c.definition("bin/run.sh", "util", 1); len(got) != 1 || got[0] != "lib/util.sh:1" {
		t.Errorf("definition through sources.paths: %v", got)
	}
	if !strings.Contains(c.logs.String(), `sources.paths "../x"`) {
		t.Errorf("the dropped entry is not logged:\n%s", c.logs.String())
	}
}

// g's repo file excludes vendor/: the outer walk does not reach it, so a root
// named on vendor keeps a backend of its own, as for a git-ignored one.
func TestExcludedNestedRootHasOwnBackend(t *testing.T) {
	g := filepath.Join(t.TempDir(), "g")
	put(t, filepath.Join(g, ".bash-lsp.toml"), "[files]\nexclude = [\"vendor/\"]\n")
	put(t, filepath.Join(g, "m.sh"), "echo outer\n")
	put(t, filepath.Join(g, "vendor", "v.sh"), "vf() { :; }\n")
	put(t, filepath.Join(g, "vendor", "w.sh"), "source ./v.sh\nvf\n")
	put(t, filepath.Join(g, "vendor", "bad.sh"), "if then\n")
	nested := filepath.Join(g, "vendor")
	answers := func(folders []string) string {
		c := clientWith(t, clientCfg{folders: folders})
		c.root = g
		return c.vendorAnswers("vendor")
	}
	want := answers([]string{nested})
	for _, must := range []string{"closed: symbols [vendor/v.sh:1 vf]", "open: definition [vendor/v.sh:1]", "diagnostics vendor/bad.sh: 1 pushes"} {
		if !strings.Contains(want, must) {
			t.Fatalf("the nested root alone lacks %q:\n%s", must, want)
		}
	}
	for name, folders := range map[string][]string{"outer then nested": {g, nested}, "nested then outer": {nested, g}} {
		if got := answers(folders); got != want {
			t.Errorf("%s:\n%s\nwant (nested root alone):\n%s", name, got, want)
		}
	}
}
