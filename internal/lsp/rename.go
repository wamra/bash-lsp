package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/redact"
)

// ---- rename ----
//
// textDocument/prepareRename and textDocument/rename run the engine of
// analysis (PrepareRename, PlanRename) on the snapshot and translate its
// outcome:
//
//   - a refusal is an error response (RequestFailed, -32803) whose message is
//     "<code>: <reason>" and whose data holds the stable code and the sites
//     the refusal names; there is never a partial edit;
//   - a plan becomes a WorkspaceEdit, as `changes`, or as `documentChanges`
//     (versioned text document edits) when the client supports them;
//   - sites the rename left alone (exclusions, same-named symbols, warnings)
//     ride along as one change annotation that needs confirmation, or, when
//     the client cannot show annotations, as one window/showMessage.

// codeRequestFailed is LSP 3.17 LSPErrorCodes.RequestFailed.
const codeRequestFailed = -32803

const (
	// renameAnnotationID names the change annotation of the sites a rename
	// left unchanged.
	renameAnnotationID = "bash-lsp.unrenamed"
	// maxRenameListed bounds the sites of one kind named in the annotation or
	// the message; the rest are counted.
	maxRenameListed = 50
	// maxRenameText bounds the source text shown for one excluded site, after
	// redaction. maxRenameMessage bounds a refusal message.
	maxRenameText    = 200
	maxRenameMessage = 1000
)

type renameParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	NewName      string                 `json:"newName"`
}

// prepareRenameResult is the PrepareRenameResult variant with a placeholder.
type prepareRenameResult struct {
	Range       Range  `json:"range"`
	Placeholder string `json:"placeholder"`
}

// renameTextEdit is a TextEdit; with an annotation id it is an
// AnnotatedTextEdit.
type renameTextEdit struct {
	Range        Range  `json:"range"`
	NewText      string `json:"newText"`
	AnnotationID string `json:"annotationId,omitempty"`
}

type versionedDocument struct {
	URI     string `json:"uri"`
	Version *int   `json:"version"` // null for a file the client has not opened
}

type textDocumentEdit struct {
	TextDocument versionedDocument `json:"textDocument"`
	Edits        []renameTextEdit  `json:"edits"`
}

type changeAnnotation struct {
	Label             string `json:"label"`
	NeedsConfirmation bool   `json:"needsConfirmation"`
	Description       string `json:"description"`
}

// renameEdit is a WorkspaceEdit: changes, or documentChanges and the
// annotations they cite.
type renameEdit struct {
	Changes           map[string][]renameTextEdit `json:"changes,omitempty"`
	DocumentChanges   []textDocumentEdit          `json:"documentChanges,omitempty"`
	ChangeAnnotations map[string]changeAnnotation `json:"changeAnnotations,omitempty"`
}

// failureResponse is an error response with a data member.
type failureResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   failure         `json:"error"`
}

type failure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type refusalData struct {
	Code  string     `json:"code"`
	Sites []Location `json:"sites,omitempty"`
}

// cutText cuts s to at most n bytes at a rune boundary and marks the cut.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// outward is a text for the client: redacted first, cut after.
func outward(s string, n int) string { return cutText(redact.String(s), n) }

// failRequest turns a refusal of the engine into the response of the request
// being handled, and anything else into an internal error. The response is
// sent here, because it carries a data member; the handler returns deferred.
func (s *server) failRequest(snap *analysis.Snapshot, err error) (any, *rpcError) {
	var ref *analysis.RenameRefusal
	if !errors.As(err, &ref) {
		s.log.Printf("rename: %v", err)
		return nil, &rpcError{codeInternalError, "internal error in rename"}
	}
	return s.refuseRequest(ref.Code, ref.Reason, s.locations(snap, ref.Sites), ref.Code)
}

// refuseRequest answers the current request with RequestFailed and the
// message "<code>: <reason>".
func (s *server) refuseRequest(code, reason string, sites []Location, dataCode string) (any, *rpcError) {
	if s.cur != nil && s.isCancelled(idKey(s.curID)) {
		return nil, cancelledError()
	}
	msg := outward(code+": "+reason, maxRenameMessage)
	if len(s.curID) == 0 {
		return nil, &rpcError{codeRequestFailed, msg}
	}
	s.finish(idKey(s.curID))
	s.c.send(failureResponse{JSONRPC: "2.0", ID: s.curID, Error: failure{
		Code: codeRequestFailed, Message: msg, Data: refusalData{Code: dataCode, Sites: sites}}})
	return deferred{}, nil
}

func (s *server) prepareRename(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil {
		return nil, rerr
	}
	if !ok {
		return s.refuseRequest(analysis.RefuseNotSymbol, "the document is not part of the workspace", nil, analysis.RefuseNotSymbol)
	}
	rng, name, err := snap.PrepareRename(path, off)
	if err != nil {
		return s.failRequest(snap, err)
	}
	if err := probeRename(snap, path, off); err != nil {
		return s.failRequest(snap, err)
	}
	r := s.rangeIn(converters{}, snap, path, rng)
	if !s.caps().RenamePrepare {
		return r, nil // a client without prepareSupport reads a bare Range
	}
	return prepareRenameResult{Range: r, Placeholder: redact.String(name)}, nil
}

// probeName is a valid name nothing is likely to use: a rename to it fails
// only for reasons that no other name would change.
const probeName = "bash_lsp_rename_probe"

// probeRename plans a rename of the symbol at the cursor to probeName and
// returns the refusals no new name can cure: the ones about the symbol or the
// text around its sites (half-typed regions, skipped files, dynamic scope,
// names given to eval, unset, declare -n and the like, shell-special
// variables). Refusals about the new name (conflicts, invalid, reserved) do
// not count, and neither does a plan that succeeds.
func probeRename(snap *analysis.Snapshot, path string, off int) error {
	_, err := snap.PlanRename(path, off, probeName)
	var ref *analysis.RenameRefusal
	if !errors.As(err, &ref) {
		return nil
	}
	switch ref.Code {
	case analysis.RefuseRecovered, analysis.RefuseSkippedFile, analysis.RefuseDynamicScope, analysis.RefuseNameReference:
		return err
	case analysis.RefuseConflict:
		if strings.HasPrefix(ref.Reason, "renaming a shell-special variable") {
			return err
		}
	}
	return nil
}

func (s *server) rename(raw json.RawMessage) (any, *rpcError) {
	var p renameParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil {
		return nil, rerr
	}
	if !ok {
		return s.refuseRequest(analysis.RefuseNotSymbol, "the document is not part of the workspace", nil, analysis.RefuseNotSymbol)
	}
	asked := s.docs[path]
	askedVersion := 0
	if asked != nil {
		askedVersion = asked.version
	}
	plan, err := snap.PlanRename(path, off, p.NewName)
	if err != nil {
		return s.failRequest(snap, err)
	}
	// The plan is of the snapshot. An open document it does not match (the
	// document moved on, or the snapshot has not caught up) must not get
	// edits computed for other text.
	if s.docs[path] != asked || (asked != nil && asked.version != askedVersion) || !s.planIsCurrent(snap, plan) {
		return nil, &rpcError{codeContentModified, "the document changed while the rename was planned"}
	}
	if s.cur != nil && s.isCancelled(idKey(s.curID)) {
		return nil, cancelledError()
	}
	return s.buildRenameEdit(snap, plan), nil
}

// planIsCurrent reports whether every open document the plan edits has, in the
// snapshot, the text the client last sent for it.
func (s *server) planIsCurrent(snap *analysis.Snapshot, plan *analysis.RenamePlan) bool {
	last := ""
	for _, e := range plan.Edits {
		if e.Path == last {
			continue
		}
		last = e.Path
		d := s.docs[e.Path]
		if d == nil {
			continue
		}
		if f := fileOf(snap, e.Path); f == nil || !bytes.Equal(f.Src, d.text) {
			return false
		}
	}
	return true
}

// buildRenameEdit builds the edit of a plan in the form the client supports.
func (s *server) buildRenameEdit(snap *analysis.Snapshot, plan *analysis.RenamePlan) renameEdit {
	caps := s.caps()
	note, label := s.renameNote(snap, plan)
	annotate := caps.DocumentChanges && caps.ChangeAnnotations && (len(plan.Excluded) > 0 || len(plan.Others) > 0)
	if !annotate && (len(plan.Excluded) > 0 || len(plan.Warnings) > 0) {
		s.c.notify("window/showMessage", showMessageParams{Type: messageTypeWarning, Message: note})
	}
	cs := converters{}
	var out renameEdit
	var perFile []textDocumentEdit
	for _, e := range plan.Edits {
		te := renameTextEdit{Range: s.rangeIn(cs, snap, e.Path, e.Range), NewText: e.NewText}
		if annotate {
			te.AnnotationID = renameAnnotationID
		}
		if n := len(perFile); n == 0 || perFile[n-1].TextDocument.URI != s.uriOf(e.Path) {
			td := versionedDocument{URI: s.uriOf(e.Path)}
			if d := s.docs[e.Path]; d != nil {
				v := d.version
				td.Version = &v
			}
			perFile = append(perFile, textDocumentEdit{TextDocument: td})
		}
		last := &perFile[len(perFile)-1]
		last.Edits = append(last.Edits, te)
	}
	if caps.DocumentChanges {
		out.DocumentChanges = perFile
		if annotate {
			out.ChangeAnnotations = map[string]changeAnnotation{
				renameAnnotationID: {Label: label, NeedsConfirmation: true, Description: note}}
		}
		return out
	}
	out.Changes = make(map[string][]renameTextEdit, len(perFile))
	for _, f := range perFile {
		out.Changes[f.TextDocument.URI] = append(out.Changes[f.TextDocument.URI], f.Edits...)
	}
	return out
}

// renameNote lists what the rename left alone: the label ("N sites left
// unchanged") and the text of the annotation or message. Every string that
// comes from a source file or from the client is redacted before it is cut.
func (s *server) renameNote(snap *analysis.Snapshot, plan *analysis.RenamePlan) (note, label string) {
	left := len(plan.Excluded) + len(plan.Others)
	label = fmt.Sprintf("%d sites left unchanged", left)
	var b strings.Builder
	b.WriteString("bash-lsp rename of " + redact.String(plan.Old) + ":")
	if left > 0 {
		b.WriteString(" " + label + ".")
	}
	if len(plan.Excluded) > 0 {
		b.WriteString("\nNot proven to name " + redact.String(plan.Old) + ", left as they are:")
		for i, x := range plan.Excluded {
			if i == maxRenameListed {
				fmt.Fprintf(&b, "\n  and %d more", len(plan.Excluded)-i)
				break
			}
			fmt.Fprintf(&b, "\n  %s:%d (%s, %s) %s", s.relPath(x.Loc.Path), x.Loc.Range.Start.Line, x.Res, x.Reason,
				oneLine(outward(x.Text, maxRenameText)))
		}
	}
	if len(plan.Others) > 0 {
		b.WriteString("\nOther symbols named " + redact.String(plan.Old) + ", not renamed:")
		for i, l := range plan.Others {
			if i == maxRenameListed {
				fmt.Fprintf(&b, "\n  and %d more", len(plan.Others)-i)
				break
			}
			fmt.Fprintf(&b, "\n  %s:%d", s.relPath(l.Path), l.Range.Start.Line)
		}
	}
	for _, w := range plan.Warnings {
		b.WriteString("\nWarning: " + outward(w, maxRenameMessage))
	}
	return redact.String(b.String()), label
}

// oneLine puts a source excerpt on one line.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}
