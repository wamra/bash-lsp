package lsp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// ---- workspace folders ----
//
// One workspace per folder. A file belongs to exactly one: the folder whose
// root holds it, the innermost when folders nest (an enclosing folder's
// workspace excludes the subtree of a folder inside it). A document outside
// every folder is served by the loose backend, which holds only the open
// documents outside every folder (as the server does without a root). Each
// workspace sees only its own files: a `source` that leads into another
// folder is not followed, and no answer (definition, references, symbols,
// call hierarchy) crosses folders. workspace/symbol merges the folders.

// folder is one workspace folder.
type folder struct {
	client string // the root as the client spelled it, cleaned
	ws     *workspace.Workspace
}

// resolveRoot is the absolute, symlink-resolved spelling of a root; "" when
// it does not resolve.
func resolveRoot(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	return real
}

// openFolders opens the roots one after the other, each without the
// subtrees of the others nested in it, so no file is read or parsed twice
// and the cost is the sum of the folders. A root that does not open is
// logged and left out; one that resolves to a root already open is a
// duplicate.
func (s *server) openFolders(roots []string) []*folder {
	reals := make([]string, len(roots))
	for i, r := range roots {
		reals[i] = resolveRoot(r)
	}
	var out []*folder
	for i, r := range roots {
		if reals[i] == "" {
			s.log.Printf("cannot open workspace folder %s: it does not resolve", r)
			continue
		}
		if s.servedRoot(out, reals[i]) {
			continue
		}
		var others []string
		for j, o := range reals {
			if j != i && o != "" {
				others = append(others, o)
			}
		}
		ws, err := workspace.OpenExcluding(r, workspace.Limits{}, others)
		if err != nil {
			s.log.Printf("cannot open workspace folder %s: %v", r, err)
			continue
		}
		out = append(out, &folder{client: r, ws: ws})
	}
	return out
}

// servedRoot reports whether one of fs has real as its root.
func (s *server) servedRoot(fs []*folder, real string) bool {
	for _, f := range fs {
		if f.ws.IsRoot(real) {
			return true
		}
	}
	return false
}

// route maps a path the client spelled to the backend that serves it and
// the canonical path it is known by there.
func (s *server) route(p string) (backend, string) {
	if f, c := s.folderOfClient(p); f != nil {
		return f.ws, c
	}
	return s.loose, s.loose.Canon(p)
}

// folderOfClient is the folder that holds the client path p, and p's
// canonical spelling in it; nil outside every folder.
func (s *server) folderOfClient(p string) (*folder, string) {
	p = filepath.Clean(p)
	for _, f := range s.folders {
		if c := f.ws.Canon(p); f.ws.Owns(c) {
			return f, c
		}
	}
	return nil, ""
}

// folderOf is the folder that holds the canonical path p, by its spelling
// alone (no file is touched); nil outside every folder.
func (s *server) folderOf(p string) *folder {
	for _, f := range s.folders {
		if f.ws.Owns(p) {
			return f
		}
	}
	return nil
}

// snapshotHolding is the current snapshot that holds the file p, by
// lookup in each (a document outside every folder can be spelled inside
// one, through a symlink out of it); the loose one when none does.
func (s *server) snapshotHolding(p string) *analysis.Snapshot {
	for _, be := range s.backends() {
		if snap := be.Snapshot(); snap.Facts(p) != nil {
			return snap
		}
	}
	return s.loose.Snapshot()
}

// backends are the folders' workspaces in the client's order, then the
// loose documents.
func (s *server) backends() []backend {
	out := make([]backend, 0, len(s.folders)+1)
	for _, f := range s.folders {
		out = append(out, f.ws)
	}
	return append(out, s.loose)
}

// toClient rebases a canonical path onto the root of its folder as the
// client spelled it.
func (s *server) toClient(p string) string {
	if f := s.folderOf(p); f != nil {
		if rel, ok := under(f.ws.Root(), p); ok {
			return filepath.Join(f.client, rel)
		}
	}
	return p
}

// didChangeFolders applies workspace/didChangeWorkspaceFolders: removed
// folders are dropped with their workspaces, added ones are opened (on the
// message loop, as any other change), enclosing folders give up or take back
// the subtrees of nested ones, and open documents move to whatever serves
// them now and are published again from there.
func (s *server) didChangeFolders(raw json.RawMessage) *rpcError {
	var p didChangeWorkspaceFoldersParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	s.wait()
	changed := false
	dropped := map[backend]bool{}
	for _, wf := range p.Event.Removed {
		path, err := workspace.PathFromURI(wf.URI)
		if err != nil {
			s.log.Printf("ignoring removed folder %q: %v", wf.URI, err)
			continue
		}
		path = filepath.Clean(path)
		real := resolveRoot(path)
		for i, f := range s.folders {
			if f.client == path || real != "" && f.ws.IsRoot(real) {
				dropped[f.ws] = true
				s.folders = append(s.folders[:i:i], s.folders[i+1:]...)
				changed = true
				break
			}
		}
	}
	for _, wf := range p.Event.Added {
		path, err := workspace.PathFromURI(wf.URI)
		if err != nil {
			s.log.Printf("ignoring added folder %q: %v", wf.URI, err)
			continue
		}
		path = filepath.Clean(path)
		real := resolveRoot(path)
		if real == "" {
			s.log.Printf("cannot open workspace folder %s: it does not resolve", path)
			continue
		}
		if s.servedRoot(s.folders, real) {
			continue
		}
		ws, err := workspace.OpenExcluding(path, workspace.Limits{}, s.folderRoots())
		if err != nil {
			s.log.Printf("cannot open workspace folder %s: %v", path, err)
			continue
		}
		s.folders = append(s.folders, &folder{client: path, ws: ws})
		changed = true
	}
	if !changed {
		return nil
	}
	roots := s.folderRoots()
	for _, f := range s.folders {
		f.ws.SetExcluded(roots) // only the roots nested in f count; a no-op when unchanged
	}
	s.rehome(dropped)
	return nil
}

// folderRoots are the roots of the open folders.
func (s *server) folderRoots() []string {
	out := make([]string, len(s.folders))
	for i, f := range s.folders {
		out[i] = f.ws.Root()
	}
	return out
}

// rehome moves each open document whose backend or canonical path changed:
// its overlay leaves the old backend (unless that was dropped) and is set in
// the new one, and its diagnostics are published by the new one.
func (s *server) rehome(dropped map[backend]bool) {
	docs := make([]*document, 0, len(s.docs))
	for _, d := range s.docs {
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	for _, d := range docs {
		p, err := workspace.PathFromURI(d.uri)
		if err != nil {
			continue
		}
		be, path := s.route(p)
		if be == d.be && path == d.path {
			continue
		}
		if !dropped[d.be] {
			d.be.CloseOverlay(d.path)
		}
		delete(s.docs, d.path)
		d.be, d.path = be, path
		s.docs[path] = d
		s.byURI[d.uri] = path
		be.SetOverlay(path, d.version, d.text)
		s.publish(be, d, path)
	}
}

// ---- files changed outside the editor ----

// watchedFilesID is the registration id of the file watcher.
const watchedFilesID = "workspace/didChangeWatchedFiles"

// registerWatchers asks a client that registers didChangeWatchedFiles
// dynamically to watch every file. The answer is handled by response; the
// loop does not wait for it. A client without dynamic registration gets no
// request; it still sends didSave and didChange.
func (s *server) registerWatchers() {
	if !s.watchDynamic || s.registered {
		return
	}
	s.registered = true
	params := registrationParams{Registrations: []registration{{
		ID:     watchedFilesID,
		Method: "workspace/didChangeWatchedFiles",
		RegisterOptions: didChangeWatchedFilesRegistrationOptions{
			Watchers: []fileSystemWatcher{{GlobPattern: "**/*"}},
		},
	}}}
	s.call("client/registerCapability", params)
}

// call sends a request to the client. Its id is a string the client cannot
// mistake for one of its own requests' ids (they are answered by the
// server, not the client).
func (s *server) call(method string, params any) {
	s.reqSeq++
	id := fmt.Sprintf("bash-lsp/%d", s.reqSeq)
	s.pending[id] = method
	s.c.request(id, method, params)
}

// response handles the client's answer to a request the server sent. An
// answer to anything else (an unknown id, a second answer) is ignored. An
// error is logged: for the watcher registration, the server carries on
// without file events.
func (s *server) response(m message) {
	var id string
	if json.Unmarshal(m.ID, &id) != nil {
		return
	}
	method, ok := s.pending[id]
	if !ok {
		return
	}
	delete(s.pending, id)
	if len(m.Error) > 0 && string(m.Error) != "null" {
		msg := string(m.Error)
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		s.log.Printf("%s failed: %s; files changed outside the editor are seen only on didSave or reopen", method, msg)
	}
}

// didChangeWatched applies workspace/didChangeWatchedFiles: each folder
// refreshes the paths it holds, all in one batch (one rebuild). Paths outside
// every folder, URIs that are not file URIs, and (inside the workspace)
// open documents and skipped directories are ignored.
func (s *server) didChangeWatched(raw json.RawMessage) *rpcError {
	var p didChangeWatchedFilesParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	s.wait()
	var order []*folder
	paths := map[*folder][]string{}
	for _, ch := range p.Changes {
		path, err := workspace.PathFromURI(ch.URI)
		if err != nil || !filepath.IsAbs(path) {
			continue
		}
		f, _ := s.folderOfClient(path)
		if f == nil {
			continue
		}
		if _, ok := paths[f]; !ok {
			order = append(order, f)
		}
		paths[f] = append(paths[f], filepath.Clean(path))
	}
	for _, f := range order {
		f.ws.Refresh(paths[f]...)
	}
	return nil
}

// didSave refreshes the saved file. An open document keeps its buffer (the
// workspace ignores a refresh of a path with an overlay), so this matters
// for a file saved while not open.
func (s *server) didSave(raw json.RawMessage) *rpcError {
	var p didSaveTextDocumentParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	s.wait()
	path, err := workspace.PathFromURI(p.TextDocument.URI)
	if err != nil || !filepath.IsAbs(path) {
		return nil
	}
	if f, _ := s.folderOfClient(path); f != nil {
		f.ws.Refresh(filepath.Clean(path))
	}
	return nil
}
