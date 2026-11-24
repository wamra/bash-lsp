package lsp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/workspace"
)

// ---- workspace folders ----
//
// One workspace per outermost folder. A folder nested inside another
// workspace folder is served by the outer folder's backend when the outer walk
// reaches it: it is not excluded from the outer walk and gets no backend of
// its own, so answers are the same whether or not the nested
// folder is also named. A nested folder the outer walk does not reach (it is
// git-ignored, or inside a skipped directory such as node_modules) is indexed
// by nobody else, so it gets its own backend, exactly as if it were the only
// folder; a document inside it is served there, by the innermost folder.
// Removing the outer folder while the inner remains gives the inner its own
// backend; removing the inner while the outer remains changes nothing;
// adding an outer folder after the inner absorbs the inner. A document
// outside every folder is served by the loose backend, which holds only the
// open documents outside every folder (as the server does without a root).
// Each workspace sees only its own files: a `source` that leads into a
// folder that is not nested in it is not followed, and no answer
// (definition, references, symbols, call hierarchy) crosses workspaces.
// workspace/symbol merges them.

// folder is one served workspace folder.
type folder struct {
	client string // the root as the client spelled it, cleaned
	ws     *workspace.Workspace
	st     config.Settings      // the folder's settings (see reloadSettings)
	diag   analysis.DiagOptions // their diagnostic part
	tool   *scTool              // ShellCheck of the folder; nil until needed (lint.go)
}

// declaredFolder is a folder the client named, served or not.
type declaredFolder struct {
	client string // as the client spelled it, cleaned
	real   string // resolveRoot(client); "" when it does not resolve
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

// declare records the client's folders. A root that does not resolve is
// logged and kept (it serves nothing). A root named twice (the same cleaned
// spelling, or the same directory) is kept once, so one removal undeclares it.
func (s *server) declare(roots []string) []declaredFolder {
	out := make([]declaredFolder, 0, len(roots))
	for _, r := range roots {
		r = filepath.Clean(r)
		real := resolveRoot(r)
		dup := false
		for _, e := range out {
			if e.client == r || real != "" && s.sameRoot(e.real, real) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		if real == "" {
			s.log.Printf("cannot open workspace folder %s: it does not resolve", r)
		}
		out = append(out, declaredFolder{client: r, real: real})
	}
	return out
}

// reconcile returns the folders that serve the declared ones: each declared
// root not inside a served folder whose walk reaches it (a root the outer walk
// skips keeps a backend of its own), outermost first, reusing a folder of
// existing with the same root and opening the others (each file is read and
// parsed once: no two served folders overlap). A root that does not open is
// logged and left out, so a folder nested in it is served instead. A root
// that is already served (a duplicate) adds nothing. The result is in the
// client's order.
func (s *server) reconcile(declared []declaredFolder, existing []*folder) []*folder {
	order := make([]int, len(declared))
	for i := range order {
		order[i] = i
	}
	// An enclosing root is a prefix of the roots inside it, so shorter first
	// puts every outer root before its nested ones.
	sort.SliceStable(order, func(x, y int) bool { return len(declared[order[x]].real) < len(declared[order[y]].real) })
	var out []*folder
	at := map[*folder]int{}
	for _, i := range order {
		d := declared[i]
		if d.real == "" || s.covered(out, d.real) {
			continue
		}
		var f *folder
		for _, e := range existing {
			if e.ws.IsRoot(d.real) {
				f = e
				break
			}
		}
		if f == nil {
			st := s.loadSettings(d.client)
			ws, err := workspace.OpenWith(d.client, st.Limits(), nil, st.Workspace(s.pathProfile().Commands()))
			if err != nil {
				s.log.Printf("cannot open workspace folder %s: %v", d.client, err)
				continue
			}
			for _, n := range ws.Report().Notices {
				s.log.Printf("workspace %s: %s", d.client, n)
			}
			f = &folder{client: d.client, ws: ws, st: st, diag: diagOptionsOf(st)}
		}
		out = append(out, f)
		at[f] = i
	}
	sort.SliceStable(out, func(x, y int) bool { return at[out[x]] < at[out[y]] })
	return out
}

// covered reports whether one of fs serves real: it holds real (its root or
// below) and its walk reaches it. A directory inside a folder that the walk
// skips is not covered; it needs a backend of its own.
func (s *server) covered(fs []*folder, real string) bool {
	for _, f := range fs {
		if f.ws.Contains(real) && f.ws.Reaches(real) {
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

// folderOfClient is the innermost folder (the longest root) that holds the
// client path p, and p's canonical spelling in it; nil outside every folder.
func (s *server) folderOfClient(p string) (*folder, string) {
	p = filepath.Clean(p)
	var best *folder
	var canon string
	for _, f := range s.folders {
		if c := f.ws.Canon(p); f.ws.Owns(c) && (best == nil || len(f.ws.Root()) > len(best.ws.Root())) {
			best, canon = f, c
		}
	}
	return best, canon
}

// folderOf is the innermost folder that holds the canonical path p, by its
// spelling alone (no file is touched); nil outside every folder.
func (s *server) folderOf(p string) *folder {
	var best *folder
	for _, f := range s.folders {
		if f.ws.Owns(p) && (best == nil || len(f.ws.Root()) > len(best.ws.Root())) {
			best = f
		}
	}
	return best
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
// folders leave the declared list, added ones join it, and the served
// folders are reconciled with it (see reconcile): a served folder no longer
// needed is dropped with its workspace, a newly outermost one is opened.
// Open documents move to whatever serves them now and are published again
// from there. Only the declared list changes on the message loop; reconcile's
// walks run on the folder worker (see queueFolderJob), so edits and
// notifications go on while a folder opens, and requests wait for the result.
func (s *server) didChangeFolders(raw json.RawMessage) *rpcError {
	var p didChangeWorkspaceFoldersParams
	if rerr := decode(raw, &p); rerr != nil {
		return rerr
	}
	if !s.wait() {
		return nil
	}
	changed := false
	for _, wf := range p.Event.Removed {
		path, err := workspace.PathFromURI(wf.URI)
		if err != nil {
			s.log.Printf("ignoring removed folder %q: %v", wf.URI, err)
			continue
		}
		path = filepath.Clean(path)
		real := resolveRoot(path)
		for i, d := range s.declared {
			if d.client == path || real != "" && s.sameRoot(d.real, real) {
				s.declared = append(s.declared[:i:i], s.declared[i+1:]...)
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
		d := s.declare([]string{path})[0]
		dup := false
		for _, e := range s.declared {
			if d.real != "" && s.sameRoot(e.real, d.real) {
				dup = true
				break
			}
		}
		if d.real == "" || dup {
			continue
		}
		s.declared = append(s.declared, d)
		changed = true
	}
	if !changed {
		return nil
	}
	s.queueFolderJob()
	return nil
}

// isGitignore reports whether path names a .gitignore file: the only files
// whose change can alter which directories a folder's walk reaches. fold is
// the folder's CaseInsensitive: on a case-folding volume `.GitIgnore` is the
// same file.
func isGitignore(path string, fold bool) bool {
	if fold {
		return strings.EqualFold(filepath.Base(path), ".gitignore")
	}
	return filepath.Base(path) == ".gitignore"
}

// sameRoot reports whether two resolved roots name one directory: equal
// spellings, or the same directory on disk (a case variant on a
// case-insensitive volume). Empty (unresolved) roots are never the same.
func (s *server) sameRoot(x, y string) bool {
	if x == "" || y == "" {
		return false
	}
	return x == y || workspace.SameDir(x, y)
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
		s.log.Printf("%s failed: %s; %s", method, msg, failureConsequence(method))
	}
}

// failureConsequence says what the server does without the client's answer to
// the request it sent.
func failureConsequence(method string) string {
	switch method {
	case "client/registerCapability":
		return "files changed outside the editor are seen only on didSave or reopen"
	case "workspace/semanticTokens/refresh":
		return "highlighting may stay stale until the client asks again"
	case "workspace/inlayHint/refresh":
		return "inlay hints may stay stale until the client asks again"
	}
	return "the server carries on without it"
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
	if !s.wait() {
		return nil
	}
	var order []*folder
	ignores, reload := false, false
	full := map[*folder]bool{}
	paths := map[*folder][]string{}
	for _, ch := range p.Changes {
		path, err := workspace.PathFromURI(ch.URI)
		if err != nil || !filepath.IsAbs(path) {
			continue
		}
		if s.isSettingsFile(path) { // the user file lies outside every folder
			reload = true
		}
		f, _ := s.folderOfClient(path)
		if f == nil {
			continue
		}
		if _, ok := paths[f]; !ok {
			order = append(order, f)
		}
		paths[f] = append(paths[f], filepath.Clean(path))
		if isGitignore(path, f.ws.CaseInsensitive()) {
			// An open .gitignore keeps its buffer, which the walk never
			// reads, so the whole folder is refreshed from disk.
			ignores = true
			full[f] = true
		}
	}
	for _, f := range order {
		if full[f] {
			f.ws.Refresh()
		} else {
			f.ws.Refresh(paths[f]...)
		}
		s.refreshOthers(f.ws, nil)
	}
	if ignores {
		s.queueFolderJob()
	}
	if reload {
		s.reloadSettings()
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
	if !s.wait() {
		return nil
	}
	path, err := workspace.PathFromURI(p.TextDocument.URI)
	if err != nil || !filepath.IsAbs(path) {
		return nil
	}
	if s.isSettingsFile(path) {
		defer s.reloadSettings()
	}
	if f, _ := s.folderOfClient(path); f != nil {
		if isGitignore(path, f.ws.CaseInsensitive()) {
			f.ws.Refresh()
			s.refreshOthers(f.ws, nil)
			s.queueFolderJob()
			return nil
		}
		f.ws.Refresh(filepath.Clean(path))
		s.refreshOthers(f.ws, nil)
	}
	return nil
}
