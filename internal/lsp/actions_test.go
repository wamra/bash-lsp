package lsp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

type testHint struct {
	Position     Position `json:"position"`
	Label        string   `json:"label"`
	Kind         int      `json:"kind"`
	PaddingLeft  bool     `json:"paddingLeft"`
	PaddingRight bool     `json:"paddingRight"`
}

func (c *testClient) inlay(rel string, r map[string]any) []testHint {
	c.t.Helper()
	raw := c.call("textDocument/inlayHint", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "range": r})
	if string(raw) == "null" {
		c.t.Fatal("inlayHint answered null, want a list")
	}
	var out []testHint
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func wholeRange() map[string]any { return rng(0, 0, 100000, 0) }

func u16(s string) int { return len(utf16.Encode([]rune(s))) }

func TestInlayHintCapability(t *testing.T) {
	if got := capabilities()["inlayHintProvider"]; got != true {
		t.Fatalf("inlayHintProvider = %v", got)
	}
	got, _ := json.Marshal(capabilities()["codeActionProvider"])
	if string(got) != `{"codeActionKinds":["quickfix","refactor.rewrite","source.fixAll"]}` {
		t.Fatalf("codeActionProvider = %s", got)
	}
}

func TestHintsSourceAndDefaults(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib/util.sh"), "u() { :; }\n")
	put(t, filepath.Join(root, "cp.sh"), "cp2() { local src=\"$1\" dst=\"$2\"; }\n")
	put(t, filepath.Join(root, "dyn.sh"), "HERE=\"$(dirname \"$0\")\"\nsource \"$HERE/lib/util.sh\"\n")
	c := client(t, root)
	c.open("dyn.sh")
	got := c.inlay("dyn.sh", wholeRange())
	if len(got) != 1 || got[0].Label != "→ lib/util.sh" || got[0].Kind != 0 || !got[0].PaddingLeft ||
		got[0].Position != (Position{1, len(`source "$HERE/lib/util.sh"`)}) {
		t.Fatalf("%+v", got)
	}
	c.openText("cp.sh", "cp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 a b\n")
	if got := c.inlay("cp.sh", wholeRange()); len(got) != 0 {
		t.Fatalf("parameter names are off by default: %+v", got)
	}
}

func TestHintsParameterNamesEnabled(t *testing.T) {
	userConfig(t, "[inlay_hints]\nparameter_names = true\nsource_targets = false\n")
	root := t.TempDir()
	put(t, filepath.Join(root, "lib/util.sh"), "u() { :; }\n")
	c := client(t, root)
	c.openText("a.sh", "cp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 a b\nsource \"${0%/*}/lib/util.sh\"\n")
	got := c.inlay("a.sh", wholeRange())
	if len(got) != 2 || got[0].Label != "src:" || got[1].Label != "dst:" || got[0].Kind != 2 || !got[0].PaddingRight || got[0].PaddingLeft {
		t.Fatalf("%+v", got)
	}
	if got[0].Position != (Position{1, 4}) || got[1].Position != (Position{1, 6}) {
		t.Fatalf("%+v", got)
	}
	// Only the lines asked for.
	if got := c.inlay("a.sh", rng(2, 0, 3, 0)); len(got) != 0 {
		t.Fatalf("range: %+v", got)
	}
	if got := c.inlay("a.sh", rng(1, 5, 1, 7)); len(got) != 1 || got[0].Label != "dst:" {
		t.Fatalf("range: %+v", got)
	}
}

func TestHintsUTF16(t *testing.T) {
	userConfig(t, "[inlay_hints]\nparameter_names = true\n")
	root := t.TempDir()
	put(t, filepath.Join(root, "lib/u.sh"), "u() { :; }\n")
	c := client(t, root)
	line1 := "cp2 \"😀\" 😀b"
	line2 := "x=\"😀\"; source \"${0%/*}/lib/u.sh\""
	c.openText("a.sh", "cp2() { local src=\"$1\" dst=\"$2\"; }\n"+line1+"\n"+line2+"\n")
	got := c.inlay("a.sh", wholeRange())
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	want := []Position{
		{1, u16("cp2 ")},
		{1, u16("cp2 \"😀\" ")},
		{2, u16(line2)},
	}
	for i, h := range got {
		if h.Position != want[i] {
			t.Errorf("hint %d (%s) at %+v, want %+v", i, h.Label, h.Position, want[i])
		}
	}
	// A range that starts after the emoji, in UTF-16 units.
	if got := c.inlay("a.sh", rng(1, u16("cp2 \"😀\" "), 1, u16(line1))); len(got) != 1 || got[0].Label != "dst:" {
		t.Fatalf("range after the emoji: %+v", got)
	}
}

func TestHintsRedacted(t *testing.T) {
	root := t.TempDir()
	secret := "AKIAIOSFODNN7EXAMPLE"
	put(t, filepath.Join(root, "lib", secret+".sh"), "u() { :; }\n")
	c := client(t, root)
	c.openText("a.sh", "source \"${0%/*}/lib/"+secret+".sh\"\n")
	got := c.inlay("a.sh", wholeRange())
	if len(got) != 1 || strings.Contains(got[0].Label, secret) {
		t.Fatalf("%+v", got)
	}
}

func TestInlayHintInvertedRange(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib/u.sh"), "u() { :; }\n")
	c := client(t, root)
	c.openText("a.sh", "source \"${0%/*}/lib/u.sh\"\n")
	if got := c.inlay("a.sh", wholeRange()); len(got) != 1 {
		t.Fatalf("whole range: %+v", got)
	}
	if got := c.inlay("a.sh", rng(1, 0, 0, 0)); len(got) != 0 {
		t.Fatalf("inverted range: %+v", got)
	}
}

func TestInlayHintUnknownDocument(t *testing.T) {
	c := client(t, t.TempDir())
	if got := c.inlay("nope.sh", wholeRange()); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// A request cancelled while it waits for the first build is answered -32800
// and its handler does not run.
func TestInlayHintCancelled(t *testing.T) {
	var n atomic.Int64
	g := newGate()
	setHooks(t, g, nil, countingHook("textDocument/inlayHint", &n))
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\n")
	c := clientWith(t, clientCfg{root: root})
	g.waitEntered(t)
	c.sendReq(`1000`, "workspace/symbol", map[string]any{"query": ""})
	c.sendReq(`7`, "textDocument/inlayHint", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}, "range": wholeRange()})
	c.cancel(`7`)
	c.cancel(`1000`)
	if r := c.waitReply(`1000`); r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("sentinel: %+v", r)
	}
	g.release()
	if r := c.waitReply(`7`); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("reply %+v, want error -32800", r)
	}
	if n.Load() != 0 {
		t.Fatalf("the handler ran %d times", n.Load())
	}
}

var inlayRefreshCap = map[string]any{"inlayHint": map[string]any{"refreshSupport": true}}

func inlayRefreshes(c *testClient) int {
	n := 0
	for _, r := range c.requestsFromServer() {
		if r.Method == "workspace/inlayHint/refresh" {
			n++
		}
	}
	return n
}

func TestInlayRefreshAfterReload(t *testing.T) {
	user := userConfig(t, "")
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientWith(t, clientCfg{root: root, workspaceCap: inlayRefreshCap})
	c.openText("a.sh", "rm $f\n")
	if _, ds := c.waitPush("a.sh"); !hasCode(ds, "WB1401") {
		t.Fatalf("no WB1401 to begin with: %v", codes(ds))
	}
	// A reload that changes nothing about the hints sends no refresh. The
	// findings republished without WB1401 show that the reload happened.
	writeUserConfig(t, user, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.notify("workspace/didChangeConfiguration", map[string]any{})
	if _, ds := c.waitPushWhere("a.sh", func(_ int, ds []Diagnostic) bool { return !hasCode(ds, "WB1401") }); hasCode(ds, "WB1401") {
		t.Fatalf("reload did not happen: %v", codes(ds))
	}
	c.docSymbols("a.sh") // requests are handled in order
	if n := inlayRefreshes(c); n != 0 {
		t.Fatalf("%d refresh requests after an unrelated change", n)
	}
	writeUserConfig(t, user, "[inlay_hints]\nparameter_names = true\n")
	c.notify("workspace/didChangeConfiguration", map[string]any{})
	c.docSymbols("a.sh")
	if n := inlayRefreshes(c); n != 1 {
		t.Fatalf("%d refresh requests after the hints changed, want 1", n)
	}
	for _, r := range c.requestsFromServer() {
		if r.Method == "workspace/inlayHint/refresh" && len(r.Params) != 0 {
			t.Errorf("refresh request carries params %s, want none", r.Params)
		}
	}
}

func TestInlayRefreshNeedsCapability(t *testing.T) {
	user := userConfig(t, "")
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "echo\n")
	writeUserConfig(t, user, "[inlay_hints]\nparameter_names = true\n")
	c.notify("workspace/didChangeConfiguration", map[string]any{})
	c.docSymbols("a.sh")
	if n := inlayRefreshes(c); n != 0 {
		t.Fatalf("%d refresh requests to a client that did not offer them", n)
	}
}

// ---- code actions ----

func actionTitles(as []testAction) string {
	var out []string
	for _, a := range as {
		out = append(out, a.Kind+": "+a.Title)
	}
	return strings.Join(out, " | ")
}

func (c *testClient) openAndSettle(rel, text string) {
	c.t.Helper()
	c.openText(rel, text)
	c.waitPush(rel)
}

func TestAddShebangAction(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib/util.sh"), "")
	c := client(t, root)
	c.openAndSettle("main.sh", "source ./lib/util.sh\n")
	c.openAndSettle("lib/util.sh", "u() { :; }\n")
	as := c.actions("main.sh", rng(0, 0, 0, 0), nil, "quickfix")
	if len(as) != 1 || as[0].Kind != "quickfix" || as[0].IsPreferred == nil || *as[0].IsPreferred {
		t.Fatalf("%s", actionTitles(as))
	}
	es := as[0].Edit.Changes[c.uri("main.sh")]
	if len(es) != 1 || es[0].NewText != "#!/usr/bin/env bash\n" || es[0].Range != (Range{}) {
		t.Fatalf("%+v", es)
	}
	if as := c.actions("lib/util.sh", rng(0, 0, 0, 0), nil); len(as) != 0 {
		t.Fatalf("a sourced file is offered a shebang: %s", actionTitles(as))
	}
	c.openAndSettle("has.sh", "#!/bin/sh\necho\n")
	if as := c.actions("has.sh", rng(0, 0, 0, 0), nil); len(as) != 0 {
		t.Fatalf("%s", actionTitles(as))
	}
}

func TestQuoteVariableAction(t *testing.T) {
	c := client(t, t.TempDir())
	c.openAndSettle("a.sh", "#!/bin/bash\necho 😀 $f\n[[ $f ]]\nx=$f\n")
	col := u16("echo 😀 $")
	as := c.actions("a.sh", rng(1, col, 1, col), nil, "refactor.rewrite")
	if len(as) != 1 || as[0].Kind != "refactor.rewrite" || as[0].IsPreferred == nil || *as[0].IsPreferred {
		t.Fatalf("%s", actionTitles(as))
	}
	es := as[0].Edit.Changes[c.uri("a.sh")]
	want := Range{Position{1, u16("echo 😀 ")}, Position{1, u16("echo 😀 $f")}}
	if len(es) != 1 || es[0].NewText != "\"$f\"" || es[0].Range != want {
		t.Fatalf("%+v, want %+v", es, want)
	}
	for _, p := range []Position{{2, 4}, {3, 3}} {
		if as := c.actions("a.sh", rng(p.Line, p.Character, p.Line, p.Character), nil, "refactor.rewrite"); len(as) != 0 {
			t.Errorf("offered at %+v: %s", p, actionTitles(as))
		}
	}
	// Not asked for: no refactor in a quick-fix request.
	if as := c.actions("a.sh", rng(1, col, 1, col), nil, "quickfix"); strings.Contains(actionTitles(as), "refactor") {
		t.Fatalf("%s", actionTitles(as))
	}
}

func TestCodeActionKindFilter(t *testing.T) {
	c := client(t, t.TempDir())
	c.openAndSettle("a.sh", "rm $f\n")
	all := actionTitles(c.actions("a.sh", rng(0, 4, 0, 4), nil))
	for _, k := range []string{"quickfix: ", "refactor.rewrite: "} {
		if !strings.Contains(all, k) {
			t.Errorf("no %q action in %s", k, all)
		}
	}
	if strings.Contains(all, "source.fixAll") {
		t.Errorf("fixAll offered with nothing safe to fix: %s", all)
	}
	// A parent kind asks for its children.
	if got := actionTitles(c.actions("a.sh", rng(0, 4, 0, 4), nil, "refactor")); !strings.HasPrefix(got, "refactor.rewrite: ") || strings.Contains(got, "quickfix") {
		t.Errorf("only refactor: %s", got)
	}
}

func TestFixAllNotOfferedForWB1401(t *testing.T) {
	c := client(t, t.TempDir())
	c.openAndSettle("a.sh", "#!/bin/bash\nrm $f\n")
	if as := c.actions("a.sh", rng(0, 0, 5, 0), nil, "source.fixAll"); len(as) != 0 {
		t.Fatalf("%s", actionTitles(as))
	}
}

func fixAllFixtures(t *testing.T) (*tree.File, []pubDiag) {
	t.Helper()
	src := []byte("echo 😀 a\necho b\n")
	f := front.Parse("/x.sh", src, front.Detect("/x.sh", src))
	conv := f.NewConverter()
	at := func(off int) Position { l, ch := conv.ToUTF16(f.PosAt(off)); return Position{l, ch} }
	mk := func(code, text string, safe bool, a, b int) pubDiag {
		fx := analysis.Fix{Title: text, Safe: safe, Edits: []analysis.TextEdit{{Range: tree.Range{Start: f.PosAt(a), End: f.PosAt(b)}, NewText: text}}}
		return pubDiag{diag: Diagnostic{Code: code}, fixes: []lspFix{{title: text, safe: safe, src: fx,
			edits: []TextEdit{{Range: Range{at(a), at(b)}, NewText: text}}}}}
	}
	one := strings.Index(string(src), "a\n")
	two := strings.Index(string(src), "b\n")
	return f, []pubDiag{
		mk("X1", "A", true, one, one+1),
		mk("X2", "unsafe", false, 0, 4),
		mk("X3", "overlap", true, one, one+1),
		mk("X4", "B", true, two, two+1),
	}
}

func TestFixAllAction(t *testing.T) {
	f, pub := fixAllFixtures(t)
	a, ok := fixAllAction("file:///x.sh", f, pub, nil)
	if !ok || a.Kind != "source.fixAll" {
		t.Fatalf("%+v %v", a, ok)
	}
	es := a.Edit.Changes["file:///x.sh"]
	if len(es) != 2 || es[0].NewText != "A" || es[1].NewText != "B" || es[0].Range.Start != (Position{0, u16("echo 😀 ")}) {
		t.Fatalf("%+v", es)
	}
	if _, ok := fixAllAction("file:///x.sh", f, pub, []string{"quickfix"}); ok {
		t.Error("offered for a quickfix-only request")
	}
	if _, ok := fixAllAction("file:///x.sh", f, pub, []string{"source"}); !ok {
		t.Error("not offered for the parent kind")
	}
	// Only a finding with no fix, and only an unsafe one: nothing to offer.
	if _, ok := fixAllAction("file:///x.sh", f, []pubDiag{{diag: Diagnostic{Code: "WB1401"}}, pub[1]}, nil); ok {
		t.Error("offered with nothing safe")
	}
}

// An inlay-hint answer cut at the cap says so once per session; one that is
// not cut says nothing.
func TestInlayHintCapSaysOnce(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "big.sh"), "f() { local x=\"$1\"; }\n"+strings.Repeat("f a\n", 5500))
	put(t, filepath.Join(root, "small.sh"), "f() { local x=\"$1\"; }\nf a\n")
	userConfig(t, "[inlay_hints]\nparameter_names = true\n")
	c := client(t, root)
	c.open("small.sh")
	c.open("big.sh")
	if got := c.inlay("small.sh", wholeRange()); len(got) != 1 {
		t.Fatalf("%d hints in the small file", len(got))
	}
	if l := c.logsContaining("inlayHint"); len(l) != 0 {
		t.Fatalf("notice without a cut: %q", l)
	}
	for i := 0; i < 2; i++ {
		if got := c.inlay("big.sh", wholeRange()); len(got) != 5000 {
			t.Fatalf("%d hints, want 5000", len(got))
		}
	}
	c.waitLog("inlayHint")
	if l := c.logsContaining("inlayHint"); len(l) != 1 {
		t.Fatalf("%d notices, want one: %q", len(l), l)
	}
}
