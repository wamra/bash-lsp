// Package lsp is the stdio Language Server Protocol server over the analysis
// engine. Stdout carries protocol frames only; logs go to the log writer.
package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// backend is what the server needs from a workspace.
type backend interface {
	Snapshot() *analysis.Snapshot
	SetOverlay(path string, version int, text []byte)
	CloseOverlay(path string)
	Refresh(paths ...string)
	Canon(path string) string // the key SetOverlay stores path under
}

// docsOnly serves open documents when the client gave no root.
type docsOnly struct {
	mu    sync.Mutex
	files map[string]*analysis.FileFacts
	gen   uint64
	snap  *analysis.Snapshot
}

func newDocsOnly() *docsOnly {
	d := &docsOnly{files: map[string]*analysis.FileFacts{}}
	d.rebuild()
	return d
}

func (d *docsOnly) rebuild() {
	d.gen++
	files := make(map[string]*analysis.FileFacts, len(d.files))
	for p, f := range d.files {
		files[p] = f
	}
	d.snap = analysis.Build(files, analysis.Options{}, d.gen)
}

func (d *docsOnly) Snapshot() *analysis.Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snap
}

func (d *docsOnly) SetOverlay(path string, _ int, text []byte) {
	var f *tree.File
	if len(text) > workspace.DefaultMaxFileBytes {
		f = workspace.OversizeFile(path, fmt.Sprintf("%s: %d bytes exceeds the %d byte file limit",
			filepath.Base(path), len(text), workspace.DefaultMaxFileBytes))
	} else {
		f = parse.File(path, text)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[path] = analysis.Bind(f)
	d.rebuild()
}

func (d *docsOnly) CloseOverlay(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.files[path]; ok {
		delete(d.files, path)
		d.rebuild()
	}
}

func (d *docsOnly) Refresh(...string) {}

func (d *docsOnly) Canon(path string) string { return filepath.Clean(path) }

type document struct {
	uri     string
	path    string  // canonical, the key in server.docs
	be      backend // the workspace that holds its overlay
	version int
	text    []byte
}

type server struct {
	c       *conn
	log     *log.Logger
	version string

	initialized  bool
	shutdown     bool
	hierarchical bool
	resolvedOnly bool     // initialization option references.resolvedOnly
	watchDynamic bool     // the client registers didChangeWatchedFiles dynamically
	roots        []string // the workspace folders as the client spelled them

	started bool
	ready   chan struct{} // closed when folders is set
	// folders are the open workspace folders, in the client's order. Set by
	// the opener goroutine before ready is closed; after that, read and
	// changed on the message loop only.
	folders []*folder
	loose   *docsOnly // documents outside every folder

	docs  map[string]*document // open documents by canonical path
	byURI map[string]string    // canonical path of an open document by its URI

	registered bool              // client/registerCapability sent
	reqSeq     int               // ids of requests to the client
	pending    map[string]string // method of each request awaiting a response, by id

	exit    bool
	exitErr error
}

// Serve runs the server until the client exits. It returns nil after `exit`
// following `shutdown`, and an error if `exit` arrives first or the input ends.
func Serve(in io.Reader, out io.Writer, logw io.Writer, version string) error {
	if logw == nil {
		logw = io.Discard
	}
	s := &server{
		c:       newConn(in, out),
		log:     log.New(logw, "bash-lsp: ", log.LstdFlags),
		version: version,
		ready:   make(chan struct{}),
		docs:    map[string]*document{},
		byURI:   map[string]string{},
		pending: map[string]string{},
	}
	for !s.exit {
		body, err := s.c.read()
		if err != nil {
			if errors.Is(err, errBadFrame) {
				s.log.Printf("fatal framing error: %v", err)
			}
			if err == io.EOF {
				return errors.New("lsp: input ended before exit")
			}
			return fmt.Errorf("lsp: reading input: %w", err)
		}
		s.dispatch(body)
		if err := s.c.writeErr(); err != nil {
			return fmt.Errorf("lsp: writing output: %w", err)
		}
	}
	return s.exitErr
}

func (s *server) dispatch(body []byte) {
	// Unmarshal validates the syntax itself and reports a failure of it as a
	// *json.SyntaxError, so a separate json.Valid pass over a large body (a
	// didOpen carries the whole document) is not needed to tell the two apart.
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			s.c.reply(nil, nil, &rpcError{codeParseError, "parse error: invalid JSON"})
			return
		}
		s.c.reply(nil, nil, &rpcError{codeInvalidRequest, "invalid request: " + err.Error()})
		return
	}
	if m.Method == "" {
		s.response(m)
		return
	}
	isReq := len(m.ID) > 0
	if m.Method == "exit" {
		s.exit = true
		if !s.shutdown {
			s.exitErr = errors.New("lsp: exit before shutdown")
		}
		return
	}
	if m.Method == "$/cancelRequest" {
		return
	}
	switch {
	case m.Method == "initialize":
		// handled below
	case !s.initialized:
		if isReq {
			s.c.reply(m.ID, nil, &rpcError{codeNotInitialized, "server not initialized"})
		}
		return
	case s.shutdown:
		if isReq {
			s.c.reply(m.ID, nil, &rpcError{codeInvalidRequest, "server is shut down"})
		}
		return
	}
	result, rerr := s.safeHandle(m)
	if isReq {
		s.c.reply(m.ID, result, rerr)
	}
}

// safeHandle runs a handler, turning a panic into an internal error.
func (s *server) safeHandle(m message) (result any, rerr *rpcError) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("panic in %s: %v\n%s", m.Method, r, debug.Stack())
			result, rerr = nil, &rpcError{codeInternalError, fmt.Sprintf("internal error in %s", m.Method)}
		}
	}()
	return s.handle(m)
}

func (s *server) handle(m message) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		return s.initialize(m.Params)
	case "initialized":
		s.startWorkspace()
		s.registerWatchers()
		return nil, nil
	case "shutdown":
		s.shutdown = true
		return nil, nil
	case "textDocument/didOpen":
		return nil, s.didOpen(m.Params)
	case "textDocument/didChange":
		return nil, s.didChange(m.Params)
	case "textDocument/didClose":
		return nil, s.didClose(m.Params)
	case "textDocument/didSave":
		return nil, s.didSave(m.Params)
	case "workspace/didChangeWatchedFiles":
		return nil, s.didChangeWatched(m.Params)
	case "workspace/didChangeWorkspaceFolders":
		return nil, s.didChangeFolders(m.Params)
	case "textDocument/definition":
		return s.definition(m.Params)
	case "textDocument/references":
		return s.references(m.Params)
	case "textDocument/implementation":
		return s.implementation(m.Params)
	case "textDocument/hover":
		return s.hover(m.Params)
	case "textDocument/documentSymbol":
		return s.documentSymbol(m.Params)
	case "workspace/symbol":
		return s.workspaceSymbol(m.Params)
	case "textDocument/prepareCallHierarchy":
		return s.prepareCallHierarchy(m.Params)
	case "callHierarchy/incomingCalls":
		return s.incomingCalls(m.Params)
	case "callHierarchy/outgoingCalls":
		return s.outgoingCalls(m.Params)
	}
	if len(m.ID) > 0 {
		return nil, &rpcError{codeMethodNotFound, "method not found: " + m.Method}
	}
	return nil, nil // unknown notification
}

func decode(raw json.RawMessage, v any) *rpcError {
	if len(raw) == 0 {
		return &rpcError{codeInvalidParams, "missing params"}
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &rpcError{codeInvalidParams, "invalid params: " + err.Error()}
	}
	return nil
}

// ---- lifecycle ----

func (s *server) initialize(raw json.RawMessage) (any, *rpcError) {
	if s.initialized {
		return nil, &rpcError{codeInvalidRequest, "already initialized"}
	}
	var p initializeParams
	if len(raw) > 0 {
		if rerr := decode(raw, &p); rerr != nil {
			return nil, rerr
		}
	}
	s.initialized = true
	s.hierarchical = p.Capabilities.TextDocument.DocumentSymbol.Hierarchical
	s.resolvedOnly = resolvedOnlyOption(p.InitializationOptions, s.log)
	s.watchDynamic = p.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
	s.roots = rootsOf(p, s.log)
	return initializeResult{Capabilities: capabilities(), ServerInfo: serverInfo{Name: "bash-lsp", Version: s.version}}, nil
}

// resolvedOnlyOption reads `{"references": {"resolvedOnly": true}}` from the
// initialization options. Anything else, or a value of the wrong type, means
// the default (false); a malformed value is logged, never an error.
func resolvedOnlyOption(raw json.RawMessage, lg *log.Logger) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var o struct {
		References struct {
			ResolvedOnly bool `json:"resolvedOnly"`
		} `json:"references"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		lg.Printf("ignoring initializationOptions: %v", err)
		return false
	}
	return o.References.ResolvedOnly
}

// rootsOf picks the workspace folders, else rootUri, else rootPath. A
// folder whose URI is not a file URI is logged and left out.
func rootsOf(p initializeParams, lg *log.Logger) []string {
	var uris []string
	switch {
	case len(p.WorkspaceFolders) > 0:
		for _, f := range p.WorkspaceFolders {
			uris = append(uris, f.URI)
		}
	case p.RootURI != nil && *p.RootURI != "":
		uris = []string{*p.RootURI}
	case p.RootPath != nil && *p.RootPath != "":
		return []string{filepath.Clean(*p.RootPath)}
	}
	var out []string
	for _, uri := range uris {
		path, err := workspace.PathFromURI(uri)
		if err != nil {
			lg.Printf("ignoring root %q: %v", uri, err)
			continue
		}
		out = append(out, filepath.Clean(path))
	}
	return out
}

// startWorkspace opens the workspace folders in the background, one after
// the other; every later request waits for them. Called on `initialized`
// and, for clients that skip it, on the first request that needs the
// workspace.
func (s *server) startWorkspace() {
	if s.started {
		return
	}
	s.started = true
	s.loose = newDocsOnly()
	if len(s.roots) == 0 {
		close(s.ready)
		return
	}
	go func() {
		defer close(s.ready)
		defer func() {
			if r := recover(); r != nil {
				s.log.Printf("panic opening workspace: %v\n%s", r, debug.Stack())
				s.folders = nil
			}
		}()
		s.folders = s.openFolders(s.roots)
	}()
}

// wait waits for the first workspace build.
func (s *server) wait() {
	s.startWorkspace()
	<-s.ready
}

// ---- paths ----

func under(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// uriOf names path to the client: a document the client opened keeps the URI
// it used; other files get the root-rebased spelling.
func (s *server) uriOf(path string) string {
	if d := s.docs[path]; d != nil {
		return d.uri
	}
	return workspace.URIFromPath(s.toClient(path))
}

// locate maps a document URI to the workspace that serves it and the
// canonical path it is known by there.
func (s *server) locate(uri string) (backend, string, *rpcError) {
	p, err := workspace.PathFromURI(uri)
	if err != nil {
		return nil, "", &rpcError{codeInvalidParams, err.Error()}
	}
	be, path := s.route(p)
	return be, path, nil
}

// docOf finds the open document of uri: by the URI it was opened with, else
// by where the URI leads now.
func (s *server) docOf(uri string) (*document, *rpcError) {
	if path, ok := s.byURI[uri]; ok {
		return s.docs[path], nil
	}
	_, path, rerr := s.locate(uri)
	if rerr != nil {
		return nil, rerr
	}
	return s.docs[path], nil
}

// ---- document sync ----

func (s *server) didOpen(raw json.RawMessage) *rpcError {
	var p didOpenParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	s.wait()
	be, path, rerr := s.locate(p.TextDocument.URI)
	if rerr != nil {
		s.log.Printf("didOpen ignored: %s", rerr.Message)
		return nil
	}
	d := &document{uri: p.TextDocument.URI, path: path, be: be, version: p.TextDocument.Version, text: []byte(p.TextDocument.Text)}
	s.docs[path] = d
	s.byURI[d.uri] = path
	be.SetOverlay(path, d.version, d.text)
	s.publish(be, d, path)
	return nil
}

func (s *server) didChange(raw json.RawMessage) *rpcError {
	var p didChangeParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	s.wait()
	d, rerr := s.docOf(p.TextDocument.URI)
	if rerr != nil {
		s.log.Printf("didChange ignored: %s", rerr.Message)
		return nil
	}
	if d == nil {
		s.log.Printf("didChange for a document that is not open: %s", p.TextDocument.URI)
		return nil
	}
	d.text = applyChanges(d.text, p.ContentChanges)
	d.version = p.TextDocument.Version
	d.be.SetOverlay(d.path, d.version, d.text)
	s.publish(d.be, d, d.path)
	return nil
}

func (s *server) didClose(raw json.RawMessage) *rpcError {
	var p didCloseParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	s.wait()
	d, rerr := s.docOf(p.TextDocument.URI)
	delete(s.byURI, p.TextDocument.URI)
	if rerr != nil || d == nil {
		return nil
	}
	delete(s.docs, d.path)
	d.be.CloseOverlay(d.path)
	// Diagnostics belong to open documents: clear them.
	s.c.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{URI: p.TextDocument.URI, Diagnostics: []Diagnostic{}})
	return nil
}

func (s *server) publish(be backend, d *document, path string) {
	diags := []Diagnostic{}
	if ff := be.Snapshot().Facts(path); ff != nil && ff.File != nil {
		f := ff.File
		if f.Skipped != "" {
			diags = append(diags, Diagnostic{Severity: 3, Code: "WB1002", Source: "bash-lsp", Message: f.Skipped})
		}
		cs := converters{}
		for _, e := range f.Errors {
			diags = append(diags, Diagnostic{Range: cs.rangeOf(f, e.Range), Severity: 1, Code: "WB1001", Source: "bash-lsp", Message: e.Msg})
		}
	}
	s.c.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{URI: d.uri, Diagnostics: diags})
}

// ---- positions ----

// converters converts the ranges of one request to UTF-16, one tree.Converter
// per file: a request with many ranges on one long line scans that line once.
type converters map[*tree.File]*tree.Converter

func (cs converters) rangeOf(f *tree.File, r tree.Range) Range {
	c := cs[f]
	if c == nil {
		c = f.NewConverter()
		cs[f] = c
	}
	var out Range
	out.Start.Line, out.Start.Character = c.ToUTF16(r.Start)
	out.End.Line, out.End.Character = c.ToUTF16(r.End)
	return out
}

// rawRange converts without a file: byte columns stand in for UTF-16 units.
func rawRange(r tree.Range) Range {
	return Range{Position{r.Start.Line - 1, r.Start.Col - 1}, Position{r.End.Line - 1, r.End.Col - 1}}
}

func fileOf(snap *analysis.Snapshot, path string) *tree.File {
	if ff := snap.Facts(path); ff != nil {
		return ff.File
	}
	return nil
}

func (s *server) rangeIn(cs converters, snap *analysis.Snapshot, path string, r tree.Range) Range {
	if f := fileOf(snap, path); f != nil {
		return cs.rangeOf(f, r)
	}
	return rawRange(r)
}

func (s *server) locations(snap *analysis.Snapshot, ls []analysis.Location) []Location {
	out := make([]Location, 0, len(ls))
	cs := converters{}
	for _, l := range ls {
		out = append(out, Location{URI: s.uriOf(l.Path), Range: s.rangeIn(cs, snap, l.Path, l.Range)})
	}
	return out
}

// cursor resolves a request position to a snapshot, path and byte offset.
// ok is false when the document is unknown to the snapshot.
func (s *server) cursor(uri string, pos Position) (snap *analysis.Snapshot, path string, offset int, ok bool, rerr *rpcError) {
	s.wait()
	be, path, rerr := s.locate(uri)
	if rerr != nil {
		return
	}
	snap = be.Snapshot()
	f := fileOf(snap, path)
	if f == nil {
		return snap, path, 0, false, nil
	}
	return snap, path, f.FromUTF16(pos.Line, pos.Character).Offset, true, nil
}

// ---- requests ----

func (s *server) definition(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return []Location{}, rerr
	}
	return s.locations(snap, snap.Definition(path, off)), nil
}

func (s *server) references(raw json.RawMessage) (any, *rpcError) {
	var p referenceParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return []Location{}, rerr
	}
	if s.resolvedOnly {
		return s.locations(snap, snap.ResolvedReferences(path, off, p.Context.IncludeDeclaration)), nil
	}
	return s.locations(snap, snap.References(path, off, p.Context.IncludeDeclaration)), nil
}

func (s *server) implementation(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return []Location{}, rerr
	}
	return s.locations(snap, snap.Implementation(path, off)), nil
}

func (s *server) hover(raw json.RawMessage) (any, *rpcError) {
	var p textDocumentPositionParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	snap, path, off, ok, rerr := s.cursor(p.TextDocument.URI, p.Position)
	if rerr != nil || !ok {
		return nil, rerr
	}
	md, rng, found := snap.Hover(path, off)
	if !found {
		return nil, nil
	}
	return hover{Contents: markupContent{Kind: "markdown", Value: md}, Range: s.rangeIn(converters{}, snap, path, rng)}, nil
}

func symbolKind(k analysis.DeclKind) int {
	if k == analysis.DeclVariable {
		return kindVariable
	}
	return kindFunction // functions and aliases
}

func (s *server) documentSymbol(raw json.RawMessage) (any, *rpcError) {
	var p documentSymbolParams
	if rerr := decode(raw, &p); rerr != nil {
		return nil, rerr
	}
	s.wait()
	be, path, rerr := s.locate(p.TextDocument.URI)
	if rerr != nil {
		return nil, rerr
	}
	snap := be.Snapshot()
	f := fileOf(snap, path)
	syms := snap.DocumentSymbols(path)
	if f == nil || len(syms) == 0 {
		if s.hierarchical {
			return []DocumentSymbol{}, nil
		}
		return []SymbolInformation{}, nil
	}
	cs := converters{}
	if s.hierarchical {
		out := make([]DocumentSymbol, 0, len(syms))
		for _, d := range syms {
			out = append(out, hierSymbol(cs, f, d))
		}
		return out, nil
	}
	uri := s.uriOf(path)
	out := make([]SymbolInformation, 0, len(syms))
	var flat func(d analysis.DocSymbol, container string)
	flat = func(d analysis.DocSymbol, container string) {
		out = append(out, SymbolInformation{Name: d.Name, Kind: symbolKind(d.Kind),
			Location: Location{URI: uri, Range: cs.rangeOf(f, d.Range)}, ContainerName: container})
		for _, c := range d.Children {
			flat(c, d.Name)
		}
	}
	for _, d := range syms {
		flat(d, "")
	}
	return out, nil
}

func hierSymbol(cs converters, f *tree.File, d analysis.DocSymbol) DocumentSymbol {
	detail := d.Detail
	if d.Kind == analysis.DeclAlias {
		detail = "alias"
	}
	out := DocumentSymbol{Name: d.Name, Detail: detail, Kind: symbolKind(d.Kind),
		Range: cs.rangeOf(f, d.Range), SelectionRange: cs.rangeOf(f, d.Selection)}
	for _, c := range d.Children {
		out.Children = append(out.Children, hierSymbol(cs, f, c))
	}
	return out
}

// maxWorkspaceSymbols caps workspace/symbol, as analysis does per snapshot.
const maxWorkspaceSymbols = 200

// workspaceSymbol merges the symbols of every workspace folder and of the
// documents outside them. Each list comes ranked from its snapshot; the
// lists are interleaved (first of each, then second of each, in folder
// order, the outside documents last), so no folder crowds the others out
// of the cap.
func (s *server) workspaceSymbol(raw json.RawMessage) (any, *rpcError) {
	var p workspaceSymbolParams
	if len(raw) > 0 {
		if rerr := decode(raw, &p); rerr != nil {
			return nil, rerr
		}
	}
	s.wait()
	type list struct {
		snap  *analysis.Snapshot
		infos []analysis.SymbolInfo
	}
	var lists []list
	for _, be := range s.backends() {
		snap := be.Snapshot()
		lists = append(lists, list{snap, snap.WorkspaceSymbols(p.Query, maxWorkspaceSymbols)})
	}
	out := []SymbolInformation{}
	cs := converters{}
	for i := 0; len(out) < maxWorkspaceSymbols; i++ {
		more := false
		for _, l := range lists {
			if i >= len(l.infos) || len(out) == maxWorkspaceSymbols {
				continue
			}
			more = true
			in := l.infos[i]
			out = append(out, SymbolInformation{Name: in.Name, Kind: symbolKind(in.Kind),
				Location: Location{URI: s.uriOf(in.Path), Range: s.rangeIn(cs, l.snap, in.Path, in.Range)}})
		}
		if !more {
			break
		}
	}
	return out, nil
}
