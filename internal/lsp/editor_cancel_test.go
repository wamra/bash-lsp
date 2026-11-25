package lsp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/extool"
)

// editorMethods is every request the editor features added: the methods
// `handle` switches on for completion, signature help, rename, structure,
// highlights, links, inlay hints, semantic tokens and the deferred hover.
var editorMethods = []string{
	"textDocument/completion",
	"completionItem/resolve",
	"textDocument/signatureHelp",
	"textDocument/semanticTokens/full",
	"textDocument/semanticTokens/range",
	"textDocument/semanticTokens/full/delta",
	"textDocument/foldingRange",
	"textDocument/selectionRange",
	"textDocument/documentHighlight",
	"textDocument/documentLink",
	"textDocument/inlayHint",
	"textDocument/prepareRename",
	"textDocument/rename",
	"textDocument/hover",
}

const editorSrc = "#!/bin/bash\nf() { :; }\nprintf '%s\\n' hi\nf a b\n"

// editorParams are well-formed params for each editor method over a.sh.
func editorParams(c *testClient, method string) any {
	doc := map[string]any{"uri": c.uri("a.sh")}
	at := func(line, ch int) map[string]any {
		return map[string]any{"textDocument": doc, "position": Position{line, ch}}
	}
	whole := map[string]any{"start": Position{0, 0}, "end": Position{4, 0}}
	switch method {
	case "completionItem/resolve":
		return map[string]any{"label": "printf", "data": map[string]any{"uri": c.uri("a.sh"), "label": "printf", "kind": "builtin"}}
	case "textDocument/semanticTokens/full", "textDocument/foldingRange", "textDocument/documentLink":
		return map[string]any{"textDocument": doc}
	case "textDocument/semanticTokens/range", "textDocument/inlayHint":
		return map[string]any{"textDocument": doc, "range": whole}
	case "textDocument/semanticTokens/full/delta":
		return map[string]any{"textDocument": doc, "previousResultId": "none"}
	case "textDocument/selectionRange":
		return map[string]any{"textDocument": doc, "positions": []Position{{3, 1}}}
	case "textDocument/rename":
		p := at(1, 1)
		p["newName"] = "g"
		return p
	case "textDocument/hover":
		return at(2, 2)
	case "textDocument/signatureHelp":
		return at(3, 4)
	}
	return at(1, 1)
}

// helpClient is a client over one file with a fake help runner.
func helpClient(t *testing.T, run func(context.Context, extool.Tool, []string, extool.RunOptions) (extool.Result, error)) *testClient {
	t.Helper()
	root := t.TempDir()
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	openFile(t, c, root, "a.sh", editorSrc)
	c.symbols("") // the first build is done
	return c
}

// A request of each editor method that is queued behind a blocked request and
// cancelled is answered -32800 and its handler never runs.
func TestEveryNewRequestCancellable(t *testing.T) {
	var calls atomic.Int32
	for _, method := range editorMethods {
		t.Run(method, func(t *testing.T) {
			var ran atomic.Int64
			g := newGate()
			var srv *server
			setHooks(t, nil, nil, func(m string) {
				if m == method {
					ran.Add(1)
				}
				if m == "workspace/symbol" {
					g.wait()
				}
			})
			root := t.TempDir()
			c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) {
				srv = s
				s.helpRun = countingRunner(&calls)
			}})
			openFile(t, c, root, "a.sh", editorSrc)
			c.docSymbols("a.sh") // the first build is done and the document is open
			c.sendReq(`900`, "workspace/symbol", map[string]any{"query": ""})
			g.waitEntered(t) // the loop is inside the first request
			c.sendReq(`901`, method, editorParams(c, method))
			c.cancel(`901`)
			deadline := time.Now().Add(5 * time.Second)
			for !srv.isCancelled("901") {
				if time.Now().After(deadline) {
					t.Fatal("the cancel was not read")
				}
				time.Sleep(time.Millisecond)
			}
			g.release()
			if r := c.waitReply(`900`); r.Error != nil {
				t.Fatalf("blocked request: %+v", r.Error)
			}
			r := c.waitReply(`901`)
			if r.Error == nil || r.Error.Code != codeRequestCancelled {
				t.Fatalf("reply %+v %s, want -32800", r.Error, r.Result)
			}
			if n := ran.Load(); n != 0 {
				t.Fatalf("the handler ran %d times", n)
			}
			if calls.Load() != 0 {
				t.Fatalf("%d help runs for a cancelled request", calls.Load())
			}
		})
	}
}

// Hover and completion resolve are answered later; one cancelled while its
// lookup runs is answered -32800 and the lookup's context is done.
func TestDeferredLookupsCancelledMidLookup(t *testing.T) {
	for _, method := range []string{"textDocument/hover", "completionItem/resolve"} {
		t.Run(method, func(t *testing.T) {
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
			c := helpClient(t, run)
			c.sendReq(`910`, method, editorParams(c, method))
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the lookup never started")
			}
			c.cancel(`910`)
			r := c.waitReply(`910`)
			if r.Error == nil || r.Error.Code != codeRequestCancelled {
				t.Fatalf("reply %+v %s, want -32800", r.Error, r.Result)
			}
			select {
			case <-sawDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the lookup's context was not cancelled")
			}
		})
	}
}

// While a folder job is pending every editor request waits; it runs, in
// order, once the job is applied, and a request cancelled meanwhile is
// answered at once without running.
func TestNewRequestsHeldDuringFolderJob(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	ran := map[string]int{}
	g := newGate()
	setHooks(t, nil, g, func(m string) {
		mu.Lock()
		ran[m]++
		mu.Unlock()
	})
	root := t.TempDir()
	second := folderRoot(t, "b.sh", "newfolder_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = countingRunner(&calls) }})
	openFile(t, c, root, "a.sh", editorSrc)
	c.symbols("")
	c.folders([]string{second}, nil)
	g.waitEntered(t) // the job is running and blocked
	ids := map[string]string{}
	for i, m := range editorMethods {
		id := string(rune('A'+i)) + "1"
		ids[m] = `"` + id + `"`
		c.sendReq(ids[m], m, editorParams(c, m))
	}
	// A request cancelled while held is answered without waiting for the job.
	cancelled := editorMethods[0]
	c.cancel(ids[cancelled])
	if r := c.waitReply(ids[cancelled]); r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("cancelled held %s: %+v", cancelled, r.Error)
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	for _, m := range editorMethods {
		if ran[m] != 0 {
			t.Errorf("%s ran %d times during the folder job", m, ran[m])
		}
	}
	mu.Unlock()
	for _, m := range editorMethods[1:] {
		if c.hasReply(ids[m]) {
			t.Errorf("%s was answered during the folder job", m)
		}
	}
	g.release()
	for _, m := range editorMethods[1:] {
		if r := c.waitReply(ids[m]); r.Error != nil {
			t.Errorf("%s after the job: %+v", m, r.Error)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range editorMethods[1:] {
		if ran[m] != 1 {
			t.Errorf("%s ran %d times after the job, want 1", m, ran[m])
		}
	}
	if ran[cancelled] != 0 {
		t.Errorf("the cancelled %s ran", cancelled)
	}
}

// Two help lookups that take 2 s (a hover and a completion resolve) do not
// hold the loop: an edit and a documentSymbol are answered meanwhile, with
// both lookups still running.
func TestNoNewRequestBlocksLoop(t *testing.T) {
	var running atomic.Int32
	started := make(chan struct{}, 4)
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		running.Add(1)
		defer running.Add(-1)
		started <- struct{}{}
		select {
		case <-time.After(2 * time.Second):
			return extool.Result{Stdout: []byte(helpSample)}, nil
		case <-ctx.Done():
			return extool.Result{Exit: -1}, ctx.Err()
		}
	}
	c := helpClient(t, run)
	c.sendReq(`920`, "textDocument/hover", editorParams(c, "textDocument/hover"))
	c.sendReq(`921`, "completionItem/resolve", editorParams(c, "completionItem/resolve"))
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("a lookup never started")
		}
	}
	before := c.diagCount("a.sh")
	c.change("a.sh", fullText(editorSrc+"g() { :; }\n"))
	syms := c.docSymbols("a.sh")
	found := false
	for _, s := range syms {
		found = found || s.Name == "g"
	}
	if !found {
		t.Fatalf("documentSymbol after the edit lacks g: %+v", syms)
	}
	if !c.waitDiagCount("a.sh", before+1, 5*time.Second) {
		t.Fatal("the edit was not published while the lookups ran")
	}
	if running.Load() != 2 || c.hasReply(`920`) || c.hasReply(`921`) {
		t.Fatalf("the lookups ended before the loop answered (running %d)", running.Load())
	}
	c.cancel(`920`)
	c.cancel(`921`)
	for _, id := range []string{`920`, `921`} {
		c.waitReply(id) // -32800, or -32801 for the hover: the document changed
	}
}
