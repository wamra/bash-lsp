package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Rename and prepareRename on open buffers whose line breaks include a lone
// CR: the edits, applied in LSP positions, give the renamed text.
func TestRenameOnLoneCRBuffers(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		at   Position
	}{
		{"lone CR in a comment", "# a\rb\nfoo=1\necho \"$foo\"\n", Position{2, 1}},
		{"CR at the end of the file", "foo=1\necho \"$foo\"\r", Position{0, 1}},
		{"CR CR LF", "# x\r\r\nfoo=1\necho \"$foo\"\n", Position{2, 1}},
		{"emoji on an earlier line", "# \U0001F600\rb\nfoo=1\necho \"$foo\"\n", Position{2, 1}},
		{"emoji before the name", "x='\U0001F600'; foo=1;\recho \"$foo\"\n", Position{0, 9}},
		{"name after a lone CR", "x=1\rfoo=1;\recho \"$foo\"\n", Position{1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientWith(t, renameCaps(true, true, true))
			c.openText("a.sh", tc.src)
			c.waitDiag("a.sh")
			pr := c.prepareAt("a.sh", tc.at)
			var prep renamePrepared
			if pr.Error != nil || json.Unmarshal(pr.Result, &prep) != nil {
				t.Fatalf("prepareRename: %+v %s", pr.Error, pr.Result)
			}
			from, to := lspOffset(t, tc.src, prep.Range.Start), lspOffset(t, tc.src, prep.Range.End)
			if got := tc.src[from:to]; got != "foo" || prep.Placeholder != "foo" {
				t.Fatalf("prepareRename range covers %q, placeholder %q", got, prep.Placeholder)
			}
			res := c.renameAt("a.sh", tc.at, "bar")
			got := applyLSPEdits(t, tc.src, editsByURI(res)[c.uri("a.sh")])
			if want := strings.ReplaceAll(tc.src, "foo", "bar"); got != want {
				t.Fatalf("renamed text %q, want %q", got, want)
			}
		})
	}
}

// Closed files with a lone CR, as the requested file and as the target of an
// edit made from another file.
func TestRenameOnLoneCRFilesReadFromDisk(t *testing.T) {
	root := t.TempDir()
	lib := "# a\rb\nfoo() { :; }\n"
	main := "source ./lib.sh\nfoo\n"
	solo := "# \U0001F600\rb\nbaz=1;\recho \"$baz\"\r"
	writeFile(t, root, "lib.sh", lib)
	writeFile(t, root, "main.sh", main)
	writeFile(t, root, "solo.sh", solo)
	cfg := renameCaps(true, true, true)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("main.sh")
	c.waitDiag("main.sh")

	if r := c.prepareAt("main.sh", Position{1, 1}); r.Error != nil {
		t.Fatalf("prepareRename: %+v", r.Error)
	}
	by := editsByURI(c.renameAt("main.sh", Position{1, 1}, "bar"))
	if got, want := applyLSPEdits(t, lib, by[c.uri("lib.sh")]), strings.ReplaceAll(lib, "foo", "bar"); got != want {
		t.Fatalf("lib.sh %q, want %q", got, want)
	}
	if got, want := applyLSPEdits(t, main, by[c.uri("main.sh")]), strings.ReplaceAll(main, "foo", "bar"); got != want {
		t.Fatalf("main.sh %q, want %q", got, want)
	}

	// The requested file is closed.
	if r := c.prepareAt("solo.sh", Position{2, 1}); r.Error != nil {
		t.Fatalf("prepareRename on a closed file: %+v", r.Error)
	}
	by = editsByURI(c.renameAt("solo.sh", Position{2, 1}, "qux"))
	if got, want := applyLSPEdits(t, solo, by[c.uri("solo.sh")]), strings.ReplaceAll(solo, "baz", "qux"); got != want {
		t.Fatalf("solo.sh %q, want %q", got, want)
	}
	for _, name := range []string{"lib.sh", "solo.sh"} { // nothing was written
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || (name == "lib.sh" && string(b) != lib) || (name == "solo.sh" && string(b) != solo) {
			t.Fatalf("%s changed on disk: %q %v", name, b, err)
		}
	}
}
