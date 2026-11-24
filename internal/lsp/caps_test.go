package lsp

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func initServer(t *testing.T, params string) *server {
	t.Helper()
	s := newServer(strings.NewReader(""), io.Discard, io.Discard, "test")
	if _, rerr := s.initialize(json.RawMessage(params)); rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}
	return s
}

func TestClientCapsDecoded(t *testing.T) {
	full := `{"capabilities":{
	  "textDocument":{
	    "rename":{"prepareSupport":true},
	    "signatureHelp":{"signatureInformation":{"parameterInformation":{"labelOffsetSupport":true}}},
	    "foldingRange":{"lineFoldingOnly":true,"rangeLimit":5000}},
	  "workspace":{
	    "workspaceEdit":{"documentChanges":true,"changeAnnotationSupport":{"groupsOnLabel":true}},
	    "semanticTokens":{"refreshSupport":true},
	    "inlayHint":{"refreshSupport":true}}}}`
	want := clientCaps{RenamePrepare: true, DocumentChanges: true, ChangeAnnotations: true, LabelOffset: true,
		LineFoldingOnly: true, RangeLimit: 5000, SemanticTokensRefresh: true, InlayHintRefresh: true}
	if got := initServer(t, full).caps(); got != want {
		t.Errorf("caps = %+v, want %+v", got, want)
	}
	if got := initServer(t, `{"capabilities":{}}`).caps(); got != (clientCaps{}) {
		t.Errorf("empty caps = %+v, want zero", got)
	}
	// changeAnnotationSupport counts when present, even as an empty object.
	if got := initServer(t, `{"capabilities":{"workspace":{"workspaceEdit":{"changeAnnotationSupport":{}}}}}`).caps(); !got.ChangeAnnotations {
		t.Error("empty changeAnnotationSupport object should set ChangeAnnotations")
	}
	if got := initServer(t, `{"capabilities":{"workspace":{"workspaceEdit":{"changeAnnotationSupport":null}}}}`).caps(); got.ChangeAnnotations {
		t.Error("null changeAnnotationSupport should not set ChangeAnnotations")
	}
}

func TestResponseLogNamesMethod(t *testing.T) {
	var logs syncBuffer
	s := newServer(strings.NewReader(""), io.Discard, &logs, "test")
	s.pending = map[string]string{"bash-lsp/1": "workspace/semanticTokens/refresh", "bash-lsp/2": "client/registerCapability"}
	errBody := json.RawMessage(`{"code":-32601,"message":"nope"}`)
	s.response(message{ID: json.RawMessage(`"bash-lsp/1"`), Error: errBody})
	out := logs.String()
	if !strings.Contains(out, "workspace/semanticTokens/refresh failed") || !strings.Contains(out, "nope") {
		t.Errorf("log %q does not name the method and error", out)
	}
	if strings.Contains(out, "changed outside the editor") {
		t.Errorf("log %q carries the watcher wording", out)
	}
	s.response(message{ID: json.RawMessage(`"bash-lsp/2"`), Error: errBody})
	if !strings.Contains(logs.String(), "files changed outside the editor are seen only on didSave or reopen") {
		t.Errorf("the watcher failure lost its wording: %q", logs.String())
	}
}
