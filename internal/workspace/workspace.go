// Package workspace discovers shell files under a root, keeps editor
// overlays, and publishes immutable analysis snapshots, one generation per
// change.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	path    string // display spelling
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

// Workspace is safe for concurrent use.
type Workspace struct {
	root string
	lim  Limits

	mu       sync.Mutex // guards everything below except snap
	disk     map[string]*entry
	overlays map[string]*overlay
	gen      uint64
	skipped  int
	limits   []string
	rcCache  map[string][]string // dir -> raw source-path entries ("" cache miss is absent)
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
	w := &Workspace{root: real, lim: lim, disk: map[string]*entry{}, overlays: map[string]*overlay{}, rcCache: map[string][]string{}}
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

// SetOverlay installs the unsaved editor buffer for path and rebuilds.
func (w *Workspace) SetOverlay(path string, version int, text []byte) {
	path = w.abs(path)
	e := parseEntry(path, text)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.overlays[key(path)] = &overlay{version: version, entry: e}
	w.rebuild()
}

// CloseOverlay drops the overlay of path: the file reverts to its disk
// content, or disappears if it is not on disk.
func (w *Workspace) CloseOverlay(path string) {
	k := key(w.abs(path))
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.overlays[k]; !ok {
		return
	}
	delete(w.overlays, k)
	w.rebuild()
}

// Refresh re-reads the given paths from disk (created, changed or deleted).
// With no paths it reconciles the whole workspace: size and mtime first, then
// content hash.
func (w *Workspace) Refresh(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rcCache = map[string][]string{}
	changed := false
	if len(paths) == 0 {
		changed = w.reconcileAll()
	} else {
		for _, p := range paths {
			if w.refreshOne(w.abs(p)) {
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
	r := Report{Files: len(w.disk), Skipped: w.skipped, LimitsHit: append([]string(nil), w.limits...), Dialects: map[string]int{}}
	for _, e := range w.disk {
		r.Dialects[e.dialect]++
	}
	return r
}

func (w *Workspace) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	return filepath.Clean(p)
}

func parseEntry(path string, src []byte) *entry {
	d := front.Detect(path, src)
	f := front.Parse(path, src, d)
	return &entry{path: path, hash: sha256.Sum256(src), dialect: d.String(), facts: analysis.Bind(f)}
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

// classify reports whether path (with the given size) is a shell file by the
// brief's rules, and whether it is a zsh file that is listed and skipped.
func classify(path string) (shell, zsh bool) {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case shellExts[ext]:
		return true, false
	case ext == ".zsh":
		return false, true
	case ext == "":
		return hasShellShebang(path), false
	}
	return false, false
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

// walk lists candidate shell files under the root in lexical order.
func (w *Workspace) walk() (cands []candidate, skipped int, limits []string) {
	visited := map[string]bool{key(w.root): true}
	countHit := false
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
				rec(full, target, relp, set)
				continue
			}
			if !de.Type().IsRegular() || set.Ignored(relp, false) {
				continue
			}
			shell, zsh := classify(full)
			if zsh {
				skipped++
				continue
			}
			if !shell {
				continue
			}
			fi, err := de.Info()
			if err != nil {
				continue
			}
			if fi.Size() > w.lim.MaxFileBytes {
				skipped++
				limits = append(limits, fmt.Sprintf("%s: %d bytes exceeds the %d byte file limit", relp, fi.Size(), w.lim.MaxFileBytes))
				continue
			}
			if len(cands) >= w.lim.MaxFiles {
				skipped++
				if !countHit {
					countHit = true
					limits = append(limits, fmt.Sprintf("file limit of %d reached: remaining files skipped from %s", w.lim.MaxFiles, relp))
				}
				continue
			}
			cands = append(cands, candidate{full, fi.Size(), fi.ModTime().UnixNano()})
		}
	}
	rec(w.root, w.root, "", &ignore.Set{})
	return
}

// reconcileAll re-walks the root and brings w.disk in line. Caller holds w.mu.
func (w *Workspace) reconcileAll() (changed bool) {
	cands, skipped, limits := w.walk()
	w.skipped, w.limits = skipped, limits

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
	loaded := make([]*entry, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, c := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			src, err := os.ReadFile(c.path)
			if err != nil {
				return
			}
			h := sha256.Sum256(src)
			if e := w.disk[key(c.path)]; e != nil && e.hash == h { // read-only access: map is not written during this phase
				loaded[i] = &entry{path: e.path, size: c.size, mtime: c.mtime, hash: h, dialect: e.dialect, facts: e.facts}
				return
			}
			e := parseEntry(c.path, src)
			e.size, e.mtime = c.size, c.mtime
			loaded[i] = e
		}()
	}
	wg.Wait()
	for i, e := range loaded {
		k := key(todo[i].path)
		if e == nil {
			if _, ok := w.disk[k]; ok {
				delete(w.disk, k)
				changed = true
			}
			continue
		}
		if old := w.disk[k]; old == nil || old.facts != e.facts {
			changed = true
		}
		w.disk[k] = e
	}
	return changed
}

// refreshOne re-reads one path. Caller holds w.mu.
func (w *Workspace) refreshOne(path string) (changed bool) {
	k := key(path)
	drop := func() bool {
		if _, ok := w.disk[k]; ok {
			delete(w.disk, k)
			return true
		}
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || !w.accepts(path) || fi.Size() > w.lim.MaxFileBytes {
		return drop()
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return drop()
	}
	h := sha256.Sum256(src)
	if old := w.disk[k]; old != nil && old.hash == h {
		old.size, old.mtime = fi.Size(), fi.ModTime().UnixNano()
		return false
	}
	e := parseEntry(path, src)
	e.size, e.mtime = fi.Size(), fi.ModTime().UnixNano()
	w.disk[k] = e
	return true
}

// accepts applies the discovery rules to one path: inside the root, not under
// a skipped or git-ignored directory, and a shell file.
func (w *Workspace) accepts(path string) bool {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || !w.within(path) {
		return false
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
			return false
		}
		if last && set.Ignored(relp, false) {
			return false
		}
		dir, relDir = filepath.Join(dir, part), relp
	}
	shell, _ := classify(path)
	return shell
}

// ---- .shellcheckrc ----

// searchPaths returns the source-path= entries of the nearest .shellcheckrc at
// or above path's directory, inside the root only. SCRIPTDIR is path's dir.
// Called from analysis.Build, i.e. with w.mu held (or before w is shared).
func (w *Workspace) searchPaths(path string) []string {
	dir := filepath.Dir(path)
	for w.within(dir) {
		raw, ok := w.rcCache[dir]
		if !ok {
			raw = readRC(filepath.Join(dir, ".shellcheckrc"))
			w.rcCache[dir] = raw
		}
		if raw != nil {
			var out []string
			for _, v := range raw {
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

// readRC returns the source-path values of an rc file; nil when the file is
// absent, an empty non-nil slice when present without source-path entries.
func readRC(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		v, ok := strings.CutPrefix(line, "source-path=")
		if !ok {
			continue
		}
		for _, p := range strings.Split(strings.TrimSpace(v), string(os.PathListSeparator)) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
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
