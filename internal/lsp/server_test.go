package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/workspace"
)

// ---- test client ----

type testClient struct {
	t    *testing.T
	root string
	in   *io.PipeWriter
	out  *io.PipeReader
	done chan error

	mu      sync.Mutex
	cond    *sync.Cond
	nextID  int
	replies map[string]*rpcReply
	diags   map[string][][]Diagnostic
	vers    map[string][]int  // the version of each push in diags, -1 when absent
	logMsgs []string          // window/logMessage texts, in arrival order
	shown   []json.RawMessage // params of window/showMessage notifications
	closed  bool

	// Requests from the server (method set, id set), in arrival order, and
	// how the client answers them: respond returns the result and the error
	// to send; nil answers {"result": null}.
	serverReqs []serverReq
	respond    func(serverReq) (any, *rpcError)
	logs       *syncBuffer
}

type serverReq struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// syncBuffer is a log writer the test can read while the server writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// clientCfg configures clientWith. Zero values give the defaults of client.
type clientCfg struct {
	root         string         // sent as rootUri unless folders is set
	folders      []string       // sent as workspaceFolders
	hierarchical bool           // hierarchicalDocumentSymbolSupport
	initOpts     any            // initializationOptions
	workspaceCap map[string]any // capabilities.workspace
	documentCap  map[string]any // added to capabilities.textDocument
	respond      func(serverReq) (any, *rpcError)
	onServer     func(*server) // called with the server before it runs; tests that call its methods
}

type rpcReply struct {
	Result  json.RawMessage
	Error   *rpcError
	ErrData json.RawMessage // the error's data member, as sent
}

func copyCorpus(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "corpus")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func client(t *testing.T, root string) *testClient { return clientOpt(t, root, false) }

func clientOpt(t *testing.T, root string, hierarchical bool) *testClient {
	t.Helper()
	return clientInit(t, root, hierarchical, nil)
}

// clientInit starts Serve on in-memory pipes and runs initialize +
// initialized; initOpts, when not nil, is sent as initializationOptions.
func clientInit(t *testing.T, root string, hierarchical bool, initOpts any) *testClient {
	t.Helper()
	return clientWith(t, clientCfg{root: root, hierarchical: hierarchical, initOpts: initOpts})
}

func clientWith(t *testing.T, cfg clientCfg) *testClient {
	t.Helper()
	root := cfg.root
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &testClient{t: t, root: root, in: inW, out: outR, done: make(chan error, 1),
		replies: map[string]*rpcReply{}, diags: map[string][][]Diagnostic{}, vers: map[string][]int{}, respond: cfg.respond, logs: &syncBuffer{}}
	c.cond = sync.NewCond(&c.mu)
	if root == "" {
		c.root = t.TempDir() // a place to name documents; not sent as the root
	}
	srv := newServer(inR, outW, c.logs, "test")
	if cfg.onServer != nil {
		cfg.onServer(srv)
	}
	go func() {
		err := srv.serve()
		outW.Close()
		c.done <- err
	}()
	go c.readLoop()
	t.Cleanup(func() { inW.Close(); outR.Close() })

	caps := map[string]any{"textDocument": map[string]any{
		"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": cfg.hierarchical}}}
	if cfg.workspaceCap != nil {
		caps["workspace"] = cfg.workspaceCap
	}
	for k, v := range cfg.documentCap {
		caps["textDocument"].(map[string]any)[k] = v
	}
	params := map[string]any{"processId": nil, "capabilities": caps}
	if cfg.folders != nil {
		var fs []any
		for _, f := range cfg.folders {
			fs = append(fs, map[string]any{"uri": workspace.URIFromPath(f), "name": filepath.Base(f)})
		}
		params["workspaceFolders"] = fs
	} else if root != "" {
		params["rootUri"] = workspace.URIFromPath(root)
	}
	if cfg.initOpts != nil {
		params["initializationOptions"] = cfg.initOpts
	}
	c.call("initialize", params)
	c.notify("initialized", map[string]any{})
	return c
}

func (c *testClient) readLoop() {
	cn := newConn(c.out, io.Discard)
	for {
		body, err := cn.read()
		if err != nil {
			c.mu.Lock()
			c.closed = true
			c.cond.Broadcast()
			c.mu.Unlock()
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			c.t.Errorf("server wrote a non-JSON frame: %q", body)
			continue
		}
		c.mu.Lock()
		if m.Method == "textDocument/publishDiagnostics" {
			var p publishDiagnosticsParams
			_ = json.Unmarshal(m.Params, &p)
			c.diags[p.URI] = append(c.diags[p.URI], p.Diagnostics)
			v := -1
			if p.Version != nil {
				v = *p.Version
			}
			c.vers[p.URI] = append(c.vers[p.URI], v)
		} else if m.Method == "window/logMessage" {
			var p logMessageParams
			_ = json.Unmarshal(m.Params, &p)
			c.logMsgs = append(c.logMsgs, p.Message)
		} else if m.Method == "window/showMessage" {
			c.shown = append(c.shown, m.Params)
		} else if len(m.ID) > 0 && m.Method != "" { // a request from the server
			r := serverReq{ID: m.ID, Method: m.Method, Params: m.Params}
			c.serverReqs = append(c.serverReqs, r)
			respond := c.respond
			c.mu.Unlock()
			var result any
			var rerr *rpcError
			if respond != nil {
				result, rerr = respond(r)
			}
			resp := map[string]any{"jsonrpc": "2.0", "id": r.ID}
			if rerr != nil {
				resp["error"] = rerr
			} else {
				resp["result"] = result
			}
			b, _ := json.Marshal(resp)
			// Off this goroutine: the server may be writing to us meanwhile.
			go c.in.Write(append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(b))), b...))
			c.mu.Lock()
		} else if len(m.ID) > 0 {
			rep := &rpcReply{Result: m.Result}
			if len(m.Error) > 0 && string(m.Error) != "null" {
				rep.Error = &rpcError{}
				_ = json.Unmarshal(m.Error, rep.Error)
				var d struct {
					Data json.RawMessage `json:"data"`
				}
				_ = json.Unmarshal(m.Error, &d)
				rep.ErrData = d.Data
			}
			c.replies[string(m.ID)] = rep
		}
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

func (c *testClient) sendRaw(body []byte) {
	frame := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	if _, err := c.in.Write(frame); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *testClient) notify(method string, params any) {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	c.sendRaw(b)
}

func (c *testClient) waitReply(id string) *rpcReply {
	c.t.Helper()
	deadline := time.AfterFunc(10*time.Second, func() { c.mu.Lock(); c.closed = true; c.cond.Broadcast(); c.mu.Unlock() })
	defer deadline.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.replies[id] == nil {
		if c.closed {
			c.t.Fatalf("no reply to request %s (server stopped or timed out)", id)
		}
		c.cond.Wait()
	}
	r := c.replies[id]
	delete(c.replies, id)
	return r
}

func (c *testClient) rawCall(method string, params any) *rpcReply {
	c.t.Helper()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	c.sendRaw(b)
	return c.waitReply(id)
}

// call sends a request and returns its result; an error reply fails the test.
func (c *testClient) call(method string, params any) json.RawMessage {
	c.t.Helper()
	r := c.rawCall(method, params)
	if r.Error != nil {
		c.t.Fatalf("%s: error %d %s", method, r.Error.Code, r.Error.Message)
	}
	return r.Result
}

func (c *testClient) uri(rel string) string { return workspace.URIFromPath(filepath.Join(c.root, rel)) }

func (c *testClient) open(rel string) {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.root, rel))
	if err != nil {
		c.t.Fatal(err)
	}
	c.openText(rel, string(b))
}

func (c *testClient) openText(rel, text string) {
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": c.uri(rel), "languageId": "shellscript", "version": 1, "text": text}})
}

func (c *testClient) change(rel string, changes ...map[string]any) {
	c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": c.uri(rel), "version": 2},
		"contentChanges": changes})
}

func fullText(s string) map[string]any { return map[string]any{"text": s} }

func rng(sl, sc, el, ec int) map[string]any {
	return map[string]any{"start": map[string]any{"line": sl, "character": sc}, "end": map[string]any{"line": el, "character": ec}}
}

func rangedEdit(sl, sc, el, ec int, text string) map[string]any {
	return map[string]any{"range": rng(sl, sc, el, ec), "text": text}
}

// waitDiag returns the next diagnostics push for rel, in arrival order.
func (c *testClient) waitDiag(rel string) []Diagnostic {
	c.t.Helper()
	uri := c.uri(rel)
	deadline := time.AfterFunc(10*time.Second, func() { c.mu.Lock(); c.closed = true; c.cond.Broadcast(); c.mu.Unlock() })
	defer deadline.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.diags[uri]) == 0 {
		if c.closed {
			c.t.Fatalf("no diagnostics pushed for %s", rel)
		}
		c.cond.Wait()
	}
	d := c.diags[uri][0]
	c.diags[uri] = c.diags[uri][1:]
	c.vers[uri] = c.vers[uri][1:]
	return d
}

// pos finds the 0-based line of the first line containing text and returns the
// position col characters into that text.
func (c *testClient) pos(rel, text string, col int) Position {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.root, rel))
	if err != nil {
		c.t.Fatal(err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if at := strings.Index(line, text); at >= 0 {
			return Position{i, at + col}
		}
	}
	c.t.Fatalf("%s: no line contains %q", rel, text)
	return Position{}
}

func (c *testClient) locs(method, rel string, p Position, extra map[string]any) []Location {
	c.t.Helper()
	params := map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p}
	for k, v := range extra {
		params[k] = v
	}
	var out []Location
	if err := json.Unmarshal(c.call(method, params), &out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// short renders locations as "relpath:line" (1-based) for comparison.
func (c *testClient) short(ls []Location) []string {
	out := []string{}
	for _, l := range ls {
		p, err := workspace.PathFromURI(l.URI)
		if err != nil {
			c.t.Fatal(err)
		}
		rel, err := filepath.Rel(c.root, p)
		if err != nil {
			rel = p
		}
		out = append(out, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), l.Range.Start.Line+1))
	}
	return out
}

func (c *testClient) definition(rel, text string, col int) []string {
	c.t.Helper()
	return c.short(c.locs("textDocument/definition", rel, c.pos(rel, text, col), nil))
}

func (c *testClient) references(rel, text string, col int) []string {
	c.t.Helper()
	return c.short(c.locs("textDocument/references", rel, c.pos(rel, text, col),
		map[string]any{"context": map[string]any{"includeDeclaration": true}}))
}

type symEntry struct {
	Name          string           `json:"name"`
	Kind          int              `json:"kind"`
	Detail        string           `json:"detail"`
	ContainerName string           `json:"containerName"`
	Children      []symEntry       `json:"children"`
	Location      *json.RawMessage `json:"location"`
	Selection     *Range           `json:"selectionRange"`
	Range         Range            `json:"range"`
}

func (c *testClient) docSymbols(rel string) []symEntry {
	c.t.Helper()
	var out []symEntry
	raw := c.call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}})
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func (c *testClient) symbolNames(rel string) []string {
	var out []string
	for _, s := range c.docSymbols(rel) {
		out = append(out, s.Name)
	}
	return out
}

func eq(got []string, want ...string) bool {
	return strings.Join(got, ",") == strings.Join(want, ",")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (c *testClient) shutdownExit() error {
	c.t.Helper()
	c.call("shutdown", nil)
	c.notify("exit", nil)
	select {
	case err := <-c.done:
		return err
	case <-time.After(10 * time.Second):
		c.t.Fatal("Serve did not return after exit")
		return nil
	}
}

// ---- tests ----

func TestInitializeCapabilities(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go Serve(inR, outW, io.Discard, "9.9.9")
	t.Cleanup(func() { inW.Close(); outR.Close() })
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`)
	go inW.Write(append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...))
	got, err := newConn(outR, io.Discard).read()
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
			ServerInfo   map[string]any `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatal(err)
	}
	want := `{"callHierarchyProvider":true,"codeActionProvider":{"codeActionKinds":["quickfix","refactor.rewrite"]},` +
		`"completionProvider":{"resolveProvider":true,"triggerCharacters":["$","{","-","/"]},"definitionProvider":true,` +
		`"diagnosticProvider":{"interFileDependencies":true,"workspaceDiagnostics":false},"documentFormattingProvider":true,` +
		`"documentHighlightProvider":true,"documentLinkProvider":{"resolveProvider":false},` +
		`"documentRangeFormattingProvider":true,"documentSymbolProvider":true,"foldingRangeProvider":true,"hoverProvider":true,"implementationProvider":true,"inlayHintProvider":true,` +
		`"positionEncoding":"utf-16","referencesProvider":true,"renameProvider":true,"selectionRangeProvider":true,` +
		`"semanticTokensProvider":{"full":{"delta":true},"legend":{"tokenModifiers":["declaration","readonly","modification","defaultLibrary","local","exported","array","associative","integer","nameref","environment","external","unresolved"],"tokenTypes":["function","variable","parameter","macro"]},"range":true},` +
		`"signatureHelpProvider":{"retriggerCharacters":[" "],"triggerCharacters":[" "]},` +
		`"textDocumentSync":{"change":2,"openClose":true,"save":{"includeText":false}},` +
		`"workspace":{"workspaceFolders":{"changeNotifications":"workspace/didChangeWorkspaceFolders","supported":true}},"workspaceSymbolProvider":true}`
	have, _ := json.Marshal(resp.Result.Capabilities) // map keys marshal sorted
	if string(have) != want {
		t.Fatalf("capabilities\n got %s\nwant %s", have, want)
	}
	if resp.Result.ServerInfo["name"] != "bash-lsp" || resp.Result.ServerInfo["version"] != "9.9.9" {
		t.Fatalf("serverInfo %v", resp.Result.ServerInfo)
	}
}

func TestFirstDocumentSymbolNotEmpty(t *testing.T) { // AC-07, C13: no sleep between initialized, didOpen and the request
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	if n := len(c.docSymbols("main.sh")); n == 0 {
		t.Fatal("empty")
	}
}

func TestDefinitionAndReferences(t *testing.T) { // C01, C08 over the wire, with UTF-16 positions
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	if got := c.definition("main.sh", `  build_release "$name"`, 3); !eq(got, "lib/util.sh:4") {
		t.Fatal(got)
	}
	if got := c.references("main.sh", `local name="$1"`, 6); len(got) != 3 {
		t.Fatal(got)
	}
	// Declaration excluded when the client asks for it.
	p := c.pos("main.sh", `local name="$1"`, 6)
	got := c.locs("textDocument/references", "main.sh", p, map[string]any{"context": map[string]any{"includeDeclaration": false}})
	if len(got) != 2 {
		t.Fatal(c.short(got))
	}
}

func TestLocationURIsKeepClientSpelling(t *testing.T) {
	root := copyCorpus(t) // on macOS TempDir is under the /var symlink; the workspace resolves it
	c := client(t, root)
	c.open("main.sh")
	ls := c.locs("textDocument/definition", "main.sh", c.pos("main.sh", `  build_release "$name"`, 3), nil)
	if len(ls) != 1 || ls[0].URI != c.uri("lib/util.sh") {
		t.Fatalf("got %+v, want uri %s", ls, c.uri("lib/util.sh"))
	}
}

func TestImplementationAndHover(t *testing.T) {
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	p := c.pos("main.sh", `  build_release "$name"`, 3)
	if got := c.short(c.locs("textDocument/implementation", "main.sh", p, nil)); !eq(got, "lib/util.sh:4") {
		t.Fatal(got)
	}
	var h struct {
		Contents struct{ Kind, Value string }
		Range    Range
	}
	raw := c.call("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": p})
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	if h.Contents.Kind != "markdown" || !strings.Contains(h.Contents.Value, "build_release") || h.Range.End.Character <= h.Range.Start.Character {
		t.Fatalf("hover %s", raw)
	}
	// Nothing under the cursor: null.
	raw = c.call("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": Position{0, 0}})
	if string(raw) != "null" {
		t.Fatalf("hover on shebang: %s", raw)
	}
}

func TestWorkspaceSymbol(t *testing.T) {
	c := client(t, copyCorpus(t))
	var out []SymbolInformation
	if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": "build_rel"}), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || out[0].Name != "build_release" || out[0].Kind != 12 || out[0].Location.URI != c.uri("lib/util.sh") {
		t.Fatalf("%+v", out)
	}
}

func TestHierarchicalAndFlatSymbols(t *testing.T) {
	c := clientOpt(t, copyCorpus(t), true)
	c.open("main.sh")
	var deploy *symEntry
	syms := c.docSymbols("main.sh")
	for i := range syms {
		if syms[i].Name == "deploy" {
			deploy = &syms[i]
		}
	}
	if deploy == nil || deploy.Kind != 12 {
		t.Fatalf("no deploy function in %+v", syms)
	}
	var kids []string
	for _, k := range deploy.Children {
		kids = append(kids, k.Name)
		if k.Kind != 13 {
			t.Fatalf("child %s kind %d", k.Name, k.Kind)
		}
	}
	if !eq(kids, "name", "cmd") {
		t.Fatalf("deploy children %v", kids)
	}
	if deploy.Selection == nil || deploy.Selection.Start.Line != 3 {
		t.Fatalf("selectionRange %+v", deploy.Selection)
	}
	for _, s := range syms {
		if s.Name == "br" && (s.Kind != 12 || s.Detail != "alias") {
			t.Fatalf("alias symbol %+v", s)
		}
	}

	f := client(t, copyCorpus(t))
	f.open("main.sh")
	flat := f.docSymbols("main.sh")
	byName := map[string][]symEntry{}
	for _, s := range flat {
		byName[s.Name] = append(byName[s.Name], s)
		if s.Location == nil || len(s.Children) != 0 {
			t.Fatalf("not a flat SymbolInformation: %+v", s)
		}
	}
	for _, n := range []string{"name", "cmd"} {
		got := byName[n]
		if len(got) == 0 || got[0].ContainerName != "deploy" || got[0].Kind != 13 {
			t.Fatalf("%s: %+v", n, got)
		}
	}
	if d := byName["deploy"]; len(d) != 1 || d[0].ContainerName != "" {
		t.Fatalf("deploy: %+v", d)
	}
}

func TestSyntaxDiagnostics(t *testing.T) { // AC-11, C20
	c := client(t, copyCorpus(t))
	c.open("broken_exp.sh")
	d := c.waitDiag("broken_exp.sh")
	if len(d) != 1 || d[0].Range.Start.Line != 2 || d[0].Code != "WB1001" || d[0].Severity != 1 || d[0].Source != "bash-lsp" {
		t.Fatalf("%+v", d)
	}
	if names := c.symbolNames("broken_exp.sh"); !contains(names, "after_exp") {
		t.Fatal(names)
	}
	c.change("broken_exp.sh", fullText("#!/usr/bin/env bash\nok() { :; }\n"))
	if d := c.waitDiag("broken_exp.sh"); len(d) != 0 {
		t.Fatal("diagnostics not cleared")
	}
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": c.uri("broken_exp.sh")}})
	if d := c.waitDiag("broken_exp.sh"); len(d) != 0 {
		t.Fatalf("close must clear diagnostics: %+v", d)
	}
}

func TestSkippedFileDiagnostic(t *testing.T) {
	c := client(t, copyCorpus(t))
	c.openText("z.zsh", "print hello\n")
	d := c.waitDiag("z.zsh")
	if len(d) != 1 || d[0].Severity != 3 || d[0].Code != "WB1002" || d[0].Range != (Range{}) || d[0].Message == "" {
		t.Fatalf("%+v", d)
	}
}

func TestIncrementalEdit(t *testing.T) {
	c := client(t, copyCorpus(t))
	c.openText("t.sh", "f() { :; }\nf\n")
	use := Position{1, 0}
	def := func() []Location { return c.locs("textDocument/definition", "t.sh", use, nil) }
	if got := def(); len(got) != 1 || got[0].Range.Start != (Position{0, 0}) {
		t.Fatalf("before edit: %+v", got)
	}
	c.change("t.sh", rangedEdit(0, 0, 0, 1, "g"))
	if got := def(); len(got) != 0 {
		t.Fatalf("after renaming the function: %+v", got)
	}
	c.change("t.sh", rangedEdit(1, 0, 1, 1, "g"))
	if got := def(); len(got) != 1 || got[0].Range.Start != (Position{0, 0}) {
		t.Fatalf("after renaming the use: %+v", got)
	}
}

func TestNonASCIIPositions(t *testing.T) {
	c := client(t, copyCorpus(t))
	text := "echo \"é😀\"; foo() { :; }; foo\n"
	c.openText("u.sh", text)
	// e c h o ␠ " é = 7 units, the emoji 2, `" ; ␠` 3: the first foo starts at 12,
	// `foo() { :; }; ` is 14 more, so the last foo starts at 26 and ends at 29.
	ls := c.locs("textDocument/definition", "u.sh", Position{0, 26}, nil)
	if len(ls) != 1 {
		t.Fatalf("%+v", ls)
	}
	want := Range{Position{0, 12}, Position{0, 15}}
	if ls[0].Range != want {
		t.Fatalf("got %+v, want %+v", ls[0].Range, want)
	}
	// A cursor in the middle of the last foo works too.
	if ls := c.locs("textDocument/definition", "u.sh", Position{0, 28}, nil); len(ls) != 1 {
		t.Fatalf("%+v", ls)
	}
}

func TestLargeDocument(t *testing.T) {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\necho start\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "# Function number %d.\nfn_%d() {\n  local a=\"$1\" b=\"$2\"\n", i, i)
		fmt.Fprintf(&b, "  if [[ -n \"$a\" && \"$b\" != \"x%d\" ]]; then\n    printf '%%s %%s\\n' \"$a\" \"$b\" | tr a-z A-Z\n  else\n", i)
		fmt.Fprintf(&b, "    for f in \"$@\"; do echo \"${f:-none}\" >&2; done\n  fi\n  case \"$a\" in start|stop) echo \"$a\";; *) return %d;; esac\n}\n\n", i%250)
		if i > 0 {
			fmt.Fprintf(&b, "fn_%d a b\n", i-1)
		}
	}
	b.WriteString("fn_1999 a b\n")
	if b.Len() < 500_000 {
		t.Fatalf("generated only %d bytes", b.Len())
	}
	c := client(t, "")
	start := time.Now()
	c.openText("big.sh", b.String())
	n := 0
	for _, s := range c.docSymbols("big.sh") {
		if s.Kind == 12 && strings.HasPrefix(s.Name, "fn_") {
			n++
		}
	}
	if n != 2000 {
		t.Fatalf("%d function symbols", n)
	}
	// The timing bound is a smoke test for gross regressions; the race
	// detector makes it meaningless on a hosted runner (1.08 s here already).
	if el := time.Since(start); el > 2*time.Second && !raceEnabled {
		t.Fatalf("open + documentSymbol took %v", el)
	}
}

func TestUnknownMethodAndPanicIsolation(t *testing.T) {
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	if r := c.rawCall("foo/bar", map[string]any{}); r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("%+v", r)
	}
	c.notify("foo/notification", map[string]any{}) // ignored
	c.notify("$/cancelRequest", map[string]any{"id": 1})
	// Bad params: error, not a crash.
	if r := c.rawCall("textDocument/hover", "not an object"); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("%+v", r)
	}
	p := map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": c.pos("main.sh", `  build_release "$name"`, 3)}
	if raw := c.call("textDocument/hover", p); string(raw) == "null" {
		t.Fatal("hover empty after errors")
	}
}

func TestPanicBecomesInternalError(t *testing.T) {
	var out bytes.Buffer
	s2 := &server{c: newConn(strings.NewReader(""), &out), log: nilLogger(), ready: make(chan struct{}), started: true}
	close(s2.ready) // be is nil: dereferencing it panics
	_, rerr := s2.safeHandle(message{Method: "textDocument/hover", ID: json.RawMessage("1"),
		Params: json.RawMessage(`{"textDocument":{"uri":"file:///x.sh"},"position":{"line":0,"character":0}}`)})
	if rerr == nil || rerr.Code != -32603 {
		t.Fatalf("got %+v", rerr)
	}
}

func TestShutdownExit(t *testing.T) {
	c := client(t, copyCorpus(t))
	if raw := c.call("shutdown", nil); string(raw) != "null" {
		t.Fatalf("shutdown result %s", raw)
	}
	if r := c.rawCall("textDocument/hover", map[string]any{}); r.Error == nil || r.Error.Code != -32600 {
		t.Fatalf("request after shutdown: %+v", r)
	}
	c.notify("exit", nil)
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestExitWithoutShutdownAndEndOfInput(t *testing.T) {
	c := client(t, "")
	c.notify("exit", nil)
	if err := <-c.done; err == nil {
		t.Fatal("exit before shutdown must be an error")
	}
	c2 := client(t, "")
	c2.in.Close()
	if err := <-c2.done; err == nil {
		t.Fatal("end of input must be an error")
	}
}

// Well-framed messages that are not valid requests get an error reply and the
// session continues.
func TestMalformedMessagesKeepSessionAlive(t *testing.T) {
	c := client(t, "")
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{not json`, -32700},
		{`{"jsonrpc":"2.0","id":1`, -32700},
		{`{"jsonrpc":"2.0"} trailing`, -32700},
		{`[1,2,3]`, -32600},
		{`{"jsonrpc":"2.0","id":"s","method":42}`, -32600},
	} {
		c.sendRaw([]byte(tc.body))
		if r := c.waitReply("null"); r.Error == nil || r.Error.Code != tc.code {
			t.Fatalf("%s: %+v", tc.body, r)
		}
	}
	c.openText("a.sh", "f() { :; }\n")
	if got := c.symbolNames("a.sh"); !eq(got, "f") {
		t.Fatal(got)
	}
}

// A header block without a valid Content-Length cannot be re-aligned (its body
// length is unknown), so it ends the session; nothing but frames reaches stdout.
func TestBadFrameWithBodyIsFatal(t *testing.T) {
	for name, bad := range map[string]string{
		"missing":  "Content-Type: x\r\n\r\n" + `{"jsonrpc":"2.0","id":9,"method":"shutdown"}`,
		"invalid":  "Content-Length: abc\r\n\r\n" + `{"jsonrpc":"2.0","id":9,"method":"shutdown"}`,
		"negative": "Content-Length: -5\r\n\r\n" + `{"jsonrpc":"2.0","id":9,"method":"shutdown"}`,
	} {
		t.Run(name, func(t *testing.T) {
			init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`
			later := `{"jsonrpc":"2.0","id":2,"method":"shutdown"}`
			in := fmt.Sprintf("Content-Length: %d\r\n\r\n%s%s", len(init), init, bad) +
				fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(later), later)
			var out, logs bytes.Buffer
			err := Serve(strings.NewReader(in), &out, &logs, "t")
			if err == nil || !strings.Contains(err.Error(), "Content-Length") {
				t.Fatalf("Serve returned %v", err)
			}
			if !strings.Contains(logs.String(), "fatal framing error") {
				t.Fatalf("log: %q", logs.String())
			}
			cn := newConn(&out, io.Discard)
			frames := 0
			for {
				body, rerr := cn.read()
				if rerr != nil {
					if rerr != io.EOF {
						t.Fatalf("stdout is not whole frames: %v", rerr)
					}
					break
				}
				if !json.Valid(body) {
					t.Fatalf("bad frame %q", body)
				}
				frames++
			}
			if frames != 1 {
				t.Fatalf("%d frames on stdout, want only the initialize reply", frames)
			}
		})
	}
}

func TestDocumentThroughSymlinkedDirectoryInRoot(t *testing.T) {
	root := copyCorpus(t)
	if err := os.Symlink(filepath.Join(root, "lib"), filepath.Join(root, "liblink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c := client(t, root)
	orig, err := os.ReadFile(filepath.Join(root, "lib", "util.sh"))
	if err != nil {
		t.Fatal(err)
	}
	c.openText("liblink/util.sh", string(orig)+"build_release\n")
	if d := c.waitDiag("liblink/util.sh"); len(d) != 0 {
		t.Fatalf("%+v", d)
	}
	names := c.symbolNames("liblink/util.sh")
	if !contains(names, "build_release") || !contains(names, "log") {
		t.Fatalf("documentSymbol through a directory alias: %v", names)
	}
	use := Position{strings.Count(string(orig), "\n"), 2}
	ls := c.locs("textDocument/definition", "liblink/util.sh", use, nil)
	if len(ls) != 1 || ls[0].URI != c.uri("liblink/util.sh") {
		t.Fatalf("definition must come back under the client's URI %s: %+v", c.uri("liblink/util.sh"), ls)
	}
}

func TestRequestBeforeInitialize(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go Serve(inR, outW, io.Discard, "t")
	t.Cleanup(func() { inW.Close(); outR.Close() })
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{}}`)
	go inW.Write(append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...))
	got, err := newConn(outR, io.Discard).read()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-32002") {
		t.Fatalf("%s", got)
	}
}

func TestNoRootServesOpenDocumentsOnly(t *testing.T) {
	dir := copyCorpus(t) // exists on disk but must not be scanned
	c := client(t, "")
	c.root = dir
	c.openText("main.sh", "deploy() { :; }\ndeploy\n")
	var out []SymbolInformation
	if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": ""}), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "deploy" {
		t.Fatalf("workspace symbols without a root: %+v", out)
	}
	if got := c.short(c.locs("textDocument/definition", "main.sh", Position{1, 0}, nil)); !eq(got, "main.sh:1") {
		t.Fatal(got)
	}
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}})
	if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": ""}), &out); err != nil || len(out) != 0 {
		t.Fatalf("after close: %+v %v", out, err)
	}
}

func TestRootFromWorkspaceFoldersAndRootPath(t *testing.T) {
	root := copyCorpus(t)
	for name, params := range map[string]map[string]any{
		"workspaceFolders": {"workspaceFolders": []any{map[string]any{"uri": workspace.URIFromPath(root), "name": "w"}}, "rootUri": nil},
		"rootPath":         {"rootPath": root},
	} {
		t.Run(name, func(t *testing.T) {
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			go Serve(inR, outW, io.Discard, "t")
			t.Cleanup(func() { inW.Close(); outR.Close() })
			go func() {
				c := newConn(strings.NewReader(""), inW)
				params["capabilities"] = map[string]any{}
				c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params})
				c.notify("initialized", map[string]any{})
				c.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "workspace/symbol", "params": map[string]any{"query": "build_release"}})
			}()
			cn := newConn(outR, io.Discard)
			for i := 0; i < 2; i++ {
				body, err := cn.read()
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 && !strings.Contains(string(body), "build_release") {
					t.Fatalf("workspace not indexed from %s: %s", name, body)
				}
			}
		})
	}
}

func TestWatchedFilesRefresh(t *testing.T) {
	root := copyCorpus(t)
	c := client(t, root)
	query := func() int {
		var out []SymbolInformation
		if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": "freshly_added"}), &out); err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	if query() != 0 {
		t.Fatal("unexpected symbol")
	}
	if err := os.WriteFile(filepath.Join(root, "new.sh"), []byte("freshly_added() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": c.uri("new.sh"), "type": 1}}})
	if query() != 1 {
		t.Fatal("new file not indexed after didChangeWatchedFiles")
	}
}

func TestLargeMessage(t *testing.T) {
	// More than the 561 KB at which an earlier server exited, in a single frame.
	big := "#!/usr/bin/env bash\n" + strings.Repeat("# "+strings.Repeat("padding ", 20)+"\n", 4000) + "f() { :; }\n"
	c := client(t, "")
	c.openText("big.sh", big)
	if got := c.symbolNames("big.sh"); !eq(got, "f") {
		t.Fatal(got)
	}
}

func TestShutdownExitReturnsNil(t *testing.T) {
	c := client(t, copyCorpus(t))
	if err := c.shutdownExit(); err != nil {
		t.Fatal(err)
	}
}

func nilLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// review I1 over the wire: didClose after a save serves the saved file.
func TestDidCloseRereadsDisk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.sh"), []byte("old_fn() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	c.open("a.sh")
	c.change("a.sh", fullText("new_fn() { :; }\n"))
	if err := os.WriteFile(filepath.Join(root, "a.sh"), []byte("new_fn() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	var out []SymbolInformation
	if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": "_fn"}), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "new_fn" {
		t.Fatalf("after close: %+v", out)
	}
}

// An unresolved `source` is published as WB1101 on the path word.
func TestUnresolvedSourcePublished(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "m.sh"), []byte("source ./nope.sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	c.open("m.sh")
	d := c.waitDiag("m.sh")
	if len(d) != 1 || d[0].Code != "WB1101" || d[0].Source != "bash-lsp" || d[0].Severity != 2 ||
		d[0].Range != (Range{Position{0, 7}, Position{0, 16}}) {
		t.Fatalf("%+v", d)
	}
}

// Hover on the path word of an unresolved `source` says why.
func TestHoverOnUnresolvedSourceWire(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "m.sh"), []byte("source ./nope.sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	c.open("m.sh")
	c.waitDiag("m.sh")
	if got := c.hoverText("m.sh", "source ./nope.sh", 9); got != "`source` not resolved (unresolved/not-found)" {
		t.Fatalf("hover %q", got)
	}
}

// Native findings follow the settings: a rule disabled in the
// initialization options is not published; an unused function carries the
// Unnecessary tag.
func TestDiagnosticSettingsWire(t *testing.T) {
	root := t.TempDir()
	src := "#!/bin/bash\nf() { :; }\nrm $x\n"
	if err := os.WriteFile(filepath.Join(root, "m.sh"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "n.sh"), []byte("source ./nope.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := clientWith(t, clientCfg{root: root, initOpts: map[string]any{"diagnostics": map[string]any{"disable": []string{"WB1101", "WB9999"}},
		"severity": map[string]any{"WB1401": "error"}}})
	c.open("m.sh")
	d := c.waitDiag("m.sh")
	codes := map[string]Diagnostic{}
	for _, x := range d {
		codes[x.Code] = x
	}
	if len(d) != 2 {
		t.Fatalf("%+v", d)
	}
	if x := codes["WB1201"]; x.Severity != 4 || len(x.Tags) != 1 || x.Tags[0] != 1 {
		t.Fatalf("WB1201: %+v", x)
	}
	if x := codes["WB1401"]; x.Severity != 1 {
		t.Fatalf("WB1401: %+v", x)
	}
	c.open("n.sh")
	if d := c.waitDiag("n.sh"); len(d) != 0 {
		t.Fatalf("disabled rule published: %+v", d)
	}
	if !strings.Contains(c.logs.String(), `diagnostics.disable: unknown rule id "WB9999"`) {
		t.Fatalf("unknown rule not logged:\n%s", c.logs.String())
	}
}

// WB1301 follows bash.target: the default range flags `declare -A`, a repo
// file that starts the range at 4.0 does not, and an sh script gets the
// dialect finding instead.
func TestVersionRulesWire(t *testing.T) {
	root := t.TempDir()
	for name, src := range map[string]string{
		"a.sh": "#!/bin/bash\ndeclare -A m\n",
		"s.sh": "#!/bin/sh\n[[ -n $x ]]\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := client(t, root)
	c.open("a.sh")
	if d := c.waitDiag("a.sh"); len(d) != 1 || d[0].Code != "WB1301" || d[0].Severity != 2 ||
		d[0].Range != (Range{Position{1, 0}, Position{1, 10}}) || !strings.Contains(d[0].Message, "bash 4.0") {
		t.Fatalf("default target: %+v", d)
	}
	c.open("s.sh")
	if d := c.waitDiag("s.sh"); len(d) != 1 || d[0].Code != "WB1302" || d[0].Severity != 2 || d[0].Range != (Range{Position{1, 0}, Position{1, 2}}) {
		t.Fatalf("sh file: %+v", d)
	}

	narrow := t.TempDir()
	if err := os.WriteFile(filepath.Join(narrow, "a.sh"), []byte("#!/bin/bash\ndeclare -A m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(narrow, ".bash-lsp.toml"), []byte("[bash]\ntarget = \"4.0-5.3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = client(t, narrow)
	c.open("a.sh")
	if d := c.waitDiag("a.sh"); len(d) != 0 {
		t.Fatalf("target 4.0-5.3: %+v", d)
	}
}

// An unknown rule id in the settings is shown once per session as a
// warning (MessageType 2) and logged; an id reserved for a rule not built
// yet is neither. A recovered parser panic is logged with its text and
// published without it.
func TestUnknownRuleShownOnce(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(root, "m.sh"), []byte("A=$((0 A\nf() { :; }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := clientWith(t, clientCfg{folders: []string{a, b}, initOpts: map[string]any{"diagnostics": map[string]any{"disable": []string{"WB9999", "WB1102"}}}})
	c.root = a
	c.open("m.sh")
	d := c.waitDiag("m.sh")
	if len(d) != 1 || d[0].Code != "WB1003" || strings.Contains(d[0].Message, "runtime error") {
		t.Fatalf("%+v", d)
	}
	c.mu.Lock()
	shown := append([]json.RawMessage(nil), c.shown...)
	c.mu.Unlock()
	if len(shown) != 1 {
		t.Fatalf("%d messages: %s", len(shown), shown)
	}
	var m struct {
		Type    int    `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(shown[0], &m); err != nil || m.Type != 2 || !strings.Contains(m.Message, "WB9999") || strings.Contains(m.Message, "WB1102") {
		t.Fatalf("%s", shown[0])
	}
	logs := c.logs.String()
	if strings.Contains(logs, `"WB1102"`) || !strings.Contains(logs, "parser failure: runtime error") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// A missing file named by an absolute path into another workspace folder is
// inside the workspace: a warning, not the information of a path elsewhere.
func TestSourceIntoOtherFolderIsWarning(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	src := "source " + filepath.Join(b, "lib", "none.sh") + "\nsource /elsewhere/none.sh\n"
	if err := os.WriteFile(filepath.Join(a, "m.sh"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	c := clientWith(t, clientCfg{folders: []string{a, b}})
	c.root = a
	c.open("m.sh")
	d := c.waitDiag("m.sh")
	if len(d) != 2 || d[0].Severity != 2 || d[1].Severity != 3 {
		t.Fatalf("%+v", d)
	}
}
