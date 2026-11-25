package lsp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/helpdoc"
	"github.com/wamra/bash-lsp/internal/redact"
)

// ---- hover synopsis ----
//
// A hover on a command word that no function, alias or variable explains adds
// the synopsis the machine itself holds: `help` of bash for a builtin, man
// for a program. A cached answer is given at once. Otherwise the handler
// returns `deferred`, the lookup runs on its own goroutine under a context
// that the request's cancellation ends (which kills the child), and the
// answer is posted back to the loop as for formatting.

// helpFor returns the lookup of the folder be, nil when its settings turn help
// off. Loop only. The lookup is dropped with the tools when the folders change
// and when its settings change.
func (s *server) helpFor(be backend) *helpdoc.Lookup {
	st := s.settingsOf(be)
	if !st.Help.Enabled {
		return nil
	}
	if l := s.helpTools[be]; l != nil {
		return l
	}
	if s.helpTools == nil {
		s.helpTools = map[backend]*helpdoc.Lookup{}
	}
	l := helpdoc.New(s.helpRun, st, s.pathProfile(), s.workspaceRoots())
	l.SetLogger(s.log.Printf)
	s.helpTools[be] = l
	return l
}

// helpHover answers a hover on a word that has no hover of its own.
func (s *server) helpHover(uri string, snap *analysis.Snapshot, path string, off int) (any, *rpcError) {
	name, wrng, ok := snap.CommandWordAt(path, off)
	if !ok {
		return nil, nil
	}
	be, _, rerr := s.locate(uri)
	if rerr != nil {
		return nil, rerr
	}
	l := s.helpFor(be)
	if l == nil {
		return nil, nil
	}
	kind := "command"
	if analysis.IsBuiltin(name) {
		kind = "builtin"
	}
	rng := s.rangeIn(converters{}, snap, path, wrng)
	// The cache first: it holds only names that passed Allowed, and an answer
	// from it costs no PATH lookup or symlink resolution.
	if doc, found, known := l.Cached(kind, name); known {
		if !found {
			return nil, nil
		}
		return helpHoverResult(doc, rng), nil
	}
	if !l.Allowed(kind, name) {
		return nil, nil // refused here, on the loop: no goroutine for it
	}
	d, rerr := s.docOf(uri) // nil for a file that is not open
	if rerr != nil {
		return nil, rerr
	}
	version := 0
	if d != nil {
		version = d.version
	}
	id, lr := s.curID, s.cur
	cancelled := make(<-chan struct{})
	if lr != nil {
		cancelled = lr.done
	}
	go s.runHelp(l, kind, name, rng, d, version, id, cancelled)
	return deferred{}, nil
}

type helpResult struct {
	doc      helpdoc.Doc
	found    bool
	panicked any
}

// runHelp runs on its own goroutine and touches no server state.
func (s *server) runHelp(l *helpdoc.Lookup, kind, name string, rng Range, d *document, version int, id json.RawMessage, cancelled <-chan struct{}) {
	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel() // ends the child of a lookup that is still running
	res := make(chan helpResult, 1)
	go func() { res <- lookupHelp(ctx, l, kind, name) }()
	select {
	case r := <-res:
		s.post(func() { s.finishHelp(d, version, id, rng, r) })
	case <-cancelled:
		s.post(func() { s.answer(id, nil, cancelledError()) })
	case <-s.baseCtx.Done():
	}
}

// lookupHelp runs the lookup and turns a panic into a result.
func lookupHelp(ctx context.Context, l *helpdoc.Lookup, kind, name string) (r helpResult) {
	defer func() {
		if p := recover(); p != nil {
			r = helpResult{panicked: p}
		}
	}()
	if kind == "builtin" {
		r.doc, r.found = l.Builtin(ctx, name)
	} else {
		r.doc, r.found = l.Command(ctx, name)
	}
	return r
}

// finishHelp answers a hover on the loop.
func (s *server) finishHelp(d *document, version int, id json.RawMessage, rng Range, r helpResult) {
	switch {
	case s.isCancelled(idKey(id)):
		s.answer(id, nil, cancelledError())
	case r.panicked != nil:
		s.log.Printf("panic looking up help: %v", r.panicked)
		s.answer(id, nil, &rpcError{codeInternalError, "internal error in hover"})
	case d != nil && (s.docs[d.path] != d || d.version != version):
		s.answer(id, nil, &rpcError{codeContentModified, "the document changed while the help text was being read"})
	case !r.found:
		s.answer(id, nil, nil)
	default:
		s.answer(id, helpHoverResult(r.doc, rng), nil)
	}
}

// helpHoverResult is the hover text: the synopsis in a bash fence and where
// it came from, redacted like every outward text.
func helpHoverResult(d helpdoc.Doc, rng Range) hover {
	return hover{Contents: helpMarkup(d), Range: rng}
}

// helpMarkup is the synopsis as Markdown.
func helpMarkup(d helpdoc.Doc) markupContent {
	syn := strings.ReplaceAll(d.Synopsis, "```", "'''")
	md := "```bash\n" + syn + "\n```\n(from " + d.Source + " on this machine)"
	return markupContent{Kind: "markdown", Value: redact.String(md)}
}
