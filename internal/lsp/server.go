// Package lsp is the stdio Language Server Protocol server over the analysis
// engine. Stdout carries protocol frames only; logs go to the log writer.
package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/helpdoc"
	"github.com/wamra/bash-lsp/internal/redact"
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
		f = workspace.OversizeFile(path, text, fmt.Sprintf("%s: %d bytes exceeds the %d byte file limit",
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
	lint    docLint // what was published for it (lint.go)

	semtok semtokDoc // semantic-tokens ids and the base of the next delta (semtok.go)
}

type server struct {
	c       *conn
	log     *log.Logger
	version string

	initialized  bool
	shutdown     bool
	hierarchical bool
	ccaps        clientCaps // what the client said it supports (caps)
	watchDynamic bool       // the client registers didChangeWatchedFiles dynamically
	roots        []string   // the workspace folders as the client spelled them
	initOpts     json.RawMessage
	shownMu      sync.Mutex
	shown        map[string]bool      // messages shown to the user this session (warnOnce)
	logged       map[string]bool      // internal parser errors logged this session (logInternal)
	looseSt      config.Settings      // settings of documents outside every folder
	looseDiag    analysis.DiagOptions // its diagnostic part
	looseTool    *scTool              // ShellCheck for documents outside every folder (lint.go)
	debounce     time.Duration        // quiet time before ShellCheck runs on an edited document
	baseCtx      context.Context      // ends when Serve returns; every ShellCheck run derives from it
	baseCancel   context.CancelFunc
	loggedMsg    map[string]bool   // window/logMessage texts sent once (logMessageOnce)
	fmtSem       chan struct{}     // bounds the formatter runs going at once (lint.go)
	epoch        int               // settings reloads so far (part of a pull result id)
	reloadMsg    map[string]string // per folder root: the "not reloaded" message sent for its last failed reload (loop only)
	profileOnce  sync.Once
	profile      *extool.Profile             // the PATH profile, read once per session (pathProfile)
	helpRun      helpdoc.Runner              // starts help and man (helpdoc.go); extool.Run, a fake in tests
	helpTools    map[backend]*helpdoc.Lookup // per folder, built on first hover (loop only)
	semtok       semtokState                 // semantic-tokens bookkeeping (semtok.go)

	started bool
	ready   chan struct{} // closed when folders is set
	// folders are the open workspace folders, in the client's order; a
	// folder nested inside another is not among them (see folders.go).
	// declared are all the folders the client named. Both are set by the
	// opener goroutine before ready is closed; after that, read and changed
	// on the message loop only.
	folders  []*folder
	declared []declaredFolder
	loose    *docsOnly // documents outside every folder

	docs  map[string]*document // open documents by canonical path
	byURI map[string]string    // canonical path of an open document by its URI

	registered bool              // client/registerCapability sent
	reqSeq     int               // ids of requests to the client
	pending    map[string]string // method of each request awaiting a response, by id

	exit    bool
	exitErr error

	// The message loop (loop.go). inbound is the bounded queue the reader
	// goroutine fills; quit closes when `exit` is read and done when Serve
	// returns; wake tells a loop that is waiting for input that a request was
	// cancelled. live holds the requests read and not yet answered. cur is the
	// request whose handler is running (nil for a notification). held are the
	// requests that arrived while folder jobs were pending, in arrival order.
	inbound  chan inbound
	stage    chan inbound // between the reader and inbound: room for exit and cancels to be read behind a full queue
	posted   chan func()  // closures posted by other goroutines, read even when inbound is not
	quit     chan struct{}
	quitOnce sync.Once
	done     chan struct{}
	wake     chan struct{}
	liveMu   sync.Mutex
	live     map[string]*liveReq
	cur      *liveReq
	curID    json.RawMessage // the id of the request being handled
	held     []message

	// Folder jobs (loop.go): folderPending counts the jobs queued whose result
	// was not applied; folderQ and folderStarted are guarded by folderMu.
	folderPending int
	folderMu      sync.Mutex
	folderQ       [][]declaredFolder
	folderStarted bool
	folderSig     chan struct{}
}

// Serve runs the server until the client exits. It returns nil after `exit`
// following `shutdown`, and an error if `exit` arrives first or the input ends.
func newServer(in io.Reader, out io.Writer, logw io.Writer, version string) *server {
	if logw == nil {
		logw = io.Discard
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &server{
		debounce: checkDebounce, baseCtx: ctx, baseCancel: cancel,
		c:       newConn(in, out),
		log:     log.New(logw, "bash-lsp: ", log.LstdFlags),
		version: version,
		ready:   make(chan struct{}),
		docs:    map[string]*document{},
		byURI:   map[string]string{},
		pending: map[string]string{},

		inbound:   make(chan inbound, inboundCap),
		stage:     make(chan inbound, inboundCap),
		posted:    make(chan func(), inboundCap),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		live:      map[string]*liveReq{},
		folderSig: make(chan struct{}, 1),
		fmtSem:    make(chan struct{}, 2),
		helpRun:   extool.Run,
		reloadMsg: map[string]string{},
	}
}

func Serve(in io.Reader, out io.Writer, logw io.Writer, version string) error {
	return newServer(in, out, logw, version).serve()
}

// safeHandle runs a handler, turning a panic into an internal error.
func (s *server) safeHandle(m message) (result any, rerr *rpcError) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("panic in %s: %v\n%s", m.Method, r, debug.Stack())
			result, rerr = nil, &rpcError{codeInternalError, fmt.Sprintf("internal error in %s", m.Method)}
		}
	}()
	if _, _, req := hooks(); req != nil {
		req(m.Method)
	}
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
	case "workspace/didChangeConfiguration":
		return nil, s.didChangeConfiguration()
	case "workspace/didChangeWorkspaceFolders":
		return nil, s.didChangeFolders(m.Params)
	case "textDocument/diagnostic":
		return s.diagnostic(m.Params)
	case "textDocument/codeAction":
		return s.codeAction(m.Params)
	case "textDocument/formatting":
		return s.formatting(m.Params, false)
	case "textDocument/rangeFormatting":
		return s.formatting(m.Params, true)
	case "textDocument/definition":
		return s.definition(m.Params)
	case "textDocument/references":
		return s.references(m.Params)
	case "textDocument/implementation":
		return s.implementation(m.Params)
	case "textDocument/hover":
		return s.hover(m.Params)
	case "textDocument/inlayHint":
		return s.inlayHint(m.Params)
	case "textDocument/signatureHelp":
		return s.signatureHelp(m.Params)
	case "textDocument/completion":
		return s.completion(m.Params)
	case "completionItem/resolve":
		return s.completionResolve(m.Params)
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
	case "textDocument/foldingRange":
		return s.foldingRange(m.Params)
	case "textDocument/selectionRange":
		return s.selectionRange(m.Params)
	case "textDocument/documentHighlight":
		return s.documentHighlight(m.Params)
	case "textDocument/documentLink":
		return s.documentLink(m.Params)
	case "textDocument/semanticTokens/full":
		return s.semanticTokensFull(m.Params)
	case "textDocument/semanticTokens/full/delta":
		return s.semanticTokensDelta(m.Params)
	case "textDocument/semanticTokens/range":
		return s.semanticTokensRange(m.Params)
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
	cc := readClientCapabilities(p.Capabilities, s.log)
	s.hierarchical = cc.TextDocument.DocumentSymbol.Hierarchical
	s.initOpts = p.InitializationOptions
	s.ccaps = capsOf(cc)
	s.watchDynamic = cc.Workspace.DidChangeWatchedFiles.DynamicRegistration
	s.roots = rootsOf(p, s.log)
	if len(s.roots) > 0 {
		go s.pathProfile() // read while the client sends `initialized` and the folders are walked
	}
	return initializeResult{Capabilities: capabilities(), ServerInfo: serverInfo{Name: "bash-lsp", Version: s.version}}, nil
}

// clientCaps are the client capabilities the handlers consult, read once at
// initialize.
type clientCaps struct {
	RenamePrepare, DocumentChanges, ChangeAnnotations bool // textDocument.rename.prepareSupport; workspace.workspaceEdit.documentChanges; workspace.workspaceEdit.changeAnnotationSupport present
	LabelOffset                                       bool // textDocument.signatureHelp.signatureInformation.parameterInformation.labelOffsetSupport
	LineFoldingOnly                                   bool // textDocument.foldingRange.lineFoldingOnly
	RangeLimit                                        int  // textDocument.foldingRange.rangeLimit (0 = none)
	SemanticTokensRefresh, InlayHintRefresh           bool // workspace.semanticTokens.refreshSupport; workspace.inlayHint.refreshSupport
}

// readClientCapabilities decodes the client capabilities. A value of the
// wrong type is left out and logged: one malformed optional capability must
// not fail initialize. encoding/json skips the offending field and decodes
// the rest, so well-formed siblings still count.
func readClientCapabilities(raw json.RawMessage, lg *log.Logger) clientCapabilities {
	var cc clientCapabilities
	if len(raw) == 0 {
		return cc
	}
	if err := json.Unmarshal(raw, &cc); err != nil {
		lg.Printf("client capabilities: %v; the malformed value is ignored", err)
	}
	return cc
}

func capsOf(cc clientCapabilities) clientCaps {
	td, ws := cc.TextDocument, cc.Workspace
	ann := bytes.TrimSpace(ws.WorkspaceEdit.ChangeAnnotationSupport)
	return clientCaps{
		RenamePrepare:         td.Rename.PrepareSupport,
		DocumentChanges:       ws.WorkspaceEdit.DocumentChanges,
		ChangeAnnotations:     len(ann) > 0 && string(ann) != "null" && string(ann) != "false",
		LabelOffset:           td.SignatureHelp.SignatureInformation.ParameterInformation.LabelOffsetSupport,
		LineFoldingOnly:       td.FoldingRange.LineFoldingOnly,
		RangeLimit:            max(td.FoldingRange.RangeLimit, 0),
		SemanticTokensRefresh: ws.SemanticTokens.RefreshSupport,
		InlayHintRefresh:      ws.InlayHint.RefreshSupport,
	}
}

// caps returns the client capabilities. They are set during initialize,
// before any other request is read.
func (s *server) caps() clientCaps { return s.ccaps }

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
	s.looseSt = s.loadSettings("")
	s.looseDiag = diagOptionsOf(s.looseSt)
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
		s.declared = s.declare(s.roots)
		if open, _, _ := hooks(); open != nil {
			open()
		}
		s.folders = s.reconcile(s.declared, nil)
	}()
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
	if !s.wait() {
		return nil
	}
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
	s.refreshOthers(be, d)
	return nil
}

func (s *server) didChange(raw json.RawMessage) *rpcError {
	var p didChangeParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	if !s.wait() {
		return nil
	}
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
	s.refreshOthers(d.be, d)
	return nil
}

func (s *server) didClose(raw json.RawMessage) *rpcError {
	var p didCloseParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	if !s.wait() {
		return nil
	}
	d, rerr := s.docOf(p.TextDocument.URI)
	delete(s.byURI, p.TextDocument.URI)
	if rerr != nil || d == nil {
		return nil
	}
	delete(s.docs, d.path)
	s.resetCheck(d)
	// Every URI that led to it: the closing one, the one it was opened (and
	// its diagnostics published) under, and any other spelling it was opened
	// under (case variants).
	uris := []string{p.TextDocument.URI}
	if d.uri != p.TextDocument.URI {
		uris = append(uris, d.uri)
	}
	for u, path := range s.byURI {
		if path == d.path {
			delete(s.byURI, u)
			if !slices.Contains(uris, u) {
				uris = append(uris, u)
			}
		}
	}
	sort.Strings(uris[1:])
	d.be.CloseOverlay(d.path)
	// Diagnostics belong to open documents: clear them under each URI.
	for _, u := range uris {
		s.c.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{URI: u, Diagnostics: []Diagnostic{}})
	}
	s.refreshOthers(d.be, nil)
	return nil
}

// publish sends the native findings of an open document that was opened,
// changed or moved (see publishDoc) and schedules ShellCheck for it.
func (s *server) publish(be backend, d *document, path string) {
	s.resetCheck(d)
	s.publishDoc(d, true)
	s.scheduleCheck(d)
}

// diagnosticTagUnnecessary is LSP DiagnosticTag 1, Unnecessary.
const diagnosticTagUnnecessary = 1

// diagOptions returns the diagnostic settings of the folder be serves.
// Roots names every workspace folder, so that a `source` into another
// folder counts as inside the workspace.
func (s *server) diagOptions(be backend) analysis.DiagOptions {
	opt := s.looseDiag
	for _, f := range s.folders {
		if backend(f.ws) == be {
			opt = f.diag
		}
	}
	opt.Roots = make([]string, 0, 2*len(s.folders))
	for _, f := range s.folders { // as the client spells it and as resolved: a script may use either
		opt.Roots = append(opt.Roots, f.client, f.ws.Root())
	}
	return opt
}

// settingsOf returns the settings of the folder be serves; the loose
// documents' settings for any other backend.
func (s *server) settingsOf(be backend) config.Settings {
	for _, f := range s.folders {
		if backend(f.ws) == be {
			return f.st
		}
	}
	return s.looseSt
}

// loadSettings loads the settings of a folder root ("" for documents outside
// every folder: the user file and initialization options only) and logs what
// was ignored, including rule ids no native rule has.
func (s *server) loadSettings(root string) config.Settings {
	st := config.Load(root, s.initOpts)
	s.reportSettings(st)
	return st
}

// reportSettings logs what loading st ignored.
func (s *server) reportSettings(st config.Settings) {
	for _, w := range st.Warnings {
		s.log.Printf("settings: %s", w)
	}
	for _, w := range st.CheckRules(analysis.KnownRule) {
		s.log.Printf("settings: %s", w)
		s.warnOnce("settings: " + w)
	}
	for _, r := range st.Refused { // a repo file naming a key only the user may set
		s.logMessageOnce(messageTypeWarning, fmt.Sprintf("settings: %s: %s: %s", r.File, r.Key, r.Reason))
	}
}

func diagOptionsOf(st config.Settings) analysis.DiagOptions {
	return analysis.DiagOptions{Target: st.Target, Disabled: st.Disabled, Severity: st.Severity}
}

// pathProfile is the PATH profile commands are looked up in: path.profile of
// the user settings or initialization options (a repo file cannot set it, so
// one profile serves every folder), else $PATH. Read once per session.
func (s *server) pathProfile() *extool.Profile {
	s.profileOnce.Do(func() {
		if st := config.Load("", s.initOpts); st.PathProfile != nil {
			s.profile = extool.NewProfile(st.PathProfile)
		} else {
			s.profile = extool.ProfileFromEnv()
		}
		s.profile.SetLogger(s.log.Printf)
		for _, sk := range s.profile.Skipped() {
			s.log.Printf("path profile: skipped %s", sk)
		}
	})
	return s.profile
}

// messageTypeWarning is LSP MessageType 2, Warning.
const messageTypeWarning = 2

// warnOnce shows msg to the user (window/showMessage), once per session.
// Called from the folder opener as well as the loop.
func (s *server) warnOnce(msg string) {
	s.shownMu.Lock()
	defer s.shownMu.Unlock()
	if s.shown == nil {
		s.shown = map[string]bool{}
	}
	if s.shown[msg] {
		return
	}
	s.shown[msg] = true
	s.c.notify("window/showMessage", showMessageParams{Type: messageTypeWarning, Message: redact.String("bash-lsp " + msg)})
}

// logInternal writes the text of each recovered parser panic of f to the
// log, once per file and text: the published finding (WB1003) leaves it out.
func (s *server) logInternal(path string, f *tree.File) {
	for _, e := range f.Errors {
		if !e.Internal {
			continue
		}
		k := path + "\x00" + e.Msg
		s.shownMu.Lock()
		if s.logged == nil {
			s.logged = map[string]bool{}
		}
		seen := s.logged[k]
		s.logged[k] = true
		s.shownMu.Unlock()
		if !seen {
			s.log.Printf("%s:%d: %s", path, e.Range.Start.Line, e.Msg)
		}
	}
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
	if !s.wait() {
		rerr = cancelledError()
		return
	}
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
	if be, _, rerr := s.locate(p.TextDocument.URI); rerr == nil && s.settingsOf(be).ResolvedOnly {
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
		return s.helpHover(p.TextDocument.URI, snap, path, off)
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
	if !s.wait() {
		return nil, cancelledError()
	}
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
	if !s.wait() {
		return nil, cancelledError()
	}
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
