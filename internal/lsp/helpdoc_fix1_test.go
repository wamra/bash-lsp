package lsp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wamra/bash-lsp/internal/extool"
)

func TestHoverSynopsisCannotCloseTheFence(t *testing.T) {
	root := t.TempDir()
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		return extool.Result{Stdout: []byte("printf: printf ``` [-v var] format\n")}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi\n")
	_, h := c.helpHoverAt("a.sh", Position{1, 2})
	if n := strings.Count(h.Contents.Value, "```"); n != 2 {
		t.Fatalf("%d fences in %q, want the opening and the closing one", n, h.Contents.Value)
	}
}

func TestHelpSettingsChangeDropsTheLookup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helpers are not used on Windows")
	}
	root := t.TempDir()
	dir := t.TempDir()
	mk := func(n string) string {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	man, bash1, bash2 := mk("man"), mk("bash1"), mk("bash2")
	cfg := userConfig(t, "[help]\nbash_path = \""+bash1+"\"\nman_path = \""+man+"\"\n")
	var calls atomic.Int32
	var last atomic.Value
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		calls.Add(1)
		last.Store(filepath.Base(tl.Path))
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi\n")
	p := Position{1, 2}
	c.helpHoverAt("a.sh", p)
	c.helpHoverAt("a.sh", p) // cached
	if calls.Load() != 1 || last.Load() != "bash1" {
		t.Fatalf("before the change: %d runs, last %v", calls.Load(), last.Load())
	}
	writeUserConfig(t, cfg, "[help]\nbash_path = \""+bash2+"\"\nman_path = \""+man+"\"\n")
	c.notify("workspace/didChangeConfiguration", map[string]any{})
	c.docSymbols("a.sh") // after the reload: requests are handled in order
	_, h := c.helpHoverAt("a.sh", p)
	if calls.Load() != 2 || last.Load() != "bash2" || !strings.Contains(h.Contents.Value, "this machine") {
		t.Fatalf("after the change: %d runs, last %v, %q", calls.Load(), last.Load(), h.Contents.Value)
	}
}
