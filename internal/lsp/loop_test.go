package lsp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers ----

// gate blocks a test hook until released. entered is signalled (without
// blocking) each time a hook call starts to wait.
type gate struct {
	entered chan struct{}
	rel     chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{entered: make(chan struct{}, 64), rel: make(chan struct{})} }

func (g *gate) wait() {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.rel
}

func (g *gate) release() { g.once.Do(func() { close(g.rel) }) }

func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook was not reached")
	}
}

// setHooks installs the test seams for the rest of the test and releases
// every gate when it ends.
func setHooks(t *testing.T, open, job *gate, req func(method string)) {
	t.Helper()
	hookMu.Lock()
	openFoldersHook, folderJobHook, requestHook = nil, nil, req
	if open != nil {
		openFoldersHook = open.wait
	}
	if job != nil {
		folderJobHook = job.wait
	}
	hookMu.Unlock()
	t.Cleanup(func() {
		if open != nil {
			open.release()
		}
		if job != nil {
			job.release()
		}
		hookMu.Lock()
		openFoldersHook, folderJobHook, requestHook = nil, nil, nil
		hookMu.Unlock()
	})
}

// sendReq sends a request whose id is the JSON text idJSON.
func (c *testClient) sendReq(idJSON, method string, params any) {
	c.t.Helper()
	p, _ := json.Marshal(params)
	c.sendRaw([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, idJSON, method, p)))
}

func (c *testClient) cancel(idJSON string) {
	c.t.Helper()
	c.sendRaw([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":%s}}`, idJSON)))
}

// hasReply reports whether a reply to idJSON has arrived (and leaves it).
func (c *testClient) hasReply(idJSON string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replies[idJSON] != nil
}

func (c *testClient) diagCount(rel string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.diags[c.uri(rel)])
}

func (c *testClient) waitDiagCount(rel string, n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c.diagCount(rel) >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return c.diagCount(rel) >= n
}

func docSymbolParams(c *testClient, rel string) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}}
}

func oneFileRoot(t *testing.T, name, text string) string {
	root := t.TempDir()
	put(t, filepath.Join(root, name), text)
	return root
}

// countingHook counts the calls of the given method.
func countingHook(method string, n *atomic.Int64) func(string) {
	return func(m string) {
		if m == method {
			n.Add(1)
		}
	}
}

const docSymbolMethod = "textDocument/documentSymbol"

// ---- cancellation ----

// A request cancelled while it waits for the first build is answered
// -32800 without its handler running; a cancel spelled differently from the id
// (7 and "7" are different ids) cancels nothing.
func TestCancelQueuedRequestIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name, id, cancelID string
		cancelled          bool
	}{
		{"integer id", `7`, `7`, true},
		{"string id", `"7"`, `"7"`, true},
		{"string cancel of integer id", `7`, `"7"`, false},
		{"integer cancel of string id", `"7"`, `7`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int64
			g := newGate()
			setHooks(t, g, nil, countingHook(docSymbolMethod, &n))
			root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
			c := clientWith(t, clientCfg{root: root})
			g.waitEntered(t)
			// Sentinel: a request the loop is stuck in. Its own cancel is
			// read after the cancel under test (the reader is sequential),
			// so when it is answered the cancel under test has been seen.
			c.sendReq(`1000`, "workspace/symbol", map[string]any{"query": ""})
			c.sendReq(tc.id, docSymbolMethod, docSymbolParams(c, "a.sh"))
			c.cancel(tc.cancelID)
			c.cancel(`1000`)
			if r := c.waitReply(`1000`); r.Error == nil || r.Error.Code != codeRequestCancelled {
				t.Fatalf("sentinel: %+v", r)
			}
			g.release()
			r := c.waitReply(tc.id)
			if tc.cancelled {
				if r.Error == nil || r.Error.Code != -32800 {
					t.Fatalf("reply %+v, want error -32800", r)
				}
				if n.Load() != 0 {
					t.Fatalf("the handler ran %d times", n.Load())
				}
				return
			}
			if r.Error != nil {
				t.Fatalf("reply %+v, want a result", r)
			}
			if n.Load() != 1 {
				t.Fatalf("the handler ran %d times, want 1", n.Load())
			}
		})
	}
}

// A request already past the queue and inside its handler when the cancel
// arrives completes, and its result is replaced by the cancel error.
func TestCancelRunningRequestResultReplaced(t *testing.T) {
	g := newGate()
	var srv *server
	setHooks(t, nil, nil, func(m string) {
		if m == docSymbolMethod {
			g.wait()
		}
	})
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { srv = s }})
	c.symbols("") // the first build is done
	c.sendReq(`5`, docSymbolMethod, docSymbolParams(c, "a.sh"))
	g.waitEntered(t) // the handler counter is read first: it is running
	c.cancel(`5`)
	deadline := time.Now().Add(5 * time.Second)
	for !srv.isCancelled("5") {
		if time.Now().After(deadline) {
			t.Fatal("the cancel was not seen")
		}
		time.Sleep(time.Millisecond)
	}
	g.release()
	if r := c.waitReply(`5`); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("reply %+v, want error -32800", r)
	}
	// The id is free again: a new request with the same id is answered.
	c.sendReq(`5`, docSymbolMethod, docSymbolParams(c, "a.sh"))
	if r := c.waitReply(`5`); r.Error != nil {
		t.Fatalf("reply after reuse %+v", r)
	}
}

// A flood of cancels for ids never sent grows nothing.
func TestCancelUnknownIdIgnored(t *testing.T) {
	c := client(t, oneFileRoot(t, "a.sh", "a_fn() { :; }\n"))
	c.symbols("")
	body := []byte(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":123456}}`)
	frame := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	chunk := []byte(strings.Repeat(string(frame), 1000))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 100; i++ {
		if _, err := c.in.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.symbols(""); len(got) == 0 {
		t.Fatal("no symbols after the flood")
	}
	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc
	t.Logf("TotalAlloc grew by %d bytes over 100,000 cancels", grew)
	if grew > 8<<20 {
		t.Fatalf("TotalAlloc grew by %d bytes, want under 8 MiB", grew)
	}
}

// ---- the bounded queue ----

func TestInboundQueueBounded(t *testing.T) {
	g := newGate()
	setHooks(t, g, nil, nil)
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root})
	g.waitEntered(t)
	c.openText("a.sh", "a_fn() { :; }\n")

	const total = 10000
	frame := func(i int) []byte {
		text := fmt.Sprintf("f%d() { :; }\n", i)
		text += strings.Repeat("# padding padding padding padding padding padding padding\n", (4096-len(text))/58)
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didChange", "params": map[string]any{
			"textDocument": map[string]any{"uri": c.uri("a.sh"), "version": i + 2}, "contentChanges": []any{fullText(text)}}})
		return append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(b))), b...)
	}
	size := len(frame(0))
	var accepted atomic.Int64
	written := make(chan struct{})
	go func() {
		defer close(written)
		for i := 0; i < total; i++ {
			n, err := c.in.Write(frame(i))
			accepted.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	limit := int64((inboundCap+2)*size + 64<<10)
	got := accepted.Load()
	t.Logf("accepted %d bytes (%d frames of %d bytes), limit %d", got, got/int64(size), size, limit)
	if got > limit {
		t.Fatalf("the pipe accepted %d bytes while the loop was held, limit %d", got, limit)
	}
	g.release()
	select {
	case <-written:
	case <-time.After(60 * time.Second):
		t.Fatal("the writer did not finish")
	}
	names := c.symbolNames("a.sh")
	if len(names) != 1 || names[0] != fmt.Sprintf("f%d", total-1) {
		t.Fatalf("symbols after the burst: %v, want f%d only", names, total-1)
	}
	if n := c.diagCount("a.sh"); n != total+1 {
		t.Fatalf("%d diagnostics pushes, want %d", n, total+1)
	}
}

// ---- exit ----

func TestExitDuringFirstBuild(t *testing.T) {
	g := newGate()
	setHooks(t, g, nil, nil)
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root})
	g.waitEntered(t)
	c.sendReq(`3`, docSymbolMethod, docSymbolParams(c, "a.sh"))
	c.notify("exit", nil)
	select {
	case err := <-c.done:
		if err == nil || !strings.Contains(err.Error(), "exit before shutdown") {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return while the first build was held")
	}
}

// ---- folder changes ----

func folderRoot(t *testing.T, file, text string) string {
	return oneFileRoot(t, file, text)
}

func TestFolderAddDoesNotBlockEdits(t *testing.T) {
	g := newGate()
	setHooks(t, nil, g, nil)
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	second := folderRoot(t, "b.sh", "newfolder_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root})
	c.open("a.sh")
	c.symbols("") // the first build is done
	if !c.waitDiagCount("a.sh", 1, 5*time.Second) {
		t.Fatal("no diagnostics for the opened document")
	}
	before := c.diagCount("a.sh")
	c.folders([]string{second}, nil)
	g.waitEntered(t) // the job is running and blocked
	c.change("a.sh", fullText("a2_fn() { :; }\n"))
	if !c.waitDiagCount("a.sh", before+1, 2*time.Second) {
		t.Fatal("the edit was not answered while the folder job was blocked")
	}
	g.release()
	got := c.symbols("newfolder_fn")
	if len(got) != 1 || !strings.HasSuffix(got[0], " newfolder_fn") {
		t.Fatalf("workspace/symbol after the folder job: %v", got)
	}
}

func TestRequestAfterFolderChangeWaits(t *testing.T) {
	g := newGate()
	setHooks(t, nil, g, nil)
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	second := folderRoot(t, "b.sh", "newfolder_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root})
	c.symbols("")
	c.folders([]string{second}, nil)
	g.waitEntered(t)
	q := map[string]any{"query": "newfolder"}
	c.sendReq(`42`, "workspace/symbol", q)
	time.Sleep(200 * time.Millisecond)
	if c.hasReply(`42`) {
		t.Fatal("the request was answered while the folder job was blocked")
	}
	start := time.Now()
	c.cancel(`42`)
	r := c.waitReply(`42`)
	if r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("reply %+v, want error -32800", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the cancelled held request took %v", d)
	}
	c.sendReq(`43`, "workspace/symbol", q)
	g.release()
	r = c.waitReply(`43`)
	var out []SymbolInformation
	if r.Error != nil || json.Unmarshal(r.Result, &out) != nil || len(out) != 1 || out[0].Name != "newfolder_fn" {
		t.Fatalf("reply %+v %+v", r, out)
	}
}

func TestFolderJobsRunInOrder(t *testing.T) {
	g := newGate()
	setHooks(t, nil, g, nil)
	root := oneFileRoot(t, "a.sh", "base_fn() { :; }\n")
	a := folderRoot(t, "a.sh", "a_only_fn() { :; }\n")
	b := folderRoot(t, "b.sh", "b_only_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root})
	c.symbols("")
	c.folders([]string{a}, nil)
	g.waitEntered(t)
	c.folders(nil, []string{a})
	c.folders([]string{b}, nil)
	g.release()
	got := c.symbols("_only_fn")
	if len(got) != 1 || !strings.HasSuffix(got[0], " b_only_fn") {
		t.Fatalf("workspace/symbol: %v, want b_only_fn only", got)
	}
}

// ---- post ----

// Closures posted from another goroutine run on the message loop: under
// -race, writing the document table from them while the client keeps editing
// is clean.
func TestPostRunsOnLoop(t *testing.T) {
	var srv *server
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { srv = s }})
	c.open("a.sh")
	c.symbols("")
	done := make(chan struct{})
	var ran atomic.Int64
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			srv.post(func() {
				srv.docs["posted"] = &document{}
				delete(srv.docs, "posted")
				ran.Add(1)
			})
		}
	}()
	for i := 0; i < 200; i++ {
		c.change("a.sh", fullText(fmt.Sprintf("a%d_fn() { :; }\n", i)))
	}
	<-done
	c.symbols("") // everything before it has been handled
	if ran.Load() != 200 {
		t.Fatalf("%d closures ran, want 200", ran.Load())
	}
}

// ---- the cancel fast path ----

// cancelFrameID agrees with a full decode on every shape it accepts, and
// declines the ones it cannot judge.
func TestCancelFrameID(t *testing.T) {
	for _, tc := range []struct {
		body string
		id   string
		ok   bool
	}{
		{`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":7}}`, `7`, true},
		{`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":"7"}}`, `"7"`, true},
		{` { "params" : { "x":[1,{"id":9}], "id" : "a b" } , "method" : "$/cancelRequest" } `, `"a b"`, true},
		{`{"Method":"$/cancelRequest","Params":{"ID":3}}`, `3`, true},
		{`{"method":"$/cancelRequest","params":{"id":{"a":[1,2]}}}`, `{"a":[1,2]}`, true},
		{`{"method":"$/cancelRequest"}`, ``, true},
		{`{"method":"$/cancelRequest","params":[1]}`, ``, true},
		{`{"method":"$/cancelRequest","params":{"id":1},"method":"x"}`, ``, false},
		{`{"method":"x","params":{"s":"$/cancelRequest"}}`, ``, false},
		{`{"method":"$\/cancelRequest","params":{"id":1}}`, ``, false},
		{"{\"meth\\u006fd\":\"$/cancelRequest\",\"params\":{\"id\":1}}", ``, false},
		{`{"method":5,"x":"$/cancelRequest"}`, ``, false},
		{`{"method":"$/cancelRequest","params":{"id":1}`, ``, false},
		{`["$/cancelRequest"]`, ``, false},
		{`{"method":"$/cancelRequest","params":{"id":1}} x`, ``, false},
	} {
		id, ok := cancelFrameID([]byte(tc.body))
		if ok != tc.ok || string(id) != tc.id {
			t.Errorf("%s: got %q, %v want %q, %v", tc.body, id, ok, tc.id, tc.ok)
		}
	}
}

// Whatever cancelFrameID accepts, a full decode reads the same way.
func FuzzCancelFrameID(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":7}}`))
	f.Add([]byte(` {"params":{"x":[1,{"id":9}],"id":"a b"},"method":"$/cancelRequest"}`))
	f.Add([]byte(`{"Method":"$/cancelRequest","Params":{"ID":{"a":[1,2]}}}`))
	f.Add([]byte(`{"method":"$/cancelRequest","method":"x"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		id, ok := cancelFrameID(body)
		if !ok {
			return
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("accepted a frame the decoder rejects: %v", err)
		}
		if m.Method != "$/cancelRequest" {
			t.Fatalf("accepted method %q", m.Method)
		}
		var p struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if string(p.ID) != string(id) {
			t.Fatalf("id %q, decoder reads %q", id, p.ID)
		}
	})
}
