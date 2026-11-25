package lsp

import (
	"math"
	"unicode/utf8"

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

func TestClientCapsRefreshFlagsAreDistinct(t *testing.T) {
	sem := initServer(t, `{"capabilities":{"workspace":{"semanticTokens":{"refreshSupport":true}}}}`).caps()
	if !sem.SemanticTokensRefresh || sem.InlayHintRefresh {
		t.Errorf("semanticTokens only: %+v", sem)
	}
	inl := initServer(t, `{"capabilities":{"workspace":{"inlayHint":{"refreshSupport":true}}}}`).caps()
	if inl.SemanticTokensRefresh || !inl.InlayHintRefresh {
		t.Errorf("inlayHint only: %+v", inl)
	}
}

func TestMalformedOptionalCapabilityDoesNotFailInitialize(t *testing.T) {
	var logs syncBuffer
	s := newServer(strings.NewReader(""), io.Discard, &logs, "test")
	params := `{"capabilities":{
	  "textDocument":{"rename":{"prepareSupport":"yes"},"foldingRange":{"rangeLimit":1e30,"lineFoldingOnly":true},"signatureHelp":7},
	  "workspace":{"workspaceEdit":5,"semanticTokens":{"refreshSupport":true}}}}`
	if _, rerr := s.initialize(json.RawMessage(params)); rerr != nil {
		t.Fatalf("initialize failed: %v", rerr)
	}
	got := s.caps()
	if got.RenamePrepare || !got.LineFoldingOnly || !got.SemanticTokensRefresh {
		t.Errorf("well-formed siblings lost: %+v", got)
	}
	if got.RangeLimit < 0 {
		t.Errorf("RangeLimit %d", got.RangeLimit)
	}
	if !strings.Contains(logs.String(), "client capabilities") {
		t.Errorf("malformed capability not logged: %q", logs.String())
	}
}

func TestFoldingRangeLimitHuge(t *testing.T) {
	folds := []lineFold{{StartLine: 0, EndLine: 3}, {StartLine: 1, EndLine: 2}}
	if got := foldingRanges(folds, clientCaps{RangeLimit: math.MaxInt}); len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestResponseLogIsRedactedAndRuneSafe(t *testing.T) {
	var logs syncBuffer
	s := newServer(strings.NewReader(""), io.Discard, &logs, "test")
	s.pending = map[string]string{"bash-lsp/1": "workspace/semanticTokens/refresh", "bash-lsp/2": "workspace/semanticTokens/refresh"}
	secret := "ghp_" + strings.Repeat("a", 36)
	s.response(message{ID: json.RawMessage(`"bash-lsp/1"`), Error: json.RawMessage(`{"code":1,"message":"token=` + secret + `"}`)})
	if strings.Contains(logs.String(), secret) {
		t.Errorf("secret in log: %q", logs.String())
	}
	// A 3-byte rune straddling the 200-byte cut.
	body := `{"message":"` + strings.Repeat("a", 186) + strings.Repeat("€", 20) + `"}`
	s.response(message{ID: json.RawMessage(`"bash-lsp/2"`), Error: json.RawMessage(body)})
	if !utf8.ValidString(logs.String()) {
		t.Errorf("log is not valid UTF-8: %q", logs.String())
	}
}
