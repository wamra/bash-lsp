package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

func (c *testClient) prepareAt(rel string, p Position) *rpcReply {
	return c.rawCall("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p})
}

func wantRefusal(t *testing.T, r *rpcReply, code string) {
	t.Helper()
	if r.Error == nil || r.Error.Code != -32803 || refusalCode(r) != code || !strings.HasPrefix(r.Error.Message, code+":") || len(r.Result) != 0 {
		t.Fatalf("reply %+v data %s result %s, want refusal %s", r.Error, r.ErrData, r.Result, code)
	}
}

// ---- CRLF ----

func TestRenameWorksOnCRLFFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib.sh", "# a\r\nfoo() { :; }\r\n")
	writeFile(t, root, "main.sh", "source ./lib.sh\r\nfoo\r\n")
	cfg := renameCaps(true, false, false)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("main.sh")
	c.waitDiag("main.sh")
	if r := c.prepareAt("main.sh", Position{1, 1}); r.Error != nil {
		t.Fatalf("prepare: %+v", r.Error)
	}
	res := c.renameAt("main.sh", Position{1, 1}, "bar")
	if got := strings.Join(c.editLines(res), " "); got != "lib.sh:2 main.sh:2" {
		t.Fatalf("edits %s", got)
	}
}

// ---- stale texts ----

func TestRenameRefusesAClosedFileChangedOnDisk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.sh", "source ./b.sh\nfoo\n")
	writeFile(t, root, "b.sh", "foo() { :; }\n")
	cfg := renameCaps(true, true, false)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("a.sh")
	c.waitDiag("a.sh")
	if res := c.renameAt("a.sh", Position{1, 1}, "bar"); len(res.DocumentChanges) != 2 {
		t.Fatalf("before the change: %+v", res)
	}
	// No didChangeWatchedFiles: the snapshot is stale.
	for name, text := range map[string]string{"size": "foo() { :; }\n# x\n", "same size": "foo() { x; }\n"} {
		writeFile(t, root, "b.sh", text)
		r := c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{1, 1}, "bar"))
		if r.Error == nil || r.Error.Code != codeContentModified {
			t.Fatalf("%s: %+v %s", name, r.Error, r.Result)
		}
	}
	if err := os.Remove(filepath.Join(root, "b.sh")); err != nil {
		t.Fatal(err)
	}
	r := c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{1, 1}, "bar"))
	if r.Error == nil || r.Error.Code != codeContentModified {
		t.Fatalf("deleted: %+v %s", r.Error, r.Result)
	}
}

func TestRenameChecksEveryOpenDocumentOfThePlan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.sh", "source ./b.sh\nfoo\n")
	writeFile(t, root, "b.sh", "foo() { :; }\n")
	var srv *server
	var target atomic.Value
	target.Store("")
	var undo func() // on the loop: puts the previous request's edit back
	setHooks(t, nil, nil, func(m string) {
		if m != "textDocument/rename" {
			return
		}
		if undo != nil {
			undo()
			undo = nil
		}
		for path, d := range srv.docs {
			if filepath.Base(path) == target.Load().(string) {
				orig := d.text
				d.text = append([]byte("# edited\n"), orig...)
				undo = func() { d.text = orig }
			}
		}
	})
	cfg := renameCaps(true, true, false)
	cfg.root = root
	cfg.onServer = func(s *server) { srv = s }
	c := clientWith(t, cfg)
	c.open("a.sh")
	c.open("b.sh")
	c.waitDiag("a.sh")
	c.waitDiag("b.sh")
	for _, which := range []string{"a.sh", "b.sh"} {
		target.Store(which)
		r := c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{1, 1}, "bar"))
		if r.Error == nil || r.Error.Code != codeContentModified {
			t.Fatalf("%s out of step: %+v %s", which, r.Error, r.Result)
		}
	}
}

func TestRenameAnswerCarriesTheVersionItWasBuiltFrom(t *testing.T) {
	root := oneFileRoot(t, "a.sh", "foo() { :; }\nfoo\n")
	cfg := renameCaps(true, true, false)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("a.sh")
	c.change("a.sh", rangedEdit(0, 0, 0, 0, "")) // version 2
	c.sendReq(`500`, "textDocument/rename", c.renameParams("a.sh", Position{0, 1}, "bar"))
	// Sent behind the request: a later edit, version 3.
	c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": c.uri("a.sh"), "version": 3},
		"contentChanges": []any{rangedEdit(0, 0, 0, 0, "# x\n")}})
	r := c.waitReply(`500`)
	var res renameResult
	if r.Error != nil || json.Unmarshal(r.Result, &res) != nil || len(res.DocumentChanges) != 1 {
		t.Fatalf("reply %+v %s", r.Error, r.Result)
	}
	if v := res.DocumentChanges[0].TextDocument.Version; v == nil || *v != 2 {
		t.Fatalf("version %v, want 2 (the text the edits were computed for)", v)
	}
}

// ---- the probe ----

func TestPrepareRenameRefusesASkippedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "m.sh", "source ./big.sh\nf() { :; }\nf\n")
	writeFile(t, root, "big.sh", "# "+strings.Repeat("x", analysis.MaxFileBytes+1)+"\n")
	cfg := renameCaps(true, false, false)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("m.sh")
	c.waitDiag("m.sh")
	wantRefusal(t, c.rawCall("textDocument/rename", c.renameParams("m.sh", Position{1, 0}, "g")), "skipped-file")
	wantRefusal(t, c.prepareAt("m.sh", Position{1, 0}), "skipped-file")
}

// A workspace that uses the probe's own name can hide a name-independent
// refusal from prepareRename; rename still refuses.
func TestPrepareRenameProbeNameCollisionIsDocumented(t *testing.T) {
	src := "f() { :; }\nf\nbash_lsp_rename_probe() { :; }\ncat <<EOF\nf\n"
	c := clientWith(t, renameCaps(true, false, false))
	c.openText("a.sh", src)
	c.waitDiag("a.sh")
	wantRefusal(t, c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{1, 0}, "g")), "recovered-region")
	r := c.prepareAt("a.sh", Position{1, 0})
	t.Logf("prepareRename with the probe name in use: error %+v", r.Error) // either outcome is allowed; rename decides
}

func TestPrepareRenameRefusalCarriesTheSites(t *testing.T) {
	c := clientWith(t, renameCaps(true, false, false))
	c.openText("a.sh", "f() { :; }\nf\nunset -f f\n")
	c.waitDiag("a.sh")
	r := c.prepareAt("a.sh", Position{1, 0})
	wantRefusal(t, r, "candidate-site")
	var d struct {
		Sites []Location `json:"sites"`
	}
	if err := json.Unmarshal(r.ErrData, &d); err != nil || len(d.Sites) != 1 || d.Sites[0].URI != c.uri("a.sh") ||
		d.Sites[0].Range != (Range{Position{1, 0}, Position{1, 1}}) {
		t.Fatalf("data %s", r.ErrData)
	}
}

func TestPrepareRenamePlaceholderIsRedacted(t *testing.T) {
	c := clientWith(t, renameCaps(true, false, false))
	c.openText("a.sh", "AKIAIOSFODNN7EXAMPLE() { :; }\nAKIAIOSFODNN7EXAMPLE\n")
	c.waitDiag("a.sh")
	r := c.prepareAt("a.sh", Position{0, 2})
	var got renamePrepared
	if r.Error != nil || json.Unmarshal(r.Result, &got) != nil || got.Placeholder != "[redacted]" {
		t.Fatalf("reply %+v %s", r.Error, r.Result)
	}
}

// ---- the note ----

func noteServer() *server { return newServer(strings.NewReader(""), nil, nil, "test") }

func exclusion(path string, line int, text string) analysis.RenameExclusion {
	return analysis.RenameExclusion{Loc: analysis.Location{Path: path, Range: tree.Range{Start: tree.Pos{Line: line}}},
		Res: analysis.Candidate, Reason: "string-eval", Text: text}
}

func TestRenameNoteRedactsOldNameAndWholeNote(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	s := noteServer()
	// plan.Old, a path and an other's path all carry the secret.
	plan := &analysis.RenamePlan{Old: secret,
		Excluded: []analysis.RenameExclusion{exclusion("/w/"+secret+".sh", 3, "x")},
		Others:   []analysis.Location{{Path: "/w/" + secret + "/o.sh", Range: tree.Range{Start: tree.Pos{Line: 2}}}}}
	note, label := s.renameNote(nil, plan)
	if strings.Contains(note, secret) || strings.Contains(label, secret) || !strings.Contains(note, "[redacted]") {
		t.Fatalf("note %q label %q", note, label)
	}
}

func TestRenameNoteCaps(t *testing.T) {
	s := noteServer()
	var ex []analysis.RenameExclusion
	for i := 0; i < 120; i++ {
		ex = append(ex, exclusion("/w/a.sh", i+1, "f"))
	}
	note, label := s.renameNote(nil, &analysis.RenamePlan{Old: "f", Excluded: ex})
	if label != "120 sites left unchanged" || !strings.Contains(note, "and 70 more") || strings.Count(note, "a.sh:") != maxRenameListed {
		t.Fatalf("label %q note %q", label, note)
	}
	// Long excerpts: the whole note stays under the bound, on a rune boundary.
	long := strings.Repeat("é", 150)
	var ex2 []analysis.RenameExclusion
	for i := 0; i < 50; i++ {
		ex2 = append(ex2, exclusion("/w/"+strings.Repeat("d", 60)+"/a.sh", i+1, long))
	}
	note, _ = s.renameNote(nil, &analysis.RenamePlan{Old: "f", Excluded: ex2, Warnings: []string{strings.Repeat("w", 5000)}})
	if len(note) > maxRenameNote || !utf8.ValidString(note) || !strings.Contains(note, " more") {
		t.Fatalf("note of %d bytes, valid=%v", len(note), utf8.ValidString(note))
	}
	_, l := s.renameNote(nil, &analysis.RenamePlan{Old: "f", Excluded: ex[:1]})
	if l != "1 site left unchanged" {
		t.Fatalf("label %q", l)
	}
	_, l = s.renameNote(nil, &analysis.RenamePlan{Old: "f", Others: []analysis.Location{{Path: "/w/o.sh"}}})
	if l != "1 other symbol named f not renamed" {
		t.Fatalf("others label %q", l)
	}
}

// A rename with only same-named other symbols is annotated and needs
// confirmation, as specified.
func TestRenameOthersOnlyNeedsConfirmation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.sh", "f() { :; }\nf\n")
	writeFile(t, root, "b.sh", "f() { :; }\nf\n")
	cfg := renameCaps(true, true, true)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("a.sh")
	c.waitDiag("a.sh")
	res := c.renameAt("a.sh", Position{0, 0}, "g")
	a, ok := res.ChangeAnnotations["bash-lsp.unrenamed"]
	if !ok || !a.NeedsConfirmation || !strings.Contains(a.Description, "b.sh:1") || strings.Contains(a.Label, "sites left") {
		t.Fatalf("annotations %+v", res.ChangeAnnotations)
	}
}

// ---- cancellation inside the handler ----

func TestRenameCancelledDuringTheHandler(t *testing.T) {
	var srv *server
	var armed atomic.Bool
	setHooks(t, nil, nil, func(m string) { // on the loop, after the request started
		if armed.Load() && (m == "textDocument/rename" || m == "textDocument/prepareRename") {
			srv.liveMu.Lock()
			srv.live[idKey(srv.curID)].cancelled = true
			srv.liveMu.Unlock()
		}
	})
	root := oneFileRoot(t, "a.sh", "f() { :; }\nf\neval \"f x\"\n")
	cfg := renameCaps(true, false, false)
	cfg.root = root
	cfg.onServer = func(s *server) { srv = s }
	c := clientWith(t, cfg)
	c.open("a.sh")
	c.waitDiag("a.sh")
	armed.Store(true)
	// An exclusion would show a message; a refusal would answer -32803.
	r := c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{0, 0}, "g"))
	if r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("rename: %+v %s", r.Error, r.Result)
	}
	if len(c.shown) != 0 {
		t.Fatalf("a cancelled rename showed %s", c.shown)
	}
	r = c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{0, 0}, "if"))
	if r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("refusal: %+v data %s", r.Error, r.ErrData)
	}
	r = c.prepareAt("a.sh", Position{2, 1}) // not a symbol: a refusal
	if r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("prepare refusal: %+v", r.Error)
	}
}

// ---- URIs ----

func TestRenameURIsWithSpaceHashAndPercent(t *testing.T) {
	root := t.TempDir()
	lib, main := "x y#z%41.sh", "m a#in%20.sh"
	writeFile(t, root, lib, "foo() { :; }\n")
	writeFile(t, root, main, "source './"+lib+"'\nfoo\n")
	cfg := renameCaps(true, true, false)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open(main)
	c.waitDiag(main)
	res := c.renameAt(main, Position{1, 1}, "bar")
	var got []string
	for _, d := range res.DocumentChanges {
		got = append(got, d.TextDocument.URI)
	}
	want := []string{c.uri(main), c.uri(lib)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("uris %v, want %v", got, want)
	}
}
