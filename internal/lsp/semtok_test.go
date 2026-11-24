package lsp

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/workspace"
)

// ---- helpers ----

type semTokens struct {
	ResultID string   `json:"resultId"`
	Data     []uint32 `json:"data"`
}

type semDelta struct {
	ResultID string `json:"resultId"`
	Edits    []struct {
		Start       int      `json:"start"`
		DeleteCount int      `json:"deleteCount"`
		Data        []uint32 `json:"data"`
	} `json:"edits"`
}

var semMods = []string{"declaration", "readonly", "modification", "defaultLibrary", "local", "exported", "array",
	"associative", "integer", "nameref", "environment", "external", "unresolved"}
var semTypes = []string{"function", "variable", "parameter", "macro"}

// decodeSem spells relative 5-tuples as "(line,col,len,type,{mods})".
func decodeSem(data []uint32) []string {
	var out []string
	line, col := 0, 0
	for i := 0; i+5 <= len(data); i += 5 {
		if data[i] > 0 {
			line += int(data[i])
			col = int(data[i+1])
		} else {
			col += int(data[i+1])
		}
		var mods []string
		for b, n := range semMods {
			if data[i+4]&(1<<b) != 0 {
				mods = append(mods, n)
			}
		}
		out = append(out, fmt.Sprintf("(%d,%d,%d,%s,{%s})", line, col, data[i+2], semTypes[data[i+3]], strings.Join(mods, ",")))
	}
	return out
}

func (c *testClient) semFull(rel string) semTokens {
	c.t.Helper()
	var r semTokens
	if err := json.Unmarshal(c.call("textDocument/semanticTokens/full", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}}), &r); err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c *testClient) semRange(rel string, r map[string]any) semTokens {
	c.t.Helper()
	var out semTokens
	if err := json.Unmarshal(c.call("textDocument/semanticTokens/range", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}, "range": r}), &out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// semDeltaOf asks for a delta; it returns the raw reply, which holds either
// `data` (a full result) or `edits`.
func (c *testClient) semDeltaRaw(rel, prev string) json.RawMessage {
	c.t.Helper()
	return c.call("textDocument/semanticTokens/full/delta", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}, "previousResultId": prev})
}

func isFullReply(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	_, hasData := m["data"]
	_, hasEdits := m["edits"]
	return hasData && !hasEdits
}

// applyEdits applies a delta's edits to data.
func applyEdits(t *testing.T, data []uint32, d semDelta) []uint32 {
	t.Helper()
	out := slices.Clone(data)
	for i := len(d.Edits) - 1; i >= 0; i-- { // edits refer to the old array: from the back
		e := d.Edits[i]
		if e.Start%5 != 0 || e.DeleteCount%5 != 0 || e.Start+e.DeleteCount > len(out) {
			t.Fatalf("bad edit %+v on %d values", e, len(out))
		}
		out = slices.Concat(out[:e.Start], e.Data, out[e.Start+e.DeleteCount:])
	}
	return out
}

const semSample = "alias ll=f\nf() { local -r x=1; echo \"$x\" \"$1\"; nosuch; }\nll\n"

// ---- tests ----

func TestSemanticTokensCapability(t *testing.T) {
	sp, ok := capabilities()["semanticTokensProvider"].(map[string]any)
	if !ok {
		t.Fatal("no semanticTokensProvider")
	}
	b, _ := json.Marshal(sp)
	want := `{"full":{"delta":true},"legend":{"tokenModifiers":["declaration","readonly","modification","defaultLibrary","local","exported","array","associative","integer","nameref","environment","external","unresolved"],"tokenTypes":["function","variable","parameter","macro"]},"range":true}`
	if string(b) != want {
		t.Fatalf("semanticTokensProvider = %s\nwant %s", b, want)
	}
}

func TestSemanticTokensFullDecoded(t *testing.T) {
	c := client(t, t.TempDir())
	c.openText("a.sh", semSample)
	r := c.semFull("a.sh")
	got := decodeSem(r.Data)
	want := []string{
		"(0,6,2,macro,{declaration})",
		"(0,9,1,function,{})",
		"(1,0,1,function,{declaration})",
		"(1,15,1,variable,{declaration,readonly,local})",
		"(1,20,4,function,{defaultLibrary})",
		"(1,27,1,variable,{readonly,local})",
		"(1,32,1,parameter,{})",
		"(1,36,6,function,{unresolved})",
		"(2,0,2,macro,{})",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tokens:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.ResultID == "" {
		t.Error("no resultId")
	}
}

func TestTokensUTF16(t *testing.T) {
	c := client(t, t.TempDir())
	// The emoji is a surrogate pair: two UTF-16 units before each name on its line.
	c.openText("a.sh", "e=\"😀\"; echo \"😀$e\" \"😀$1\"; 😀=1; h() { :; }; h\n")
	got := decodeSem(c.semFull("a.sh").Data)
	want := []string{
		"(0,0,1,variable,{declaration})",
		"(0,8,4,function,{defaultLibrary})",
		"(0,17,1,variable,{})",
		"(0,24,1,parameter,{})",
		"(0,28,4,function,{})",
		"(0,34,1,function,{declaration})",
		"(0,46,1,function,{})",
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %s in\n%s", w, strings.Join(got, "\n"))
		}
	}
}

func TestEncodeTokensRelative(t *testing.T) {
	c := client(t, t.TempDir())
	c.openText("a.sh", "a=1\n  b=2; echo $a\n")
	data := c.semFull("a.sh").Data
	// a: (0,0,1); b: line +1 col 2; echo: same line +5; the name in $a: +6
	want := []uint32{0, 0, 1, 1, 1, 1, 2, 1, 1, 1, 0, 5, 4, 0, 8, 0, 6, 1, 1, 0}
	if !slices.Equal(data, want) {
		t.Fatalf("data = %v\nwant %v", data, want)
	}
}

func TestDiffTokens(t *testing.T) {
	tup := func(n ...uint32) []uint32 { return n }
	a := tup(0, 0, 1, 1, 0, 0, 2, 1, 0, 0, 1, 2, 1, 0, 0)
	cases := []struct {
		name       string
		prev, next []uint32
		start, del int
		data       []uint32
	}{
		{"equal", a, slices.Clone(a), 15, 0, nil},
		{"insert at end", a, slices.Concat(a, tup(1, 0, 1, 1, 0)), 15, 0, tup(1, 0, 1, 1, 0)},
		{"delete middle", a, slices.Concat(a[:5], a[10:]), 5, 5, nil},
		{"replace middle", a, slices.Concat(a[:5], tup(0, 3, 3, 3, 3), a[10:]), 5, 5, tup(0, 3, 3, 3, 3)},
		{"from empty", nil, a, 0, 0, a},
		{"to empty", a, nil, 0, 15, nil},
		{"insert at start", a, slices.Concat(tup(9, 9, 9, 9, 9), a), 0, 0, tup(9, 9, 9, 9, 9)},
	}
	for _, tc := range cases {
		s, d, data := DiffTokens(tc.prev, tc.next)
		if s != tc.start || d != tc.del || !slices.Equal(data, tc.data) {
			t.Errorf("%s: got (%d,%d,%v), want (%d,%d,%v)", tc.name, s, d, data, tc.start, tc.del, tc.data)
		}
		if s%5 != 0 || d%5 != 0 {
			t.Errorf("%s: edit not on tuple boundaries", tc.name)
		}
		got := slices.Concat(tc.prev[:s], data, tc.prev[s+d:])
		if !slices.Equal(got, slices.Clone(tc.next)) && !(len(got) == 0 && len(tc.next) == 0) {
			t.Errorf("%s: applying the edit gives %v, want %v", tc.name, got, tc.next)
		}
	}
}

func TestDeltaStaleResultId(t *testing.T) {
	a, nested := nestedTree(t)
	c := clientWith(t, clientCfg{folders: []string{a, nested}})
	c.root = a
	c.open("m.sh")
	c.open("nested/n.sh")
	first := c.semFull("m.sh")
	other := c.semFull("nested/n.sh")
	if first.ResultID == other.ResultID {
		t.Fatalf("two documents share result id %q", first.ResultID)
	}
	c.change("m.sh", fullText("source ./nested/n.sh\nnf\nnf\n"))
	second := c.semFull("m.sh")
	c.change("m.sh", fullText("source ./nested/n.sh\nnf\nnf\necho $HOME\n"))
	fresh := c.semFull("m.sh")

	// The current id gets a delta (control); every stale id gets a full result, even
	// though the control request issued a newer id since.
	if isFullReply(c.semDeltaRaw("m.sh", fresh.ResultID)) {
		t.Fatal("the current id should get a delta")
	}
	check := func(what, prev string) {
		t.Helper()
		raw := c.semDeltaRaw("m.sh", prev)
		if !isFullReply(raw) {
			t.Fatalf("%s: got a delta, want a full result: %s", what, raw)
		}
		var got semTokens
		_ = json.Unmarshal(raw, &got)
		want := c.semFull("m.sh")
		if !slices.Equal(got.Data, want.Data) {
			t.Fatalf("%s: full result differs from /full:\n%v\n%v", what, got.Data, want.Data)
		}
	}
	check("first id", first.ResultID)
	check("an older id", second.ResultID)
	check("another document's id", other.ResultID)
	check("an empty id", "")
	check("an unknown id", "999.999")

	// An id from before a rehome: removing the outer folder moves n.sh to
	// the inner folder's own backend.
	before := c.semFull("nested/n.sh")
	c.folders(nil, []string{a})
	c.waitReplyFolder()
	raw := c.semDeltaRaw("nested/n.sh", before.ResultID)
	if !isFullReply(raw) {
		t.Fatalf("id from before a rehome got a delta: %s", raw)
	}
	var got semTokens
	_ = json.Unmarshal(raw, &got)
	if want := c.semFull("nested/n.sh"); !slices.Equal(got.Data, want.Data) {
		t.Fatalf("after rehome: %v, want %v", got.Data, want.Data)
	}
	if got.ResultID == before.ResultID {
		t.Fatalf("a result id was reused after a rehome: %q", got.ResultID)
	}
}

// waitReplyFolder makes sure a folder change was applied: a request is
// answered only after it.
func (c *testClient) waitReplyFolder() { c.symbols("") }

func TestDeltaRoundTrip(t *testing.T) {
	root := copyCorpus(t)
	c := client(t, root)
	rel := "lib/util.sh"
	src, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	c.openText(rel, text)
	last := c.semFull(rel)
	rnd := rand.New(rand.NewSource(7))
	inserts := []string{"x=1\n", "f() { :; }\n", "echo \"$x\" $1\n", "\n", "alias q=ls\n", "😀=1\n", "$HOME", "nosuch\n"}
	deltas := 0
	for i := 0; i < 200; i++ {
		switch rnd.Intn(3) {
		case 0: // insert at a line start
			lines := strings.SplitAfter(text, "\n")
			k := rnd.Intn(len(lines))
			text = strings.Join(lines[:k], "") + inserts[rnd.Intn(len(inserts))] + strings.Join(lines[k:], "")
		case 1: // delete a line
			lines := strings.SplitAfter(text, "\n")
			if len(lines) > 3 {
				k := rnd.Intn(len(lines))
				text = strings.Join(lines[:k], "") + strings.Join(lines[k+1:], "")
			}
		default: // cut a few ASCII bytes
			if n := len(text); n > 20 {
				k := rnd.Intn(n - 5)
				text = text[:k] + text[min(k+1+rnd.Intn(3), n):]
			}
		}
		if !utf8Valid(text) {
			text = string(src)
		}
		c.change(rel, fullText(text))
		raw := c.semDeltaRaw(rel, last.ResultID)
		fullNow := rawFull(t, c, rel, text)
		var next semTokens
		if isFullReply(raw) {
			_ = json.Unmarshal(raw, &next)
		} else {
			var d semDelta
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			deltas++
			next = semTokens{ResultID: d.ResultID, Data: applyEdits(t, last.Data, d)}
		}
		if !slices.Equal(next.Data, fullNow) {
			t.Fatalf("edit %d: applying the delta gives\n%v\nwant\n%v", i, decodeSem(next.Data), decodeSem(fullNow))
		}
		if next.ResultID == last.ResultID {
			t.Fatalf("edit %d: the result id did not change", i)
		}
		last = next
	}
	if deltas < 150 {
		t.Errorf("only %d of 200 requests got a delta", deltas)
	}
}

// rawFull is the data of the whole document as a range request gives it; a
// range request leaves the stored delta base alone.
func rawFull(t *testing.T, c *testClient, rel, _ string) []uint32 {
	t.Helper()
	return c.semRange(rel, rng(0, 0, 1<<20, 0)).Data
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestRangeSubset(t *testing.T) {
	root := copyCorpus(t)
	c := client(t, root)
	c.open("lib/util.sh")
	full := decodeSem(c.semFull("lib/util.sh").Data)
	var maxLine int
	fmt.Sscanf(strings.Split(strings.TrimPrefix(full[len(full)-1], "("), ",")[0], "%d", &maxLine)
	for _, w := range [][2]int{{0, 3}, {2, 6}, {4, 5}, {0, maxLine + 1}, {maxLine, maxLine + 10}} {
		got := decodeSem(c.semRange("lib/util.sh", rng(w[0], 0, w[1], 0)).Data)
		var want []string
		for _, tk := range full {
			var line int
			fmt.Sscanf(strings.TrimPrefix(tk, "("), "%d", &line)
			if line >= w[0] && line < w[1] {
				want = append(want, tk)
			}
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("lines %v: range gave\n%v\nwant\n%v", w, got, want)
		}
	}
}

var refreshCap = map[string]any{"semanticTokens": map[string]any{"refreshSupport": true}}

// refreshClient opens m.sh (which calls `nf`, defined in nested/n.sh) and
// n.sh in one folder; the client holds tokens for m.sh. delay is the
// debounce of the refresh request.
func refreshClient(t *testing.T, cap map[string]any, delay time.Duration) *testClient {
	t.Helper()
	a, _ := nestedTree(t)
	c := clientWith(t, clientCfg{folders: []string{a}, workspaceCap: cap,
		onServer: func(s *server) { s.semtok.delay = delay }})
	c.root = a
	c.open("m.sh")
	c.open("nested/n.sh")
	c.semFull("m.sh")
	c.symbols("")
	return c
}

const nfDefined, nfMissing = "nf() { :; }\n", "ng() { :; }\n"

// Another file changes what an open document's tokens are: one refresh, after the debounce.
func TestSemanticTokensRefresh(t *testing.T) {
	c := refreshClient(t, refreshCap, 30*time.Millisecond)
	for i, text := range []string{nfMissing, nfDefined} { // nf unresolved, then resolved again
		before := refreshesAfter(c, 0)
		c.change("nested/n.sh", fullText(text))
		c.symbols("")
		if got := refreshesAfter(c, before+1) - before; got != 1 {
			t.Fatalf("edit %d: %d refresh requests, want 1", i, got)
		}
		c.semFull("m.sh") // the client asks again
	}
	for _, r := range c.requestsFromServer() {
		if r.Method == "workspace/semanticTokens/refresh" && len(r.Params) != 0 {
			t.Errorf("refresh request carries params %s, want none", r.Params)
		}
	}
}

// An edit that leaves every other document's tokens as they were sends nothing.
func TestSemanticTokensRefreshOnlyWhenChanged(t *testing.T) {
	c := refreshClient(t, refreshCap, 30*time.Millisecond)
	before := refreshesAfter(c, 0)
	c.change("nested/n.sh", fullText("nf() { :; }\nng() { :; }\n")) // m.sh still resolves nf
	c.symbols("")
	if got := refreshesAfter(c, 0) - before; got != 0 { // long past the 30 ms debounce
		t.Errorf("%d refresh requests for an edit that changes no tokens", got)
	}
}

// Without the client capability nothing is sent.
func TestSemanticTokensRefreshNeedsCapability(t *testing.T) {
	c := refreshClient(t, nil, 30*time.Millisecond)
	c.change("nested/n.sh", fullText(nfMissing))
	c.symbols("")
	if got := refreshesAfter(c, 0); got != 0 {
		t.Errorf("%d refresh requests without client support", got)
	}
}

// A burst of edits asks the client to refresh at most once, after the burst.
func TestSemanticTokensRefreshBurst(t *testing.T) {
	c := refreshClient(t, refreshCap, 700*time.Millisecond)
	before := refreshesAfter(c, 0)
	for i := 0; i < 100; i++ {
		text := nfDefined
		if i%2 == 1 {
			text = nfMissing // the last edit leaves nf unresolved
		}
		c.change("nested/n.sh", fullText(text))
	}
	c.symbols("")
	if got := refreshesAfter(c, before+1) - before; got != 1 {
		t.Errorf("%d refresh requests for a burst of 100 edits, want 1", got)
	}
}

// A document that moves to another backend asks for a refresh when its tokens
// change with it: with the folder gone, `nosuchcmd` is no longer looked up in
// the PATH profile.
func TestSemanticTokensRefreshAfterRehome(t *testing.T) {
	a, nested := nestedTree(t)
	c := clientWith(t, clientCfg{folders: []string{a, nested}, workspaceCap: refreshCap,
		onServer: func(s *server) { s.semtok.delay = 30 * time.Millisecond }})
	c.root = a
	c.openText("m.sh", "nosuchcmd\n")
	c.openText("nested/n.sh", "nf() { :; }\n")
	first := c.semFull("m.sh")
	c.semFull("nested/n.sh")
	if got := decodeSem(first.Data); len(got) != 1 || !strings.Contains(got[0], "unresolved") {
		t.Fatalf("tokens in the folder: %v", got)
	}
	before := refreshesAfter(c, 0)
	c.folders(nil, []string{a})
	c.symbols("")
	if got := refreshesAfter(c, before+1) - before; got != 1 {
		t.Errorf("%d refresh requests after a rehome that changed tokens, want 1", got)
	}
}

// One batch of watched-file changes that changes tokens in two folders asks
// the client to refresh once.
func TestSemanticTokensRefreshOncePerBatch(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, f := range []string{a, b} {
		put(t, filepath.Join(f, "m.sh"), "source ./lib.sh\nf\n")
		put(t, filepath.Join(f, "lib.sh"), "f() { :; }\n")
	}
	c := clientWith(t, clientCfg{folders: []string{a, b}, workspaceCap: refreshCap,
		onServer: func(s *server) { s.semtok.delay = 30 * time.Millisecond }})
	c.root = a
	c.openText("m.sh", "source ./lib.sh\nf\n")
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": workspaceURI(filepath.Join(b, "m.sh")), "languageId": "shellscript", "version": 1, "text": "source ./lib.sh\nf\n"}})
	c.semFull("m.sh")
	var other semTokens
	if err := json.Unmarshal(c.call("textDocument/semanticTokens/full", map[string]any{
		"textDocument": map[string]any{"uri": workspaceURI(filepath.Join(b, "m.sh"))}}), &other); err != nil {
		t.Fatal(err)
	}
	c.symbols("")
	before := refreshesAfter(c, 0)
	put(t, filepath.Join(a, "lib.sh"), "g() { :; }\n")
	put(t, filepath.Join(b, "lib.sh"), "g() { :; }\n")
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{
		map[string]any{"uri": workspaceURI(filepath.Join(a, "lib.sh")), "type": 2},
		map[string]any{"uri": workspaceURI(filepath.Join(b, "lib.sh")), "type": 2}}})
	c.symbols("")
	if got := refreshesAfter(c, before+1) - before; got != 1 {
		t.Errorf("%d refresh requests for one batch over two folders, want 1", got)
	}
}

func TestSemanticTokensFreshDocumentDelta(t *testing.T) {
	c := client(t, t.TempDir())
	for i, prev := range []string{"", "1.1", "2.1", "x"} { // a delta before any full result: a full result
		rel := fmt.Sprintf("d%d.sh", i)
		c.openText(rel, semSample)
		raw := c.semDeltaRaw(rel, prev)
		if !isFullReply(raw) {
			t.Fatalf("previousResultId %q on a fresh document got a delta: %s", prev, raw)
		}
	}
}

func TestSemanticTokensInvertedRange(t *testing.T) {
	c := client(t, t.TempDir())
	c.openText("a.sh", semSample)
	if got := c.semRange("a.sh", rng(2, 0, 0, 0)); len(got.Data) != 0 {
		t.Errorf("an inverted range gave %v", got.Data)
	}
}

func workspaceURI(p string) string { return workspace.URIFromPath(p) }

// refreshCount counts the refresh requests the server has sent so far.
func refreshCount(c *testClient) int {
	n := 0
	for _, r := range c.requestsFromServer() {
		if r.Method == "workspace/semanticTokens/refresh" {
			n++
		}
	}
	return n
}

// refreshesAfter returns the refresh count once it has reached atLeast (the
// requests are queued to the message loop and go out after the request that
// caused them is answered; give them 10 s) and then stayed there for a moment.
func refreshesAfter(c *testClient, atLeast int) int {
	deadline := time.Now().Add(10 * time.Second)
	for refreshCount(c) < atLeast && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a second, unwanted request would show here
	return refreshCount(c)
}

// Continuous edits cannot postpone the refresh for ever: it goes out at most
// maxWait after the first pending document, while the edits go on.
func TestSemanticTokensRefreshMaxWait(t *testing.T) {
	a, _ := nestedTree(t)
	c := clientWith(t, clientCfg{folders: []string{a}, workspaceCap: refreshCap,
		onServer: func(s *server) { s.semtok.delay, s.semtok.maxWait = 300*time.Millisecond, 600*time.Millisecond }})
	c.root = a
	c.open("m.sh")
	c.open("nested/n.sh")
	c.semFull("m.sh")
	c.symbols("")
	before := refreshCount(c)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		c.change("nested/n.sh", fullText(fmt.Sprintf("ng() { : %d; }\n", i))) // nf stays unresolved
		c.symbols("")
		time.Sleep(100 * time.Millisecond) // quieter than the debounce never happens
		if refreshCount(c) > before {
			return
		}
	}
	t.Fatal("no refresh while edits went on past maxWait")
}

// A document closed before the debounce fires is not refreshed.
func TestSemanticTokensRefreshNotForClosedDocument(t *testing.T) {
	c := refreshClient(t, refreshCap, 300*time.Millisecond)
	before := refreshCount(c)
	c.change("nested/n.sh", fullText(nfMissing)) // m.sh's tokens change
	c.symbols("")
	c.closeDoc("m.sh")
	if got := refreshesAfter(c, 0) - before; got != 0 {
		t.Errorf("%d refresh requests for a closed document", got)
	}
	time.Sleep(400 * time.Millisecond)
	if got := refreshCount(c) - before; got != 0 {
		t.Errorf("%d refresh requests for a closed document after the debounce", got)
	}
}

// The delay is trailing: edits spread over longer than the debounce send
// nothing until the edits stop.
func TestSemanticTokensRefreshDelayIsTrailing(t *testing.T) {
	c := refreshClient(t, refreshCap, 300*time.Millisecond)
	before := refreshCount(c)
	for i := 0; i < 10; i++ { // about 600 ms in all, twice the debounce
		c.change("nested/n.sh", fullText(fmt.Sprintf("ng() { : %d; }\n", i)))
		c.symbols("")
		time.Sleep(50 * time.Millisecond)
		if got := refreshCount(c) - before; got != 0 {
			t.Fatalf("refresh after edit %d, before the edits stopped", i)
		}
	}
	if got := refreshesAfter(c, before+1) - before; got != 1 {
		t.Errorf("%d refresh requests after the burst, want 1", got)
	}
}
