package lsp

import (
	"encoding/json"
	slashpath "path"
	"path/filepath"
	"sort"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/tree"
)

// ---- folding, selection, highlights and links ----
//
// Method strings, capability keys and structure fields as in
// vscode-languageserver-protocol and vscode-languageserver-types 3.17.2
// (main.d.ts: FoldingRangeKind "comment" | "imports" | "region",
// DocumentHighlightKind Text 1 / Read 2 / Write 3, DocumentLink {range,
// target?, tooltip?, data?}, SelectionRange {range, parent?}; protocol.d.ts:
// documentHighlightProvider, documentLinkProvider, foldingRangeProvider,
// selectionRangeProvider).

const (
	highlightRead  = 2
	highlightWrite = 3
)

type foldingRange struct {
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Kind      string `json:"kind,omitempty"`
}

type selectionRange struct {
	Range  Range           `json:"range"`
	Parent *selectionRange `json:"parent,omitempty"`
}

type documentHighlight struct {
	Range Range `json:"range"`
	Kind  int   `json:"kind"`
}

type documentLink struct {
	Range   Range  `json:"range"`
	Target  string `json:"target"`
	Tooltip string `json:"tooltip,omitempty"`
}

type foldingRangeParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

// maxSelectionPositions is the most positions of one selectionRange request
// that get a chain; the answer still has one entry per position.
const maxSelectionPositions = 1000

type selectionRangeParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Positions    []Position             `json:"positions"`
}

// lineFold is a fold in LSP lines, 0-based and inclusive.
type lineFold struct {
	StartLine, EndLine int
	Kind               string
}

// lspFolds maps folds given as byte offsets of f to LSP lines, in start order
// with enclosing folds first, dropping repeats.
func lspFolds(f *tree.File, folds []analysis.Fold) []lineFold {
	out := make([]lineFold, 0, len(folds))
	seen := map[lineFold]bool{}
	for _, fo := range folds {
		lf := lineFold{f.LSPLine(fo.StartOffset), f.LSPLine(fo.EndOffset), fo.Kind}
		if lf.EndLine > lf.StartLine && !seen[lf] {
			seen[lf] = true
			out = append(out, lf)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].StartLine != out[b].StartLine {
			return out[a].StartLine < out[b].StartLine
		}
		return out[a].EndLine > out[b].EndLine
	})
	return out
}

// foldingRanges makes the wire folds. With lineFoldingOnly the client folds
// whole lines and shows the closing line, so a fold ends on the line before
// the closing keyword or bracket (comment and region folds keep their end). A
// fold left with one line is dropped. With a range limit at most that many
// are returned, outermost first (by nesting depth, then in source order), and
// listed in source order.
func foldingRanges(folds []lineFold, c clientCaps) []foldingRange {
	out := make([]foldingRange, 0, len(folds))
	for _, f := range folds {
		end := f.EndLine
		if c.LineFoldingOnly && f.Kind == "" {
			end--
		}
		if end > f.StartLine {
			out = append(out, foldingRange{StartLine: f.StartLine, EndLine: end, Kind: f.Kind})
		}
	}
	if c.RangeLimit <= 0 || len(out) <= c.RangeLimit {
		return out
	}
	// Outermost first: by nesting depth, then in source order. out is in start
	// order with enclosing folds before the folds inside them, so a stack of
	// the folds still open gives each fold's depth.
	depth := make([]int, len(out))
	var open []int
	for i, f := range out {
		for len(open) > 0 && out[open[len(open)-1]].EndLine < f.EndLine {
			open = open[:len(open)-1]
		}
		depth[i] = len(open)
		open = append(open, i)
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return depth[idx[a]] < depth[idx[b]] })
	idx = idx[:min(c.RangeLimit, len(idx))]
	sort.Ints(idx)
	kept := make([]foldingRange, len(idx))
	for i, k := range idx {
		kept[i] = out[k]
	}
	return kept
}

func (s *server) foldingRange(raw json.RawMessage) (any, *rpcError) {
	var p foldingRangeParams
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
	snap := be.Snapshot()
	f := fileOf(snap, path)
	if f == nil {
		return []foldingRange{}, nil
	}
	return foldingRanges(lspFolds(f, snap.Folds(path)), s.caps()), nil
}

// interpreterInProfile reports whether target, a plain slash-form absolute
// path (analysis.Shebang), is in a directory of the PATH profile. A path that
// is not absolute on this platform is in none.
func (s *server) interpreterInProfile(target string) bool {
	return s.pathProfile().HasDir(filepath.FromSlash(slashpath.Dir(target)))
}

// handlerCancelled reports whether the request being handled was cancelled
// since the handler began, for a handler to check between steps.
func (s *server) handlerCancelled() bool {
	if s.cur == nil {
		return false
	}
	select {
	case <-s.cur.done:
		return true
	default:
		return false
	}
}

// cutNotice tells the client, once per session, that an answer was cut at its
// bound.
func (s *server) cutNotice(msg string) { s.logMessageOnce(messageTypeInfo, msg) }

func (s *server) selectionRange(raw json.RawMessage) (any, *rpcError) {
	var p selectionRangeParams
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
	snap := be.Snapshot()
	f := fileOf(snap, path)
	out := make([]*selectionRange, 0, len(p.Positions))
	cs := converters{}
	for i, pos := range p.Positions {
		if s.handlerCancelled() {
			return nil, cancelledError()
		}
		if f == nil || i >= maxSelectionPositions {
			if f != nil && i == maxSelectionPositions {
				s.cutNotice("selectionRange: a request holds more positions than are answered; each position past the first 1000 gets an empty range")
			}
			out = append(out, &selectionRange{Range: Range{Start: pos, End: pos}})
			continue
		}
		chain := snap.SelectionChain(path, f.FromUTF16(pos.Line, pos.Character).Offset)
		if len(chain) == 0 {
			out = append(out, &selectionRange{Range: Range{Start: pos, End: pos}})
			continue
		}
		var parent *selectionRange
		for i := len(chain) - 1; i >= 0; i-- {
			parent = &selectionRange{Range: cs.rangeOf(f, chain[i]), Parent: parent}
		}
		out = append(out, parent)
	}
	return out, nil
}

func (s *server) documentHighlight(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return nil, rerr
	}
	hs, cut := snap.Highlights(path, off)
	if cut {
		s.cutNotice("documentHighlight: the symbol has more occurrences in this file than one answer holds; the rest are left out")
	}
	if len(hs) == 0 {
		return nil, nil
	}
	cs := converters{}
	out := make([]documentHighlight, 0, len(hs))
	for _, h := range hs {
		kind := highlightRead
		if h.Write {
			kind = highlightWrite
		}
		out = append(out, documentHighlight{Range: s.rangeIn(cs, snap, path, h.Range), Kind: kind})
	}
	return out, nil
}

func (s *server) documentLink(raw json.RawMessage) (any, *rpcError) {
	var p foldingRangeParams
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
	snap := be.Snapshot()
	links, cut := snap.Links(path)
	if cut {
		s.cutNotice("documentLink: the file has more links than one answer holds; the rest are left out")
	}
	cs := converters{}
	out := make([]documentLink, 0, len(links)+1)
	// The shebang interpreter: the path is the file's text, so it is linked
	// only when it sits in a directory of the PATH profile this server
	// already holds. Nothing on the file system is looked at.
	if l, ok := snap.Shebang(path); ok && s.interpreterInProfile(l.Target) {
		links = append([]analysis.Link{l}, links...)
	}
	for _, l := range links {
		out = append(out, documentLink{Range: s.rangeIn(cs, snap, path, l.Range), Target: s.uriOf(l.Target), Tooltip: l.Tooltip})
	}
	return out, nil
}
