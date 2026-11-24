package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/extool"
)

// helpTools writes a bash and a man program outside any workspace and returns
// the init options that name them.
func helpInitOpts(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helpers are not used on Windows")
	}
	dir := t.TempDir()
	h := map[string]any{}
	for _, n := range []string{"bash", "man"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		h[n+"_path"] = p
	}
	for k, v := range extra {
		h[k] = v
	}
	return map[string]any{"help": h}
}

const helpSample = "printf: printf [-v var] format [arguments]\n    Invented text for the test.\n"

type hoverReply struct {
	Contents struct{ Kind, Value string }
	Range    Range
}

func (c *testClient) helpHoverAt(rel string, p Position) (*rpcReply, hoverReply) {
	c.t.Helper()
	r := c.rawCall("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p})
	var h hoverReply
	if r.Error == nil && string(r.Result) != "null" {
		if err := json.Unmarshal(r.Result, &h); err != nil {
			c.t.Fatalf("%v: %s", err, r.Result)
		}
	}
	return r, h
}

func TestHoverBuiltinSynopsisDeferred(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\necho \U0001F600; printf '%s\\n' hi\nf() { :; }\n")
	p := Position{1, 11} // two characters into printf; the emoji before it is two UTF-16 units

	c.sendReq("700", "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}, "position": p})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup never started")
	}
	// The loop is free while the lookup waits.
	if syms := c.docSymbols("a.sh"); len(syms) == 0 {
		t.Fatal("documentSymbol answered empty")
	}
	if c.hasReply("700") {
		t.Fatal("the hover was answered before the lookup finished")
	}
	close(release)
	r := c.waitReply("700")
	if r.Error != nil {
		t.Fatalf("hover error %+v", r.Error)
	}
	var h hoverReply
	if err := json.Unmarshal(r.Result, &h); err != nil {
		t.Fatalf("%v: %s", err, r.Result)
	}
	want := "```bash\nprintf [-v var] format [arguments]\n```\n(from help on this machine)"
	if h.Contents.Kind != "markdown" || !strings.Contains(h.Contents.Value, want) {
		t.Fatalf("hover %q lacks %q", h.Contents.Value, want)
	}
	// The range is the command word, in UTF-16 units: the emoji before it is two.
	if h.Range.Start.Line != 1 || h.Range.Start.Character != 9 || h.Range.End.Character != 15 {
		t.Fatalf("range %+v", h.Range)
	}

	// Cached now: answered at once, no second run.
	r2, h2 := c.helpHoverAt("a.sh", p)
	if r2.Error != nil || !strings.Contains(h2.Contents.Value, want) || calls.Load() != 1 {
		t.Fatalf("second hover: %+v %q, %d runs", r2.Error, h2.Contents.Value, calls.Load())
	}
}

func TestHoverCancelKillsLookup(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	started := make(chan struct{}, 1)
	sawDone := make(chan struct{}, 1)
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			sawDone <- struct{}{}
			return extool.Result{Exit: -1}, ctx.Err()
		case <-time.After(10 * time.Second):
			return extool.Result{Stdout: []byte(helpSample)}, nil
		}
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi\n")
	p := c.pos("a.sh", "printf", 1)
	c.sendReq("710", "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}, "position": p})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup never started")
	}
	c.cancel("710")
	r := c.waitReply("710")
	if r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("reply %+v %s, want -32800", r.Error, r.Result)
	}
	select {
	case <-sawDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner's context was not cancelled")
	}
}

func TestHoverDocumentChangedDuringLookup(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		started <- struct{}{}
		<-release
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi\n")
	p := c.pos("a.sh", "printf", 1)
	c.sendReq("720", "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}, "position": p})
	<-started
	c.change("a.sh", fullText("#!/bin/bash\n\nprintf hi\n"))
	c.docSymbols("a.sh") // answered after the change was applied: requests are handled in order
	close(release)
	r := c.waitReply("720")
	if r.Error == nil || r.Error.Code != codeContentModified {
		t.Fatalf("reply %+v %s, want -32801", r.Error, r.Result)
	}
}

func TestHoverNotACommandIsSynchronous(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	var calls atomic.Int32
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		calls.Add(1)
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi $(date)\nf() { :; }\nf\n")
	for _, tc := range []struct {
		text string
		col  int
	}{{"hi", 1}, {"$(date)", 0}, {"f() ", 0}, {"#!/bin", 2}} {
		r, h := c.helpHoverAt("a.sh", c.pos("a.sh", tc.text, tc.col))
		if r.Error != nil {
			t.Fatalf("%q: %+v", tc.text, r.Error)
		}
		if strings.Contains(h.Contents.Value, "this machine") {
			t.Errorf("%q: hover carries a synopsis: %q", tc.text, h.Contents.Value)
		}
	}
	// The function name keeps its existing hover.
	if _, h := c.helpHoverAt("a.sh", Position{3, 0}); !strings.Contains(h.Contents.Value, "f") || strings.Contains(h.Contents.Value, "this machine") {
		t.Errorf("function hover %q", h.Contents.Value)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d runs for words that are not commands", n)
	}
}

func TestHoverHelpDisabledNeverSpawns(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	var calls atomic.Int32
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		calls.Add(1)
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, map[string]any{"enabled": false}), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi\n")
	r, _ := c.helpHoverAt("a.sh", c.pos("a.sh", "printf", 1))
	if r.Error != nil || string(r.Result) != "null" || calls.Load() != 0 {
		t.Fatalf("reply %s %+v, %d runs", r.Result, r.Error, calls.Load())
	}
}

func TestHoverExternalCommandUsesMan(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	var mu sync.Mutex
	var argv [][]string
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		mu.Lock()
		argv = append(argv, args)
		mu.Unlock()
		return extool.Result{Stdout: []byte("NAME\n     ls – invented\n\nSYNOPSIS\n     ls [-a] [file ...]\n\nDESCRIPTION\n     x\n")}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nls -l\n")
	r, h := c.helpHoverAt("a.sh", c.pos("a.sh", "ls", 1))
	if r.Error != nil || !strings.Contains(h.Contents.Value, "```bash\nls [-a] [file ...]\n```\n(from man on this machine)") {
		t.Fatalf("reply %s %+v", r.Result, r.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(argv) != 1 || len(argv[0]) != 2 || argv[0][0] != "--" || argv[0][1] != "ls" {
		t.Fatalf("argv %q", argv)
	}
}

func TestHoverSynopsisIsRedacted(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		return extool.Result{Stdout: []byte("printf: printf [-v var] password=hunter2 format\n")}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", "#!/bin/bash\nprintf hi\n")
	_, h := c.helpHoverAt("a.sh", c.pos("a.sh", "printf", 1))
	if strings.Contains(h.Contents.Value, "hunter2") || !strings.Contains(h.Contents.Value, "[redacted]") {
		t.Fatalf("hover %q", h.Contents.Value)
	}
}

// openFile writes the document to disk (for c.pos) and opens it.
func openFile(t *testing.T, c *testClient, root, rel, text string) {
	t.Helper()
	put(t, filepath.Join(root, rel), text)
	c.open(rel)
}
