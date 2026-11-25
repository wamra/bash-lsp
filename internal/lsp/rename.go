package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	// maxRenameNote bounds the whole text of the annotation or the message.
	maxRenameNote = 4096
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
	if _, err := probeRename(snap, path, off); err != nil {
		return s.failRequest(snap, err)
	}
	r := s.rangeIn(converters{}, snap, path, rng)
	if !s.caps().RenamePrepare {
		return r, nil // a client without prepareSupport reads a bare Range
	}
	return prepareRenameResult{Range: r, Placeholder: redact.String(name)}, nil
}

// probeName is a valid name that a workspace is unlikely to use. A workspace
// that does use it can make the probe fail on a name conflict, which hides
// the refusals the probe looks for: prepareRename then accepts and rename
// decides.
const probeName = "bash_lsp_rename_probe"

// probeRename plans a rename of the symbol at the cursor to probeName. It
// returns the plan, or the refusals no new name can cure (the engine marks
// them NameIndependent: half-typed regions, skipped files, dynamic scope,
// names given to eval, unset, declare -n and the like, shell-special
// variables). Refusals about the new name do not count: the plan is then nil.
func probeRename(snap *analysis.Snapshot, path string, off int) (*analysis.RenamePlan, error) {
	plan, err := snap.PlanRename(path, off, probeName)
	if err == nil {
		return plan, nil
	}
	var ref *analysis.RenameRefusal
	if errors.As(err, &ref) && !ref.NameIndependent {
		return nil, nil
	}
	return nil, err
}

// currentText is the text the client has of path: the open document's, else
// the file on disk. stale is true when it is not the text of the snapshot (the
// document moved on, the snapshot lags, the file changed on disk, or it cannot
// be read). The snapshot of an open document equals its text; a closed file
// is read once, after a stat that spares the read when the size differs.
func (s *server) currentText(snap *analysis.Snapshot, path string) (text []byte, stale bool) {
	f := fileOf(snap, path)
	if f == nil {
		return nil, true
	}
	if d := s.docs[path]; d != nil {
		return d.text, !bytes.Equal(f.Src, d.text)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != int64(len(f.Src)) {
		return nil, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, true
	}
	return b, !bytes.Equal(f.Src, b)
}

var errContentModified = &rpcError{codeContentModified, "a file changed after the workspace was analysed; try the rename again"}

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
	// The handler runs on the loop, so no didChange comes between the cursor
	// and the answer. What can differ is the snapshot against what the client
	// has: checkTexts compares every file the answer would edit.
	texts := map[string][]byte{}
	if rerr := s.checkText(snap, path, texts); rerr != nil {
		return nil, rerr
	}
	plan, err := snap.PlanRename(path, off, p.NewName)
	if err != nil {
		return s.failRequest(snap, err)
	}
	for _, e := range plan.Edits {
		if _, done := texts[e.Path]; done {
			continue
		}
		if rerr := s.checkText(snap, e.Path, texts); rerr != nil {
			return nil, rerr
		}
	}
	if s.cur != nil && s.isCancelled(idKey(s.curID)) {
		return nil, cancelledError()
	}
	return s.buildRenameEdit(snap, plan), nil
}

// checkText records the client's text of path in texts, or answers
// ContentModified when it is not the snapshot's.
func (s *server) checkText(snap *analysis.Snapshot, path string, texts map[string][]byte) *rpcError {
	text, stale := s.currentText(snap, path)
	if stale {
		return errContentModified
	}
	texts[path] = text
	return nil
}

// buildRenameEdit builds the edit of a plan in the form the client supports.
func (s *server) buildRenameEdit(snap *analysis.Snapshot, plan *analysis.RenamePlan) renameEdit {
	caps := s.caps()
	annotate := caps.DocumentChanges && caps.ChangeAnnotations && (len(plan.Excluded) > 0 || len(plan.Others) > 0)
	note, label := s.renameNote(snap, plan)
	if !annotate && (len(plan.Excluded) > 0 || len(plan.Warnings) > 0 || len(plan.Others) > 0) {
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

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// renameNote lists what the rename left alone: the label and the text of the
// annotation or message. Every string that comes from a source file or from
// the client is redacted before it is cut, and the whole note once more
// after it is composed; the note is at most maxRenameNote bytes, with a
// closing "and N more" line where a list was cut.
func (s *server) renameNote(snap *analysis.Snapshot, plan *analysis.RenamePlan) (note, label string) {
	old := redact.String(plan.Old)
	switch {
	case len(plan.Excluded) > 0:
		label = plural(len(plan.Excluded), "site", "sites") + " left unchanged"
	case len(plan.Others) > 0:
		label = plural(len(plan.Others), "other symbol", "other symbols") + " named " + old + " not renamed"
	}
	var b strings.Builder
	b.WriteString("bash-lsp rename of " + old + ":")
	if label != "" {
		b.WriteString(" " + label + ".")
	}
	list := func(title string, n int, line func(i int) string) {
		b.WriteString("\n" + title)
		for i := 0; i < n; i++ {
			l := line(i)
			if i == maxRenameListed || b.Len()+len(l)+48 > maxRenameNote {
				fmt.Fprintf(&b, "\n  and %d more", n-i)
				return
			}
			b.WriteString("\n  " + l)
		}
	}
	if len(plan.Excluded) > 0 {
		list("Not proven to name "+old+", left as they are:", len(plan.Excluded), func(i int) string {
			x := plan.Excluded[i]
			return fmt.Sprintf("%s:%d (%s, %s) %s", s.relPath(x.Loc.Path), x.Loc.Range.Start.Line, x.Res, x.Reason,
				oneLine(outward(x.Text, maxRenameText)))
		})
	}
	if len(plan.Others) > 0 {
		list(plural(len(plan.Others), "other symbol", "other symbols")+" named "+old+", not renamed:", len(plan.Others), func(i int) string {
			return fmt.Sprintf("%s:%d", s.relPath(plan.Others[i].Path), plan.Others[i].Range.Start.Line)
		})
	}
	for _, w := range plan.Warnings {
		if b.Len()+len(w) > maxRenameNote-48 {
			b.WriteString("\nWarning: more warnings not shown")
			break
		}
		b.WriteString("\nWarning: " + outward(w, maxRenameMessage))
	}
	return cutText(redact.String(b.String()), maxRenameNote), label
}

// oneLine puts a source excerpt on one line.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}
