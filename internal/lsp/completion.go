package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/helpdoc"
	"github.com/wamra/bash-lsp/internal/redact"
)

// ---- completion ----
//
// Field names as in LSP 3.17 (CompletionList, CompletionItem, CompletionItemKind).
// Completion starts no program: script symbols come from the snapshot, the
// commands of the PATH profile from its names, the options of a command from
// a help text that an earlier resolve or hover has already cached.
// completionItem/resolve is the only path that may start `help` or `man`, and
// like hover it answers `deferred` while the lookup runs.

// CompletionItemKind values (3.17).
const (
	itemFunction = 3
	itemVariable = 6
	itemModule   = 9 // an alias
	itemProperty = 10
	itemKeyword  = 14
	itemFile     = 17
	itemFolder   = 19
	itemConstant = 21 // a builtin
	itemOperator = 24
)

// completionItem is CompletionItem; Data comes back in the resolve request.
type completionItem struct {
	Label         string         `json:"label"`
	Kind          int            `json:"kind"`
	Detail        string         `json:"detail,omitempty"`
	Documentation *markupContent `json:"documentation,omitempty"`
	SortText      string         `json:"sortText"`
	TextEdit      TextEdit       `json:"textEdit"`
	Data          any            `json:"data,omitempty"`
}

// completionList is CompletionList.
type completionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []completionItem `json:"items"`
}

// resolveLookups counts the goroutines resolve started for a lookup (tests
// read it: a refused word must not start one).
var resolveLookups atomic.Int32

// completionData is what an item carries to resolve: the document it was
// asked in, its label and what it stands for ("function", "alias",
// "variable", "builtin", "keyword", "command", "paramop", "file", "folder",
// "option").
type completionData struct {
	URI   string `json:"uri"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// completionOptions is the `completionProvider` capability.
func completionOptions() map[string]any {
	return map[string]any{"triggerCharacters": []string{"$", "{", "-", "/"}, "resolveProvider": true}
}

// itemKinds maps an analysis kind to the wire kind and the name resolve reads.
func itemKinds(k analysis.CompletionKind) (int, string) {
	switch k {
	case analysis.CompFunction:
		return itemFunction, "function"
	case analysis.CompVariable:
		return itemVariable, "variable"
	case analysis.CompAlias:
		return itemModule, "alias"
	case analysis.CompBuiltin:
		return itemConstant, "builtin"
	case analysis.CompKeyword:
		return itemKeyword, "keyword"
	case analysis.CompFile:
		return itemFile, "file"
	case analysis.CompFolder:
		return itemFolder, "folder"
	}
	return itemOperator, "paramop"
}

// helpKind is the helpdoc kind of a command word: builtin for a builtin or a
// reserved word, command for anything else.
func helpKind(name string) string {
	if analysis.IsBuiltin(name) {
		return "builtin"
	}
	return "command"
}

func (s *server) completion(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	uri := p.TextDocument.URI
	empty := completionList{Items: []completionItem{}}
	snap, path, off, ok, rerr := s.cursor(uri, p.Position)
	if rerr != nil || !ok {
		return empty, rerr
	}
	be, _, rerr := s.locate(uri)
	if rerr != nil {
		return empty, rerr
	}
	st := s.settingsOf(be)
	ctx, cmd, items, incomplete := snap.Completions(path, off)
	out := completionList{Items: make([]completionItem, 0, len(items))}
	cs := converters{}
	seen := map[string]bool{}
	add := func(it completionItem, gated bool) {
		g := 0
		if gated {
			g = 1
		}
		it.SortText = fmt.Sprintf("%d%05d", g, len(out.Items))
		out.Items = append(out.Items, it)
	}
	for _, it := range items {
		kind, name := itemKinds(it.Kind)
		detail, gated := it.Detail, false
		if it.Feature != "" {
			if d, g := analysis.FeatureNeeds(it.Feature, st.Target.Min); g {
				detail, gated = d, true
			} else if it.Kind == analysis.CompKeyword {
				detail = "keyword"
			} else {
				detail = "parameter expansion operator"
			}
		}
		seen[it.Label] = true
		add(completionItem{Label: it.Label, Kind: kind, Detail: redact.String(detail),
			TextEdit: TextEdit{Range: s.rangeIn(cs, snap, path, it.Replace), NewText: it.Insert},
			Data:     completionData{URI: uri, Label: it.Label, Kind: name}}, gated)
	}
	switch ctx {
	case analysis.CtxOption:
		for _, it := range s.optionItems(be, snap, path, off, cmd, cs, uri) {
			add(it, false)
		}
	case analysis.CtxCommand:
		if st.Completion.PathCommands {
			prefix, rng, found := snap.CompletionWord(path, off)
			if found && !strings.ContainsAny(prefix, `/\`) {
				names, more := s.pathProfile().Names(prefix, max(0, analysis.MaxCompletionItems-len(items)))
				for _, n := range names {
					if seen[n] {
						continue
					}
					add(completionItem{Label: redact.String(n), Kind: itemFunction, Detail: "command",
						TextEdit: TextEdit{Range: s.rangeIn(cs, snap, path, rng), NewText: n},
						Data:     completionData{URI: uri, Label: n, Kind: "command"}}, false)
				}
				incomplete = incomplete || more
			}
		}
	}
	out.IsIncomplete = incomplete
	return out, nil
}

// optionItems are the options of cmd from a help text already in the cache:
// nothing when it is not there.
func (s *server) optionItems(be backend, snap *analysis.Snapshot, path string, off int, cmd string, cs converters, uri string) []completionItem {
	l := s.helpFor(be)
	if l == nil {
		return nil
	}
	doc, found, _ := l.Cached(helpKind(cmd), cmd)
	if !found {
		return nil
	}
	prefix, rng, ok := snap.CompletionWord(path, off)
	if !ok {
		return nil
	}
	var out []completionItem
	for _, o := range doc.Options {
		if !strings.HasPrefix(o.Flag, prefix) {
			continue
		}
		flag := redact.String(o.Flag)
		it := completionItem{Label: flag, Kind: itemProperty, Detail: redact.String(o.Arg),
			TextEdit: TextEdit{Range: s.rangeIn(cs, snap, path, rng), NewText: flag},
			Data:     completionData{URI: uri, Label: o.Flag, Kind: "option"}}
		if o.Text != "" {
			it.Documentation = &markupContent{Kind: "plaintext", Value: redact.String(o.Text)}
		}
		out = append(out, it)
	}
	return out
}

// completionResolve adds the documentation of an item: the hover text of a
// script symbol, the synopsis of a builtin, a reserved word or a command. The
// item comes back as the client sent it, with `documentation` added when
// there is something to say.
func (s *server) completionResolve(raw json.RawMessage) (any, *rpcError) {
	var item map[string]any
	if rerr := decode(raw, &item); rerr != nil {
		return nil, rerr
	}
	var d completionData
	if m, ok := item["data"].(map[string]any); ok {
		d.URI, _ = m["uri"].(string)
		d.Label, _ = m["label"].(string)
		d.Kind, _ = m["kind"].(string)
	}
	if d.URI == "" || d.Label == "" {
		return item, nil
	}
	if !s.wait() {
		return nil, cancelledError()
	}
	be, path, rerr := s.locate(d.URI)
	if rerr != nil {
		return item, nil // a URI that leads nowhere: nothing to add
	}
	switch d.Kind {
	case "function", "alias", "variable":
		kinds := map[string]analysis.DeclKind{"function": analysis.DeclFunction, "alias": analysis.DeclAlias, "variable": analysis.DeclVariable}
		if md, ok := be.Snapshot().SymbolDoc(path, kinds[d.Kind], d.Label); ok {
			item["documentation"] = markupContent{Kind: "markdown", Value: redact.String(md)}
		}
		return item, nil
	case "builtin", "keyword", "command":
		return s.resolveHelp(be, d, item)
	}
	return item, nil
}

func (s *server) resolveHelp(be backend, d completionData, item map[string]any) (any, *rpcError) {
	l := s.helpFor(be)
	if l == nil {
		return item, nil
	}
	kind := "command"
	if d.Kind != "command" {
		kind = "builtin"
	}
	if doc, found, known := l.Cached(kind, d.Label); known {
		if found {
			item["documentation"] = helpMarkup(doc)
		}
		return item, nil
	}
	if !l.Allowed(kind, d.Label) {
		return item, nil
	}
	id, lr := s.curID, s.cur
	cancelled := make(<-chan struct{})
	if lr != nil {
		cancelled = lr.done
	}
	resolveLookups.Add(1)
	go s.runResolve(l, kind, d.Label, item, id, cancelled)
	return deferred{}, nil
}

// runResolve runs on its own goroutine and touches no server state.
func (s *server) runResolve(l *helpdoc.Lookup, kind, name string, item map[string]any, id json.RawMessage, cancelled <-chan struct{}) {
	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel() // ends the child of a lookup that is still running
	res := make(chan helpResult, 1)
	go func() { res <- lookupHelp(ctx, l, kind, name) }()
	select {
	case r := <-res:
		s.post(func() { s.finishResolve(id, item, r) })
	case <-cancelled:
		s.post(func() { s.answer(id, nil, cancelledError()) })
	case <-s.baseCtx.Done():
	}
}

// finishResolve answers a resolve on the loop. A lookup that failed leaves the
// item as it was: a missing synopsis is no reason to fail a completion.
func (s *server) finishResolve(id json.RawMessage, item map[string]any, r helpResult) {
	switch {
	case s.isCancelled(idKey(id)):
		s.answer(id, nil, cancelledError())
	case r.panicked != nil:
		s.log.Printf("panic looking up help: %v", r.panicked)
		s.answer(id, item, nil)
	default:
		if r.found {
			item["documentation"] = helpMarkup(r.doc)
		}
		s.answer(id, item, nil)
	}
}
