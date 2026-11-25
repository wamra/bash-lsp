package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const lspFoldSrc = "# one\n# two\n# three\nf() {\n  if true; then\n    cat <<EOF\nbody\nEOF\n  fi\n}\n# region setup\nx=1\n# endregion\n"

func foldText(fs []foldingRange) string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("(%d,%d,%s)", f.StartLine, f.EndLine, f.Kind))
	}
	return strings.Join(out, " ")
}

var structFolds = []lineFold{{StartLine: 0, EndLine: 2, Kind: "comment"}, {StartLine: 3, EndLine: 9, Kind: ""}, {StartLine: 4, EndLine: 8, Kind: ""}, {StartLine: 5, EndLine: 7, Kind: ""}, {StartLine: 10, EndLine: 12, Kind: "region"}}

func TestFoldingRangesCaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps clientCaps
		want string
	}{
		{"plain", clientCaps{}, "(0,2,comment) (3,9,) (4,8,) (5,7,) (10,12,region)"},
		{"lineFoldingOnly", clientCaps{LineFoldingOnly: true}, "(0,2,comment) (3,8,) (4,7,) (5,6,) (10,12,region)"},
		{"rangeLimit 2", clientCaps{RangeLimit: 2}, "(0,2,comment) (3,9,)"},
		{"rangeLimit 3", clientCaps{RangeLimit: 3}, "(0,2,comment) (3,9,) (10,12,region)"},
		{"rangeLimit 4", clientCaps{RangeLimit: 4}, "(0,2,comment) (3,9,) (4,8,) (10,12,region)"},
		{"rangeLimit above the count", clientCaps{RangeLimit: 9}, "(0,2,comment) (3,9,) (4,8,) (5,7,) (10,12,region)"},
		{"both", clientCaps{LineFoldingOnly: true, RangeLimit: 3}, "(0,2,comment) (3,8,) (10,12,region)"},
	} {
		if got := foldText(foldingRanges(structFolds, tc.caps)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	// a two-line bracket fold has no line left to fold with lineFoldingOnly
	if got := foldingRanges([]lineFold{{StartLine: 0, EndLine: 1, Kind: ""}, {StartLine: 2, EndLine: 3, Kind: "comment"}}, clientCaps{LineFoldingOnly: true}); foldText(got) != "(2,3,comment)" {
		t.Errorf("two-line fold: %s", foldText(got))
	}
}

func TestFoldingRangeRequest(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.sh", lspFoldSrc)
	c := client(t, ws)
	c.open("a.sh")
	raw := c.call("textDocument/foldingRange", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	var got []foldingRange
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if want := "(0,2,comment) (3,9,) (4,8,) (5,7,) (10,12,region)"; foldText(got) != want {
		t.Fatalf("%s, want %s", foldText(got), want)
	}
	if strings.Contains(string(raw), "startCharacter") {
		t.Fatalf("columns on a fold: %s", raw)
	}
}

type selNode struct {
	Range  Range    `json:"range"`
	Parent *selNode `json:"parent"`
}

func (c *testClient) selection(rel string, ps ...Position) []selNode {
	c.t.Helper()
	raw := c.call("textDocument/selectionRange", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "positions": ps})
	var out []selNode
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func TestSelectionRangeRequest(t *testing.T) {
	ws := t.TempDir()
	const src = "f() { echo \"$x\"; }\n"
	writeFile(t, ws, "a.sh", src)
	c := client(t, ws)
	c.open("a.sh")
	got := c.selection("a.sh", c.pos("a.sh", "$x", 1), Position{5, 0})
	if len(got) != 2 {
		t.Fatalf("%d answers for two positions", len(got))
	}
	var texts []string
	for n := &got[0]; n != nil; n = n.Parent {
		r := n.Range
		if r.Start.Line != 0 || r.End.Line > 1 {
			t.Fatalf("range %+v", r)
		}
		line := src
		if r.End.Line == 1 {
			texts = append(texts, line[r.Start.Character:])
		} else {
			texts = append(texts, line[r.Start.Character:r.End.Character])
		}
	}
	want := []string{"x", "$x", "\"$x\"", "echo \"$x\"", "{ echo \"$x\"; }", "f() { echo \"$x\"; }", src}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("chain %q, want %q", texts, want)
	}
	// a position past the end still answers one range
	if got[1].Parent != nil && got[1].Range.Start.Line > 6 {
		t.Fatalf("far position: %+v", got[1])
	}
}

func (c *testClient) highlights(rel string, p Position) []documentHighlight {
	c.t.Helper()
	raw := c.call("textDocument/documentHighlight", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p})
	var out []documentHighlight
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func TestDocumentHighlightRequest(t *testing.T) {
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	var got []string
	for _, h := range c.highlights("main.sh", c.pos("main.sh", `local name="$1"`, 6)) {
		got = append(got, fmt.Sprintf("%d:%d", h.Range.Start.Line+1, h.Kind))
	}
	if want := "5:3 6:2 7:2"; strings.Join(got, " ") != want {
		t.Fatalf("highlights %v, want %s", got, want)
	}
	if hs := c.highlights("main.sh", Position{0, 0}); len(hs) != 0 {
		t.Fatalf("highlights on the shebang: %v", hs)
	}
}

func TestStructureUTF16(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "lib.sh", "g() { :; }\n")
	const src = "x=1\n: \"😀\" \"$x\"\necho \"😀\"; source ./lib.sh\n"
	writeFile(t, ws, "a.sh", src)
	c := client(t, ws)
	c.open("a.sh")
	hs := c.highlights("a.sh", Position{0, 0})
	if len(hs) != 2 || hs[0].Range != (Range{Position{0, 0}, Position{0, 1}}) || hs[0].Kind != 3 ||
		hs[1].Range != (Range{Position{1, 9}, Position{1, 10}}) || hs[1].Kind != 2 {
		t.Fatalf("highlights: %+v", hs)
	}
	raw := c.call("textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	var links []documentLink
	if err := json.Unmarshal(raw, &links); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if len(links) != 1 || links[0].Range != (Range{Position{2, 18}, Position{2, 26}}) || links[0].Target != c.uri("lib.sh") || links[0].Tooltip != "source → lib.sh" {
		t.Fatalf("links: %s", raw)
	}
	sel := c.selection("a.sh", Position{1, 9})
	if sel[0].Range != (Range{Position{1, 9}, Position{1, 10}}) || sel[0].Parent.Range != (Range{Position{1, 8}, Position{1, 10}}) {
		t.Fatalf("selection around the emoji: %+v", sel[0])
	}
}

func TestAC09DocumentLink(t *testing.T) {
	c := client(t, copyCorpus(t))
	c.open("dyn.sh")
	raw := c.call("textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": c.uri("dyn.sh")}})
	var links []documentLink
	if err := json.Unmarshal(raw, &links); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	var got []string
	for _, l := range links {
		got = append(got, fmt.Sprintf("%d:%s", l.Range.Start.Line+1, c.relOf(l.Target)))
	}
	if want := "3:lib/util.sh"; !contains(got, want) {
		t.Fatalf("links %v, want %s among them (%s)", got, want, raw)
	}
	if strings.Contains(string(raw), "resolveProvider") || strings.Contains(string(raw), `"data"`) {
		t.Fatalf("links carry resolve data: %s", raw)
	}
}

func TestDocumentLinkShebangAndGlob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shebang paths are Unix paths")
	}
	ws := t.TempDir()
	outside := t.TempDir() // exists, but is not a directory of the PATH profile
	writeFile(t, outside, "myinterp", "")
	loop := "for f in mods/*.sh; do . \"$f\"; done\n"
	// testBin is the profile's directory: a name in it is linked whether or
	// not the file is there, because nothing is stat'ed
	writeFile(t, ws, "a.sh", "#!"+testBin+"/ls\n"+loop)
	writeFile(t, ws, "viaargs.sh", "#!"+testBin+"/cat -e\n")
	writeFile(t, ws, "missing.sh", "#!"+testBin+"/nothing\n")
	writeFile(t, ws, "outside.sh", "#!"+outside+"/myinterp\n")
	writeFile(t, ws, "env.sh", "#!usr/bin/env bash\n")
	writeFile(t, ws, "usrbin.sh", "#!/usr/bin/env bash\n")
	writeFile(t, ws, "unc.sh", "#!//host/share/bash\n")
	writeFile(t, ws, "uncback.sh", "#!\\\\host\\share\\bash\n")
	writeFile(t, ws, "volume.sh", "#!C:/Git/bin/bash\n")
	writeFile(t, ws, "dots.sh", "#!"+testBin+"/../x/sh\n")
	if err := os.MkdirAll(filepath.Join(ws, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, ws, "mods/m1.sh", "m1() { :; }\n")
	writeFile(t, ws, "mods/m2.sh", "m2() { :; }\n")
	c := client(t, ws)
	links := func(rel string) []documentLink {
		c.open(rel)
		raw := c.call("textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}})
		var out []documentLink
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		return out
	}
	// the glob loader gives no link; the shebang gives one, first
	interp := testBin + "/ls"
	ls := links("a.sh")
	if len(ls) != 1 || !strings.HasSuffix(ls[0].Target, "/ls") || ls[0].Range != (Range{Position{0, 2}, Position{0, 2 + len(interp)}}) {
		t.Fatalf("a.sh: %+v", ls)
	}
	if ls := links("viaargs.sh"); len(ls) != 1 || !strings.HasSuffix(ls[0].Target, "/cat") {
		t.Fatalf("viaargs.sh: %+v", ls)
	}
	if ls := links("missing.sh"); len(ls) != 1 || !strings.HasSuffix(ls[0].Target, "/nothing") {
		t.Fatalf("a name in a profile directory is linked without a stat: %+v", ls)
	}
	for _, rel := range []string{"outside.sh", "env.sh", "usrbin.sh", "unc.sh", "uncback.sh", "volume.sh", "dots.sh"} {
		if ls := links(rel); len(ls) != 0 {
			t.Fatalf("%s: %+v", rel, ls)
		}
	}
}

func TestDocumentLinkShebangFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shebang paths are Unix paths")
	}
	ws := t.TempDir()
	writeFile(t, ws, "a.sh", "#!"+testBin+"/ls\n. ./lib.sh\n")
	writeFile(t, ws, "lib.sh", "f() { :; }\n")
	c := client(t, ws)
	c.open("a.sh")
	raw := c.call("textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	var ls []documentLink
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if len(ls) != 2 || !strings.HasSuffix(ls[0].Target, "/ls") || ls[0].Range.Start.Line != 0 || ls[1].Range.Start.Line != 1 {
		t.Fatalf("links %s, want the shebang first, then the source", raw)
	}
}

// Folds are in LSP lines: a lone CR ends a line, so every fold after one
// starts and ends a line later than the line numbers of the tools say.
func TestFoldingRangeCountsLoneCRLines(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"function", "# a\rb\nf() {\n  echo hi\n  echo ho\n}\n", "(0,1,comment) (2,5,)"},
		{"function after a lone CR", "x=1;\rf() {\n  echo hi\n  echo ho\n}\n", "(1,4,)"},
		{"comment run and region", "x=1\r# c1\n# c2\n# region r\nf() {\n  echo hi\n  echo ho\n}\n# endregion\n", "(1,2,comment) (3,8,region) (4,7,)"},
		{"lone CR between comments", "# c1\n\r# c2\n", ""},
		{"one tool line, two LSP lines", "f() { :;\r}\n", "(0,1,)"},
		{"CRLF and lone CR", "a=1\r\nb=2;\rf() {\r\n  :\r}\r\n", "(2,4,)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, t.TempDir())
			c.openText("a.sh", tc.src)
			c.waitDiag("a.sh")
			raw := c.call("textDocument/foldingRange", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
			var got []foldingRange
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			if foldText(got) != tc.want {
				t.Fatalf("%s, want %s", foldText(got), tc.want)
			}
		})
	}
}

// documentHighlight and documentLink answer at most 5,000 entries and say so
// once per session, however many requests are cut.
func TestHighlightAndLinkAreCappedAndSayOnce(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.sh", "f() { :; }\n"+strings.Repeat("f\n", 5500))
	writeFile(t, ws, "b.sh", strings.Repeat(". ./a.sh\n", 5500))
	c := client(t, ws)
	c.open("a.sh")
	c.open("b.sh")
	for i := 0; i < 2; i++ {
		if hs := c.highlights("a.sh", Position{0, 0}); len(hs) != 5000 {
			t.Fatalf("%d highlights, want 5000", len(hs))
		}
		raw := c.call("textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": c.uri("b.sh")}})
		var ls []documentLink
		if err := json.Unmarshal(raw, &ls); err != nil || len(ls) != 5000 {
			t.Fatalf("%d links (%v), want 5000", len(ls), err)
		}
	}
	c.waitLog("documentHighlight")
	c.waitLog("documentLink")
	if n := len(c.logsContaining("documentHighlight")) + len(c.logsContaining("documentLink")); n != 2 {
		t.Fatalf("%d notices, want one each: %q", n, c.logMsgs)
	}
	// a cut-free answer says nothing
	c2 := client(t, copyCorpus(t))
	c2.open("main.sh")
	c2.highlights("main.sh", c2.pos("main.sh", `local name="$1"`, 6))
	if l := c2.logsContaining("documentHighlight"); len(l) != 0 {
		t.Fatalf("notice without a cut: %q", l)
	}
}

// A request with more positions than maxSelectionPositions answers the first
// maxSelectionPositions and an empty range at each position after them, so
// the answer still has one entry per position, and says so once.
func TestSelectionRangeCapsThePositions(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.sh", "f() { echo \"$x\"; }\n")
	c := client(t, ws)
	c.open("a.sh")
	n := maxSelectionPositions + 250
	ps := make([]Position, n)
	for i := range ps {
		ps[i] = c.pos("a.sh", "$x", 1)
		if i >= maxSelectionPositions {
			ps[i].Character = 2 + i - maxSelectionPositions // distinct positions after the cap
		}
	}
	for i := 0; i < 2; i++ {
		got := c.selection("a.sh", ps...)
		if len(got) != n {
			t.Fatalf("%d answers for %d positions", len(got), n)
		}
		for i, g := range got {
			switch {
			case i < maxSelectionPositions && g.Parent == nil:
				t.Fatalf("position %d, inside the cap, has no chain", i)
			case i >= maxSelectionPositions && (g.Parent != nil || g.Range.Start != ps[i] || g.Range.End != ps[i]):
				t.Fatalf("position %d, past the cap: %+v, want an empty range at %+v", i, g, ps[i])
			}
		}
	}
	c.waitLog("selectionRange")
	if l := c.logsContaining("selectionRange"); len(l) != 1 {
		t.Fatalf("%d notices, want one: %q", len(l), l)
	}
}

// A request cancelled while it works stops between positions and answers
// RequestCancelled.
func TestSelectionRangeStopsWhenCancelled(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.sh", "f() { echo \"$x\"; }\n")
	var srv *server
	c := clientWith(t, clientCfg{root: ws, onServer: func(s *server) { srv = s }})
	c.open("a.sh")
	c.selection("a.sh", Position{0, 0}) // the first build is done
	params, err := json.Marshal(selectionRangeParams{
		TextDocument: textDocumentIdentifier{URI: c.uri("a.sh")},
		Positions:    []Position{{0, 1}, {0, 2}, {0, 3}}})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res  any
		rerr *rpcError
	}
	done := make(chan outcome, 1)
	srv.post(func() {
		lr := &liveReq{done: make(chan struct{})}
		close(lr.done)
		srv.cur = lr
		res, rerr := srv.selectionRange(params)
		srv.cur = nil
		done <- outcome{res, rerr}
	})
	o := <-done
	if o.rerr == nil || o.rerr.Code != codeRequestCancelled {
		t.Fatalf("answer %v, error %+v, want RequestCancelled", o.res, o.rerr)
	}
}
