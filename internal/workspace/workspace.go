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
	"unicode"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/ignore"
	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// Default limits, used when a Limits field is zero.
const (
	defaultMaxFileBytes = DefaultMaxFileBytes
	defaultMaxFiles     = DefaultMaxFiles
)

// DefaultMaxFiles is the file count limit when Limits.MaxFiles is zero.
const DefaultMaxFiles = 20000

// DefaultMaxFileBytes is the file size limit when Limits.MaxFileBytes is zero.
// It applies to open documents too.
const DefaultMaxFileBytes = analysis.MaxFileBytes

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

// keyFor is the map key of a path: lower-cased when the volume is
// case-insensitive (always on Windows; detected per root elsewhere); the
// original spelling is kept for display. strings.ToLower is simple
// per-rune folding; names that differ only by Unicode normalisation (NFC
// against NFD) get different keys.
func keyFor(p string, fold bool) string {
	if fold {
		return strings.ToLower(p)
	}
	return p
}

// key is the map key of p in this workspace.
func (w *Workspace) key(p string) string { return keyFor(p, w.fold) }

// skipDir reports whether a directory named name is never walked, with this
// workspace's case rule.
func (w *Workspace) skipDir(name string) bool {
	if !w.fold {
		return skipDirs[name]
	}
	for d := range skipDirs {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// inSkipDir reports whether a component of path below the root (the last
// included) is a skipped directory name. String-only: no file is touched.
func (w *Workspace) inSkipDir(path string) bool {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if w.skipDir(part) {
			return true
		}
	}
	return false
}

// readDir lists a directory to find the spelling of a name on a folding
// volume; a variable so tests can count the listings.
var readDir = os.ReadDir

// detectFold reports whether the volume of a resolved root is
// case-insensitive; a variable so tests can force it.
var detectFold = caseInsensitive

// flipCase swaps the case of every cased letter of s; ok is false when s
// has none.
func flipCase(s string) (string, bool) {
	rs := []rune(s)
	ok := false
	for i, r := range rs {
		if u := unicode.ToUpper(r); u != r {
			rs[i], ok = u, true
		} else if l := unicode.ToLower(r); l != r {
			rs[i], ok = l, true
		}
	}
	return string(rs), ok
}

// sameFile reports whether a and b both exist (not following a final
// symlink) and are the same file.
func sameFile(a, b string) bool {
	fa, err := os.Lstat(a)
	if err != nil {
		return false
	}
	fb, err := os.Lstat(b)
	return err == nil && os.SameFile(fa, fb)
}

// caseInsensitive is the create-free check of a resolved root: the root
// spelled with its case flipped must exist and be the same directory. A
// root with no letters, or whose flipped spelling does not exist, is
// case-sensitive. The flipped root resolves only if every directory on the
// way folds, so a case-sensitive volume mounted below a folding one is seen
// as sensitive; a folding volume whose mount point is the root itself is
// told apart by a second probe, inside the root: its first entry with a
// letter, spelled with its case flipped, must be the same file.
func caseInsensitive(root string) bool {
	flipped, ok := flipCase(root)
	if !ok || !sameFile(root, flipped) {
		return false
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return true
	}
	for _, e := range ents {
		if f, ok := flipCase(e.Name()); ok {
			return sameFile(filepath.Join(root, e.Name()), filepath.Join(root, f))
		}
	}
	return true
}

type entry struct {
	path    string // display spelling (canonical: resolved root, walk's directory names)
	size    int64
	mtime   int64
	hash    [sha256.Size]byte
	dialect string
	facts   *analysis.FileFacts
}

type overlay struct {
	entry *entry
}

type skipKind uint8

const (
	skipZsh   skipKind = iota + 1 // listed and skipped
	skipBig                       // over MaxFileBytes
	skipCount                     // over MaxFiles
)

type skipInfo struct {
	kind   skipKind
	line   string // LimitsHit line for skipBig
	path   string // display spelling of the skipped file
	reason string
}

// Skip is a file the discovery listed but did not load, with the reason.
type Skip struct {
	Path   string // absolute, in the spelling snapshots use
	Reason string
}

func (w *Workspace) zshSkip(path string) skipInfo {
	return skipInfo{kind: skipZsh, path: path, reason: "zsh is not supported yet"}
}

func (w *Workspace) countSkip(path string) skipInfo {
	return skipInfo{kind: skipCount, path: path, reason: fmt.Sprintf("file limit of %d reached", w.lim.MaxFiles)}
}

func (w *Workspace) bigSkip(path string, size int64) skipInfo {
	return skipInfo{kind: skipBig, line: w.bigLine(w.relSlash(path), size), path: path,
		reason: fmt.Sprintf("%d bytes exceeds the %d byte file limit", size, w.lim.MaxFileBytes)}
}

// rcInfo is a cached .shellcheckrc read: raw is nil when no usable file.
type rcInfo struct {
	raw   []string
	stat  bool // file existed when read
	size  int64
	mtime int64
}

const maxRCBytes = 64 << 10

// maxIgnoreBytes caps a .gitignore read. Real ones are a few KiB; the largest
// in big monorepos stay well under 100 KiB. A larger one is treated as absent.
const maxIgnoreBytes = 1 << 20

// Workspace is safe for concurrent use.
type Workspace struct {
	root string
	lim  Limits
	fold bool // keys fold case: Windows, or a case-insensitive volume

	mu       sync.Mutex // guards everything below except snap
	disk     map[string]*entry
	overlays map[string]*overlay
	gen      uint64
	skips    map[string]skipInfo
	dirName  map[string]string // key(real dir) -> the walk's logical path for it
	rcCache  map[string]rcInfo
	excl     map[string]bool    // keys of directories whose subtrees another workspace owns
	listings map[string]listing // directory listings for spelling on a folding volume, by key
	snap     atomic.Pointer[analysis.Snapshot]
}

// Open walks root, parses and binds every shell file and builds generation 1.
func Open(root string, lim Limits) (*Workspace, error) { return OpenExcluding(root, lim, nil) }

// OpenExcluding is Open without the subtrees of exclude: the roots of other
// workspaces nested inside this one (a file belongs to one workspace). An
// entry not strictly inside the root is ignored. Entries are absolute,
// symlink-resolved paths.
func OpenExcluding(root string, lim Limits, exclude []string) (*Workspace, error) {
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
	w := &Workspace{root: real, lim: lim, fold: windows || detectFold(real), disk: map[string]*entry{}, overlays: map[string]*overlay{},
		skips: map[string]skipInfo{}, dirName: map[string]string{}, rcCache: map[string]rcInfo{}, listings: map[string]listing{}}
	w.excl = w.exclusions(exclude)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reconcileAll()
	w.rebuild()
	return w, nil
}

// Root is the absolute, symlink-resolved workspace root.
func (w *Workspace) Root() string { return w.root }

// CaseInsensitive reports whether paths fold case in this workspace: on
// Windows, or when Open found the root's volume case-insensitive.
func (w *Workspace) CaseInsensitive() bool { return w.fold }

// IsRoot reports whether path (absolute, resolved) is the root, with this
// workspace's case rule. It touches no file.
func (w *Workspace) IsRoot(path string) bool { return w.key(filepath.Clean(path)) == w.key(w.root) }

// Contains reports whether the canonical path is the root or below it, with
// this workspace's case rule; excluded subtrees count. It touches no file.
func (w *Workspace) Contains(path string) bool { return w.within(filepath.Clean(path)) }

// Owns reports whether the canonical path is this workspace's: inside the
// root and not in an excluded subtree. It touches no file.
func (w *Workspace) Owns(path string) bool {
	path = filepath.Clean(path)
	if !w.within(path) {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.excluded(path)
}

// Reaches reports whether the discovery walk descends into dir (absolute,
// resolved): dir is the root, or it is inside the root, not in an excluded
// subtree, not a skipped directory name, and neither it nor a directory above
// it is git-ignored. A folder this workspace does not reach is indexed by
// nobody unless it is served on its own. Only .gitignore files are read.
func (w *Workspace) Reaches(dir string) bool {
	dir = filepath.Clean(dir)
	if !w.within(dir) {
		return false
	}
	if w.IsRoot(dir) {
		return true
	}
	w.mu.Lock()
	excluded := w.excluded(dir)
	w.mu.Unlock()
	if excluded {
		return false
	}
	if w.skipDir(filepath.Base(dir)) {
		return false
	}
	// The spelling relative to the root keeps the case the patterns see;
	// a case variant of the root's own spelling falls back to the folded one.
	rel, err := filepath.Rel(w.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel, err = filepath.Rel(w.key(w.root), w.key(dir)); err != nil {
			return false
		}
	}
	return !w.ignoredPath(rel, true)
}

// SameDir reports whether x and y (absolute, resolved) name one directory:
// equal spellings, or both exist and are the same file on disk (a case
// variant on a case-insensitive volume). It needs no volume flag.
func SameDir(x, y string) bool {
	if x == y {
		return true
	}
	fx, err := os.Stat(x)
	if err != nil {
		return false
	}
	fy, err := os.Stat(y)
	return err == nil && os.SameFile(fx, fy)
}

// SetExcluded replaces the excluded subtrees (see OpenExcluding) and, when
// they changed, re-walks the root and rebuilds. Overlays below a newly
// excluded directory stay until CloseOverlay.
func (w *Workspace) SetExcluded(dirs []string) {
	ex := w.exclusions(dirs)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(ex) == len(w.excl) {
		same := true
		for k := range ex {
			if !w.excl[k] {
				same = false
			}
		}
		if same {
			return
		}
	}
	w.excl = ex
	w.reconcileAll()
	w.rebuild()
}

// exclusions keys the entries of dirs strictly inside the root.
func (w *Workspace) exclusions(dirs []string) map[string]bool {
	ex := map[string]bool{}
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		d = filepath.Clean(d)
		if w.within(d) && !w.IsRoot(d) {
			ex[w.key(d)] = true
		}
	}
	return ex
}

// excluded reports whether path is in an excluded subtree. Caller holds
// w.mu (or w is not shared yet).
func (w *Workspace) excluded(path string) bool {
	if len(w.excl) == 0 {
		return false
	}
	for d := path; ; {
		if w.excl[w.key(d)] {
			return true
		}
		if w.IsRoot(d) {
			return false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// Snapshot is the current immutable snapshot.
func (w *Workspace) Snapshot() *analysis.Snapshot { return w.snap.Load() }

// SetOverlay installs the unsaved editor buffer for path and rebuilds. The
// path is canonicalised to the spelling the walk uses, so the overlay replaces
// the file's disk entry. A buffer over MaxFileBytes is not parsed: the file is
// held as skipped while the overlay lasts. version is not used.
func (w *Workspace) SetOverlay(path string, version int, text []byte) {
	hash := sha256.Sum256(text)
	w.mu.Lock()
	path = w.canon(path, true)
	// Opening a document that equals its file on disk is the common case:
	// reuse the facts the walk built instead of parsing the same bytes again.
	var e *entry
	if d := w.disk[w.key(path)]; d != nil && d.hash == hash && d.path == path {
		e = d
	}
	w.mu.Unlock()
	if e == nil {
		if int64(len(text)) > w.lim.MaxFileBytes {
			// Held to the disk limit: skipped, with the line doctor prints for a
			// file that size, and its disk entry (if any) hidden as for any overlay.
			e = &entry{path: path, hash: hash, facts: analysis.Bind(OversizeFile(path, text, w.bigLine(w.relSlash(path), int64(len(text)))))}
		} else {
			e = parseEntry(path, text)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.overlays[w.key(path)] = &overlay{entry: e}
	w.rebuild()
}

// listing is a directory's names on a folding volume, by folded name, as of
// the directory's modification time.
type listing struct {
	mtime int64
	names map[string]string // keyFor(name, true) -> name; an exact name maps to itself
}

// maxListings bounds the listing cache; past it the cache starts over.
const maxListings = 64

// names lists dir through the cache: a directory is read again only when
// its modification time changed (a rename or create in it changes it), so
// repeated events in an unchanged directory cost one stat. nil when dir
// cannot be listed. Caller holds w.mu.
func (w *Workspace) names(dir string) map[string]string {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	k := w.key(dir)
	if l, ok := w.listings[k]; ok && l.mtime == fi.ModTime().UnixNano() {
		return l.names
	}
	ents, err := readDir(dir)
	if err != nil {
		return nil
	}
	names := make(map[string]string, 2*len(ents))
	for _, e := range ents {
		if f := keyFor(e.Name(), true); names[f] == "" {
			names[f] = e.Name()
		}
		names[e.Name()] = e.Name()
	}
	if len(w.listings) >= maxListings {
		w.listings = map[string]listing{}
	}
	w.listings[k] = listing{mtime: fi.ModTime().UnixNano(), names: names}
	return names
}

// diskPath respells c (canonical, inside the root) element by element as
// the directories on disk spell them: on a folding volume an event can name
// a file or directory by a spelling that differs in case from the one on
// disk, and after a case-only rename the workspace still holds the old one.
// Elements from the first one not found on disk onwards are kept as given. Caller holds
// w.mu; w.fold is set.
func (w *Workspace) diskPath(c string) string {
	rel, err := filepath.Rel(w.root, c)
	if err != nil || rel == "." {
		return c
	}
	parts := strings.Split(rel, string(filepath.Separator))
	out := w.root
	for i, part := range parts {
		names := w.names(out)
		real := ""
		if names != nil {
			if real = names[part]; real == "" {
				real = names[keyFor(part, true)]
			}
		}
		if real == "" {
			return filepath.Join(append([]string{out}, parts[i:]...)...)
		}
		out = filepath.Join(out, real)
	}
	return out
}

// OversizeFile is the tree of a document over the size limit: no text, no
// statements, Skipped set to reason, and the dialect detection gives path and
// text (only the first lines of text are read).
func OversizeFile(path string, text []byte, reason string) *tree.File {
	f := tree.NewFile(path, nil)
	f.Skipped = reason
	f.Dialect = parse.Detect(path, text)
	return f
}

// CloseOverlay drops the overlay of path and re-reads the file: it takes its
// content on disk now (the editor may have saved it, or created it, while it
// was open), or disappears if it is not on disk or not a file the walk lists.
func (w *Workspace) CloseOverlay(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.canon(path, true)
	k := w.key(p)
	if _, ok := w.overlays[k]; !ok {
		return
	}
	delete(w.overlays, k)
	w.refreshOne(p)
	w.rebuild()
}

// maxRefreshPaths is the batch size above which Refresh re-walks the root
// instead of checking each path (each check reads the .gitignore files of
// every directory above the path; the walk reads each once).
const maxRefreshPaths = 512

// Refresh re-reads the given paths from disk (created, changed or deleted:
// the state on disk decides, so a delete and a create of one path arrive in
// either order) and rebuilds at most once. With no paths it reconciles the
// whole workspace: size and mtime first, then content hash. A path is
// accepted exactly when the full walk would list it. Paths outside the root
// or in an excluded subtree are ignored, and so is a path with an open
// overlay: the editor's text wins, and CloseOverlay re-reads the file. A
// directory (created, or deleted while files below it are held), a
// .gitignore, or more than maxRefreshPaths paths re-walk the whole root.
func (w *Workspace) Refresh(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	changed := w.rcStale()
	if changed {
		w.rcCache = map[string]rcInfo{}
	}
	full := len(paths) == 0 || len(paths) > maxRefreshPaths
	var todo []string
	seen := map[string]bool{} // one refresh per file (a rename can name it twice)
	for _, p := range paths {
		if full {
			break
		}
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		c := w.canon(p, false)
		k := w.key(c)
		if !w.within(c) || w.excluded(c) || w.overlays[k] != nil || w.inSkipDir(c) {
			continue
		}
		gi := filepath.Base(c) == ".gitignore"
		if dir := !gi && w.isDirEvent(c, k); gi || dir {
			// Inside a skipped or ignored directory nothing is listed and no
			// .gitignore is read: the walk would not change. A .gitignore is
			// judged by the directory it sits in (it applies even when it
			// ignores itself), a directory by itself.
			at := c
			if gi {
				at = filepath.Dir(c)
			}
			if rel, err := filepath.Rel(w.root, at); err == nil && rel != "." && w.ignoredPath(rel, true) {
				continue
			}
			full = true
			break
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		if w.fold { // the spelling on disk, of every element (case-only renames)
			c = w.diskPath(c)
		}
		todo = append(todo, c)
	}
	if full {
		if w.reconcileAll() {
			changed = true
		}
	} else {
		for _, c := range todo {
			if w.refreshOne(c) {
				changed = true
			}
		}
	}
	if changed {
		w.rebuild()
	}
}

// isDirEvent reports whether the canonical path c (key k) names a directory
// now, or named one the walk held files under. Caller holds w.mu.
func (w *Workspace) isDirEvent(c, k string) bool {
	if fi, err := os.Lstat(c); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fi.IsDir()
		}
		// As the walk: a link to a directory inside the root is walked.
		rt, err := filepath.EvalSymlinks(c)
		if err != nil || !w.within(rt) {
			return false
		}
		ti, err := os.Stat(rt)
		return err == nil && ti.IsDir()
	}
	if _, ok := w.disk[k]; ok {
		return false
	}
	if _, ok := w.skips[k]; ok {
		return false
	}
	if _, ok := w.dirName[k]; ok {
		return true
	}
	prefix := k + string(filepath.Separator)
	for dk := range w.disk {
		if strings.HasPrefix(dk, prefix) {
			return true
		}
	}
	for sk := range w.skips {
		if strings.HasPrefix(sk, prefix) {
			return true
		}
	}
	return false
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

// Skipped lists the files the last discovery skipped, with the reason for
// each, sorted by path. Its length is Report().Skipped.
func (w *Workspace) Skipped() []Skip {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Skip, 0, len(w.skips))
	for _, s := range w.skips {
		out = append(out, Skip{Path: s.path, Reason: s.reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Canon returns the key SetOverlay would store path under: the spelling the
// discovery walk uses (symlinks in parent directories and in the final element
// resolved, directory aliases renamed to the walk's names, and on a folding
// volume the spelling of the file on disk or of its overlay). Snapshot
// queries take this spelling.
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
	out := filepath.Join(rdir, base)
	for d := rdir; ; {
		if name, ok := w.dirName[w.key(d)]; ok {
			rest, _ := filepath.Rel(d, rdir)
			out = filepath.Join(name, rest, base)
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if w.fold { // the spelling the workspace already holds the file under
		k := w.key(out)
		if e := w.disk[k]; e != nil {
			return e.path
		}
		if o := w.overlays[k]; o != nil {
			return o.entry.path
		}
		if s, ok := w.skips[k]; ok {
			return s.path
		}
	}
	return out
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
	f := parse.File(path, src)
	return &entry{path: path, hash: sha256.Sum256(src), dialect: f.Dialect.String(), facts: analysis.Bind(f)}
}

// readConfig reads a small file the walk consults (.gitignore, .shellcheckrc):
// a regular file, or a symlink that resolves to a regular file inside the
// root, of at most max bytes. ok is false for anything else. Nothing that is
// not a regular file is opened: opening a FIFO blocks until a writer appears,
// and every request waits for the first build.
func (w *Workspace) readConfig(path string, max int64) (data []byte, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		rt, err := filepath.EvalSymlinks(path)
		if err != nil || !w.within(rt) {
			return nil, false
		}
		if fi, err = os.Stat(rt); err != nil {
			return nil, false
		}
		path = rt
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, false
	}
	data, big, err := readBounded(path, max)
	if err != nil || big {
		return nil, false
	}
	return data, true
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
	w.snap.Store(analysis.Build(files, analysis.Options{Root: w.root, SearchPaths: w.searchPaths, Prev: w.snap.Load(), Oversize: w.oversize}, w.gen))
}

// oversize reports whether path is a file the walk skipped for its size.
// Caller holds w.mu (Build calls it from rebuild).
func (w *Workspace) oversize(path string) bool {
	s, ok := w.skips[w.key(path)]
	return ok && s.kind == skipBig
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
	rel, err := filepath.Rel(w.key(w.root), w.key(real))
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
	w.dirName = map[string]string{w.key(w.root): w.root}
	visited := map[string]bool{w.key(w.root): true}
	var rec func(dir, real, rel string, set *ignore.Set)
	rec = func(dir, real, rel string, set *ignore.Set) {
		if data, ok := w.readConfig(filepath.Join(dir, ".gitignore"), maxIgnoreBytes); ok {
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
				if w.skipDir(name) || set.Ignored(relp, true) || visited[w.key(target)] || w.excluded(target) {
					continue
				}
				visited[w.key(target)] = true
				w.dirName[w.key(target)] = full
				rec(full, target, relp, set)
				continue
			}
			if !de.Type().IsRegular() || set.Ignored(relp, false) {
				continue
			}
			switch classify(full) {
			case isZsh:
				w.skips[w.key(full)] = w.zshSkip(full)
				continue
			case notShell:
				continue
			}
			fi, err := de.Info()
			if err != nil {
				continue
			}
			if fi.Size() > w.lim.MaxFileBytes {
				w.skips[w.key(full)] = w.bigSkip(full, fi.Size())
				continue
			}
			if len(cands) >= w.lim.MaxFiles {
				w.skips[w.key(full)] = w.countSkip(full)
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
		k := w.key(c.path)
		seen[k] = true
		if e := w.disk[k]; e != nil && e.size == c.size && e.mtime == c.mtime && e.path == c.path {
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
			if e := w.disk[w.key(c.path)]; e != nil && e.hash == h && e.path == c.path { // map is only read during this phase
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
		k := w.key(todo[i].path)
		if r.e == nil {
			if _, ok := w.disk[k]; ok {
				delete(w.disk, k)
				changed = true
			}
			if r.big { // grew past the limit after the stat
				w.skips[k] = w.bigSkip(todo[i].path, todo[i].size)
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
	k := w.key(path)
	drop := func() bool {
		delete(w.skips, k)
		if _, ok := w.disk[k]; ok {
			delete(w.disk, k)
			return true
		}
		return false
	}
	if !w.within(path) || w.excluded(path) { // nothing outside is even stat'ed
		return drop()
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
		w.skips[k] = w.zshSkip(path)
		return d
	}
	if fi.Size() > w.lim.MaxFileBytes {
		d := drop()
		w.skips[k] = w.bigSkip(path, fi.Size())
		return d
	}
	old := w.disk[k]
	if old == nil && len(w.disk) >= w.lim.MaxFiles {
		w.skips[k] = w.countSkip(path)
		return false
	}
	src, big, err := readBounded(path, w.lim.MaxFileBytes)
	if err != nil {
		return drop()
	}
	if big {
		d := drop()
		w.skips[k] = w.bigSkip(path, fi.Size())
		return d
	}
	delete(w.skips, k)
	h := sha256.Sum256(src)
	if old != nil && old.hash == h && old.path == path {
		old.size, old.mtime = fi.Size(), fi.ModTime().UnixNano()
		return false
	}
	e := parseEntry(path, src)
	e.size, e.mtime = fi.Size(), fi.ModTime().UnixNano()
	w.disk[k] = e
	return true
}

// accepts applies the walk's rules to one canonical path: inside the root,
// not in an excluded subtree, not under a skipped or git-ignored directory,
// and a shell (or zsh) file.
func (w *Workspace) accepts(path string) status {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || !w.within(path) || w.excluded(path) {
		return notShell
	}
	if w.ignoredPath(rel, false) {
		return notShell
	}
	return classify(path)
}

// ignoredPath reports whether the walk would not reach rel (relative to the
// root): a directory above it is skipped or git-ignored, or rel itself is
// git-ignored (as a directory when dir). Skipped names are checked before
// any .gitignore is read. The .gitignore files of the directories on the
// way are read.
func (w *Workspace) ignoredPath(rel string, dir bool) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		if w.skipDir(part) {
			return true
		}
	}
	set := &ignore.Set{}
	d, relDir := w.root, ""
	for i, part := range parts {
		if data, ok := w.readConfig(filepath.Join(d, ".gitignore"), maxIgnoreBytes); ok {
			set = set.Clone()
			set.Add(ignore.Parse(relDir, data))
		}
		relp := part
		if relDir != "" {
			relp = relDir + "/" + part
		}
		if i < len(parts)-1 {
			if set.Ignored(relp, true) {
				return true
			}
		} else if set.Ignored(relp, dir) {
			return true
		}
		d, relDir = filepath.Join(d, part), relp
	}
	return false
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

// readRC reads dir/.shellcheckrc. raw is nil when the file is absent, not a
// regular file, larger than 64 KiB, or a symlink resolving outside the root;
// non-nil (possibly empty) otherwise.
func (w *Workspace) readRC(dir string) rcInfo {
	file := filepath.Join(dir, ".shellcheckrc")
	var info rcInfo
	fi, err := os.Stat(file)
	if err != nil {
		return info
	}
	info.stat, info.size, info.mtime = true, fi.Size(), fi.ModTime().UnixNano()
	data, ok := w.readConfig(file, maxRCBytes)
	if !ok {
		return info
	}
	info.raw = []string{}
	for _, line := range strings.Split(string(data), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "source-path=")
		if !ok {
			continue
		}
		// One path per line: ShellCheck takes the whole value, so a space or
		// a path-list separator belongs to the path. Quotes are optional.
		if p := unquoteRC(strings.TrimSpace(v)); p != "" {
			info.raw = append(info.raw, p)
		}
	}
	return info
}

// unquoteRC removes one pair of matching quotes around an rc value.
func unquoteRC(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
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
