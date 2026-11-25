package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

// The structures of a rename as a client decodes them (LSP 3.17).
type renamePrepared struct {
	Range       Range  `json:"range"`
	Placeholder string `json:"placeholder"`
}

type wireEdit struct {
	Range        Range  `json:"range"`
	NewText      string `json:"newText"`
	AnnotationID string `json:"annotationId"`
}

type wireDocEdit struct {
	TextDocument struct {
		URI     string `json:"uri"`
		Version *int   `json:"version"`
	} `json:"textDocument"`
	Edits []wireEdit `json:"edits"`
}

type wireAnnotation struct {
	Label             string `json:"label"`
	NeedsConfirmation bool   `json:"needsConfirmation"`
	Description       string `json:"description"`
}

type renameResult struct {
	Changes           map[string][]wireEdit     `json:"changes"`
	DocumentChanges   []wireDocEdit             `json:"documentChanges"`
	ChangeAnnotations map[string]wireAnnotation `json:"changeAnnotations"`
}

func renameCaps(prepare, docChanges, annotations bool) clientCfg {
	ws := map[string]any{}
	if docChanges || annotations {
		we := map[string]any{"documentChanges": docChanges}
		if annotations {
			we["changeAnnotationSupport"] = map[string]any{"groupsOnLabel": true}
		}
		ws["workspaceEdit"] = we
	}
	return clientCfg{workspaceCap: ws, documentCap: map[string]any{"rename": map[string]any{"prepareSupport": prepare}}}
}

func renameClient(t *testing.T, prepare, docChanges, annotations bool) *testClient {
	t.Helper()
	cfg := renameCaps(prepare, docChanges, annotations)
	cfg.root = copyCorpus(t)
	return clientWith(t, cfg)
}

// refusalCode is the stable reason code in the error's data.
func refusalCode(r *rpcReply) string {
	var d struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.ErrData, &d)
	return d.Code
}

func (c *testClient) renameParams(rel string, p Position, newName string) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p, "newName": newName}
}

func (c *testClient) renameAt(rel string, p Position, newName string) renameResult {
	c.t.Helper()
	raw := c.call("textDocument/rename", c.renameParams(rel, p, newName))
	var out renameResult
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return out
}

// editLines lists "rel:line" (1-based) of every edit, in the order given.
func (c *testClient) editLines(r renameResult) []string {
	c.t.Helper()
	var out []string
	add := func(uri string, es []wireEdit) {
		for _, e := range es {
			out = append(out, fmt.Sprintf("%s:%d", c.relOf(uri), e.Range.Start.Line+1))
		}
	}
	if len(r.DocumentChanges) > 0 {
		for _, d := range r.DocumentChanges {
			add(d.TextDocument.URI, d.Edits)
		}
		return out
	}
	for u, es := range r.Changes {
		add(u, es)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// rangeText is the text of the range in the file (UTF-16 columns).
func (c *testClient) rangeText(rel string, r Range) string {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.root, rel))
	if err != nil {
		c.t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	line := []rune(lines[r.Start.Line])
	col, out := 0, []rune{}
	for _, ch := range line {
		if col >= r.Start.Character && col < r.End.Character {
			out = append(out, ch)
		}
		col += u16(string(ch))
	}
	return string(out)
}

func TestPrepareRenameRange(t *testing.T) {
	c := renameClient(t, true, false, false)
	c.open("main.sh")
	p := c.pos("main.sh", `local name="$1"`, 6)
	raw := c.call("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": p})
	var got renamePrepared
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if got.Placeholder != "name" {
		t.Fatalf("placeholder %q: %s", got.Placeholder, raw)
	}
	if txt := c.rangeText("main.sh", got.Range); txt != "name" {
		t.Fatalf("range %+v covers %q", got.Range, txt)
	}
	if got.Range.Start.Line != 4 {
		t.Fatalf("range %+v", got.Range)
	}
}

func TestPrepareRenameWithoutPrepareSupportIsARange(t *testing.T) {
	c := renameClient(t, false, false, false)
	c.open("main.sh")
	p := c.pos("main.sh", `local name="$1"`, 6)
	raw := c.call("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": p})
	var got Range
	if err := json.Unmarshal(raw, &got); err != nil || strings.Contains(string(raw), "placeholder") {
		t.Fatalf("%v: %s", err, raw)
	}
	if txt := c.rangeText("main.sh", got); txt != "name" {
		t.Fatalf("range %+v covers %q", got, txt)
	}
}

func TestPrepareRenameRefusedOnBuiltin(t *testing.T) {
	c := renameClient(t, true, false, false)
	c.open("main.sh")
	p := c.pos("main.sh", `local name="$1"`, 1) // inside the builtin `local`
	r := c.rawCall("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": c.uri("main.sh")}, "position": p})
	if r.Error == nil || r.Error.Code != -32803 {
		t.Fatalf("reply %+v %s, want error -32803", r.Error, r.Result)
	}
	if !strings.HasPrefix(r.Error.Message, "not-a-symbol:") {
		t.Fatalf("message %q", r.Error.Message)
	}
	if got := refusalCode(r); got != "not-a-symbol" {
		t.Fatalf("data %s", r.ErrData)
	}
}

func TestRenameChangesC24(t *testing.T) {
	c := renameClient(t, true, false, false)
	c.open("main.sh")
	res := c.renameAt("main.sh", c.pos("main.sh", `local name="$1"`, 6), "nm")
	if len(res.DocumentChanges) != 0 || len(res.ChangeAnnotations) != 0 {
		t.Fatalf("documentChanges %v annotations %v without client support", res.DocumentChanges, res.ChangeAnnotations)
	}
	want := []string{"main.sh:5", "main.sh:6", "main.sh:7"}
	if got := c.editLines(res); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("edits %v, want %v", got, want)
	}
	for _, es := range res.Changes {
		for _, e := range es {
			if e.NewText != "nm" || c.rangeText("main.sh", e.Range) != "name" || e.AnnotationID != "" {
				t.Fatalf("edit %+v covers %q", e, c.rangeText("main.sh", e.Range))
			}
		}
	}
	if len(c.shown) != 0 {
		t.Fatalf("message shown: %s", c.shown)
	}
}

func TestRenameDocumentChangesVersioned(t *testing.T) {
	c := renameClient(t, true, true, false)
	c.open("main.sh")
	// A change that keeps the layout: the version moves to 2.
	c.change("main.sh", rangedEdit(0, 0, 0, 0, ""))
	res := c.renameAt("main.sh", c.pos("main.sh", `build_release "$name"`, 2), "ship")
	if len(res.Changes) != 0 {
		t.Fatalf("changes %v next to documentChanges", res.Changes)
	}
	var open, closed int
	for _, d := range res.DocumentChanges {
		switch c.relOf(d.TextDocument.URI) {
		case "main.sh":
			if d.TextDocument.Version == nil || *d.TextDocument.Version != 2 {
				t.Fatalf("main.sh version %v, want 2", d.TextDocument.Version)
			}
			open++
		default:
			if d.TextDocument.Version != nil {
				t.Fatalf("%s is not open: version %d, want null", c.relOf(d.TextDocument.URI), *d.TextDocument.Version)
			}
			closed++
		}
		for _, e := range d.Edits {
			if e.AnnotationID != "" {
				t.Fatalf("annotated edit without annotation support: %+v", e)
			}
		}
	}
	if open != 1 || closed < 2 {
		t.Fatalf("%d open and %d closed files: %+v", open, closed, res.DocumentChanges)
	}
	raw := c.rawCall("textDocument/rename", c.renameParams("main.sh", c.pos("main.sh", `build_release "$name"`, 2), "ship")).Result
	if !strings.Contains(string(raw), `"version":null`) {
		t.Fatalf("a closed file's version is not an explicit null: %s", raw)
	}
}

func TestRenameAnnotatedWhenExcluded(t *testing.T) {
	c := renameClient(t, true, true, true)
	c.open("main.sh")
	res := c.renameAt("main.sh", c.pos("main.sh", `build_release "$name"`, 2), "ship") // main.sh:6
	n := 0
	for _, d := range res.DocumentChanges {
		for _, e := range d.Edits {
			n++
			if e.AnnotationID != "bash-lsp.unrenamed" {
				t.Fatalf("edit %+v is not annotated", e)
			}
		}
	}
	if n != 4 {
		t.Fatalf("%d edits, want 4: %+v", n, res.DocumentChanges)
	}
	a, ok := res.ChangeAnnotations["bash-lsp.unrenamed"]
	if !ok || len(res.ChangeAnnotations) != 1 {
		t.Fatalf("annotations %+v", res.ChangeAnnotations)
	}
	if !a.NeedsConfirmation || !strings.HasSuffix(a.Label, "sites left unchanged") || a.Label != "3 sites left unchanged" {
		t.Fatalf("annotation %+v", a)
	}
	for _, want := range []string{"main.sh:9", "main.sh:10", "main.sh:8", "orphan.sh:2"} {
		if !strings.Contains(a.Description, want) {
			t.Errorf("description lacks %s: %q", want, a.Description)
		}
	}
	if len(c.shown) != 0 {
		t.Fatalf("a message was shown next to the annotation: %s", c.shown)
	}
}

func TestRenameAnnotationNeedsDocumentChanges(t *testing.T) {
	// changeAnnotationSupport without documentChanges cannot carry an
	// annotated edit: the plain form and the message apply.
	c := renameClient(t, true, false, true)
	c.open("main.sh")
	res := c.renameAt("main.sh", c.pos("main.sh", `build_release "$name"`, 2), "ship")
	if len(res.ChangeAnnotations) != 0 || len(res.DocumentChanges) != 0 || len(res.Changes) == 0 {
		t.Fatalf("result %+v", res)
	}
	if len(c.shown) != 1 {
		t.Fatalf("%d messages: %s", len(c.shown), c.shown)
	}
}

func TestRenameShowMessageWithoutAnnotations(t *testing.T) {
	c := renameClient(t, true, true, false)
	c.open("main.sh")
	res := c.renameAt("main.sh", c.pos("main.sh", `build_release "$name"`, 2), "ship")
	if len(res.ChangeAnnotations) != 0 {
		t.Fatalf("annotations %+v", res.ChangeAnnotations)
	}
	if len(c.shown) != 1 {
		t.Fatalf("%d messages: %s", len(c.shown), c.shown)
	}
	var m struct {
		Type    int    `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(c.shown[0], &m); err != nil || m.Type != 2 {
		t.Fatalf("%v %s", err, c.shown[0])
	}
	for _, want := range []string{"main.sh:8", "main.sh:9", "main.sh:10", "orphan.sh:2"} {
		if !strings.Contains(m.Message, want) {
			t.Errorf("message lacks %s: %q", want, m.Message)
		}
	}
	// A rename with nothing left unchanged shows nothing.
	c.shown = nil
	c.renameAt("main.sh", c.pos("main.sh", `local name="$1"`, 6), "nm")
	if len(c.shown) != 0 {
		t.Fatalf("message for a clean rename: %s", c.shown)
	}
}

func TestRenameUTF16Wire(t *testing.T) {
	// The name sits after an astral emoji (two UTF-16 units, four bytes) and
	// after a combining mark (two code points, one cell); lines end in CRLF.
	text := "# 😀 f\r\nf() { :; }\r\n: 😀; f\r\né; f\r\n"
	c := clientWith(t, renameCaps(true, true, true))
	c.openText("u.sh", text)
	c.waitDiag("u.sh")
	pos := Position{2, 6} // ": " 2, emoji 2, "; " 2
	res := c.renameAt("u.sh", pos, "g")
	got := map[int][2]int{}
	for _, d := range res.DocumentChanges {
		for _, e := range d.Edits {
			if e.Range.Start.Line != e.Range.End.Line || e.NewText != "g" {
				t.Fatalf("edit %+v", e)
			}
			got[e.Range.Start.Line] = [2]int{e.Range.Start.Character, e.Range.End.Character}
		}
	}
	want := map[int][2]int{1: {0, 1}, 2: {6, 7}, 3: {4, 5}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("edits %v, want %v", got, want)
	}
}

func TestRenameCancelledWhileQueued(t *testing.T) {
	var n atomic.Int64
	g := newGate()
	setHooks(t, g, nil, countingHook("textDocument/rename", &n))
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\na_fn\n")
	c := clientWith(t, clientCfg{root: root})
	g.waitEntered(t)
	c.sendReq(`1000`, "workspace/symbol", map[string]any{"query": ""})
	c.sendReq(`7`, "textDocument/rename", c.renameParams("a.sh", Position{0, 1}, "b_fn"))
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

func TestRenameContentModifiedWhenTheSnapshotLagsTheDocument(t *testing.T) {
	var srv *server
	var stale atomic.Bool
	setHooks(t, nil, nil, func(m string) { // runs on the loop, before the handler
		if m == "textDocument/rename" && stale.Load() {
			for _, d := range srv.docs {
				d.version++
				d.text = append([]byte("# edited\n"), d.text...)
			}
		}
	})
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\na_fn\n")
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { srv = s }})
	c.open("a.sh")
	c.waitDiag("a.sh")
	if res := c.renameAt("a.sh", Position{0, 1}, "b_fn"); len(res.Changes) == 0 {
		t.Fatal("no edits while the snapshot is current")
	}
	stale.Store(true)
	r := c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{0, 1}, "b_fn"))
	if r.Error == nil || r.Error.Code != codeContentModified {
		t.Fatalf("reply %+v %s, want -32801", r.Error, r.Result)
	}
}

func TestRenameRefusalIsAnErrorNeverAPartialEdit(t *testing.T) {
	c := renameClient(t, true, true, true)
	c.open("main.sh")
	p := c.pos("main.sh", `build_release "$name"`, 2)
	for newName, code := range map[string]string{"if": "reserved-name", "9x": "invalid-name", "build_release": "no-op"} {
		r := c.rawCall("textDocument/rename", c.renameParams("main.sh", p, newName))
		if r.Error == nil || r.Error.Code != -32803 || !strings.HasPrefix(r.Error.Message, code+":") || string(r.Result) != "" {
			t.Fatalf("%s: reply %+v %s", newName, r.Error, r.Result)
		}
		if got := refusalCode(r); got != code {
			t.Fatalf("%s: data %s", newName, r.ErrData)
		}
	}
	if len(c.shown) != 0 {
		t.Fatalf("a refusal showed %s", c.shown)
	}
}

func TestRenameOutwardTextIsRedacted(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	// A refusal echoes the new name the client sent.
	root := oneFileRoot(t, "a.sh", "a_fn() { :; }\na_fn\n")
	cfg := renameCaps(true, true, true)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("a.sh")
	r := c.rawCall("textDocument/rename", c.renameParams("a.sh", Position{0, 1}, secret+" x"))
	if r.Error == nil || strings.Contains(r.Error.Message, secret) || !strings.Contains(r.Error.Message, "[redacted]") {
		t.Fatalf("reply %+v", r.Error)
	}
	// The note of the unchanged sites: the excluded text and the warnings.
	s := newServer(strings.NewReader(""), nil, nil, "test")
	plan := &analysis.RenamePlan{Old: "a_fn", New: "b_fn",
		Excluded: []analysis.RenameExclusion{{
			Loc:    analysis.Location{Path: "/w/a.sh", Range: tree.Range{Start: tree.Pos{Line: 3}}},
			Res:    analysis.Candidate,
			Reason: "string-eval",
			Text:   "a_fn " + secret + "\ntoken=hunter2hunter2"}},
		Warnings: []string{"exported: " + secret}}
	note, label := s.renameNote(nil, plan)
	if label != "1 site left unchanged" {
		t.Fatalf("label %q", label)
	}
	if strings.Contains(note, secret) || strings.Contains(note, "hunter2") || strings.Count(note, "[redacted]") < 3 {
		t.Fatalf("note %q", note)
	}
}

func TestRenameLongExcludedTextIsCutAfterRedaction(t *testing.T) {
	// A secret that straddles the display cut must be redacted whole, not
	// cut in half first; the cut falls on a rune boundary.
	const secret = "AKIAIOSFODNN7EXAMPLE"
	s := newServer(strings.NewReader(""), nil, nil, "test")
	for pad := 170; pad <= 200; pad++ {
		text := strings.Repeat("é", pad/2) + strings.Repeat("x", pad%2) + secret + strings.Repeat("é", 400)
		plan := &analysis.RenamePlan{Old: "a_fn", Excluded: []analysis.RenameExclusion{{
			Loc: analysis.Location{Path: "/w/a.sh"}, Res: analysis.Candidate, Reason: "string-eval", Text: text}}}
		note, _ := s.renameNote(nil, plan)
		if strings.Contains(note, "AKIA") || !utf8.ValidString(note) || strings.Contains(note, "\uFFFD") {
			t.Fatalf("pad %d: note %q", pad, note)
		}
		if len(note) > 600 {
			t.Fatalf("pad %d: note of %d bytes", pad, len(note))
		}
	}
}

func TestRenameUnknownDocument(t *testing.T) {
	c := renameClient(t, true, false, false)
	r := c.rawCall("textDocument/rename", c.renameParams("nope.sh", Position{0, 0}, "x"))
	if r.Error == nil || r.Error.Code != -32803 || !strings.HasPrefix(r.Error.Message, "not-a-symbol:") {
		t.Fatalf("reply %+v %s", r.Error, r.Result)
	}
}

func TestRenameCapability(t *testing.T) {
	init := func(params string) string {
		got, rerr := newServer(strings.NewReader(""), nil, nil, "t").initialize(json.RawMessage(params))
		if rerr != nil {
			t.Fatal(rerr)
		}
		b, _ := json.Marshal(got.(initializeResult).Capabilities["renameProvider"])
		return string(b)
	}
	if got := init(`{"capabilities":{"textDocument":{"rename":{"prepareSupport":true}}}}`); got != `{"prepareProvider":true}` {
		t.Fatalf("with prepareSupport: %s", got)
	}
	for _, p := range []string{`{"capabilities":{}}`, `{"capabilities":{"textDocument":{"rename":{"prepareSupport":false}}}}`, `{"capabilities":{"textDocument":{"rename":{}}}}`} {
		if got := init(p); got != `true` {
			t.Fatalf("%s: %s", p, got)
		}
	}
}

// Rename of a function through a 1,000-file `source` chain, and the prepare
// on the 561 KB file, stay inside their budgets.
func TestRenameLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("budget test")
	}
	root := t.TempDir()
	for i := 0; i < 1000; i++ {
		var b strings.Builder
		b.WriteString("#!/usr/bin/env bash\n")
		if i > 0 {
			fmt.Fprintf(&b, "source ./s%04d.sh\n", i-1)
		}
		for k := 0; k < 20; k++ {
			fmt.Fprintf(&b, "f%d_%d() {\n  local a=\"$1\"\n", i, k)
			if i > 0 {
				fmt.Fprintf(&b, "  f%d_%d \"$a\"\n", i-1, k)
			}
			b.WriteString("  echo \"$a\"\n}\n")
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("s%04d.sh", i)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var big strings.Builder
	big.WriteString("#!/usr/bin/env bash\necho start\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&big, "# Function number %d.\nfn_%d() {\n  local a=\"$1\" b=\"$2\"\n  if [[ -n \"$a\" && \"$b\" != \"x%d\" ]]; then\n    printf '%%s %%s\\n' \"$a\" \"$b\" | tr a-z A-Z\n  else\n    for f in \"$@\"; do echo \"${f:-none}\" >&2; done\n  fi\n  case \"$a\" in start|stop) echo \"$a\";; *) return %d;; esac\n}\n\n", i, i, i, i%250)
		if i > 0 {
			fmt.Fprintf(&big, "fn_%d a b\n", i-1)
		}
	}
	big.WriteString("fn_1999 a b\n")
	if err := os.WriteFile(filepath.Join(root, "big.sh"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := renameCaps(true, true, true)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("big.sh")
	c.waitDiag("big.sh")
	// cold is the first call (nothing cached), best the fastest of five.
	measure := func(method string, p map[string]any) (cold, best time.Duration) {
		for i := 0; i < 5; i++ {
			start := time.Now()
			r := c.rawCall(method, p)
			if r.Error != nil {
				t.Fatalf("%s: %+v", method, r.Error)
			}
			e := time.Since(start)
			if i == 0 {
				cold = e
			}
			if best == 0 || e < best {
				best = e
			}
		}
		return cold, best
	}
	at := func(rel string, p Position) map[string]any {
		return map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p}
	}
	chain := c.renameParams("s0000.sh", c.pos("s0000.sh", "f0_0() {", 1), "renamed_0")
	rnCold, rn := measure("textDocument/rename", chain)
	prepCold, prep := measure("textDocument/prepareRename", at("big.sh", c.pos("big.sh", "fn_1000() {", 3)))
	chainPrepCold, chainPrep := measure("textDocument/prepareRename", at("s0000.sh", c.pos("s0000.sh", "f0_0() {", 1)))
	t.Logf("rename in a 1,000-file chain: cold %v, best %v; prepareRename in the 561 KB file: cold %v, best %v; prepareRename in the chain: cold %v, best %v",
		rnCold, rn, prepCold, prep, chainPrepCold, chainPrep)
	if raceEnabled {
		return
	}
	if rn > 200*time.Millisecond {
		t.Errorf("rename took %v, budget 200 ms", rn)
	}
	if prep > 20*time.Millisecond {
		t.Errorf("prepareRename took %v, budget 20 ms", prep)
	}
}

// A prepareRename that succeeds must not be followed by a rename that always
// refuses: the refusals that do not depend on the new name are decided at
// prepare time, the ones that do (a conflict with the new name) are not.
func TestPrepareRenameRefusesWhatNoNewNameCanFix(t *testing.T) {
	type tc struct {
		name, src string
		pos       Position
		code      string
	}
	for _, x := range []tc{
		{"special variable", "f() {\n  local IFS=,\n  echo \"$IFS\"\n}\nf\n", Position{1, 9}, "name-conflict"},
		{"name reference", "f() { :; }\nf\nunset -f f\n", Position{0, 0}, "name-reference"},
		{"half-typed heredoc", "f() { :; }\nf\ncat <<EOF\nf\n", Position{1, 0}, "recovered-region"},
		{"dynamic scope", "v=1\nouter() {\n  local v=2\n  inner\n}\ninner() { echo \"$v\"; }\nouter\n", Position{0, 0}, "dynamic-scope"},
	} {
		t.Run(x.name, func(t *testing.T) {
			c := clientWith(t, renameCaps(true, false, false))
			c.openText("a.sh", x.src)
			c.waitDiag("a.sh")
			rn := c.rawCall("textDocument/rename", c.renameParams("a.sh", x.pos, "zz_fresh_name"))
			if rn.Error == nil || refusalCode(rn) != x.code {
				t.Fatalf("rename: %+v %s %s, want a refusal %s", rn.Error, rn.ErrData, rn.Result, x.code)
			}
			pr := c.rawCall("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}, "position": x.pos})
			if pr.Error == nil || pr.Error.Code != -32803 || refusalCode(pr) != x.code || !strings.HasPrefix(pr.Error.Message, x.code+":") {
				t.Fatalf("prepareRename: %+v %s %s, want the refusal %s", pr.Error, pr.ErrData, pr.Result, x.code)
			}
		})
	}
	t.Run("conflict with the new name is for rename", func(t *testing.T) {
		c := clientWith(t, renameCaps(true, false, false))
		c.openText("a.sh", "a_fn() { :; }\nb_fn() { :; }\na_fn\n")
		c.waitDiag("a.sh")
		pos := Position{0, 1}
		if r := c.rawCall("textDocument/rename", c.renameParams("a.sh", pos, "b_fn")); r.Error == nil || refusalCode(r) != "name-conflict" {
			t.Fatalf("rename: %+v %s", r.Error, r.Result)
		}
		if r := c.rawCall("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}, "position": pos}); r.Error != nil {
			t.Fatalf("prepareRename refused: %+v", r.Error)
		}
	})
}

// Warnings alone (nothing excluded) are shown as one message, annotation
// support or not: there is nothing to confirm per site.
func TestRenameWarningsAloneAreAMessage(t *testing.T) {
	for _, ann := range []bool{false, true} {
		t.Run(fmt.Sprint("annotations=", ann), func(t *testing.T) {
			c := clientWith(t, renameCaps(true, true, ann))
			c.openText("a.sh", "export VAR=1\necho \"$VAR\"\n")
			c.waitDiag("a.sh")
			res := c.renameAt("a.sh", Position{0, 8}, "NEW_VAR")
			if len(res.ChangeAnnotations) != 0 {
				t.Fatalf("annotations %+v", res.ChangeAnnotations)
			}
			if len(c.shown) != 1 || !strings.Contains(string(c.shown[0]), "exported") || strings.Contains(string(c.shown[0]), "sites left unchanged") {
				t.Fatalf("messages %s", c.shown)
			}
			for _, d := range res.DocumentChanges {
				for _, e := range d.Edits {
					if e.AnnotationID != "" {
						t.Fatalf("annotated edit %+v", e)
					}
				}
			}
		})
	}
}

// A client without annotation support still hears of another symbol of the
// same name that the rename leaves alone, even when nothing else is unsure.
func TestRenameShowMessageNamesOtherSymbolsWithoutAnnotations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.sh", "f() { :; }\nf\n")
	writeFile(t, root, "b.sh", "f() { :; }\nf\n")
	cfg := renameCaps(true, true, false)
	cfg.root = root
	c := clientWith(t, cfg)
	c.open("a.sh")
	res := c.renameAt("a.sh", Position{0, 0}, "g")
	if got := c.editLines(res); strings.Join(got, " ") != "a.sh:1 a.sh:2" {
		t.Fatalf("edits %v", got)
	}
	if len(c.shown) != 1 {
		t.Fatalf("%d messages: %s", len(c.shown), c.shown)
	}
	var m struct {
		Type    int    `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(c.shown[0], &m); err != nil || m.Type != 2 || !strings.Contains(m.Message, "b.sh:1") || !strings.Contains(m.Message, "other symbol") {
		t.Fatalf("%v %s", err, c.shown[0])
	}
}
