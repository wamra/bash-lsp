package lsp

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/format"
	"github.com/wamra/bash-lsp/tree"
)

// ---- helpers ----

func setFormatHook(t *testing.T, fn func()) {
	t.Helper()
	hookMu.Lock()
	formatHook = fn
	hookMu.Unlock()
	t.Cleanup(func() { hookMu.Lock(); formatHook = nil; hookMu.Unlock() })
}

func fmtParams(c *testClient, rel string, extra map[string]any) map[string]any {
	p := map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)},
		"options": map[string]any{"tabSize": 4, "insertSpaces": false}}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func parseEdits(t *testing.T, raw json.RawMessage) []TextEdit {
	t.Helper()
	if string(raw) == "null" {
		t.Fatalf("answered null, want a list")
	}
	var out []TextEdit
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func (c *testClient) formatting(rel string, extra map[string]any) []TextEdit {
	c.t.Helper()
	return parseEdits(c.t, c.call("textDocument/formatting", fmtParams(c, rel, extra)))
}

func (c *testClient) rangeFormatting(rel string, r map[string]any, extra map[string]any) []TextEdit {
	c.t.Helper()
	p := fmtParams(c, rel, extra)
	p["range"] = r
	return parseEdits(c.t, c.call("textDocument/rangeFormatting", p))
}

// apply applies edits (one, from a formatter) to text.
func apply(t *testing.T, text string, edits []TextEdit) string {
	t.Helper()
	f := tree.NewFile("", []byte(text))
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		s := f.FromUTF16(e.Range.Start.Line, e.Range.Start.Character).Offset
		en := f.FromUTF16(e.Range.End.Line, e.Range.End.Character).Offset
		text = text[:s] + e.NewText + text[en:]
	}
	return text
}

// ---- tests ----

func TestFormatting(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	src := "#!/bin/bash\nif true;then echo   a;fi\nf() {\necho b\n}\n"
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	edits := c.formatting("a.sh", nil)
	want, err := format.Source("a.sh", []byte(src), format.OptionsFor("a.sh", root, format.Settings{}, &format.LSPOptions{TabSize: 4}))
	if err != nil || len(edits) != 1 {
		t.Fatalf("%v %+v", err, edits)
	}
	if got := apply(t, src, edits); got != string(want) {
		t.Fatalf("formatted\n%q\nwant\n%q", got, want)
	}
	if edits[0].Range.Start.Line != 1 || edits[0].Range.Start.Character != 0 {
		t.Fatalf("the edit should start at the first changed line: %+v", edits[0].Range)
	}
	// The editor's options: spaces, four of them.
	spaced := c.formatting("a.sh", map[string]any{"options": map[string]any{"tabSize": 4, "insertSpaces": true}})
	if got := apply(t, src, spaced); !strings.Contains(got, "\n    echo b\n") {
		t.Fatalf("insertSpaces ignored: %q", got)
	}
	// Formatted text needs no edit.
	c.change("a.sh", fullText(string(want)))
	c.waitPush("a.sh")
	if got := c.formatting("a.sh", nil); len(got) != 0 {
		t.Fatalf("already formatted: %+v", got)
	}
}

func TestFormattingRefusalLogsReason(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	src := "echo ok\necho \"abc\necho more\n"
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	_, ferr := format.Source("a.sh", []byte(src), format.Options{})
	if ferr == nil {
		t.Fatal("the formatter accepted a broken file")
	}
	if got := c.formatting("a.sh", nil); len(got) != 0 {
		t.Fatalf("refusal returned edits: %+v", got)
	}
	if msg := c.waitLog(ferr.Error()); !strings.Contains(msg, "format") {
		t.Fatalf("log text %q", msg)
	}
	before := len(c.logsContaining(ferr.Error()))
	c.formatting("a.sh", nil)
	c.waitLogCount(ferr.Error(), before+1)
}

func (c *testClient) waitLogCount(sub string, n int) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.logsContaining(sub)) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("fewer than %d log messages containing %q: %q", n, sub, c.logMsgs)
}

func TestRangeFormatting(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	src := "echo   one\necho   two\necho   three\n"
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	edits := c.rangeFormatting("a.sh", rng(1, 0, 1, 4), nil)
	if len(edits) != 1 {
		t.Fatalf("%+v", edits)
	}
	if got := apply(t, src, edits); got != "echo   one\necho two\necho   three\n" {
		t.Fatalf("range formatting touched more than the second line: %q", got)
	}
	// A range over the blank line between statements touches none: no edit.
	c.change("a.sh", fullText("echo   x\n\necho   y\n"))
	c.waitPush("a.sh")
	if got := c.rangeFormatting("a.sh", rng(1, 0, 1, 0), nil); len(got) != 0 {
		t.Fatalf("blank line: %+v", got)
	}
	// Refusal: a syntax error anywhere in the file.
	c.change("a.sh", fullText("echo   one\necho \"abc\n"))
	c.waitPush("a.sh")
	if got := c.rangeFormatting("a.sh", rng(0, 0, 0, 4), nil); len(got) != 0 {
		t.Fatalf("refused range returned edits: %+v", got)
	}
	c.waitLog("syntax")
}

func TestFormattingHonoursDialectSetting(t *testing.T) {
	src := "[[ $a<$b ]]\n"
	root := t.TempDir()
	put(t, filepath.Join(root, "x.sh"), "")
	c := client(t, root)
	c.openText("x.sh", src)
	c.waitPush("x.sh")
	if got := apply(t, src, c.formatting("x.sh", nil)); got != "[[ $a < $b ]]\n" {
		t.Fatalf("default dialect: %q", got)
	}
	c2 := clientInit(t, root, false, map[string]any{"bash": map[string]any{"dialect": "sh"}})
	c2.openText("x.sh", src)
	c2.waitPush("x.sh")
	if got := c2.formatting("x.sh", nil); len(got) != 0 {
		t.Fatalf("bash.dialect = sh: %+v", got)
	}
	c2.waitLog("grammar-dependent")
	if got := c2.rangeFormatting("x.sh", rng(0, 0, 0, 2), nil); len(got) != 0 {
		t.Fatalf("bash.dialect = sh, range: %+v", got)
	}
	// The repo file sets it too.
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[bash]\ndialect = \"sh\"\n")
	c3 := client(t, root)
	c3.openText("x.sh", src)
	c3.waitPush("x.sh")
	if got := c3.formatting("x.sh", nil); len(got) != 0 {
		t.Fatalf(".bash-lsp.toml dialect: %+v", got)
	}
}

func TestFormattingUsesSettingsAndEditorconfig(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	src := "f() {\necho a\n}\n"
	c := client(t, root)
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	if got := apply(t, src, c.formatting("a.sh", nil)); got != "f() {\n\techo a\n}\n" {
		t.Fatalf("tabs by default: %q", got)
	}
	put(t, filepath.Join(root, ".editorconfig"), "[*.sh]\nindent_style = space\nindent_size = 2\n")
	if got := apply(t, src, c.formatting("a.sh", nil)); got != "f() {\n  echo a\n}\n" {
		t.Fatalf(".editorconfig: %q", got)
	}
	put(t, filepath.Join(root, ".editorconfig"), "")
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[format]\nindent = 3\n")
	c2 := client(t, root)
	c2.openText("a.sh", src)
	c2.waitPush("a.sh")
	if got := apply(t, src, c2.formatting("a.sh", nil)); got != "f() {\n   echo a\n}\n" {
		t.Fatalf("[format] indent: %q", got)
	}
}

func TestFormattingNeverRunsOnEdits(t *testing.T) {
	var runs atomic.Int64
	setFormatHook(t, func() { runs.Add(1) })
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "echo   a\n")
	for i := 2; i < 20; i++ {
		c.changeTo("a.sh", i, "echo   a\necho   b\n")
	}
	c.symbols("")
	if n := runs.Load(); n != 0 {
		t.Fatalf("the formatter ran %d times without a formatting request", n)
	}
	c.formatting("a.sh", nil)
	if n := runs.Load(); n != 1 {
		t.Fatalf("the formatter ran %d times for one request", n)
	}
}

func TestFormattingIsOffTheLoop(t *testing.T) {
	g := newGate()
	setFormatHook(t, g.wait)
	t.Cleanup(g.release)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "echo   a\n")
	c.waitPush("a.sh")
	c.sendReq("90", "textDocument/formatting", fmtParams(c, "a.sh", nil))
	g.waitEntered(t)
	// The loop is free: requests are answered and edits are published.
	start := time.Now()
	c.symbols("")
	c.changeTo("a.sh", 2, "echo   a\necho   b\n")
	c.waitPush("a.sh")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the loop was blocked by the formatter for %v", d)
	}
	if c.hasReply("90") {
		t.Fatal("the formatting reply arrived before the formatter finished")
	}
	g.release()
	r := c.waitReply("90")
	if r.Error == nil || r.Error.Code != codeContentModified {
		t.Fatalf("a document that changed while it was formatted: %+v %s", r.Error, r.Result)
	}
}

func TestFormattingHonoursCancellation(t *testing.T) {
	g := newGate()
	setFormatHook(t, g.wait)
	t.Cleanup(g.release)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "echo   a\n")
	c.waitPush("a.sh")
	c.sendReq("91", "textDocument/formatting", fmtParams(c, "a.sh", nil))
	g.waitEntered(t)
	c.cancel("91")
	start := time.Now()
	r := c.waitReply("91")
	if r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("reply to a cancelled formatting request: %+v %s", r.Error, r.Result)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the cancel took %v", d)
	}
	g.release()
	time.Sleep(100 * time.Millisecond)
	if c.hasReply("91") {
		t.Fatal("a second reply to the cancelled request")
	}
	if got := c.formatting("a.sh", nil); len(got) != 1 { // the server is fine, and formats
		t.Fatalf("after the cancelled request: %+v", got)
	}
}

func TestFormattingUnknownAndLooseDocuments(t *testing.T) {
	c := client(t, "") // no root: documents are served alone
	c.openText("loose.sh", "echo   a\n")
	c.waitPush("loose.sh")
	if got := apply(t, "echo   a\n", c.formatting("loose.sh", nil)); got != "echo a\n" {
		t.Fatalf("loose document: %q", got)
	}
	if got := c.formatting("never-opened.sh", nil); len(got) != 0 {
		t.Fatalf("unknown document: %+v", got)
	}
}

func TestFormattingTabSizeIsBounded(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	src := "f() {\necho a\n}\n"
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	got := apply(t, src, c.formatting("a.sh", map[string]any{"options": map[string]any{"tabSize": 1 << 30, "insertSpaces": true}}))
	if len(got) > 200 {
		t.Fatalf("an absurd tabSize produced %d bytes", len(got))
	}
}

// At most two formatter runs go at once; the others wait for a slot.
func TestFormattingConcurrencyIsBounded(t *testing.T) {
	g := newGate()
	var cur, peak atomic.Int64
	setFormatHook(t, func() {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		g.wait()
		cur.Add(-1)
	})
	t.Cleanup(g.release)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, map[string]any{"shellcheck": map[string]any{"enabled": false}})
	c.openText("a.sh", "echo   a\n")
	c.waitPush("a.sh")
	ids := []string{"401", "402", "403", "404", "405"}
	for _, id := range ids {
		c.sendReq(id, "textDocument/formatting", fmtParams(c, "a.sh", nil))
	}
	g.waitEntered(t)
	g.waitEntered(t)
	time.Sleep(300 * time.Millisecond) // time for a third run to start if the bound were missing
	if p := peak.Load(); p != 2 {
		t.Fatalf("%d formatter runs at once, want 2", p)
	}
	g.release()
	for _, id := range ids {
		if r := c.waitReply(id); r.Error != nil || len(parseEdits(t, r.Result)) != 1 {
			t.Fatalf("reply %s: %+v %s", id, r.Error, r.Result)
		}
	}
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak %d after release, want 2", p)
	}
}

// A result that comes back after the request was cancelled is not sent as
// edits: finishFormat answers the cancellation.
func TestFinishFormatAnswersCancelledRequest(t *testing.T) {
	var out bytes.Buffer
	s := newServer(strings.NewReader(""), &out, nil, "test")
	s.loose = newDocsOnly()
	text := []byte("echo   a\n")
	d := &document{uri: "file:///x/a.sh", path: "/x/a.sh", be: s.loose, version: 1, text: text}
	s.docs[d.path] = d
	edits := []TextEdit{{NewText: "echo a\n"}}
	id := json.RawMessage(`7`)
	s.live[idKey(id)] = &liveReq{cancelled: true, done: make(chan struct{})}
	s.finishFormat(d, 1, id, fmtResult{edits: edits})
	if got := out.String(); !strings.Contains(got, `"code":-32800`) || strings.Contains(got, "echo a") {
		t.Fatalf("reply to a cancelled request: %s", got)
	}
	out.Reset()
	id = json.RawMessage(`8`)
	s.live[idKey(id)] = &liveReq{done: make(chan struct{})}
	s.finishFormat(d, 1, id, fmtResult{edits: edits})
	if got := out.String(); !strings.Contains(got, "echo a") || strings.Contains(got, "-32800") {
		t.Fatalf("reply to a live request: %s", got)
	}
}

func TestFormattingClampsEditorconfigIndent(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	put(t, filepath.Join(root, ".editorconfig"), "[*.sh]\nindent_style = space\nindent_size = 10000000\n")
	src := "f() {\necho a\n}\n"
	c := client(t, root)
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	got := apply(t, src, c.formatting("a.sh", nil))
	if got != "f() {\n"+strings.Repeat(" ", 16)+"echo a\n}\n" {
		t.Fatalf("%d bytes: %.80q", len(got), got)
	}
}
