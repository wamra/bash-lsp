package lsp

import (
	"encoding/json"

	"github.com/wamra/bash-lsp/internal/redact"
	"github.com/wamra/bash-lsp/tree"
)

// ---- inlay hints ----
//
// Field names as in LSP 3.17 (InlayHint, InlayHintParams, InlayHintKind).

// inlayHintKindParameter is InlayHintKind.Parameter.
const inlayHintKindParameter = 2

type inlayHintParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
}

// inlayHint is InlayHint with a plain string label.
type inlayHint struct {
	Position     Position `json:"position"`
	Label        string   `json:"label"`
	Kind         int      `json:"kind,omitempty"`
	PaddingLeft  bool     `json:"paddingLeft,omitempty"`
	PaddingRight bool     `json:"paddingRight,omitempty"`
}

func (s *server) inlayHint(raw json.RawMessage) (any, *rpcError) {
	var p inlayHintParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	if !s.wait() {
		return nil, cancelledError()
	}
	be, path, rerr := s.locate(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	if d, rerr := s.docOf(p.TextDocument.URI); rerr != nil {
		return nil, rerr
	} else if d != nil {
		be, path = d.be, d.path
	}
	out := []inlayHint{}
	st := s.settingsOf(be).InlayHints
	snap := be.Snapshot()
	f := fileOf(snap, path)
	if f == nil || !(st.SourceTargets || st.ParameterNames) {
		return out, nil
	}
	r := tree.Range{Start: f.FromUTF16(p.Range.Start.Line, p.Range.Start.Character),
		End: f.FromUTF16(p.Range.End.Line, p.Range.End.Character)}
	conv := f.NewConverter()
	for _, h := range snap.Hints(path, r, st.SourceTargets, st.ParameterNames) {
		line, col := conv.ToUTF16(h.Pos)
		x := inlayHint{Position: Position{Line: line, Character: col}, Label: redact.String(h.Label)}
		if h.Param {
			x.Kind, x.PaddingRight = inlayHintKindParameter, true
		} else {
			x.PaddingLeft = true
		}
		out = append(out, x)
	}
	return out, nil
}
