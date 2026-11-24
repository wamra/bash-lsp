package lsp

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/workspace"
)

func TestIsGitignoreFolds(t *testing.T) {
	for _, c := range []struct {
		path string
		fold bool
		want bool
	}{
		{"/r/.gitignore", false, true},
		{"/r/.gitignore", true, true},
		{"/r/.GitIgnore", false, false},
		{"/r/.GITIGNORE", true, true},
		{"/r/sub/.GitIgnore", true, true},
		{"/r/gitignore", true, false},
	} {
		if got := isGitignore(c.path, c.fold); got != c.want {
			t.Errorf("isGitignore(%q, %v) = %v, want %v", c.path, c.fold, got, c.want)
		}
	}
}

// A change event for a case variant of .gitignore re-walks the folder exactly
// when the folder's volume folds case.
func TestGitignoreVariantEventFollowsVolume(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "echo a\n")
	var jobs atomic.Int32
	hookMu.Lock()
	folderJobHook = func() { jobs.Add(1) }
	hookMu.Unlock()
	t.Cleanup(func() { hookMu.Lock(); folderJobHook = nil; hookMu.Unlock() })

	var srv *server
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { srv = s }})
	c.open("a.sh") // the first build is done after a request
	c.symbols("")
	var fold bool
	done := make(chan struct{})
	srv.post(func() { fold = srv.folders[0].ws.CaseInsensitive(); close(done) })
	<-done
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{
		map[string]any{"uri": workspace.URIFromPath(filepath.Join(root, ".GITIGNORE")), "type": 2}}})
	c.symbols("") // a request waits for the job, so the count is final
	want := int32(0)
	if fold {
		want = 1
	}
	if got := jobs.Load(); got != want {
		t.Fatalf("folding volume %v: %d folder jobs after a .GITIGNORE event, want %d", fold, got, want)
	}
}

func TestResolvedOnlyCanonicalKey(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "u.sh"), "f() { :; }\nf\nunset -f f\n")
	for name, opts := range map[string]any{
		"canonical": map[string]any{"references": map[string]any{"resolved_only": true}},
		"alias":     map[string]any{"references": map[string]any{"resolvedOnly": true}},
	} {
		c := clientInit(t, root, false, opts)
		if got := c.references("u.sh", "f() {", 0); !eq(got, "u.sh:1") {
			t.Errorf("%s: references %v, want the declaration only", name, got)
		}
	}
	if got := client(t, root).references("u.sh", "f() {", 0); !eq(got, "u.sh:1", "u.sh:2") {
		t.Errorf("default: references %v", got)
	}
	// The key also comes from the repo file, per folder.
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[references]\nresolved_only = true\n")
	if got := client(t, root).references("u.sh", "f() {", 0); !eq(got, "u.sh:1") {
		t.Errorf(".bash-lsp.toml: references %v, want the declaration only", got)
	}
}

var _ = os.Getenv
var _ = time.Second
