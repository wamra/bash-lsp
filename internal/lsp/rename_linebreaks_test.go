package lsp

import (
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// lspOffset is the byte offset of an LSP position in text, where LF, CRLF and
// a lone CR each end a line and characters count UTF-16 code units.
func lspOffset(t *testing.T, text string, p Position) int {
	t.Helper()
	line, i := 0, 0
	for line < p.Line {
		if i >= len(text) {
			t.Fatalf("line %d is past the end of %q", p.Line, text)
		}
		switch text[i] {
		case '\n':
			line++
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			line++
		}
		i++
	}
	for units := 0; units < p.Character; {
		if i >= len(text) || text[i] == '\n' || text[i] == '\r' {
			t.Fatalf("character %d is past the end of line %d of %q", p.Character, p.Line, text)
		}
		r := []rune(text[i:])[0]
		units += len(utf16.Encode([]rune{r}))
		i += len(string(r))
	}
	return i
}

// applyLSPEdits applies edits given in LSP positions to text.
func applyLSPEdits(t *testing.T, text string, edits []wireEdit) string {
	t.Helper()
	type span struct {
		from, to int
		text     string
	}
	var spans []span
	for _, e := range edits {
		spans = append(spans, span{lspOffset(t, text, e.Range.Start), lspOffset(t, text, e.Range.End), e.NewText})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from > spans[j].from })
	for _, s := range spans {
		text = text[:s.from] + s.text + text[s.to:]
	}
	return text
}

// editsByURI flattens either form of a rename result to the edits of each file.
func editsByURI(r renameResult) map[string][]wireEdit {
	out := map[string][]wireEdit{}
	for u, es := range r.Changes {
		out[u] = append(out[u], es...)
	}
	for _, d := range r.DocumentChanges {
		out[d.TextDocument.URI] = append(out[d.TextDocument.URI], d.Edits...)
	}
	return out
}

var loneCRCases = []struct {
	name string
	src  string
	at   Position // the first foo, in LSP positions
}{
	{"lone CR in a comment", "# a\rb\nfoo=1\necho \"$foo\"\n", Position{2, 1}},
	{"CR at the end of the file", "foo=1\necho \"$foo\"\r", Position{0, 1}},
	{"CR CR LF", "# x\r\r\nfoo=1\necho \"$foo\"\n", Position{2, 1}},
	{"emoji on an earlier line", "# \U0001F600\rb\nfoo=1\necho \"$foo\"\n", Position{2, 1}},
	{"emoji before the name", "x='\U0001F600'; foo=1;\recho \"$foo\"\n", Position{0, 9}},
	{"name after a lone CR", "x=1;\rfoo=1;\recho \"$foo\"\n", Position{1, 0}},
}

// The line table of the wire counts a lone CR as a line break: the ranges of
// highlights, definition and references, applied as edits, rewrite exactly
// the name. (Rename itself refuses these files.)
func TestPositionsOnLoneCRBuffers(t *testing.T) {
	for _, tc := range loneCRCases {
		t.Run(tc.name, func(t *testing.T) {
			c := clientWith(t, clientCfg{root: t.TempDir()})
			c.openText("a.sh", tc.src)
			c.waitDiag("a.sh")
			var edits []wireEdit
			for _, h := range c.highlights("a.sh", tc.at) {
				edits = append(edits, wireEdit{Range: h.Range, NewText: "bar"})
			}
			if got, want := applyLSPEdits(t, tc.src, edits), strings.ReplaceAll(tc.src, "foo", "bar"); got != want {
				t.Fatalf("highlights rewrite %q, want %q", got, want)
			}
			edits = nil
			for _, l := range c.locs("textDocument/references", "a.sh", tc.at, map[string]any{"context": map[string]any{"includeDeclaration": true}}) {
				edits = append(edits, wireEdit{Range: l.Range, NewText: "bar"})
			}
			if got, want := applyLSPEdits(t, tc.src, edits), strings.ReplaceAll(tc.src, "foo", "bar"); got != want {
				t.Fatalf("references rewrite %q, want %q", got, want)
			}
			use := Position{}
			for _, e := range edits { // the last site is the use
				use = e.Range.Start
			}
			defs := c.locs("textDocument/definition", "a.sh", use, nil)
			if len(defs) != 1 {
				t.Fatalf("definition %+v", defs)
			}
			from := lspOffset(t, tc.src, defs[0].Range.Start)
			if to := lspOffset(t, tc.src, defs[0].Range.End); from != strings.Index(tc.src, "foo") || tc.src[from:to] != "foo" {
				t.Fatalf("definition covers %q at %d, want the first foo", tc.src[from:to], from)
			}
		})
	}
}

// Rename and prepareRename refuse a file with a lone CR outside a comment: the
// shell reads the CR as a character, the analysis as a blank.
func TestRenameRefusesLoneCR(t *testing.T) {
	const repro = "foo=1\rbar=2\necho $bar\n"
	c := clientWith(t, renameCaps(true, true, true))
	c.openText("a.sh", repro)
	c.waitDiag("a.sh")
	at := Position{1, 1} // bar, after the lone CR
	r := c.rawCall("textDocument/rename", c.renameParams("a.sh", at, "zzz"))
	wantRefusal(t, r, "unsupported-line-breaks")
	if !strings.Contains(r.Error.Message, "a.sh") {
		t.Fatalf("message %q does not name the file", r.Error.Message)
	}
	wantRefusal(t, c.prepareAt("a.sh", at), "unsupported-line-breaks")
	for _, tc := range loneCRCases {
		if tc.name == "lone CR in a comment" || tc.name == "emoji on an earlier line" {
			// The CR is inside a comment, which ends at LF in the shell and in
			// the lexer alike: the rename goes through.
			c.change("a.sh", fullText(tc.src))
			c.waitDiag("a.sh")
			if res := c.renameAt("a.sh", tc.at, "bar"); len(res.DocumentChanges) != 1 {
				t.Fatalf("%s: %+v", tc.name, res)
			}
			continue
		}
		c.change("a.sh", fullText(tc.src))
		wantRefusal(t, c.rawCall("textDocument/rename", c.renameParams("a.sh", tc.at, "bar")), "unsupported-line-breaks")
		wantRefusal(t, c.prepareAt("a.sh", tc.at), "unsupported-line-breaks")
	}
}

func TestRenameRefusesLoneCRInClosedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib.sh", "x=1;\rfoo() { :; }\n")
	writeFile(t, root, "main.sh", "source ./lib.sh\nfoo\n")
	writeFile(t, root, "solo.sh", "x=1;\rbaz=1\necho \"$baz\"\n")
	writeFile(t, root, "other.sh", "x=1\ry=2\n")
	writeFile(t, root, "clean.sh", "q=1\necho $q\n")
	cfg := renameCaps(true, true, true)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("main.sh")
	c.waitDiag("main.sh")
	// The requested file is clean; the edit would land in the closed lib.sh.
	r := c.rawCall("textDocument/rename", c.renameParams("main.sh", Position{1, 1}, "bar"))
	wantRefusal(t, r, "unsupported-line-breaks")
	if !strings.Contains(r.Error.Message, "lib.sh") {
		t.Fatalf("message %q does not name lib.sh", r.Error.Message)
	}
	wantRefusal(t, c.prepareAt("main.sh", Position{1, 1}), "unsupported-line-breaks")
	// The requested file is closed and has the lone CR.
	wantRefusal(t, c.rawCall("textDocument/rename", c.renameParams("solo.sh", Position{1, 1}, "qux")), "unsupported-line-breaks")
	wantRefusal(t, c.prepareAt("solo.sh", Position{1, 1}), "unsupported-line-breaks")
	// A file with a lone CR that neither receives an edit nor mentions the
	// names does not block a rename elsewhere.
	c.open("clean.sh")
	c.waitDiag("clean.sh")
	if res := c.renameAt("clean.sh", Position{0, 0}, "w"); len(res.DocumentChanges) != 1 {
		t.Fatalf("clean.sh rename: %+v", res)
	}
}
