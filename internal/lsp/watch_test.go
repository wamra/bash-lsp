package lsp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/workspace"
)

// ---- helpers ----

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// symbols is workspace/symbol for query, as "rel:line name" sorted by the
// server's order.
func (c *testClient) symbols(query string) []string {
	c.t.Helper()
	var out []SymbolInformation
	if err := json.Unmarshal(c.call("workspace/symbol", map[string]any{"query": query}), &out); err != nil {
		c.t.Fatal(err)
	}
	got := []string{}
	for _, s := range out {
		got = append(got, c.short([]Location{s.Location})[0]+" "+s.Name)
	}
	return got
}

func (c *testClient) watched(events ...fileEvent) {
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": events})
}

func (c *testClient) ev(rel string, typ int) fileEvent { return fileEvent{c.uri(rel), typ} }

func (c *testClient) closeDoc(rel string) {
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}})
}

func (c *testClient) folders(added, removed []string) {
	conv := func(ps []string) []any {
		out := []any{}
		for _, p := range ps {
			out = append(out, map[string]any{"uri": workspace.URIFromPath(p), "name": filepath.Base(p)})
		}
		return out
	}
	c.notify("workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"added": conv(added), "removed": conv(removed)}})
}

func (c *testClient) requestsFromServer() []serverReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]serverReq(nil), c.serverReqs...)
}

var watchCap = map[string]any{"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true}}

// ---- registration ----

func TestRegisterWatchedFilesWhenAdvertised(t *testing.T) {
	root := copyCorpus(t)
	c := clientWith(t, clientCfg{root: root, workspaceCap: watchCap})
	c.symbols("") // a round trip: anything sent on `initialized` has arrived
	reqs := c.requestsFromServer()
	if len(reqs) != 1 {
		t.Fatalf("%d requests from the server, want 1: %+v", len(reqs), reqs)
	}
	r := reqs[0]
	want := `{"registrations":[{"id":"workspace/didChangeWatchedFiles","method":"workspace/didChangeWatchedFiles",` +
		`"registerOptions":{"watchers":[{"globPattern":"**/*"}]}}]}`
	if r.Method != "client/registerCapability" || string(r.ID) != `"bash-lsp/1"` || string(r.Params) != want {
		t.Fatalf("got %s id %s params %s\nwant params %s", r.Method, r.ID, r.Params, want)
	}

	for name, cap := range map[string]map[string]any{
		"no workspace capability":    nil,
		"dynamicRegistration false":  {"didChangeWatchedFiles": map[string]any{"dynamicRegistration": false}},
		"didChangeWatchedFiles only": {"didChangeWatchedFiles": map[string]any{}},
	} {
		c := clientWith(t, clientCfg{root: root, workspaceCap: cap})
		c.symbols("")
		if reqs := c.requestsFromServer(); len(reqs) != 0 {
			t.Errorf("%s: the server sent %+v", name, reqs)
		}
	}
}

// The client's answer to the registration is handled: an error is logged and
// the session carries on (didSave and didChange still work); responses that
// answer nothing the server asked are ignored, without a reply.
func TestRegistrationResponseHandled(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root, workspaceCap: watchCap,
		respond: func(serverReq) (any, *rpcError) { return nil, &rpcError{-32601, "no dynamic registration here"} }})
	c.symbols("")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.logs.String(), "client/registerCapability failed") {
		if time.Now().After(deadline) {
			t.Fatalf("error response not logged: %q", c.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.sendRaw([]byte(`{"jsonrpc":"2.0","id":"bash-lsp/99","result":null}`))
	c.sendRaw([]byte(`{"jsonrpc":"2.0","id":77,"error":{"code":1,"message":"x"}}`))
	c.sendRaw([]byte(`{"jsonrpc":"2.0","id":"bash-lsp/1","result":null}`)) // a second answer
	c.open("a.sh")
	c.change("a.sh", fullText("b_fn() { :; }\n"))
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 b_fn") {
		t.Fatalf("didChange after a refused registration: %v", got)
	}
	c.mu.Lock()
	_, stray := c.replies["null"]
	c.mu.Unlock()
	if stray {
		t.Fatal("the server answered a response")
	}
}

// ---- watched files ----

func libRoot(t *testing.T) string {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib.sh"), "lib_fn() { :; }\n")
	put(t, filepath.Join(root, "main.sh"), "source ./lib.sh\nlib_fn\n")
	return root
}

// A file changed on disk and announced changes definition (brief).
func TestWatchedChangeMovesDefinition(t *testing.T) {
	root := libRoot(t)
	c := clientWith(t, clientCfg{root: root, workspaceCap: watchCap})
	c.open("main.sh")
	if got := c.definition("main.sh", "lib_fn", 0); !eq(got, "lib.sh:1") {
		t.Fatal(got)
	}
	put(t, filepath.Join(root, "lib.sh"), "# moved\n\nlib_fn() { :; }\n")
	if got := c.definition("main.sh", "lib_fn", 0); !eq(got, "lib.sh:1") {
		t.Fatalf("changed before the event: %v", got)
	}
	c.watched(c.ev("lib.sh", 2))
	if got := c.definition("main.sh", "lib_fn", 0); !eq(got, "lib.sh:3") {
		t.Fatalf("after the change event: %v", got)
	}
}

// Deleting a sourced library removes its symbols and leaves the call
// unresolved (brief: deletion removes symbols).
func TestWatchedDeleteOfSourcedLibrary(t *testing.T) {
	root := libRoot(t)
	c := client(t, root)
	c.open("main.sh")
	if got := c.definition("main.sh", "lib_fn", 0); !eq(got, "lib.sh:1") {
		t.Fatal(got)
	}
	if err := os.Remove(filepath.Join(root, "lib.sh")); err != nil {
		t.Fatal(err)
	}
	c.watched(c.ev("lib.sh", 3))
	if got := c.symbols("lib_fn"); len(got) != 0 {
		t.Fatalf("symbols of a deleted file: %v", got)
	}
	if got := c.definition("main.sh", "lib_fn", 0); len(got) != 0 {
		t.Fatalf("definition into a deleted file: %v", got)
	}
	// Recreated (a delete+create pair in one notification, in either order).
	put(t, filepath.Join(root, "lib.sh"), "lib_fn() { :; }\n")
	c.watched(c.ev("lib.sh", 1), c.ev("lib.sh", 3))
	if got := c.definition("main.sh", "lib_fn", 0); !eq(got, "lib.sh:1") {
		t.Fatalf("after recreate: %v", got)
	}
}

// An event for an open document is ignored (the editor's text wins), a
// delete of an open file keeps the buffer, and didClose re-reads the disk.
func TestWatchedEventForOpenDocument(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "disk_one() { :; }\n")
	c := client(t, root)
	c.openText("a.sh", "buffer_fn() { :; }\n")
	put(t, filepath.Join(root, "a.sh"), "disk_two() { :; }\n")
	c.watched(c.ev("a.sh", 2))
	if got := c.symbols("_"); !eq(got, "a.sh:1 buffer_fn") {
		t.Fatalf("event for an open document honoured: %v", got)
	}
	if err := os.Remove(filepath.Join(root, "a.sh")); err != nil {
		t.Fatal(err)
	}
	c.watched(c.ev("a.sh", 3))
	if got := c.symbols("_"); !eq(got, "a.sh:1 buffer_fn") {
		t.Fatalf("delete of an open file: %v", got)
	}
	put(t, filepath.Join(root, "a.sh"), "disk_two() { :; }\n")
	c.watched(c.ev("a.sh", 1))
	c.closeDoc("a.sh")
	if got := c.symbols("_"); !eq(got, "a.sh:1 disk_two") {
		t.Fatalf("after didClose: %v", got)
	}
	c.watched(c.ev("a.sh", 2)) // honoured now
	put(t, filepath.Join(root, "a.sh"), "disk_three() { :; }\n")
	c.watched(c.ev("a.sh", 2))
	if got := c.symbols("_"); !eq(got, "a.sh:1 disk_three") {
		t.Fatalf("event after didClose: %v", got)
	}
}

// A created directory announced alone, and a directory renamed as a delete
// and a create, end in the state on disk.
func TestWatchedDirectories(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	c := client(t, root)
	if got := c.symbols("x_fn"); len(got) != 0 { // the workspace is open: the walk has passed
		t.Fatal(got)
	}
	put(t, filepath.Join(root, "sub", "deep", "x.sh"), "x_fn() { :; }\n")
	c.watched(c.ev("sub", 1))
	if got := c.symbols("x_fn"); !eq(got, "sub/deep/x.sh:1 x_fn") {
		t.Fatalf("after create of a directory: %v", got)
	}
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub2")); err != nil {
		t.Fatal(err)
	}
	c.watched(c.ev("sub", 3), c.ev("sub2", 1))
	if got := c.symbols("x_fn"); !eq(got, "sub2/deep/x.sh:1 x_fn") {
		t.Fatalf("after rename of a directory: %v", got)
	}
}

// Events are untrusted. Malformed URIs, other schemes, remote hosts, `..`
// out of the root, a symlink out of the root, a skipped directory: none
// crashes, none reads outside the root. A large batch is handled in one go.
func TestWatchedHostileEvents(t *testing.T) {
	outside := t.TempDir()
	put(t, filepath.Join(outside, "evil.sh"), "evil_fn() { :; }\n")
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	put(t, filepath.Join(root, "node_modules", "m.sh"), "mod_fn() { :; }\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.sh"), filepath.Join(root, "evil.sh")); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	evs := []fileEvent{
		{workspace.URIFromPath(filepath.Join(outside, "evil.sh")), 1},
		{"file://" + filepath.ToSlash(root) + "/../" + filepath.Base(outside) + "/evil.sh", 1},
		{workspace.URIFromPath(filepath.Join(root, "link", "evil.sh")), 1},
		{workspace.URIFromPath(filepath.Join(root, "evil.sh")), 1},
		{workspace.URIFromPath(filepath.Join(root, "node_modules", "m.sh")), 1},
		{"http://example.invalid/a.sh", 1},
		{"file://remotehost/a.sh", 1},
		{"file:%zz", 1},
		{"file:relative.sh", 1},
		{"", 1},
		{"not a uri at all", 99},
		{workspace.URIFromPath(filepath.Join(root, strings.Repeat("y", 2<<20))), 2},
	}
	c.watched(evs...)
	// A bad shape is an invalid notification: dropped whole.
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": 5}}})
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": "x"})
	c.notify("workspace/didChangeWatchedFiles", nil)
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 a_fn") {
		t.Fatalf("after hostile events: %v", got)
	}
	// A batch larger than the per-path limit, with one real change in it.
	put(t, filepath.Join(root, "b.sh"), "b_fn() { :; }\n")
	var many []fileEvent
	for i := 0; i < 2000; i++ {
		many = append(many, c.ev(fmt.Sprintf("gone/%d.sh", i), 3))
	}
	many = append(many, c.ev("b.sh", 1))
	c.watched(many...)
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 a_fn", "b.sh:1 b_fn") {
		t.Fatalf("after a large batch: %v", got)
	}
}

// didSave refreshes that path: a file saved by the client while not open is
// re-read; an open document keeps its buffer (it is the saved text).
func TestDidSave(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "one_fn() { :; }\n")
	put(t, filepath.Join(root, "b.sh"), "b_fn() { :; }\n")
	c := client(t, root)
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 one_fn", "b.sh:1 b_fn") { // the walk has passed
		t.Fatal(got)
	}
	put(t, filepath.Join(root, "a.sh"), "two_fn() { :; }\n")
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 two_fn", "b.sh:1 b_fn") {
		t.Fatalf("after didSave: %v", got)
	}
	c.openText("b.sh", "b_buffer() { :; }\n")
	put(t, filepath.Join(root, "b.sh"), "b_disk() { :; }\n")
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": c.uri("b.sh")}, "text": "ignored"})
	if got := c.symbols("b_"); !eq(got, "b.sh:1 b_buffer") {
		t.Fatalf("didSave of an open document: %v", got)
	}
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": "http://x/y.sh"}})
	c.notify("textDocument/didSave", "garbage")
	if got := c.symbols("two"); !eq(got, "a.sh:1 two_fn") {
		t.Fatalf("session after bad didSave: %v", got)
	}
}

// ---- multi-root ----

// twoRoots makes base/a and base/b, each with the same function, a caller
// and a library of its own.
func twoRoots(t *testing.T) (base, a, b string) {
	base = t.TempDir()
	for _, r := range []string{"a", "b"} {
		put(t, filepath.Join(base, r, "lib.sh"), "same_fn() { :; }\n"+r+"_only() { :; }\n")
		put(t, filepath.Join(base, r, "main.sh"), "source ./lib.sh\n\nsame_fn\n")
	}
	return base, filepath.Join(base, "a"), filepath.Join(base, "b")
}

// Two roots with the same function name do not see each other (brief):
// definition, references, call hierarchy and item data stay in their root;
// workspace/symbol merges both.
func TestMultiRootIsolation(t *testing.T) {
	base, a, b := twoRoots(t)
	c := clientWith(t, clientCfg{folders: []string{a, b}})
	c.root = base
	c.open("a/main.sh")
	c.open("b/main.sh")
	if got := c.definition("a/main.sh", "same_fn", 0); !eq(got, "a/lib.sh:1") {
		t.Fatalf("definition in a: %v", got)
	}
	if got := c.definition("b/main.sh", "same_fn", 0); !eq(got, "b/lib.sh:1") {
		t.Fatalf("definition in b: %v", got)
	}
	if got := c.references("a/main.sh", "same_fn", 0); !eq(got, "a/lib.sh:1", "a/main.sh:3") {
		t.Fatalf("references in a: %v", got)
	}
	if got := c.references("b/lib.sh", "same_fn", 0); !eq(got, "b/lib.sh:1", "b/main.sh:3") {
		t.Fatalf("references in b (a file not open): %v", got)
	}
	if got := c.symbols("same_fn"); !eq(got, "a/lib.sh:1 same_fn", "b/lib.sh:1 same_fn") {
		t.Fatalf("workspace/symbol: %v", got)
	}
	if got := c.symbols("_only"); !eq(got, "a/lib.sh:2 a_only", "b/lib.sh:2 b_only") {
		t.Fatalf("workspace/symbol: %v", got)
	}
	items := c.prepare("a/lib.sh", "same_fn() {", 0)
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	if items[0].Detail != "lib.sh" {
		t.Fatalf("detail relative to the item's root: %q", items[0].Detail)
	}
	var got []string
	for _, x := range c.incoming(items[0]) {
		got = append(got, c.relOf(x.From.URI))
	}
	if !eq(got, "a/main.sh") {
		t.Fatalf("incoming of a's same_fn: %v", got)
	}
	// Item data naming b's file reaches b, and only b.
	var d map[string]string
	_ = json.Unmarshal(items[0].Data, &d)
	d["path"] = strings.Replace(d["path"], string(filepath.Separator)+"a"+string(filepath.Separator), string(filepath.Separator)+"b"+string(filepath.Separator), 1)
	it := items[0]
	it.Data, _ = json.Marshal(d)
	got = nil
	for _, x := range c.incoming(it) {
		got = append(got, c.relOf(x.From.URI))
	}
	if !eq(got, "b/main.sh") {
		t.Fatalf("incoming through data naming b: %v", got)
	}
	// A `source` across roots is not followed.
	put(t, filepath.Join(a, "cross.sh"), "source ../b/lib.sh\nb_only\n")
	c.open("a/cross.sh")
	if got := c.definition("a/cross.sh", "b_only", 0); len(got) != 0 {
		t.Fatalf("cross-root source followed: %v", got)
	}
}

// Nested roots: a file belongs to the innermost root only.
func TestNestedRoots(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	put(t, filepath.Join(outer, "top.sh"), "top_fn() { :; }\ninner_fn\n")
	put(t, filepath.Join(inner, "lib.sh"), "inner_fn() { :; }\n")
	put(t, filepath.Join(inner, "use.sh"), "source ./lib.sh\ninner_fn\ntop_fn\n")
	for name, order := range map[string][]string{"outer first": {outer, inner}, "inner first": {inner, outer}} {
		t.Run(name, func(t *testing.T) {
			c := clientWith(t, clientCfg{folders: order})
			c.root = outer
			if got := c.symbols("_fn"); !eq(got, "inner/lib.sh:1 inner_fn", "top.sh:1 top_fn") && !eq(got, "top.sh:1 top_fn", "inner/lib.sh:1 inner_fn") {
				t.Fatalf("each file once: %v", got)
			}
			c.open("inner/use.sh")
			if got := c.definition("inner/use.sh", "inner_fn", 0); !eq(got, "inner/lib.sh:1") {
				t.Fatalf("definition in the inner root: %v", got)
			}
			if got := c.definition("inner/use.sh", "top_fn", 0); len(got) != 0 {
				t.Fatalf("the inner root sees the outer: %v", got)
			}
			if got := c.definition("top.sh", "inner_fn", 0); len(got) != 0 {
				t.Fatalf("the outer root sees the inner: %v", got)
			}
		})
	}
}

// Folders added and removed at runtime: an added folder is opened and its
// open documents move to it; a removed folder's workspace is dropped, its
// open documents move to whatever serves them now and their diagnostics are
// published again by it.
func TestWorkspaceFoldersAddedAndRemoved(t *testing.T) {
	base, a, b := twoRoots(t)
	put(t, filepath.Join(b, "bad.sh"), "if then\n")
	c := clientWith(t, clientCfg{folders: []string{a}})
	c.root = base
	if got := c.symbols("_only"); !eq(got, "a/lib.sh:2 a_only") {
		t.Fatalf("before: %v", got)
	}
	// Opened before its folder is added: served alone until then.
	c.open("b/main.sh")
	if got := c.definition("b/main.sh", "same_fn", 0); len(got) != 0 {
		t.Fatalf("outside every root, the document is served alone: %v", got)
	}
	c.open("b/bad.sh")
	if d := c.waitDiag("b/bad.sh"); len(d) == 0 {
		t.Fatal("no diagnostics for a broken file")
	}
	c.folders([]string{b}, nil)
	if got := c.symbols("_only"); !eq(got, "a/lib.sh:2 a_only", "b/lib.sh:2 b_only") {
		t.Fatalf("after add: %v", got)
	}
	if got := c.definition("b/main.sh", "same_fn", 0); !eq(got, "b/lib.sh:1") {
		t.Fatalf("an open document moves to the added folder: %v", got)
	}
	if d := c.waitDiag("b/bad.sh"); len(d) == 0 {
		t.Fatal("diagnostics not published by the new owner")
	}
	c.folders(nil, []string{b})
	if got := c.symbols("_only"); !eq(got, "a/lib.sh:2 a_only") {
		t.Fatalf("after remove: %v", got)
	}
	if d := c.waitDiag("b/bad.sh"); len(d) == 0 {
		t.Fatal("diagnostics not published again after the folder went away")
	}
	if got := c.definition("b/main.sh", "same_fn", 0); len(got) != 0 {
		t.Fatalf("removed folder still serves: %v", got)
	}
	// Removing a folder the server does not have, adding one twice, adding
	// a folder that does not exist: no effect, no crash.
	c.folders([]string{a, filepath.Join(base, "missing")}, []string{filepath.Join(base, "never")})
	if got := c.symbols("same_fn"); !eq(got, "a/lib.sh:1 same_fn") {
		t.Fatalf("after no-op changes: %v", got)
	}
	// A nested folder added at runtime takes its subtree from the outer one.
	c.folders([]string{base}, []string{a})
	if got := c.symbols("same_fn"); !eq(got, "a/lib.sh:1 same_fn", "b/lib.sh:1 same_fn") {
		t.Fatalf("base as the only root: %v", got)
	}
	c.folders([]string{b}, nil)
	if got := c.symbols("same_fn"); !eq(got, "a/lib.sh:1 same_fn", "b/lib.sh:1 same_fn") && !eq(got, "b/lib.sh:1 same_fn", "a/lib.sh:1 same_fn") {
		t.Fatalf("b nested in base, each file once: %v", got)
	}
	c.folders(nil, []string{b})
	if got := c.symbols("same_fn"); !eq(got, "a/lib.sh:1 same_fn", "b/lib.sh:1 same_fn") {
		t.Fatalf("b's files back in base: %v", got)
	}
}

// workspaceFolders null or empty falls back to rootUri.
func TestWorkspaceFoldersNullFallsBack(t *testing.T) {
	root := libRoot(t)
	for _, folders := range []any{nil, []any{}} {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go Serve(inR, outW, io.Discard, "t")
		t.Cleanup(func() { inW.Close(); outR.Close() })
		params := map[string]any{"capabilities": map[string]any{}, "workspaceFolders": folders, "rootUri": workspace.URIFromPath(root)}
		go func() {
			w := newConn(strings.NewReader(""), inW)
			w.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params})
			w.notify("initialized", map[string]any{})
			w.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "workspace/symbol", "params": map[string]any{"query": "lib_fn"}})
		}()
		cn := newConn(outR, io.Discard)
		for i := 0; i < 2; i++ {
			body, err := cn.read()
			if err != nil {
				t.Fatal(err)
			}
			if i == 1 && !strings.Contains(string(body), "lib_fn") {
				t.Fatalf("workspaceFolders %v: %s", folders, body)
			}
		}
	}
}

// A document outside every root is served alone: it does not see the
// root's functions and the root does not see it.
func TestDocumentOutsideEveryRoot(t *testing.T) {
	root := libRoot(t)
	out := t.TempDir()
	c := client(t, root)
	uri := workspace.URIFromPath(filepath.Join(out, "o.sh"))
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "sh", "version": 1,
		"text": "out_fn() { :; }\nlib_fn\nout_fn\n"}})
	defs := func(line int) []Location {
		var ls []Location
		raw := c.call("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": Position{line, 1}})
		if err := json.Unmarshal(raw, &ls); err != nil {
			t.Fatal(err)
		}
		return ls
	}
	if ls := defs(1); len(ls) != 0 {
		t.Fatalf("an outside document reaches into the root: %+v", ls)
	}
	if ls := defs(2); len(ls) != 1 || ls[0].URI != uri {
		t.Fatalf("definition inside the outside document: %+v", ls)
	}
	c.open("main.sh")
	if got := c.references("lib.sh", "lib_fn", 0); !eq(got, "lib.sh:1", "main.sh:2") {
		t.Fatalf("the root sees the outside document: %v", got)
	}
}

// ---- case-insensitive volumes ----

// MAIN.sh and main.sh name one document on a case-insensitive volume.
func TestCaseVariantURIs(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "main.sh"), "main_fn() { :; }\nmain_fn\n")
	real, _ := filepath.EvalSymlinks(root)
	if fi, err := os.Stat(strings.ToUpper(real)); err != nil || !sameDir(t, real, fi) {
		t.Skip("case-sensitive volume")
	}
	c := client(t, root)
	c.openText("MAIN.sh", "main_fn() { :; }\nmain_fn\nextra_fn() { :; }\n")
	if got := c.symbols("_fn"); len(got) != 2 {
		t.Fatalf("a case variant made a second file: %v", got)
	}
	ls := c.locs("textDocument/definition", "MAIN.sh", Position{1, 1}, nil)
	if len(ls) != 1 || ls[0].URI != c.uri("MAIN.sh") {
		t.Fatalf("definition: %+v", ls)
	}
	c.closeDoc("MAIN.sh")
	if got := c.symbols("_fn"); !eq(got, "main.sh:1 main_fn") {
		t.Fatalf("after close, the disk spelling: %v", got)
	}
}

func sameDir(t *testing.T, p string, fi os.FileInfo) bool {
	t.Helper()
	a, err := os.Stat(p)
	return err == nil && os.SameFile(a, fi)
}

// ---- concurrency ----

// Events and requests sent while the workspace is still being opened are
// served, in order, once it is open. The handlers do not interleave with
// the open: the first one blocks the message loop in wait() until the
// opener goroutine (the one place the server works off the loop) has
// published the folders. Under -race this checks that hand-over (folders
// written by the opener, read by the loop after ready is closed); it is not
// a test of handlers running concurrently with each other, which the
// server never does.
func TestEventsDuringOpen(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 300; i++ {
		put(t, filepath.Join(root, fmt.Sprintf("f%03d.sh", i)), fmt.Sprintf("fn_%03d() { :; }\nfn_%03d\n", i, i))
	}
	c := clientWith(t, clientCfg{root: root, workspaceCap: watchCap})
	put(t, filepath.Join(root, "late.sh"), "late_fn() { :; }\n")
	c.watched(c.ev("late.sh", 1))
	c.open("f000.sh")
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": c.uri("f000.sh")}})
	if got := c.symbols("late_fn"); !eq(got, "late.sh:1 late_fn") {
		t.Fatalf("event during open: %v", got)
	}
	for i := 0; i < 20; i++ {
		put(t, filepath.Join(root, "late.sh"), fmt.Sprintf("late_fn() { :; }\nv%d_fn() { :; }\n", i))
		c.watched(c.ev("late.sh", 2))
		if got := c.symbols(fmt.Sprintf("v%d_fn", i)); len(got) != 1 {
			t.Fatalf("round %d: %v", i, got)
		}
	}
}

// A document reached through a symlink out of the root is outside every
// folder though its spelling is inside one: its call hierarchy is answered
// from the documents outside the folders.
func TestCallHierarchyOfDocumentThroughLinkOutOfRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c := client(t, root)
	text := "callee() { :; }\ncaller() { callee; }\n"
	put(t, filepath.Join(outside, "x.sh"), text)
	c.openText("link/x.sh", text)
	items := c.prepare("link/x.sh", "callee() {", 0)
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	var got []string
	for _, x := range c.incoming(items[0]) {
		got = append(got, x.From.Name)
	}
	if !eq(got, "caller") {
		t.Fatalf("incoming of a document outside every folder: %v", got)
	}
}

// Two documents outside every folder are served together, as without a
// root: a call in one finds a function of the other (the documents outside
// the folders share one snapshot; they are not one workspace each).
func TestTwoDocumentsOutsideEveryRoot(t *testing.T) {
	root := libRoot(t)
	out := t.TempDir()
	c := client(t, root)
	for name, text := range map[string]string{"p.sh": "p_only() { :; }\n", "q.sh": "q_fn() { :; }\np_only\n"} {
		c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": workspace.URIFromPath(filepath.Join(out, name)), "languageId": "sh", "version": 1, "text": text}})
	}
	var ls []Location
	raw := c.call("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": workspace.URIFromPath(filepath.Join(out, "q.sh"))}, "position": Position{1, 1}})
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].URI != workspace.URIFromPath(filepath.Join(out, "p.sh")) {
		t.Fatalf("definition across documents outside every folder: %+v", ls)
	}
}

// A document opened under one URI and closed under another that leads to
// it (a case variant on a folding volume) leaves no entry in the URI map,
// and the diagnostics published under the opening URI are cleared.
func TestCloseUnderCaseVariantURI(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "main.sh"), "if then\n")
	real, _ := filepath.EvalSymlinks(root)
	if fi, err := os.Stat(strings.ToUpper(real)); err != nil || !sameDir(t, real, fi) {
		t.Skip("case-sensitive volume")
	}
	var out syncBuffer
	s := newServer(strings.NewReader(""), &out, io.Discard, "t")
	send := func(method string, params any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		s.dispatch(b)
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"capabilities": map[string]any{}, "rootUri": workspace.URIFromPath(root)}})
	s.dispatch(b)
	send("initialized", map[string]any{})
	upper := workspace.URIFromPath(filepath.Join(root, "MAIN.sh"))
	lower := workspace.URIFromPath(filepath.Join(root, "main.sh"))
	send("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": upper, "languageId": "sh", "version": 1, "text": "if then\n"}})
	send("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": lower}})
	if len(s.byURI) != 0 || len(s.docs) != 0 {
		t.Fatalf("after close: byURI %v, %d docs", s.byURI, len(s.docs))
	}
	// The last diagnostics for the opening URI are empty.
	cn := newConn(strings.NewReader(out.String()), io.Discard)
	last := map[string]int{}
	for {
		body, err := cn.read()
		if err != nil {
			break
		}
		var m struct {
			Method string                   `json:"method"`
			Params publishDiagnosticsParams `json:"params"`
		}
		_ = json.Unmarshal(body, &m)
		if m.Method == "textDocument/publishDiagnostics" {
			last[m.Params.URI] = len(m.Params.Diagnostics)
		}
	}
	if n, ok := last[upper]; !ok || n != 0 {
		t.Fatalf("diagnostics left under %s: %v", upper, last)
	}
}

// A folder that resolves but does not open (here a file) excludes nothing:
// only open folders take a subtree from an enclosing one, at startup as at
// runtime.
func TestFolderThatFailsToOpenExcludesNothing(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "a_fn() { :; }\n")
	put(t, filepath.Join(root, "sub", "b.sh"), "b_fn() { :; }\n")
	c := clientWith(t, clientCfg{folders: []string{root, filepath.Join(root, "a.sh")}})
	c.root = root
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 a_fn", "sub/b.sh:1 b_fn") {
		t.Fatalf("startup: %v", got)
	}
	c.folders([]string{filepath.Join(root, "sub", "b.sh")}, nil)
	if got := c.symbols("_fn"); !eq(got, "a.sh:1 a_fn", "sub/b.sh:1 b_fn") {
		t.Fatalf("runtime: %v", got)
	}
}
