package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

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

// semtokRefreshDelay is the quiet time after the last change that affects
// open documents before the client is asked to refresh their tokens.
const semtokRefreshDelay = 250 * time.Millisecond

// semtokRefreshMaxWait bounds the debounce: under continuous edits the
// refresh request goes out at most this long after the first pending document.
const semtokRefreshMaxWait = 2 * time.Second

// semtokState is what the server keeps for semantic tokens.
type semtokState struct {
	docSeq  int                // documents numbered so far
	pending map[*document]bool // documents whose tokens may have changed since the client last asked
	timer   *time.Timer        // the trailing debounce of the refresh request (loop only)
	delay   time.Duration      // the debounce; zero means semtokRefreshDelay (tests set it)
	maxWait time.Duration      // the longest the debounce may be extended; zero means semtokRefreshMaxWait (tests set it)
	first   time.Time          // when the oldest pending document was queued (loop only)
}

// semtokDoc is a document's semantic-tokens state: its number in this session
// and the count of results sent for it (the result id is "<seq>.<n>"), and the
// last result sent, which is all a previousResultId can refer to.
type semtokDoc struct {
	seq, n int
	id     string
	data   []uint32
}

// dropTokenID forgets the result id of d, so that no previousResultId matches
// until a new result is sent. The counters stay (a result id is never issued
// twice for a document) and so does the data the client holds, which the
// refresh check compares with the tokens now; no delta is made from it
// without a matching id.
func (d *document) dropTokenID() { d.semtok.id = "" }

// clearTokens forgets everything the client was sent for d.
func (d *document) clearTokens() { d.semtok.id, d.semtok.data = "", nil }

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
		if d != nil {
			d.clearTokens()
		}
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
		if d != nil {
			d.clearTokens()
		}
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
	d, snap, path, f, rerr := s.tokenTarget(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	if f == nil {
		if d != nil {
			d.clearTokens()
		}
		return semanticTokens{Data: []uint32{}}, nil
	}
	r := tree.Range{Start: f.FromUTF16(p.Range.Start.Line, p.Range.Start.Character),
		End: f.FromUTF16(p.Range.End.Line, p.Range.End.Character)}
	if r.Start.Offset > r.End.Offset {
		return semanticTokens{Data: []uint32{}}, nil
	}
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

// refreshSemanticTokens notes that the tokens of docs may have changed
// (another file changed under them, or they moved to another backend) and
// queues one refresh request for the client, sent after the changes have been
// quiet for the debounce delay. Only documents the client has received tokens
// for count. Loop only.
func (s *server) refreshSemanticTokens(docs ...*document) {
	if !s.caps().SemanticTokensRefresh {
		return
	}
	queued := false
	for _, d := range docs {
		if d.semtok.data == nil {
			continue
		}
		if s.semtok.pending == nil {
			s.semtok.pending = map[*document]bool{}
		}
		s.semtok.pending[d] = true
		queued = true
	}
	if !queued {
		return
	}
	delay, maxWait := s.semtok.delay, s.semtok.maxWait
	if delay <= 0 {
		delay = semtokRefreshDelay
	}
	if maxWait <= 0 {
		maxWait = semtokRefreshMaxWait
	}
	now := time.Now()
	if s.semtok.first.IsZero() {
		s.semtok.first = now
	}
	delay = max(min(delay, s.semtok.first.Add(maxWait).Sub(now)), 0)
	if s.semtok.timer != nil {
		s.semtok.timer.Reset(delay)
		return
	}
	t := time.AfterFunc(delay, func() {
		if s.baseCtx.Err() == nil {
			s.post(s.flushTokenRefresh) // the timer's goroutine is not the loop
		}
	})
	context.AfterFunc(s.baseCtx, func() { t.Stop() })
	s.semtok.timer = t
}

// flushTokenRefresh sends workspace/semanticTokens/refresh when the tokens of
// a pending document, as they are now, differ from what the client holds.
func (s *server) flushTokenRefresh() {
	s.semtok.first = time.Time{}
	docs := make([]*document, 0, len(s.semtok.pending))
	for d := range s.semtok.pending {
		docs = append(docs, d)
	}
	clear(s.semtok.pending)
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	for _, d := range docs {
		if s.docs[d.path] != d || d.semtok.data == nil {
			continue // closed since
		}
		snap := d.be.Snapshot()
		var now []uint32
		if f := fileOf(snap, d.path); f != nil {
			now = EncodeTokens(f.NewConverter(), snap.Tokens(d.path, nil))
		}
		if !slices.Equal(now, d.semtok.data) {
			s.call("workspace/semanticTokens/refresh", nil)
			return
		}
	}
}
