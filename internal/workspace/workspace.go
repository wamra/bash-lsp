// Package workspace discovers shell files under a root, keeps editor
// overlays, and publishes immutable analysis snapshots, one generation per
// change.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash/fnv"
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

// DefaultMaxDirDepth is the directory depth limit when Limits.MaxDirDepth is
// zero: a directory more than this many levels below the root is not read.
const DefaultMaxDirDepth = 64

// Limits bound discovery. Zero values mean 2<<20, 20000 and 64.
type Limits struct {
	MaxFileBytes int64
	MaxFiles     int
	MaxDirDepth  int
}

// Report describes the last discovery and the files now held.
type Report struct {
	Files, Skipped int
	LimitsHit      []string // human-readable lines
	Dialects       map[string]int
	// Notices lists settings the workspace dropped, one line each, sorted: a
	// sources.paths entry or a .shellcheckrc source-path that resolves outside
	// the root or is not root-relative.
	Notices []string
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
//
// On a folding volume a Windows long-path prefix is dropped (`\\?\C:\x`
// and `C:\x` are one file; `\\?\UNC\s\x` is `\\s\x`).
func keyFor(p string, fold bool) string {
	if fold {
		if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
			p = `\\` + rest
		} else if rest, ok := strings.CutPrefix(p, `\\?\`); ok && len(rest) >= 2 && isLetter(rest[0]) && rest[1] == ':' {
			p = rest
		}
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

// maxFoldListing is the most entries of one directory read to find the
// spelling of a name on a folding volume; a directory with more is left
// unlisted.
const maxFoldListing = 1024

// readDirN lists dir for the spelling of a name on a folding volume, sorted by
// name, reading at most n entries (n <= 0 reads all of them); a variable so
// tests can count the listings.
var readDirN = func(dir string, n int) ([]os.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ents, err := f.ReadDir(n)
	if err == io.EOF {
		err = nil
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	return ents, err
}

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
	skipZsh    skipKind = iota + 1 // listed and skipped
	skipBig                        // over MaxFileBytes
	skipCount                      // over MaxFiles
	skipUnread                     // listed, could not be read
)

type skipInfo struct {
	kind   skipKind
	line   string // LimitsHit line for skipBig
	path   string // display spelling of the skipped file
	reason string
}

// short is the reason in a few words, as a refusal lists it.
func (s skipInfo) short() string {
	switch s.kind {
	case skipBig:
		return "too large"
	case skipCount:
		return "over the file limit"
	case skipUnread:
		return "unreadable"
	}
	return "unsupported dialect"
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

func (w *Workspace) unreadSkip(path string) skipInfo {
	return skipInfo{kind: skipUnread, path: path, reason: "the file could not be read"}
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

// The work the ignore files of one walk may cause, in counts (see
// ignore.Budget): rules compiled over all the .gitignore files, and rule
// evaluations over all the paths. Real trees use a small fraction of both.
const (
	maxIgnoreRules      = 20000
	maxIgnoreEvals      = 1000000 // floor of the evaluations
	ignoreEvalsPerEntry = 500     // added for every entry the walk visits
)

// Workspace is safe for concurrent use.
type Workspace struct {
	root string
	lim  Limits
	fold bool // keys fold case: Windows, or a case-insensitive volume

	mu        sync.Mutex // guards everything below except snap
	disk      map[string]*entry
	overlays  map[string]*overlay
	gen       uint64
	skips     map[string]skipInfo
	dirName   map[string]string // key(real dir) -> the walk's logical path for it
	rcCache   map[string]rcInfo
	rcReal    map[string]bool                // search directory -> inside the root after links; cleared with rcCache
	excl      map[string]bool                // keys of directories whose subtrees another workspace owns
	listings  map[string]listing             // directory listings for spelling on a folding volume, by key
	unlisted  map[string]string              // keys of directories too large to list for spelling -> their LimitsHit line; cleared by a full walk
	ignBudget *ignore.Budget                 // the ignore work of the last walk
	depthHit  map[string]string              // root-relative directories the walk did not enter for depth -> LimitsHit line; reset by a walk
	unwalked  map[string]analysis.Unanalysed // keys of directories the walk did not list (too deep, unreadable) -> the directory and why; reset by a walk
	unreadDir map[string]string              // keys of directories that could not be listed -> their LimitsHit line; reset by a walk
	exclFiles int                            // shell-named files, and
	exclDirs  int                            // directories, the last walk left out by files.exclude, files.include or a .gitignore
	rcNotes   map[string]string              // dropped .shellcheckrc entries, by rc directory and entry; cleared with rcCache
	snap      atomic.Pointer[analysis.Snapshot]

	// Fixed at Open (Config).
	commands func(name string) (string, bool)
	include  *ignore.Set // nil: every file the walk lists
	exclude  excludeSets // never empty
	dialect  tree.Dialect
	srcDirs  []string // sources.paths: absolute, inside the root
	notes    []string // sources.paths entries dropped
}

// Config is what the settings give a workspace besides its limits. The zero
// Config is today's discovery and analysis.
type Config struct {
	Commands    func(name string) (string, bool) // PATH profile lookup; nil = no command edges
	Include     []string                         // gitignore syntax, root-relative; nil = every file discovery lists, else only matching files (a directory pattern includes what is below it)
	Exclude     []string                         // matching files and directories are skipped, as .gitignore'd ones are; wins over a .gitignore negation and over Include
	RepoExclude []string                         // a second list of the same kind, with its own negations: a path is skipped when either list skips it
	SourcePaths []string                         // root-relative directories searched by `source`, after the .shellcheckrc ones; one that is absolute, spelled with ~ or $, or resolves outside the root is dropped and reported in Report.Notices
	Dialect     tree.Dialect                     // fallback dialect for a file that names none; 0 = Bash
}

// Open walks root, parses and binds every shell file and builds generation 1.
func Open(root string, lim Limits) (*Workspace, error) { return OpenExcluding(root, lim, nil) }

// OpenExcluding is OpenWith with a zero Config.

// OpenExcluding is Open without the subtrees of exclude: the roots of other
// workspaces nested inside this one (a file belongs to one workspace). An
// entry not strictly inside the root is ignored. Entries are absolute,
// symlink-resolved paths.
func OpenExcluding(root string, lim Limits, exclude []string) (*Workspace, error) {
	return OpenWith(root, lim, exclude, Config{})
}

// OpenWith is OpenExcluding with the workspace's settings (Config).
func OpenWith(root string, lim Limits, exclude []string, cfg Config) (*Workspace, error) {
	if lim.MaxFileBytes <= 0 {
		lim.MaxFileBytes = defaultMaxFileBytes
	}
	if lim.MaxFiles <= 0 {
		lim.MaxFiles = defaultMaxFiles
	}
	if lim.MaxDirDepth <= 0 {
		lim.MaxDirDepth = DefaultMaxDirDepth
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
		skips: map[string]skipInfo{}, dirName: map[string]string{}, rcCache: map[string]rcInfo{}, rcReal: map[string]bool{}, rcNotes: map[string]string{}, listings: map[string]listing{}, unlisted: map[string]string{}, depthHit: map[string]string{},
		commands: cfg.Commands, dialect: cfg.Dialect}
	if w.dialect == 0 {
		w.dialect = tree.Bash
	}
	w.exclude = excludeSets{patternSet(cfg.Exclude, w.fold), patternSet(cfg.RepoExclude, w.fold)}
	if cfg.Include != nil {
		w.include = patternSet(cfg.Include, w.fold)
	}
	w.srcDirs, w.notes = w.sourceDirs(cfg.SourcePaths)
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
			e = &entry{path: path, hash: hash, facts: analysis.Bind(oversizeFile(path, text, w.bigLine(w.relSlash(path), int64(len(text))), w.dialect))}
		} else {
			e = w.parseEntry(path, text)
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
// cannot be listed, or holds more than maxFoldListing entries: such a
// directory is not read again until the next full walk. Caller holds w.mu.
func (w *Workspace) names(dir string) map[string]string {
	k := w.key(dir)
	if _, ok := w.unlisted[k]; ok {
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	if l, ok := w.listings[k]; ok && l.mtime == fi.ModTime().UnixNano() {
		return l.names
	}
	ents, err := readDirN(dir, maxFoldListing+1)
	if err != nil {
		return nil
	}
	if len(ents) > maxFoldListing {
		w.unlisted[k] = fmt.Sprintf("case-folded spelling not corrected in %s: more than %d entries", w.relSlash(dir), maxFoldListing)
		delete(w.listings, k)
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
	return oversizeFile(path, text, reason, tree.Bash)
}

func oversizeFile(path string, text []byte, reason string, fallback tree.Dialect) *tree.File {
	f := tree.NewFile(path, nil)
	f.Skipped = reason
	f.Dialect = parse.DetectWith(path, text, fallback)
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
	skipN, skipSum := w.skipSig()
	if changed {
		w.rcCache = map[string]rcInfo{}
		w.rcReal = map[string]bool{}
		w.rcNotes = map[string]string{}
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
	if n, sum := w.skipSig(); n != skipN || sum != skipSum {
		changed = true
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
	first := ""
	for _, s := range w.skips {
		switch s.kind {
		case skipBig:
			big = append(big, s.line)
		case skipCount:
			if rel := w.relSlash(s.path); first == "" || rel < first {
				first = rel
			}
		}
	}
	sort.Strings(big)
	r.LimitsHit = big
	if first != "" {
		r.LimitsHit = append(r.LimitsHit, fmt.Sprintf("file limit of %d reached: further files skipped, first: %s", w.lim.MaxFiles, first))
	}
	var dirs []string
	for _, l := range w.depthHit {
		dirs = append(dirs, l)
	}
	for _, l := range w.unreadDir {
		dirs = append(dirs, l)
	}
	for _, l := range w.unlisted {
		dirs = append(dirs, l)
	}
	sort.Strings(dirs)
	r.LimitsHit = append(r.LimitsHit, dirs...)
	r.LimitsHit = append(r.LimitsHit, w.ignBudget.Hit()...)
	for _, e := range w.disk {
		r.Dialects[e.dialect]++
	}
	r.Notices = append(r.Notices, w.notes...)
	for _, n := range w.rcNotes {
		r.Notices = append(r.Notices, n)
	}
	sort.Strings(r.Notices)
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

func (w *Workspace) parseEntry(path string, src []byte) *entry {
	f := parse.FileWith(path, src, w.dialect)
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
	var unanalysed []analysis.Unanalysed
	for _, s := range w.skips {
		if _, ok := files[s.path]; !ok { // an open document stands in for the file
			unanalysed = append(unanalysed, analysis.Unanalysed{Path: s.path, Reason: s.short()})
		}
	}
	for _, u := range w.unwalked {
		unanalysed = append(unanalysed, u)
	}
	w.gen++
	w.snap.Store(analysis.Build(files, analysis.Options{Root: w.root, SearchPaths: w.searchPaths, Prev: w.snap.Load(), Oversize: w.oversize, Commands: w.commands,
		Unanalysed: unanalysed, Excluded: analysis.Excluded{Files: w.exclFiles, Dirs: w.exclDirs}}, w.gen))
}

// skipSig summarises the set of skipped files, so that Refresh can tell that
// a refresh changed it: the snapshot lists them. Caller holds w.mu.
func (w *Workspace) skipSig() (n int, sum uint64) {
	add := func(k string) {
		h := fnv.New64a()
		h.Write([]byte(k))
		sum ^= h.Sum64()
	}
	for k := range w.skips {
		add(k)
	}
	for k := range w.unwalked {
		add("dir:" + k)
	}
	add(fmt.Sprint("excluded:", w.exclFiles, ",", w.exclDirs))
	return len(w.skips) + len(w.unwalked), sum
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
	w.depthHit = map[string]string{}
	w.unwalked = map[string]analysis.Unanalysed{}
	w.unreadDir = map[string]string{}
	w.exclFiles, w.exclDirs = 0, 0
	w.unlisted = map[string]string{} // a full walk lists large directories again
	visited := map[string]bool{w.key(w.root): true}
	w.ignBudget = ignore.NewBudget(maxIgnoreRules, maxIgnoreEvals, ignoreEvalsPerEntry)
	var rec func(dir, real, rel string, set *ignore.Set, depth int)
	rec = func(dir, real, rel string, set *ignore.Set, depth int) {
		if data, ok := w.readConfig(filepath.Join(dir, ".gitignore"), maxIgnoreBytes); ok {
			set = set.Clone()
			set.Add(w.ignBudget.ParseFold(rel, data, w.fold))
		}
		ents, err := os.ReadDir(dir) // sorted by name
		if err != nil {
			w.unwalked[w.key(dir)] = analysis.Unanalysed{Path: dir, Reason: "directory unreadable"}
			w.unreadDir[w.key(dir)] = fmt.Sprintf("directory could not be read: %s and below not read", w.relSlash(dir))
			return
		}
		for _, de := range ents {
			name := de.Name()
			w.ignBudget.Visit(de.IsDir() || de.Type()&os.ModeSymlink != 0)
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
				if w.skipDir(name) || visited[w.key(target)] || w.excluded(target) {
					continue
				}
				if set.Ignored(relp, true) || w.exclude.Ignored(relp, true) {
					w.exclDirs++
					continue
				}
				if depth+1 > w.lim.MaxDirDepth {
					w.depthHit[w.key(relp)] = fmt.Sprintf("directory depth limit of %d reached: %s and below not read", w.lim.MaxDirDepth, relp)
					w.unwalked[w.key(full)] = analysis.Unanalysed{Path: full, Reason: "directory too deep"}
					continue
				}
				if de.Type()&os.ModeSymlink != 0 && w.aliasIgnored(target) {
					continue // a link to a directory the walk skips by its own name or place
				}
				visited[w.key(target)] = true
				w.dirName[w.key(target)] = full
				rec(full, target, relp, set, depth+1)
				continue
			}
			if !de.Type().IsRegular() {
				continue
			}
			if set.Ignored(relp, false) || w.exclude.Ignored(relp, false) || !w.included(relp) {
				if shellExts[strings.ToLower(filepath.Ext(name))] {
					w.exclFiles++ // by name: the file is not opened
				}
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
	rec(w.root, w.root, "", (&ignore.Set{}).WithBudget(w.ignBudget), 0)
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
			e := w.parseEntry(c.path, src)
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
			} else {
				w.skips[k] = w.unreadSkip(todo[i].path)
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
		d := drop()
		w.skips[k] = w.unreadSkip(path)
		return d
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
	e := w.parseEntry(path, src)
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
	if w.ignoredPath(rel, false) || !w.included(filepath.ToSlash(rel)) {
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
	if n := len(parts) - 1; dir && n+1 > w.lim.MaxDirDepth || n > w.lim.MaxDirDepth {
		return true // the walk does not enter a directory this deep
	}
	b := ignore.NewBudget(maxIgnoreRules, maxIgnoreEvals, ignoreEvalsPerEntry)
	set := (&ignore.Set{}).WithBudget(b)
	d, relDir := w.root, ""
	for i, part := range parts {
		if data, ok := w.readConfig(filepath.Join(d, ".gitignore"), maxIgnoreBytes); ok {
			set = set.Clone()
			set.Add(b.ParseFold(relDir, data, w.fold))
		}
		relp := part
		if relDir != "" {
			relp = relDir + "/" + part
		}
		isDir := i < len(parts)-1 || dir
		b.Visit(isDir)
		if set.Ignored(relp, isDir) || w.exclude.Ignored(relp, isDir) {
			return true
		}
		d, relDir = filepath.Join(d, part), relp
	}
	return false
}

// excludeSets are the exclude lists; a path is excluded when any list
// excludes it, and a negation works only inside its own list.
type excludeSets []*ignore.Set

func (e excludeSets) Ignored(rel string, isDir bool) bool {
	for _, s := range e {
		if s.Ignored(rel, isDir) {
			return true
		}
	}
	return false
}

// patternSet compiles files.include or files.exclude patterns (gitignore
// syntax, relative to the root) into one matcher.
func patternSet(patterns []string, fold bool) *ignore.Set {
	set := &ignore.Set{}
	set.Add(ignore.ParseFold("", []byte(strings.Join(patterns, "\n")), fold))
	return set
}

// aliasIgnored reports whether target, the resolved directory a link in the
// walk leads to, is one the walk skips under its own name or place: a skipped
// name, an ignored or excluded path as spelled from the root, or a path in an
// excluded subtree. The link's own name is checked by the caller.
func (w *Workspace) aliasIgnored(target string) bool {
	rel, err := filepath.Rel(w.root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		if rel, err = filepath.Rel(w.key(w.root), w.key(target)); err != nil {
			return false
		}
	}
	if rel == "." {
		return false
	}
	return w.skipDir(filepath.Base(target)) || w.ignoredPath(rel, true)
}

// included reports whether the file rel (slash-separated, relative to the
// root) passes files.include: always without the key, else when a pattern
// matches the file or a directory above it. A match here is the set's
// "ignored" verdict; a negated pattern ("!x") takes a file out again unless a
// directory above it matched.
func (w *Workspace) included(rel string) bool {
	if w.include == nil {
		return true
	}
	if w.include.Ignored(rel, false) {
		return true
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && w.include.Ignored(rel[:i], true) {
			return true
		}
	}
	return false
}

// sourceDirs checks the sources.paths entries. An entry must be a directory
// below the root named relative to it: one that is empty, absolute, spelled
// with ~ or $ (nothing expands them here), or that resolves outside the root,
// lexically or through a symlink, is dropped and gives a note. Duplicates
// are kept once.
func (w *Workspace) sourceDirs(entries []string) (dirs, notes []string) {
	seen := map[string]bool{}
	drop := func(e, why string) {
		notes = append(notes, fmt.Sprintf("sources.paths %q: %s (ignored)", e, why))
	}
	for _, e := range entries {
		switch {
		case e == "":
			drop(e, "empty entry")
			continue
		case filepath.IsAbs(e) || filepath.VolumeName(e) != "" || strings.HasPrefix(e, "/") || strings.HasPrefix(e, `\\`):
			drop(e, "an absolute path; use a path relative to the workspace root")
			continue
		case strings.HasPrefix(e, "~") || strings.Contains(e, "$"):
			drop(e, "~ and $ are not expanded; use a path relative to the workspace root")
			continue
		}
		d := filepath.Join(w.root, filepath.FromSlash(e))
		if !w.within(d) {
			drop(e, "resolves outside the workspace root")
			continue
		}
		if rt, err := filepath.EvalSymlinks(d); err == nil && !w.within(rt) {
			drop(e, "resolves outside the workspace root")
			continue
		}
		if !seen[w.key(d)] {
			seen[w.key(d)] = true
			dirs = append(dirs, d)
		}
	}
	return dirs, notes
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

// searchPaths returns the directories `source` looks in for path: the
// source-path= entries of the nearest .shellcheckrc at or above path's
// directory (inside the root only; SCRIPTDIR is path's dir), then the
// sources.paths of the settings. An rc entry that leads outside the root is
// dropped (and noted); nothing here touches the disk. Called from
// analysis.Build, i.e. with w.mu held (or before w is shared).
func (w *Workspace) searchPaths(path string) []string {
	dir := filepath.Dir(path)
	var out []string
	for w.within(dir) {
		info, ok := w.rcCache[dir]
		if !ok {
			info = w.readRC(dir)
			w.rcCache[dir] = info
		}
		if info.raw != nil {
			for _, v := range info.raw {
				r := resolveSearch(filepath.Dir(path), v)
				if !w.rcInside(r) {
					w.noteRC(dir, v)
					continue
				}
				out = append(out, r)
			}
			break
		}
		if dir == w.root {
			break
		}
		dir = filepath.Dir(dir)
	}
	return append(out, w.srcDirs...)
}

// rcInside is insideRoot of a search directory, answered once per directory
// until the rc files are read again.
func (w *Workspace) rcInside(r string) bool {
	if !w.within(r) {
		return false
	}
	ok, seen := w.rcReal[r]
	if !seen {
		ok = w.insideRoot(r)
		w.rcReal[r] = ok
	}
	return ok
}

// noteRC records that the source-path= entry v of dir's .shellcheckrc was
// dropped.
func (w *Workspace) noteRC(dir, v string) {
	k := dir + "\x00" + v
	if _, ok := w.rcNotes[k]; !ok {
		w.rcNotes[k] = fmt.Sprintf("%s: source-path=%s resolves outside the workspace root (ignored)", filepath.ToSlash(w.relSlash(filepath.Join(dir, ".shellcheckrc"))), v)
	}
}

// readRC reads dir/.shellcheckrc. raw is nil when the file is absent, not a
// regular file, larger than 64 KiB, or a symlink resolving outside the root;
// non-nil (possibly empty) otherwise. An absolute entry outside the root, or
// one that reaches outside through a symlink, is left out and noted; a
// relative one is checked against each script's directory by searchPaths.
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
		p := unquoteRC(strings.TrimSpace(v))
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) && !w.insideRoot(filepath.Clean(p)) {
			w.noteRC(dir, p)
			continue
		}
		info.raw = append(info.raw, p)
	}
	return info
}

// insideRoot reports whether p is inside the root by its spelling and, when it
// exists, by where its links lead.
func (w *Workspace) insideRoot(p string) bool {
	if !w.within(p) {
		return false
	}
	rt, err := filepath.EvalSymlinks(p)
	return err != nil || w.within(rt)
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
