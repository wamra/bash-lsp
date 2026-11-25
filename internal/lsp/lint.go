package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/format"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/redact"
	"github.com/wamra/bash-lsp/internal/shellcheck"
	"github.com/wamra/bash-lsp/tree"
)

// ---- findings of an open document: push, pull and quick fixes ----
//
// The loop keeps, for each open document, the findings it last sent
// (docLint.pub) with their fixes already in LSP positions. Push, pull and the
// code action handler all read that one list, so a quick fix always belongs to
// a finding the client has seen.

// lspFix is a fix of a published finding, in LSP terms.
type lspFix struct {
	title string
	safe  bool
	edits []TextEdit
	src   analysis.Fix // the fix as the analysis made it, in byte offsets (fix all merges these)
}

// pubDiag is a finding as published, with the fixes that resolve it.
type pubDiag struct {
	diag  Diagnostic
	fixes []lspFix
}

// docLint is what the server knows about the findings of one open document.
// Loop only.
type docLint struct {
	// The native findings of the snapshot of generation gen, and the file they
	// were computed from (have: both are set).
	have   bool
	gen    uint64
	native []analysis.Diagnostic
	file   *tree.File

	pub     []pubDiag // the findings as last sent
	sent    bool
	sentVer int    // the document version they were sent for
	pubGen  uint64 // the generation the client has the findings of (a pull moves gen without sending)

	// ShellCheck (see scheduleCheck): the findings of the run for scVer, once
	// scDone; scSeq numbers the runs scheduled; timer and cancel belong to the
	// latest; run keeps a run of this document from starting before the one it
	// replaced has ended.
	sc        []analysis.Diagnostic
	scErr     error // the run ended without findings: extool.ErrTimeout or ErrOutputCap (shown as WB1004)
	scTimeout time.Duration
	scDone    bool
	scVer     int
	scSeq     int
	timer     *time.Timer
	cancel    context.CancelFunc
	run       sync.Mutex
}

// findings returns the findings of d as they stand now, in LSP form. With
// cached the native part is reused when the snapshot's generation has not
// moved since it was computed; a forced publish (the text or the backend
// changed) computes it again.
func (s *server) findings(d *document, cached bool) []pubDiag {
	l := &d.lint
	snap := d.be.Snapshot()
	if gen := snap.Generation(); !cached || !l.have || l.gen != gen {
		l.native = snap.Diagnostics(d.path, s.diagOptions(d.be))
		l.file = fileOf(snap, d.path)
		l.gen, l.have = gen, true
		if l.file != nil {
			s.logInternal(d.path, l.file)
		}
	}
	return pubDiags(l.file, s.merged(d))
}

// merged is the native findings, with the ShellCheck findings for the current
// version merged in (analysis.Merge drops the native ones they duplicate). A run
// that did not finish contributes WB1004, built here under the settings in
// force now, so a reload that disables it or changes its severity applies.
func (s *server) merged(d *document) []analysis.Diagnostic {
	l := &d.lint
	if !l.scDone || l.scVer != d.version {
		return l.native
	}
	sc := l.sc
	if l.scErr != nil {
		if w, ok := s.unfinished(d, l.scErr, l.scTimeout); ok {
			sc = []analysis.Diagnostic{w}
		}
	}
	return analysis.Merge(l.native, sc)
}

// pubDiags converts findings of f to LSP positions.
func pubDiags(f *tree.File, ds []analysis.Diagnostic) []pubDiag {
	out := make([]pubDiag, 0, len(ds))
	if f == nil {
		return out
	}
	conv := f.NewConverter()
	rangeOf := func(r tree.Range) Range {
		var o Range
		o.Start.Line, o.Start.Character = conv.ToUTF16(r.Start)
		o.End.Line, o.End.Character = conv.ToUTF16(r.End)
		return o
	}
	for _, x := range ds {
		p := pubDiag{diag: Diagnostic{Range: rangeOf(x.Range), Severity: int(x.Severity), Code: x.Rule, Source: x.Origin, Message: x.Message}}
		if x.Unnecessary {
			p.diag.Tags = []int{diagnosticTagUnnecessary}
		}
		for _, fx := range x.Fixes {
			lf := lspFix{title: fx.Title, safe: fx.Safe, src: fx, edits: make([]TextEdit, 0, len(fx.Edits))}
			for _, e := range fx.Edits {
				lf.edits = append(lf.edits, TextEdit{Range: rangeOf(e.Range), NewText: e.NewText})
			}
			p.fixes = append(p.fixes, lf)
		}
		out = append(out, p)
	}
	return out
}

func diagnosticsOf(pub []pubDiag) []Diagnostic {
	out := make([]Diagnostic, 0, len(pub))
	for _, p := range pub {
		out = append(out, p.diag)
	}
	return out
}

// publishDoc sends the findings of an open document with its version. A
// forced publish always sends; otherwise nothing is sent when the findings are
// those the client already has for this version.
func (s *server) publishDoc(d *document, force bool) {
	l := &d.lint
	pub := s.findings(d, !force)
	if !force && l.sent && l.sentVer == d.version && reflect.DeepEqual(pub, l.pub) {
		l.pubGen = l.gen
		return
	}
	l.pub, l.sent, l.sentVer, l.pubGen = pub, true, d.version, l.gen
	v := d.version
	s.c.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{URI: d.uri, Version: &v, Diagnostics: diagnosticsOf(pub)})
}

// refreshOthers republishes the open documents of be, except one, whose
// findings may have changed because the backend moved to a new generation
// (another file was edited, saved, changed on disk or closed). A document
// whose findings did not change is not sent again.
func (s *server) refreshOthers(be backend, except *document) {
	gen := be.Snapshot().Generation()
	var docs []*document
	for _, d := range s.docs {
		if d != except && d.be == be && !(d.lint.sent && d.lint.pubGen == gen) {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	for _, d := range docs {
		s.publishDoc(d, false)
	}
	s.refreshSemanticTokens(docs...)
}

// ---- pull ----

// diagnostic answers textDocument/diagnostic: the findings of one document,
// or `unchanged` when the client's previous result is still current. The
// resultId is "<generation>.<version>": the generation of the backend's
// snapshot (a document's findings depend on other files) and the version of
// the open document (0 for a file that is not open).
func (s *server) diagnostic(raw json.RawMessage) (any, *rpcError) {
	var p documentDiagnosticParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	if !s.wait() {
		return nil, cancelledError()
	}
	d, rerr := s.docOf(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	var pub []pubDiag
	var gen uint64
	version := 0
	switch {
	case d != nil:
		pub, gen, version = s.findings(d, true), d.lint.gen, d.version
	default:
		be, path, rerr := s.locate(p.TextDocument.URI)
		if rerr != nil {
			return nil, rerr
		}
		snap := be.Snapshot()
		gen = snap.Generation()
		pub = pubDiags(fileOf(snap, path), snap.Diagnostics(path, s.diagOptions(be)))
	}
	id := fmt.Sprintf("%d.%d", gen, version)
	if d != nil && d.lint.scDone && d.lint.scVer == d.version {
		id += fmt.Sprintf(".s%d", d.lint.scSeq) // ShellCheck findings are merged in: the set moved without a new generation or version
	}
	if s.epoch > 0 {
		id += fmt.Sprintf(".e%d", s.epoch) // the settings were reloaded: the same snapshot gives other findings
	}
	if p.PreviousResultID == id {
		return unchangedDiagnosticReport{Kind: "unchanged", ResultID: id}, nil
	}
	return fullDiagnosticReport{Kind: "full", ResultID: id, Items: diagnosticsOf(pub)}, nil
}

// ---- code actions ----

// codeAction answers textDocument/codeAction from the findings last published
// for the document.
func (s *server) codeAction(raw json.RawMessage) (any, *rpcError) {
	var p codeActionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	if !s.wait() {
		return nil, cancelledError()
	}
	d, rerr := s.docOf(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	if d == nil || !d.lint.sent || d.lint.sentVer != d.version {
		return []CodeAction{}, nil
	}
	out := fixActions(d.uri, d.lint.pub, p.Context.Diagnostics, p.Range, p.Context.Only)
	return append(out, s.editorActions(d, p.Range, p.Context.Only)...), nil
}

// codeActionKinds are the kinds of code action the server advertises.
// source.fixAll is not among them: no fix is Safe yet, so none would ever be
// produced. fixAllAction answers it when a client asks all the same.
var codeActionKinds = []string{"quickfix", "refactor.rewrite"}

// editorActions are the code actions that belong to no finding, after the
// quick fixes of the findings: add a shebang (quickfix), quote the expansion
// under the cursor (refactor.rewrite) and fix all (source.fixAll).
func (s *server) editorActions(d *document, r Range, only []string) []CodeAction {
	snap := d.be.Snapshot()
	f := fileOf(snap, d.path)
	if f == nil {
		return nil
	}
	conv := f.NewConverter()
	edits := func(fx analysis.Fix) []TextEdit { return lspEdits(conv, fx.Edits) }
	var out []CodeAction
	if kindWanted(only, "quickfix") {
		if fx, ok := snap.AddShebang(d.path); ok {
			out = append(out, CodeAction{Title: fx.Title, Kind: "quickfix",
				Edit: workspaceEdit{Changes: map[string][]TextEdit{d.uri: edits(fx)}}})
		}
	}
	if kindWanted(only, "refactor.rewrite") {
		off := f.FromUTF16(r.Start.Line, r.Start.Character).Offset
		if fx, ok := snap.QuoteExpansion(d.path, off); ok {
			out = append(out, CodeAction{Title: fx.Title, Kind: "refactor.rewrite",
				Edit: workspaceEdit{Changes: map[string][]TextEdit{d.uri: edits(fx)}}})
		}
	}
	if a, ok := fixAllAction(d.uri, f, d.lint.pub, only); ok {
		out = append(out, a)
	}
	return out
}

// lspEdits converts edits in byte offsets to LSP positions.
func lspEdits(conv *tree.Converter, edits []analysis.TextEdit) []TextEdit {
	out := make([]TextEdit, 0, len(edits))
	for _, e := range edits {
		var t TextEdit
		t.Range.Start.Line, t.Range.Start.Character = conv.ToUTF16(e.Range.Start)
		t.Range.End.Line, t.Range.End.Character = conv.ToUTF16(e.Range.End)
		t.NewText = e.NewText
		out = append(out, t)
	}
	return out
}

// fixAllAction is the source.fixAll action: the Safe fixes of the published
// findings pub merged by analysis.FixAll, where the first of two overlapping
// fixes wins. ok is false when it was not asked for or nothing is left to apply.
func fixAllAction(uri string, f *tree.File, pub []pubDiag, only []string) (CodeAction, bool) {
	if !kindWanted(only, "source.fixAll") {
		return CodeAction{}, false
	}
	var fixes []analysis.Fix
	for _, p := range pub {
		for _, x := range p.fixes {
			fixes = append(fixes, x.src)
		}
	}
	merged := analysis.FixAll(fixes)
	if len(merged) == 0 {
		return CodeAction{}, false
	}
	edits := lspEdits(f.NewConverter(), merged)
	return CodeAction{Title: "Fix all safe problems", Kind: "source.fixAll",
		Edit: workspaceEdit{Changes: map[string][]TextEdit{uri: edits}}}, true
}

// fixActions is the fix lookup: the quick fixes of the published findings pub
// that the request asks for. A finding is asked for when it is one of ctx
// (same code and same range) or, when the client sent none, when it touches
// r. isPreferred is Fix.Safe. Every source of fixes goes through here.
func fixActions(uri string, pub []pubDiag, ctx []Diagnostic, r Range, only []string) []CodeAction {
	out := []CodeAction{}
	if !kindWanted(only, "quickfix") {
		return out
	}
	for _, p := range pub {
		if !asked(p.diag, ctx, r) {
			continue
		}
		for _, f := range p.fixes {
			out = append(out, CodeAction{
				Title: f.title, Kind: "quickfix", Diagnostics: []Diagnostic{p.diag}, IsPreferred: f.safe,
				Edit: workspaceEdit{Changes: map[string][]TextEdit{uri: f.edits}},
			})
		}
	}
	return out
}

// kindWanted reports whether a request limited to only accepts kind: no limit,
// the kind itself or a parent of it (`refactor` asks for `refactor.rewrite`).
func kindWanted(only []string, kind string) bool {
	return len(only) == 0 || slices.ContainsFunc(only, func(o string) bool {
		return o == "" || o == kind || strings.HasPrefix(kind, o+".")
	})
}

func asked(d Diagnostic, ctx []Diagnostic, r Range) bool {
	if len(ctx) > 0 {
		return slices.ContainsFunc(ctx, func(c Diagnostic) bool { return c.Code == d.Code && c.Range == d.Range })
	}
	return !before(r.End, d.Range.Start) && !before(d.Range.End, r.Start)
}

func before(a, b Position) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Character < b.Character
}

// ---- ShellCheck ----
//
// ShellCheck never runs on the message loop. An edit publishes the native
// findings at once, then (scheduleCheck) starts a timer; when it fires, a
// goroutine runs the ShellCheck runner on the document's parsed file, which was
// captured on the loop, and hands the result back with s.post. finishCheck
// takes it only if the document is still open at the same version and the run
// is still the latest scheduled; it then publishes the merge. A newer edit, a
// close or the end of the session cancels the run, which kills the child.

// checkDebounce is the quiet time after the last change before ShellCheck
// runs on a document.
var checkDebounce = 300 * time.Millisecond

// scTool is the ShellCheck program of one folder (or of the loose documents),
// found on first use, off the loop, by extool.Find: the program, the path
// profile and the workspace roots decide, never the analysed code. The tool is
// dropped when the settings or the folders change.
type scTool struct {
	path    string // shellcheck.path as configured ("" for the PATH profile)
	timeout time.Duration
	roots   []string

	once   sync.Once
	runner *shellcheck.Runner // nil when the program was not found or was refused
}

// workspaceRoots lists every workspace folder as the client spells it and as resolved:
// the program must be outside all of them, and the runner's PATH loses entries
// inside them.
func (s *server) workspaceRoots() []string {
	out := make([]string, 0, 2*len(s.folders))
	for _, f := range s.folders {
		out = append(out, f.client, f.ws.Root())
	}
	return out
}

// toolFor returns the ShellCheck tool for the documents of be, nil when the
// settings turn ShellCheck off. Loop only.
func (s *server) toolFor(be backend) *scTool {
	st := s.settingsOf(be)
	if !st.ShellCheck.Enabled {
		return nil
	}
	slot := &s.looseTool
	for _, f := range s.folders {
		if backend(f.ws) == be {
			slot = &f.tool
		}
	}
	if *slot == nil {
		*slot = &scTool{path: st.ShellCheck.Path, timeout: st.ShellCheck.Timeout, roots: s.workspaceRoots()}
	}
	return *slot
}

// dropTools forgets the tools, so the next run builds them from the current
// settings and folders. Loop only.
func (s *server) dropTools() {
	s.helpTools = nil
	s.looseTool = nil
	for _, f := range s.folders {
		f.tool = nil
	}
}

// get finds the program on first use. Not found is logged quietly; a refusal
// goes to the user (window/logMessage), once per session.
func (t *scTool) get(s *server) *shellcheck.Runner {
	t.once.Do(func() {
		tool, ref, ok := extool.Find("shellcheck", t.path, s.pathProfile(), t.roots)
		switch {
		case ref != nil:
			s.logMessageOnce(messageTypeWarning, fmt.Sprintf("shellcheck: %s refused: %s; only native findings are shown", ref.Path, ref.Reason))
		case !ok:
			s.log.Printf("shellcheck: not found; only native findings are shown")
		default:
			t.runner = shellcheck.New(tool, shellcheck.Options{Timeout: t.timeout, Roots: t.roots})
		}
	})
	return t.runner
}

// resetCheck drops the ShellCheck state of d: a run in flight is cancelled, a
// timer not yet fired is stopped and the findings kept for an older version
// are forgotten. Loop only.
func (s *server) resetCheck(d *document) {
	l := &d.lint
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	l.sc, l.scErr, l.scDone = nil, nil, false
	l.scSeq++ // a result already on its way back is stale
}

// scheduleCheck arranges a ShellCheck run for d at its current version, after
// the quiet time. The native findings of that version were just published, so
// l.file is the parsed file ShellCheck will read. Loop only.
func (s *server) scheduleCheck(d *document) {
	l := &d.lint
	tool := s.toolFor(d.be)
	if tool == nil || l.file == nil || l.file.Skipped != "" {
		return
	}
	seq, ver, file := l.scSeq, d.version, l.file
	ctx, cancel := context.WithCancel(s.baseCtx)
	l.cancel = cancel
	l.timer = time.AfterFunc(s.debounce, func() { s.runCheck(ctx, d, tool, file, ver, seq) })
}

// runCheck runs on its own goroutine: it touches no server state but the
// document's lint.run mutex (a field it only locks) and posts its result.
func (s *server) runCheck(ctx context.Context, d *document, tool *scTool, f *tree.File, ver, seq int) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("panic running shellcheck: %v", r)
		}
	}()
	d.lint.run.Lock() // one run of a file at a time: the run it replaced was cancelled and is ending
	defer d.lint.run.Unlock()
	if ctx.Err() != nil {
		return
	}
	r := tool.get(s)
	if r == nil {
		return
	}
	ds, notes, err := r.CheckNotes(ctx, f)
	if ctx.Err() != nil {
		return
	}
	s.post(func() { s.finishCheck(d, ver, seq, ds, notes, err, tool.timeout) })
}

// finishCheck takes a ShellCheck result on the loop. A result for a document
// that was closed, changed since, or rescheduled is dropped.
func (s *server) finishCheck(d *document, ver, seq int, ds []analysis.Diagnostic, notes []string, err error, timeout time.Duration) {
	l := &d.lint
	if s.docs[d.path] != d || d.version != ver || l.scSeq != seq {
		return
	}
	for _, n := range notes {
		s.logMessageOnce(messageTypeWarning, "shellcheck: "+n)
	}
	var failed error // the run ended without findings; WB1004 is built when merging
	switch {
	case err == nil:
	case errors.Is(err, extool.ErrTimeout), errors.Is(err, extool.ErrOutputCap):
		ds, failed = nil, err
	default:
		s.logMessageOnce(messageTypeError, err.Error())
		return
	}
	l.sc, l.scErr, l.scTimeout, l.scDone, l.scVer = ds, failed, timeout, true, ver
	s.publishDoc(d, false)
}

// unfinished is the WB1004 finding for a run that did not finish, under the
// folder's severity setting; ok is false when the rule is disabled. The rule
// is built here, not in analysis: it is a fact about the run, not the code.
func (s *server) unfinished(d *document, err error, timeout time.Duration) (analysis.Diagnostic, bool) {
	const rule = "WB1004"
	opt := s.diagOptions(d.be)
	f := d.lint.file
	if f == nil || opt.Disabled[rule] {
		return analysis.Diagnostic{}, false
	}
	why := "it wrote more output than allowed"
	if errors.Is(err, extool.ErrTimeout) {
		why = "it ran past its time limit"
		if timeout > 0 {
			why = fmt.Sprintf("it ran past its time limit of %s", timeout)
		}
	}
	end := f.FromUTF16(0, math.MaxInt32).Offset // the end of the first line
	w := analysis.Diagnostic{Rule: rule, Severity: analysis.SevInfo, Origin: analysis.OriginNative,
		Range:   tree.Range{Start: f.PosAt(0), End: f.PosAt(end)},
		Message: "ShellCheck did not finish: " + why + "; only bash-lsp findings are shown"}
	if sev, ok := opt.Severity[rule]; ok {
		w.Severity = sev
	}
	return w, true
}

// ---- messages to the client ----

// messageTypeError, messageTypeInfo are LSP MessageType 1 and 3.
const (
	messageTypeError = 1
	messageTypeInfo  = 3
)

// logMessage sends window/logMessage. Safe from any goroutine.
func (s *server) logMessage(typ int, msg string) {
	s.c.notify("window/logMessage", logMessageParams{Type: typ, Message: redact.String(msg)})
}

// forgetMessage lets msg be sent again by logMessageOnce.
func (s *server) forgetMessage(msg string) {
	s.shownMu.Lock()
	delete(s.loggedMsg, msg)
	s.shownMu.Unlock()
}

// logMessageOnce is logMessage, once per distinct text per session.
func (s *server) logMessageOnce(typ int, msg string) {
	s.shownMu.Lock()
	if s.loggedMsg == nil {
		s.loggedMsg = map[string]bool{}
	}
	seen := s.loggedMsg[msg]
	s.loggedMsg[msg] = true
	s.shownMu.Unlock()
	if !seen {
		s.logMessage(typ, msg)
	}
}

// ---- formatting ----
//
// A formatting request never runs on the loop and never on an edit: the
// handler captures the text and the settings, starts the work on a goroutine
// (bounded by fmtSem) and returns `deferred`; the answer is posted back to the
// loop, which drops it for a cancelled request and answers ContentModified (-32801)
// for a document that changed meanwhile. A cancelled request is
// answered at once (the formatter takes no context, so its run ends by itself
// and its result is discarded). Refusals answer an empty list and a log
// message with the formatter's own reason.

// maxTabSize caps FormattingOptions.tabSize, as the `format.indent` setting is
// capped.
const maxTabSize = 16

// formatHook is a test seam, called when a formatter run starts; guarded by
// hookMu.
var formatHook func()

type fmtJob struct {
	path, root string
	text       []byte
	st         config.Settings // only Format and Dialect are read
	opts       format.LSPOptions
	ranged     bool
	rng        Range
}

type fmtResult struct {
	edits    []TextEdit
	refusal  string
	panicked any
}

func (s *server) formatting(raw json.RawMessage, ranged bool) (any, *rpcError) {
	var p documentRangeFormattingParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	if !s.wait() {
		return nil, cancelledError()
	}
	d, rerr := s.docOf(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	if d == nil {
		return []TextEdit{}, nil
	}
	root := filepath.Dir(d.path) // a document outside every folder: its own directory
	if f := s.folderOf(d.path); f != nil {
		root = f.ws.Root()
	}
	tab := min(max(p.Options.TabSize, 0), maxTabSize)
	job := &fmtJob{path: d.path, root: root, text: bytes.Clone(d.text), st: s.settingsOf(d.be),
		opts:   format.LSPOptions{TabSize: uint(tab), InsertSpaces: p.Options.InsertSpaces},
		ranged: ranged, rng: p.Range}
	id, lr, version := s.curID, s.cur, d.version
	cancelled := make(<-chan struct{})
	if lr != nil {
		cancelled = lr.done
	}
	go s.runFormat(job, d, version, id, cancelled)
	return deferred{}, nil
}

// runFormat runs on its own goroutine and touches no server state: it waits for
// a slot, runs the job and posts the outcome.
func (s *server) runFormat(job *fmtJob, d *document, version int, id json.RawMessage, cancelled <-chan struct{}) {
	res := make(chan fmtResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				res <- fmtResult{panicked: r}
			}
		}()
		select {
		case s.fmtSem <- struct{}{}:
			defer func() { <-s.fmtSem }()
		case <-cancelled:
			return
		}
		hookMu.RLock()
		h := formatHook
		hookMu.RUnlock()
		if h != nil {
			h()
		}
		res <- job.run()
	}()
	select {
	case r := <-res:
		s.post(func() { s.finishFormat(d, version, id, r) })
	case <-cancelled:
		s.post(func() { s.answer(id, nil, cancelledError()) })
	case <-s.baseCtx.Done():
	}
}

// finishFormat answers a formatting request on the loop.
func (s *server) finishFormat(d *document, version int, id json.RawMessage, r fmtResult) {
	switch {
	case s.isCancelled(idKey(id)):
		s.answer(id, nil, cancelledError())
	case r.panicked != nil:
		s.log.Printf("panic formatting %s: %v", d.path, r.panicked)
		s.answer(id, nil, &rpcError{codeInternalError, "internal error in formatting"})
	case s.docs[d.path] != d || d.version != version:
		s.answer(id, nil, &rpcError{codeContentModified, "the document changed while it was being formatted"})
	case r.refusal != "":
		s.logMessage(messageTypeWarning, "format refused: "+r.refusal)
		s.answer(id, []TextEdit{}, nil)
	default:
		s.answer(id, r.edits, nil)
	}
}

// run formats the captured text. A formatter refusal is the result's reason.
func (j *fmtJob) run() fmtResult {
	o := format.OptionsFor(j.path, j.root, j.st.Format, &j.opts)
	f := tree.NewFile("", j.text)
	var es, ee int
	var text []byte
	if j.ranged {
		start := f.FromUTF16(j.rng.Start.Line, j.rng.Start.Character).Offset
		end := max(f.FromUTF16(j.rng.End.Line, j.rng.End.Character).Offset, start)
		var err error
		if es, ee, text, err = format.RangeWith(j.path, j.text, start, end, o, j.st.Dialect); err != nil {
			return fmtResult{refusal: err.Error()}
		}
	} else {
		out, err := format.SourceWith(j.path, j.text, o, j.st.Dialect)
		if err != nil {
			return fmtResult{refusal: err.Error()}
		}
		es, ee, text = format.Edit(j.text, out)
	}
	if es == ee && len(text) == 0 || bytes.Equal(j.text[es:ee], text) {
		return fmtResult{edits: []TextEdit{}}
	}
	var e TextEdit
	e.Range.Start.Line, e.Range.Start.Character = f.ToUTF16(f.PosAt(es))
	e.Range.End.Line, e.Range.End.Character = f.ToUTF16(f.PosAt(ee))
	e.NewText = string(text)
	return fmtResult{edits: []TextEdit{e}}
}

// ---- settings reload ----
//
// Settings are read when a folder opens. They are read again, on the loop (two
// small files), when a settings file changes: the repo file of a folder or the
// user file, reported by the watched-files handler or didSave, or when the
// client sends workspace/didChangeConfiguration (taken only as a signal to read
// the files again; its payload is not used). A load that cannot use a file
// keeps the previous settings and logs why. Open documents are published again
// under the new settings, and ShellCheck is rescheduled only when its own
// settings changed. Keys the folder's workspace consumed when it opened (the
// file filters, limits, source paths, the analysis dialect) and the path
// profile are read once per session: a change is applied to what reads the
// settings later and named in a log message as needing a restart.

func (s *server) didChangeConfiguration() *rpcError {
	if !s.wait() {
		return nil
	}
	s.reloadSettings()
	return nil
}

// isSettingsFile reports whether path (as the client spells it) is a settings
// file that is read: the user file or the .bash-lsp.toml at a folder's root.
func (s *server) isSettingsFile(path string) bool {
	path = filepath.Clean(path)
	base, dir := filepath.Base(path), filepath.Dir(path)
	// A name that cannot match is settled without touching the disk: only a
	// case variant can be the same file, and then only on a folding volume.
	if u, err := config.UserPath(); err == nil && u != "" && strings.EqualFold(base, filepath.Base(u)) && samePath(u, path) {
		return true
	}
	for _, f := range s.folders {
		fold := f.ws.CaseInsensitive()
		if base == ".bash-lsp.toml" || fold && strings.EqualFold(base, ".bash-lsp.toml") {
			if dir == f.client || dir == f.ws.Root() || fold && (s.sameRoot(dir, f.client) || s.sameRoot(dir, f.ws.Root())) {
				return true
			}
		}
	}
	return false
}

// samePath reports whether a and b name one file: equal when cleaned, or the
// same file on disk.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// reloadSettings reads the settings of every folder and of the loose documents
// again and applies the ones that changed.
func (s *server) reloadSettings() {
	hintsChanged := false
	if st, ok := s.reloaded("", s.looseSt); ok {
		old := s.looseSt
		hintsChanged = hintsChanged || old.InlayHints != st.InlayHints
		s.looseSt, s.looseDiag, s.looseTool = st, diagOptionsOf(st), nil
		s.applySettings(s.loose, old, st)
	}
	for _, f := range s.folders {
		if st, ok := s.reloaded(f.client, f.st); ok {
			old := f.st
			hintsChanged = hintsChanged || old.InlayHints != st.InlayHints
			f.st, f.diag, f.tool = st, diagOptionsOf(st), nil
			s.applySettings(f.ws, old, st)
		}
	}
	if hintsChanged && s.caps().InlayHintRefresh {
		s.call("workspace/inlayHint/refresh", nil)
	}
}

// reloaded loads the settings of root and returns them when they differ from
// old and no file fails that did not already fail when old was loaded (a file
// broken from the start does not stop the others from being reloaded);
// otherwise ok is false and old stays.
func (s *server) reloaded(root string, old config.Settings) (st config.Settings, ok bool) {
	st = config.Load(root, s.initOpts)
	if fresh := slices.DeleteFunc(slices.Clone(st.Failed), func(w string) bool { return slices.Contains(old.Failed, w) }); len(fresh) > 0 {
		for _, w := range fresh {
			s.log.Printf("settings: %s", w)
		}
		msg := "settings not reloaded, the previous ones stay: " + strings.Join(fresh, "; ")
		s.logMessageOnce(messageTypeWarning, msg)
		s.reloadMsg[root] = msg
		return old, false
	}
	if msg, ok := s.reloadMsg[root]; ok { // recovered: the same failure later is news again
		delete(s.reloadMsg, root)
		s.forgetMessage(msg)
	}
	if reflect.DeepEqual(st, old) {
		return old, false
	}
	s.reportSettings(st)
	if keys := restartKeys(old, st); len(keys) > 0 {
		s.logMessageOnce(messageTypeInfo, "settings: a restart of the language server is needed for "+strings.Join(keys, ", ")+" to take effect")
	}
	return st, true
}

// restartKeys names the settings that changed and that are read once per
// session.
func restartKeys(old, st config.Settings) []string {
	var keys []string
	add := func(changed bool, name string) {
		if changed {
			keys = append(keys, name)
		}
	}
	add(!slices.Equal(old.Include, st.Include), "files.include")
	add(!slices.Equal(old.Exclude, st.Exclude) || !slices.Equal(old.RepoExclude, st.RepoExclude), "files.exclude")
	add(!slices.Equal(old.SourcePaths, st.SourcePaths), "sources.paths")
	add(old.Dialect != st.Dialect, "bash.dialect (analysis; formatting follows it at once)")
	add(old.MaxFiles != st.MaxFiles, "limits.max_files")
	add(old.MaxFileBytes != st.MaxFileBytes, "limits.max_file_bytes")
	add(!slices.Equal(old.PathProfile, st.PathProfile), "path.profile")
	return keys
}

// applySettings publishes the open documents of be again under st. ShellCheck
// is dropped and rescheduled only when its settings changed; otherwise its
// findings stay and a document whose findings did not change is not sent.
func (s *server) applySettings(be backend, old, st config.Settings) {
	s.epoch++
	if old.Help != st.Help {
		delete(s.helpTools, be)
	}
	scChanged := old.ShellCheck != st.ShellCheck
	var docs []*document
	for _, d := range s.docs {
		if d.be == be {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	for _, d := range docs {
		if scChanged {
			s.resetCheck(d)
		}
		d.lint.have = false
		s.publishDoc(d, false)
		if scChanged {
			s.scheduleCheck(d)
		}
	}
}
