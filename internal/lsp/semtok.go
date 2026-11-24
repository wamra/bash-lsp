package lsp

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

// ---- semantic tokens ----
//
// Names as in LSP 3.17: SemanticTokensOptions (legend, range, full.delta),
// SemanticTokensLegend, SemanticTokens, SemanticTokensEdit,
// SemanticTokensDelta, the requests textDocument/semanticTokens/full,
// /full/delta and /range, and workspace/semanticTokens/refresh
// (vscode-languageserver-protocol protocol.semanticTokens.d.ts; the
// modifier names `declaration`, `readonly`, `modification` and
// `defaultLibrary` are SemanticTokenModifiers in vscode-languageserver-types
// main.d.ts).

// semanticLegend is the legend of semanticTokensProvider. The types are in
// analysis.TokenType order and the modifiers in analysis.TokenMod bit order:
// the first four modifiers are standard, the others are this server's own.
var semanticLegend = map[string]any{
	"tokenTypes": []string{"function", "variable", "parameter", "macro"},
	"tokenModifiers": []string{"declaration", "readonly", "modification", "defaultLibrary",
		"local", "exported", "array", "associative", "integer", "nameref", "environment", "external", "unresolved"},
}

func semanticTokensProvider() map[string]any {
	return map[string]any{"legend": semanticLegend, "range": true, "full": map[string]any{"delta": true}}
}

type semanticTokensParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type semanticTokensDeltaParams struct {
	TextDocument     textDocumentIdentifier `json:"textDocument"`
	PreviousResultID string                 `json:"previousResultId"`
}

type semanticTokensRangeParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
}

// semanticTokens is SemanticTokens; a result for a range, or for a document
// that is not open, has no resultId.
type semanticTokens struct {
	ResultID string   `json:"resultId,omitempty"`
	Data     []uint32 `json:"data"`
}

type semanticTokensEdit struct {
	Start       int      `json:"start"`
	DeleteCount int      `json:"deleteCount"`
	Data        []uint32 `json:"data,omitempty"`
}

type semanticTokensDelta struct {
	ResultID string               `json:"resultId"`
	Edits    []semanticTokensEdit `json:"edits"`
}

// semtokState is what the server keeps for semantic tokens.
type semtokState struct {
	docSeq    int  // documents numbered so far
	refreshed bool // a workspace/semanticTokens/refresh is queued for the loop
}

// semtokDoc is a document's semantic-tokens state: its number in this session
// and the count of results sent for it (the result id is "<seq>.<n>"), and the
// last result sent, which is all a previousResultId can refer to.
type semtokDoc struct {
	seq, n int
	id     string
	data   []uint32
}

// resetTokens forgets the result the client holds for d, so that no
// previousResultId matches until a new result is sent. The counters stay: a
// result id is never issued twice for a document.
func (d *document) resetTokens() {
	d.semtok.id, d.semtok.data = "", nil
}

// nextTokensID issues the result id of d's next stored result:
// "<docSeq>.<n>", the document's number for this session and the count of results.
func (s *server) nextTokensID(d *document) string {
	if d.semtok.seq == 0 {
		s.semtok.docSeq++
		d.semtok.seq = s.semtok.docSeq
	}
	d.semtok.n++
	return fmt.Sprintf("%d.%d", d.semtok.seq, d.semtok.n)
}

// tokenTarget finds the snapshot and file a token request is about. d is the
// open document, nil for a file that is not open; f is nil when the
// snapshot does not hold the file.
func (s *server) tokenTarget(uri string) (d *document, snap *analysis.Snapshot, path string, f *tree.File, rerr *rpcError) {
	if !s.wait() {
		return nil, nil, "", nil, cancelledError()
	}
	d, rerr = s.docOf(uri)
	if rerr != nil {
		return
	}
	if d != nil {
		snap, path = d.be.Snapshot(), d.path
	} else {
		be, p, rerr2 := s.locate(uri)
		if rerr2 != nil {
			return nil, nil, "", nil, rerr2
		}
		snap, path = be.Snapshot(), p
	}
	return d, snap, path, fileOf(snap, path), nil
}

func (s *server) semanticTokensFull(raw json.RawMessage) (any, *rpcError) {
	var p semanticTokensParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	return s.fullTokens(p.TextDocument.URI)
}

// fullTokens answers a full result and, for an open document, remembers it
// as the base of the next delta.
func (s *server) fullTokens(uri string) (any, *rpcError) {
	d, snap, path, f, rerr := s.tokenTarget(uri)
	if rerr != nil {
		return nil, rerr
	}
	if f == nil {
		return semanticTokens{Data: []uint32{}}, nil
	}
	data := EncodeTokens(f.NewConverter(), snap.Tokens(path, nil))
	res := semanticTokens{Data: data}
	if d != nil {
		res.ResultID = s.nextTokensID(d)
		d.semtok.id, d.semtok.data = res.ResultID, data
	}
	return res, nil
}

func (s *server) semanticTokensDelta(raw json.RawMessage) (any, *rpcError) {
	var p semanticTokensDeltaParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	d, snap, path, f, rerr := s.tokenTarget(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	if f == nil {
		return semanticTokens{Data: []uint32{}}, nil
	}
	data := EncodeTokens(f.NewConverter(), snap.Tokens(path, nil))
	if d == nil {
		return semanticTokens{Data: data}, nil
	}
	// A delta only against the exact result this document last sent.
	prev, base := d.semtok.id, d.semtok.data
	id := s.nextTokensID(d)
	d.semtok.id, d.semtok.data = id, data
	if p.PreviousResultID == "" || p.PreviousResultID != prev || base == nil {
		return semanticTokens{ResultID: id, Data: data}, nil
	}
	edits := []semanticTokensEdit{}
	if start, del, ins := DiffTokens(base, data); del > 0 || len(ins) > 0 {
		edits = append(edits, semanticTokensEdit{Start: start, DeleteCount: del, Data: ins})
	}
	return semanticTokensDelta{ResultID: id, Edits: edits}, nil
}

func (s *server) semanticTokensRange(raw json.RawMessage) (any, *rpcError) {
	var p semanticTokensRangeParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	_, snap, path, f, rerr := s.tokenTarget(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	if f == nil {
		return semanticTokens{Data: []uint32{}}, nil
	}
	r := tree.Range{Start: f.FromUTF16(p.Range.Start.Line, p.Range.Start.Character),
		End: f.FromUTF16(p.Range.End.Line, p.Range.End.Character)}
	return semanticTokens{Data: EncodeTokens(f.NewConverter(), snap.Tokens(path, &r))}, nil
}

// EncodeTokens is the LSP encoding of toks (sorted, single-line): five
// integers per token, the line and column relative to the token before
// (the first to 0:0), the length, type and modifier bits, all in UTF-16 units.
func EncodeTokens(c *tree.Converter, toks []analysis.Token) []uint32 {
	data := make([]uint32, 0, 5*len(toks))
	pl, pc := 0, 0
	for _, t := range toks {
		line, col := c.ToUTF16(t.Range.Start)
		_, end := c.ToUTF16(t.Range.End)
		dl, dc := line-pl, col
		if dl == 0 {
			dc = col - pc
		}
		data = append(data, uint32(dl), uint32(dc), uint32(end-col), uint32(t.Type), uint32(t.Mods))
		pl, pc = line, col
	}
	return data
}

// DiffTokens is the one edit that turns prev into next: the tuples the two
// share at the start and at the end stay, the rest is replaced. start and
// deleteCount are multiples of 5. With nothing to change, deleteCount is 0
// and data is empty.
func DiffTokens(prev, next []uint32) (start, deleteCount int, data []uint32) {
	pre := 0
	for pre+5 <= len(prev) && pre+5 <= len(next) && slices.Equal(prev[pre:pre+5], next[pre:pre+5]) {
		pre += 5
	}
	suf := 0
	for suf+5 <= len(prev)-pre && suf+5 <= len(next)-pre &&
		slices.Equal(prev[len(prev)-suf-5:len(prev)-suf], next[len(next)-suf-5:len(next)-suf]) {
		suf += 5
	}
	return pre, len(prev) - pre - suf, next[pre : len(next)-suf]
}

// refreshSemanticTokens asks a client that supports it to ask for the tokens
// of its open documents again, after the current message has been handled;
// one request covers every call made meanwhile. Called when other files
// changed under open documents (refreshOthers).
func (s *server) refreshSemanticTokens() {
	if !s.caps().SemanticTokensRefresh || s.semtok.refreshed {
		return
	}
	s.semtok.refreshed = true
	go s.post(func() { // post may not be called from the loop itself
		s.semtok.refreshed = false
		s.call("workspace/semanticTokens/refresh", nil)
	})
}
