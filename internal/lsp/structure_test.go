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
	tools := t.TempDir()
	interp := filepath.Join(tools, "myinterp")
	writeFile(t, tools, "myinterp", "")
	if err := os.Mkdir(filepath.Join(tools, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(interp, filepath.Join(tools, "alias")); err != nil {
		t.Fatal(err)
	}
	loop := "for f in mods/*.sh; do . \"$f\"; done\n"
	writeFile(t, ws, "a.sh", "#!"+interp+"\n"+loop)
	writeFile(t, ws, "viaalias.sh", "#!"+filepath.Join(tools, "alias")+" -e\n")
	writeFile(t, ws, "missing.sh", "#!"+filepath.Join(tools, "nothing")+"\n")
	writeFile(t, ws, "dir.sh", "#!"+filepath.Join(tools, "adir")+"\n")
	writeFile(t, ws, "env.sh", "#!usr/bin/env bash\n")
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
	ls := links("a.sh")
	if len(ls) != 1 || !strings.HasSuffix(ls[0].Target, "/myinterp") || ls[0].Range != (Range{Position{0, 2}, Position{0, 2 + len(interp)}}) {
		t.Fatalf("a.sh: %+v", ls)
	}
	if ls := links("viaalias.sh"); len(ls) != 1 || !strings.HasSuffix(ls[0].Target, "/alias") {
		t.Fatalf("a symlink to a file: %+v", ls)
	}
	for _, rel := range []string{"missing.sh", "dir.sh", "env.sh"} {
		if ls := links(rel); len(ls) != 0 {
			t.Fatalf("%s: %+v", rel, ls)
		}
	}
}

func TestStructureCapabilities(t *testing.T) {
	caps := capabilities()
	for _, k := range []string{"foldingRangeProvider", "selectionRangeProvider", "documentHighlightProvider"} {
		if caps[k] != true {
			t.Errorf("%s = %v", k, caps[k])
		}
	}
	b, _ := json.Marshal(caps["documentLinkProvider"])
	if string(b) != `{"resolveProvider":false}` {
		t.Errorf("documentLinkProvider = %s", b)
	}
}

// TestFoldingRangeRequestCaps sends the folding capabilities the way a client
// does: the test sets the server's client capabilities after initialize, with
// the same effect as reading them from the initialize request.
func TestFoldingRangeRequestCaps(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.sh", lspFoldSrc)
	for _, tc := range []struct {
		caps clientCaps
		want string
	}{
		{clientCaps{LineFoldingOnly: true}, "(0,2,comment) (3,8,) (4,7,) (5,6,) (10,12,region)"},
		{clientCaps{RangeLimit: 3}, "(0,2,comment) (3,9,) (10,12,region)"},
	} {
		var srv *server
		c := clientWith(t, clientCfg{root: ws, onServer: func(s *server) { srv = s }})
		srv.ccaps = tc.caps // after initialize returned; the next request is sent after this write
		c.open("a.sh")
		raw := c.call("textDocument/foldingRange", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
		var got []foldingRange
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		if foldText(got) != tc.want {
			t.Errorf("%+v: %s, want %s", tc.caps, foldText(got), tc.want)
		}
	}
}

// A file with a shebang and a source: the shebang link comes first.
func TestDocumentLinkShebangFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shebang paths are Unix paths")
	}
	ws := t.TempDir()
	tools := t.TempDir()
	interp := filepath.Join(tools, "myinterp")
	writeFile(t, tools, "myinterp", "")
	writeFile(t, ws, "a.sh", "#!"+interp+"\n. ./lib.sh\n")
	writeFile(t, ws, "lib.sh", "f() { :; }\n")
	c := client(t, ws)
	c.open("a.sh")
	raw := c.call("textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	var ls []documentLink
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if len(ls) != 2 || !strings.HasSuffix(ls[0].Target, "/myinterp") || ls[0].Range.Start.Line != 0 || ls[1].Range.Start.Line != 1 {
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
