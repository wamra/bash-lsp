package lsp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"

	"github.com/wamra/bash-lsp/analysis"
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
	sentVer int // the document version they were sent for
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
	return pubDiags(l.file, l.native)
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
			lf := lspFix{title: fx.Title, safe: fx.Safe, edits: make([]TextEdit, 0, len(fx.Edits))}
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
		return
	}
	l.pub, l.sent, l.sentVer = pub, true, d.version
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
		if d != except && d.be == be && !(d.lint.have && d.lint.gen == gen) {
			docs = append(docs, d)
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	for _, d := range docs {
		s.publishDoc(d, false)
	}
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
	return fixActions(d.uri, d.lint.pub, p.Context.Diagnostics, p.Range, p.Context.Only), nil
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
// or the kind itself.
func kindWanted(only []string, kind string) bool {
	return len(only) == 0 || slices.Contains(only, kind) || slices.Contains(only, "")
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
