// Package workspace discovers shell files under a root, keeps editor
// overlays, and publishes immutable analysis snapshots, one generation per
// change.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/internal/ignore"
)

// Default limits, used when a Limits field is zero.
const (
	defaultMaxFileBytes = 2 << 20
	defaultMaxFiles     = 20000
)

// Limits bound discovery. Zero values mean 2<<20 and 20000.
type Limits struct {
	MaxFileBytes int64
	MaxFiles     int
}

// Report describes the last discovery and the files now held.
type Report struct {
	Files, Skipped int
	LimitsHit      []string // human-readable lines
	Dialects       map[string]int
}

var (
	shellExts     = map[string]bool{".sh": true, ".bash": true, ".bats": true, ".ksh": true, ".mksh": true}
	shebangShells = map[string]bool{"sh": true, "bash": true, "dash": true, "ash": true, "ksh": true, "mksh": true, "bats": true}
	skipDirs      = map[string]bool{".git": true, "node_modules": true, ".Wammy": true}
)

const windows = runtime.GOOS == "windows"

// keyFor is the map key of a path: lower-cased on Windows, where paths are
// case-insensitive; the original spelling is kept for display.
func keyFor(p string, win bool) string {
	if win {
		return strings.ToLower(p)
	}
	return p
}

func key(p string) string { return keyFor(p, windows) }

type entry struct {
	path    string // display spelling (canonical: resolved root, walk's directory names)
	size    int64
	mtime   int64
	hash    [sha256.Size]byte
	dialect string
	facts   *analysis.FileFacts
}

type overlay struct {
	version int
	entry   *entry
}

type skipKind uint8

const (
	skipZsh   skipKind = iota + 1 // listed and skipped
	skipBig                       // over MaxFileBytes
	skipCount                     // over MaxFiles
)

type skipInfo struct {
	kind skipKind
	line string // LimitsHit line for skipBig
}

// rcInfo is a cached .shellcheckrc read: raw is nil when no usable file.
type rcInfo struct {
	raw   []string
	stat  bool // file existed when read
	size  int64
	mtime int64
}

const maxRCBytes = 64 << 10

// Workspace is safe for concurrent use.
type Workspace struct {
	root string
	lim  Limits

	mu       sync.Mutex // guards everything below except snap
	disk     map[string]*entry
	overlays map[string]*overlay
	gen      uint64
	skips    map[string]skipInfo
	dirName  map[string]string // key(real dir) -> the walk's logical path for it
	rcCache  map[string]rcInfo
	snap     atomic.Pointer[analysis.Snapshot]
}

// Open walks root, parses and binds every shell file and builds generation 1.
func Open(root string, lim Limits) (*Workspace, error) {
	if lim.MaxFileBytes <= 0 {
		lim.MaxFileBytes = defaultMaxFileBytes
	}
	if lim.MaxFiles <= 0 {
		lim.MaxFiles = defaultMaxFiles
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(real); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	w := &Workspace{root: real, lim: lim, disk: map[string]*entry{}, overlays: map[string]*overlay{},
		skips: map[string]skipInfo{}, dirName: map[string]string{}, rcCache: map[string]rcInfo{}}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reconcileAll()
	w.rebuild()
	return w, nil
}

// Root is the absolute, symlink-resolved workspace root.
func (w *Workspace) Root() string { return w.root }

// Snapshot is the current immutable snapshot.
func (w *Workspace) Snapshot() *analysis.Snapshot { return w.snap.Load() }

// SetOverlay installs the unsaved editor buffer for path and rebuilds. The
// path is canonicalised to the spelling the walk uses, so the overlay replaces
// the file's disk entry.
func (w *Workspace) SetOverlay(path string, version int, text []byte) {
	hash := sha256.Sum256(text)
	w.mu.Lock()
	path = w.canon(path, true)
	// Opening a document that equals its file on disk is the common case:
	// reuse the facts the walk built instead of parsing the same bytes again.
	var e *entry
	if d := w.disk[key(path)]; d != nil && d.hash == hash && d.path == path {
		e = d
	}
	w.mu.Unlock()
	if e == nil {
		e = parseEntry(path, text)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.overlays[key(path)] = &overlay{version: version, entry: e}
	w.rebuild()
}

// CloseOverlay drops the overlay of path: the file reverts to its disk
// content, or disappears if it is not on disk.
func (w *Workspace) CloseOverlay(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := key(w.canon(path, true))
	if _, ok := w.overlays[k]; !ok {
		return
	}
	delete(w.overlays, k)
	w.rebuild()
}

// Refresh re-reads the given paths from disk (created, changed or deleted).
// With no paths it reconciles the whole workspace: size and mtime first, then
// content hash. A path is accepted exactly when the full walk would list it.
func (w *Workspace) Refresh(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	changed := w.rcStale()
	if changed {
		w.rcCache = map[string]rcInfo{}
	}
	if len(paths) == 0 {
		if w.reconcileAll() {
			changed = true
		}
	} else {
		for _, p := range paths {
			if w.refreshOne(w.canon(p, false)) {
				changed = true
			}
		}
	}
	if changed {
		w.rebuild()
	}
}

// Report summarises discovery and the files currently held from disk.
func (w *Workspace) Report() Report {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := Report{Files: len(w.disk), Skipped: len(w.skips), Dialects: map[string]int{}}
	var big []string
	count := false
	for _, s := range w.skips {
		switch s.kind {
		case skipBig:
			big = append(big, s.line)
		case skipCount:
			count = true
		}
	}
	sort.Strings(big)
	r.LimitsHit = big
	if count {
		r.LimitsHit = append(r.LimitsHit, fmt.Sprintf("file limit of %d reached: further files skipped", w.lim.MaxFiles))
	}
	for _, e := range w.disk {
		r.Dialects[e.dialect]++
	}
	return r
}

// Canon returns the key SetOverlay would store path under: the spelling the
// discovery walk uses (symlinks in parent directories and in the final element
// resolved, directory aliases renamed to the walk's names). Snapshot queries
// take this spelling.
func (w *Workspace) Canon(path string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.canon(path, true)
}

// canon returns the canonical spelling of a client path: parent directories
// resolved through symlinks, then renamed to the walk's directory names (so a
// directory alias and its target agree). With followFinal a symlink as the
// last element is resolved too; without, it is kept (and later rejected).
// Caller holds w.mu.
func (w *Workspace) canon(p string, followFinal bool) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	p = filepath.Clean(p)
	dir, base := filepath.Split(p)
	rdir := resolveExisting(filepath.Clean(dir))
	if followFinal {
		if rt, err := filepath.EvalSymlinks(filepath.Join(rdir, base)); err == nil {
			rdir, base = filepath.Split(rt)
			rdir = filepath.Clean(rdir)
		}
	}
	for d := rdir; ; {
		if name, ok := w.dirName[key(d)]; ok {
			rest, _ := filepath.Rel(d, rdir)
			return filepath.Join(name, rest, base)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return filepath.Join(rdir, base)
}

// resolveExisting resolves symlinks in the longest existing prefix of dir.
func resolveExisting(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return dir
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(dir))
}

func parseEntry(path string, src []byte) *entry {
	d := front.Detect(path, src)
	f := front.Parse(path, src, d)
	return &entry{path: path, hash: sha256.Sum256(src), dialect: d.String(), facts: analysis.Bind(f)}
}

// readBounded reads at most max bytes; tooBig is true when the file is longer.
func readBounded(path string, max int64) (data []byte, tooBig bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return nil, true, nil
	}
	return data, false, nil
}

// rebuild publishes the next generation. Caller holds w.mu.
func (w *Workspace) rebuild() {
	files := make(map[string]*analysis.FileFacts, len(w.disk)+len(w.overlays))
	byKey := make(map[string]string, len(w.disk)+len(w.overlays))
	for k, e := range w.disk {
		files[e.path] = e.facts
		byKey[k] = e.path
	}
	for k, o := range w.overlays {
		if old, ok := byKey[k]; ok {
			delete(files, old)
		}
		files[o.entry.path] = o.entry.facts
	}
	w.gen++
	w.snap.Store(analysis.Build(files, analysis.Options{Root: w.root, SearchPaths: w.searchPaths}, w.gen))
}

// ---- discovery ----

type candidate struct {
	path  string
	size  int64
	mtime int64
}

type status uint8

const (
	notShell status = iota
	isShell
	isZsh
)

func classify(path string) status {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case shellExts[ext]:
		return isShell
	case ext == ".zsh":
		return isZsh
	case ext == "":
		if hasShellShebang(path) {
			return isShell
		}
	}
	return notShell
}

func hasShellShebang(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	buf = buf[:n]
	if !bytes.HasPrefix(buf, []byte("#!")) {
		return false
	}
	line, _, _ := bytes.Cut(buf[2:], []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) == 0 {
		return false
	}
	name := fields[0][strings.LastIndexByte(fields[0], '/')+1:]
	if name == "env" {
		name = ""
		for _, a := range fields[1:] {
			if strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
				continue
			}
			name = a[strings.LastIndexByte(a, '/')+1:]
			break
		}
	}
	return shebangShells[name]
}

func (w *Workspace) within(real string) bool {
	rel, err := filepath.Rel(w.root, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *Workspace) relSlash(p string) string {
	rel, _ := filepath.Rel(w.root, p)
	return filepath.ToSlash(rel)
}

func (w *Workspace) bigLine(rel string, size int64) string {
	return fmt.Sprintf("%s: %d bytes exceeds the %d byte file limit", rel, size, w.lim.MaxFileBytes)
}

// walk lists candidate shell files under the root in lexical order, filling
// w.skips and w.dirName. Caller holds w.mu.
func (w *Workspace) walk() (cands []candidate) {
	w.skips = map[string]skipInfo{}
	w.dirName = map[string]string{key(w.root): w.root}
	visited := map[string]bool{key(w.root): true}
	var rec func(dir, real, rel string, set *ignore.Set)
	rec = func(dir, real, rel string, set *ignore.Set) {
		if data, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			set = set.Clone()
			set.Add(ignore.Parse(rel, data))
		}
		ents, err := os.ReadDir(dir) // sorted by name
		if err != nil {
			return
		}
		for _, de := range ents {
			name := de.Name()
			full := filepath.Join(dir, name)
			relp := name
			if rel != "" {
				relp = rel + "/" + name
			}
			isDir := de.IsDir()
			target := filepath.Join(real, name)
			if de.Type()&os.ModeSymlink != 0 {
				rt, err := filepath.EvalSymlinks(full)
				if err != nil || !w.within(rt) {
					continue
				}
				fi, err := os.Stat(rt)
				if err != nil || !fi.IsDir() {
					continue // a file link duplicates a file inside the root
				}
				isDir, target = true, rt
			}
			if isDir {
				if skipDirs[name] || set.Ignored(relp, true) || visited[key(target)] {
					continue
				}
				visited[key(target)] = true
				w.dirName[key(target)] = full
				rec(full, target, relp, set)
				continue
			}
			if !de.Type().IsRegular() || set.Ignored(relp, false) {
				continue
			}
			switch classify(full) {
			case isZsh:
				w.skips[key(full)] = skipInfo{kind: skipZsh}
				continue
			case notShell:
				continue
			}
			fi, err := de.Info()
			if err != nil {
				continue
			}
			if fi.Size() > w.lim.MaxFileBytes {
				w.skips[key(full)] = skipInfo{skipBig, w.bigLine(relp, fi.Size())}
				continue
			}
			if len(cands) >= w.lim.MaxFiles {
				w.skips[key(full)] = skipInfo{kind: skipCount}
				continue
			}
			cands = append(cands, candidate{full, fi.Size(), fi.ModTime().UnixNano()})
		}
	}
	rec(w.root, w.root, "", &ignore.Set{})
	return
}

type loadResult struct {
	e   *entry
	big bool
}

// reconcileAll re-walks the root and brings w.disk in line. Caller holds w.mu.
func (w *Workspace) reconcileAll() (changed bool) {
	cands := w.walk()
	seen := make(map[string]bool, len(cands))
	var todo []candidate
	for _, c := range cands {
		k := key(c.path)
		seen[k] = true
		if e := w.disk[k]; e != nil && e.size == c.size && e.mtime == c.mtime {
			continue
		}
		todo = append(todo, c)
	}
	for k := range w.disk {
		if !seen[k] {
			delete(w.disk, k)
			changed = true
		}
	}
	loaded := make([]loadResult, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, c := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			src, big, err := readBounded(c.path, w.lim.MaxFileBytes)
			if err != nil {
				return
			}
			if big {
				loaded[i].big = true
				return
			}
			h := sha256.Sum256(src)
			if e := w.disk[key(c.path)]; e != nil && e.hash == h { // map is only read during this phase
				loaded[i].e = &entry{path: e.path, size: c.size, mtime: c.mtime, hash: h, dialect: e.dialect, facts: e.facts}
				return
			}
			e := parseEntry(c.path, src)
			e.size, e.mtime = c.size, c.mtime
			loaded[i].e = e
		}()
	}
	wg.Wait()
	for i, r := range loaded {
		k := key(todo[i].path)
		if r.e == nil {
			if _, ok := w.disk[k]; ok {
				delete(w.disk, k)
				changed = true
			}
			if r.big { // grew past the limit after the stat
				w.skips[k] = skipInfo{skipBig, w.bigLine(w.relSlash(todo[i].path), todo[i].size)}
			}
			continue
		}
		if old := w.disk[k]; old == nil || old.facts != r.e.facts {
			changed = true
		}
		w.disk[k] = r.e
	}
	return changed
}

// refreshOne re-reads one canonical path, accepting it exactly when the full
// walk would list it. Caller holds w.mu.
func (w *Workspace) refreshOne(path string) (changed bool) {
	k := key(path)
	drop := func() bool {
		delete(w.skips, k)
		if _, ok := w.disk[k]; ok {
			delete(w.disk, k)
			return true
		}
		return false
	}
	fi, err := os.Lstat(path) // a symlink is never listed by the walk
	if err != nil || !fi.Mode().IsRegular() {
		return drop()
	}
	switch w.accepts(path) {
	case notShell:
		return drop()
	case isZsh:
		d := drop()
		w.skips[k] = skipInfo{kind: skipZsh}
		return d
	}
	if fi.Size() > w.lim.MaxFileBytes {
		d := drop()
		w.skips[k] = skipInfo{skipBig, w.bigLine(w.relSlash(path), fi.Size())}
		return d
	}
	old := w.disk[k]
	if old == nil && len(w.disk) >= w.lim.MaxFiles {
		w.skips[k] = skipInfo{kind: skipCount}
		return false
	}
	src, big, err := readBounded(path, w.lim.MaxFileBytes)
	if err != nil {
		return drop()
	}
	if big {
		d := drop()
		w.skips[k] = skipInfo{skipBig, w.bigLine(w.relSlash(path), fi.Size())}
		return d
	}
	delete(w.skips, k)
	h := sha256.Sum256(src)
	if old != nil && old.hash == h {
		old.size, old.mtime = fi.Size(), fi.ModTime().UnixNano()
		return false
	}
	e := parseEntry(path, src)
	e.size, e.mtime = fi.Size(), fi.ModTime().UnixNano()
	w.disk[k] = e
	return true
}

// accepts applies the walk's rules to one canonical path: inside the root,
// not under a skipped or git-ignored directory, and a shell (or zsh) file.
func (w *Workspace) accepts(path string) status {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || !w.within(path) {
		return notShell
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	set := &ignore.Set{}
	dir, relDir := w.root, ""
	for i, part := range parts {
		if data, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			set = set.Clone()
			set.Add(ignore.Parse(relDir, data))
		}
		relp := part
		if relDir != "" {
			relp = relDir + "/" + part
		}
		last := i == len(parts)-1
		if !last && (skipDirs[part] || set.Ignored(relp, true)) {
			return notShell
		}
		if last && set.Ignored(relp, false) {
			return notShell
		}
		dir, relDir = filepath.Join(dir, part), relp
	}
	return classify(path)
}

// ---- .shellcheckrc ----

// rcStale reports whether any .shellcheckrc read so far changed on disk.
func (w *Workspace) rcStale() bool {
	for dir, info := range w.rcCache {
		fi, err := os.Stat(filepath.Join(dir, ".shellcheckrc"))
		if (err == nil) != info.stat || err == nil && (fi.Size() != info.size || fi.ModTime().UnixNano() != info.mtime) {
			return true
		}
	}
	return false
}

// searchPaths returns the source-path= entries of the nearest .shellcheckrc at
// or above path's directory, inside the root only. SCRIPTDIR is path's dir.
// Called from analysis.Build, i.e. with w.mu held (or before w is shared).
func (w *Workspace) searchPaths(path string) []string {
	dir := filepath.Dir(path)
	for w.within(dir) {
		info, ok := w.rcCache[dir]
		if !ok {
			info = w.readRC(dir)
			w.rcCache[dir] = info
		}
		if info.raw != nil {
			var out []string
			for _, v := range info.raw {
				out = append(out, resolveSearch(filepath.Dir(path), v))
			}
			return out
		}
		if dir == w.root {
			break
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

// readRC reads dir/.shellcheckrc. raw is nil when the file is absent, larger
// than 64 KiB, or a symlink resolving outside the root; non-nil (possibly
// empty) otherwise.
func (w *Workspace) readRC(dir string) rcInfo {
	file := filepath.Join(dir, ".shellcheckrc")
	var info rcInfo
	fi, err := os.Stat(file)
	if err != nil {
		return info
	}
	info.stat, info.size, info.mtime = true, fi.Size(), fi.ModTime().UnixNano()
	if rt, err := filepath.EvalSymlinks(file); err != nil || !w.within(rt) {
		return info
	}
	data, big, err := readBounded(file, maxRCBytes)
	if err != nil || big {
		return info
	}
	info.raw = []string{}
	for _, line := range strings.Split(string(data), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "source-path=")
		if !ok {
			continue
		}
		for _, p := range strings.Split(strings.TrimSpace(v), string(os.PathListSeparator)) {
			if p = strings.TrimSpace(p); p != "" {
				info.raw = append(info.raw, p)
			}
		}
	}
	return info
}

func resolveSearch(scriptDir, v string) string {
	switch {
	case v == "SCRIPTDIR":
		return scriptDir
	case strings.HasPrefix(v, "SCRIPTDIR/"):
		return filepath.Join(scriptDir, filepath.FromSlash(v[len("SCRIPTDIR/"):]))
	case filepath.IsAbs(v):
		return filepath.Clean(v)
	}
	return filepath.Join(scriptDir, filepath.FromSlash(v))
}
