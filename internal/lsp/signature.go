package lsp

import (
	"encoding/json"
	"strconv"
	"unicode/utf16"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/redact"
)

// ---- signature help ----
//
// Field names as in LSP 3.17 (vscode-languageserver-types 3.17.2, main.d.ts:
// SignatureHelp, SignatureInformation, ParameterInformation). Only script
// functions have signature help; builtins show their synopsis in hover.

// signatureHelp is SignatureHelp: one signature, with the argument under
// the cursor as activeParameter.
type signatureHelp struct {
	Signatures      []signatureInformation `json:"signatures"`
	ActiveSignature int                    `json:"activeSignature"`
	ActiveParameter int                    `json:"activeParameter"`
}

// signatureInformation is SignatureInformation.
type signatureInformation struct {
	Label         string                 `json:"label"`
	Documentation *markupContent         `json:"documentation,omitempty"`
	Parameters    []parameterInformation `json:"parameters"`
}

// parameterInformation is ParameterInformation. Label is a [start, end]
// pair of UTF-16 offsets into the signature label when the client has
// labelOffsetSupport, else the parameter's text.
type parameterInformation struct {
	Label any `json:"label"`
}

// signatureHelpOptions is the `signatureHelpProvider` capability: help
// appears when the user types a space after a call's command word.
func signatureHelpOptions() map[string]any {
	return map[string]any{"triggerCharacters": []string{" "}, "retriggerCharacters": []string{" "}}
}

func (s *server) signatureHelp(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return nil, rerr
	}
	sig, active, found := snap.SignatureAt(path, off)
	if !found {
		return nil, nil
	}
	return signatureHelp{
		Signatures:      []signatureInformation{s.signatureInfo(sig)},
		ActiveParameter: active,
	}, nil
}

// signatureInfo renders sig. The parameters are the named positions and, for
// a variadic function, the trailing `[args...]`.
func (s *server) signatureInfo(sig analysis.FuncSignature) signatureInformation {
	out := signatureInformation{Label: sig.Label, Parameters: []parameterInformation{}}
	labels := make([]string, 0, len(sig.Params)+1)
	for _, p := range sig.Params {
		if p.Name != "" {
			labels = append(labels, p.Name)
		} else {
			labels = append(labels, "$"+strconv.Itoa(p.Index))
		}
	}
	if sig.Variadic {
		labels = append(labels, "[args...]")
	}
	at := utf16Len(sig.Name) + 1 // the label is the name, then each parameter after a space
	for _, l := range labels {
		n := utf16Len(l)
		if s.caps().LabelOffset {
			out.Parameters = append(out.Parameters, parameterInformation{Label: [2]int{at, at + n}})
		} else {
			out.Parameters = append(out.Parameters, parameterInformation{Label: l})
		}
		at += n + 1
	}
	doc := sig.Doc
	if sig.Getopts != "" {
		if doc != "" {
			doc += "\n\n"
		}
		doc += "options: " + sig.Getopts
	}
	if doc != "" {
		out.Documentation = &markupContent{Kind: "plaintext", Value: redact.String(doc)}
	}
	return out
}

// utf16Len is the length of str in UTF-16 code units.
func utf16Len(str string) int {
	n := 0
	for _, r := range str {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}
