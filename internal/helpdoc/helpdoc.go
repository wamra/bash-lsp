// Package helpdoc reads the synopsis of a builtin or a command from the
// machine the server runs on, at run time: `help` of bash for builtins and
// man for commands. Nothing it reads is stored on disk or kept past the
// session, and no analysed text reaches an argument or the environment of the
// child: the name is one argv element, gated by a fixed pattern first.
//
// The programs are found by extool.Find (a program inside a workspace folder
// is refused) and started through a Runner, extool.Run in production.
package helpdoc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
)

// Option is one flag of a synopsis: Flag "-u", Arg "fd" (empty when the flag
// takes none), Text the description when the source has one.
type Option struct{ Flag, Arg, Text string }

// Doc is what a lookup found. Source is "help" or "man".
type Doc struct {
	Name, Synopsis string
	Options        []Option
	Source         string
}

// Runner starts a program: extool.Run in production, a fake in tests.
type Runner func(ctx context.Context, t extool.Tool, args []string, o extool.RunOptions) (extool.Result, error)

const (
	maxCache   = 1024 // names kept, misses included
	maxRunning = 2    // lookups running at once
	runTimeout = 2 * time.Second
	maxOutput  = 256 << 10
	maxName    = 255
)

// commandName is the only shape of name a lookup starts a program for.
var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

var onWindows = runtime.GOOS == "windows"

type entry struct {
	doc   Doc
	found bool
}

// tool is one program, found on first use.
type tool struct {
	once sync.Once
	tool extool.Tool
	ref  *extool.Refusal
	ok   bool
}

// Lookup finds docs. It is safe for concurrent use. At most two lookups run at
// once, and at most 1,024 names are remembered, misses included.
type Lookup struct {
	run   Runner
	st    config.Settings
	prof  *extool.Profile
	roots []string
	sem   chan struct{}
	bash  tool
	man   tool

	mu     sync.Mutex
	cache  map[string]entry
	logged map[string]bool
	logf   func(format string, args ...any)
}

// New builds a Lookup for the settings st (Help.Enabled, Help.BashPath and
// Help.ManPath are read), the PATH profile prof and the workspace folders
// roots, which no program may lie in.
func New(run Runner, st config.Settings, prof *extool.Profile, roots []string) *Lookup {
	return &Lookup{run: run, st: st, prof: prof, roots: append([]string(nil), roots...),
		sem: make(chan struct{}, maxRunning), cache: map[string]entry{}, logged: map[string]bool{}, logf: log.Printf}
}

// SetLogger sets where the one-time notices go (the standard logger by
// default).
func (l *Lookup) SetLogger(f func(format string, args ...any)) {
	l.mu.Lock()
	l.logf = f
	l.mu.Unlock()
}

// logOnce logs msg the first time it is seen.
func (l *Lookup) logOnce(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	first := !l.logged[msg]
	l.logged[msg] = true
	f := l.logf
	l.mu.Unlock()
	if first && f != nil {
		f("helpdoc: %s", msg)
	}
}

func key(kind, name string) string { return kind + "\x00" + name }

// Cached answers from memory and never starts a program: the doc, whether one
// was found, and whether the name was looked up before (a miss is known too).
// kind is "builtin" or "command".
func (l *Lookup) Cached(kind, name string) (Doc, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, known := l.cache[key(kind, name)]
	return e.doc, e.found, known
}

func (l *Lookup) cacheLen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.cache)
}

func (l *Lookup) store(k string, d Doc, found bool) {
	l.mu.Lock()
	if _, have := l.cache[k]; !have && len(l.cache) >= maxCache {
		l.mu.Unlock()
		l.logOnce("the cache is full (%d names); further names are looked up again each time", maxCache)
		return
	}
	l.cache[k] = entry{d, found}
	l.mu.Unlock()
}

func (l *Lookup) resolve(t *tool, name, configured string) {
	t.once.Do(func() { t.tool, t.ref, t.ok = extool.Find(name, configured, l.prof, l.roots) })
}

// status is the state of one program, as doctor words it.
func (l *Lookup) status(label string, t *tool, name, configured string) (line string, ok bool) {
	switch {
	case !l.st.Help.Enabled:
		return label + ": disabled (help.enabled = false)", false
	case onWindows:
		return label + ": unavailable (not supported on Windows)", false
	}
	l.resolve(t, name, configured)
	switch {
	case t.ref != nil:
		return fmt.Sprintf("%s: refused %s: %s", label, t.ref.Path, t.ref.Reason), false
	case !t.ok:
		return label + ": absent", false
	}
	return fmt.Sprintf("%s: available (%s)", label, t.tool.Path), true
}

// Lines are the two lines `doctor` prints: one for help (the bash program)
// and one for man. Nothing is started.
func (l *Lookup) Lines() []string {
	h, _ := l.status("help", &l.bash, "bash", l.st.Help.BashPath)
	m, _ := l.status("man", &l.man, "man", l.st.Help.ManPath)
	return []string{h, m}
}

// Available says whether each source can be used; why holds the lines of
// those that cannot.
func (l *Lookup) Available() (help, man bool, why string) {
	h, help := l.status("help", &l.bash, "bash", l.st.Help.BashPath)
	m, man := l.status("man", &l.man, "man", l.st.Help.ManPath)
	var parts []string
	if !help {
		parts = append(parts, h)
	}
	if !man {
		parts = append(parts, m)
	}
	return help, man, strings.Join(parts, "; ")
}

// Builtin looks up a bash builtin or reserved word with `help`. A name that
// is not on the builtin list starts nothing.
func (l *Lookup) Builtin(ctx context.Context, name string) (Doc, bool) {
	if !l.st.Help.Enabled {
		l.logOnce("lookups are off (help.enabled = false)")
		return Doc{}, false
	}
	if onWindows {
		l.logOnce("lookups are not supported on Windows")
		return Doc{}, false
	}
	if !analysis.IsBuiltin(name) || len(name) > maxName {
		l.logOnce("a lookup was refused: the name is not a builtin")
		return Doc{}, false
	}
	k := key("builtin", name)
	if d, found, known := l.Cached("builtin", name); known {
		return d, found
	}
	l.resolve(&l.bash, "bash", l.st.Help.BashPath)
	if !l.usable("bash", &l.bash) {
		return Doc{}, false
	}
	args := []string{"--noprofile", "--norc", "-c", `help -- "$1"`, "bash", name}
	return l.lookup(ctx, k, l.bash.tool, args, ParseHelp, name)
}

// Command looks up a program with man. The name must be a plain command word
// and the profile must find a program of that name outside every workspace
// folder.
func (l *Lookup) Command(ctx context.Context, name string) (Doc, bool) {
	if !l.st.Help.Enabled {
		l.logOnce("lookups are off (help.enabled = false)")
		return Doc{}, false
	}
	if onWindows {
		l.logOnce("lookups are not supported on Windows")
		return Doc{}, false
	}
	if len(name) > maxName || !commandName.MatchString(name) {
		l.logOnce("a lookup was refused: the name is not a plain command word")
		return Doc{}, false
	}
	path, found := l.prof.Lookup(name)
	if !found {
		l.logOnce("a lookup was refused: the name is not on the PATH profile")
		return Doc{}, false
	}
	if l.insideRoot(path) {
		l.logOnce("a lookup was refused: the command lies inside a workspace folder")
		return Doc{}, false
	}
	k := key("command", name)
	if d, found, known := l.Cached("command", name); known {
		return d, found
	}
	l.resolve(&l.man, "man", l.st.Help.ManPath)
	if !l.usable("man", &l.man) {
		return Doc{}, false
	}
	return l.lookup(ctx, k, l.man.tool, []string{"--", name}, ParseMan, name)
}

func (l *Lookup) insideRoot(path string) bool {
	forms := []string{filepath.Clean(path)}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		forms = append(forms, r)
	}
	for _, f := range forms {
		for _, root := range l.roots {
			if extool.Inside(f, root) {
				return true
			}
		}
	}
	return false
}

// usable reports whether t was found, logging a refusal once.
func (l *Lookup) usable(label string, t *tool) bool {
	switch {
	case t.ref != nil:
		l.logOnce("%s refused: %s: %s", label, t.ref.Path, t.ref.Reason)
	case !t.ok:
		l.logOnce("%s not found", label)
	}
	return t.ok
}

func (l *Lookup) lookup(ctx context.Context, k string, t extool.Tool, args []string, parse func(string, []byte) (Doc, bool), name string) (Doc, bool) {
	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	case <-ctx.Done():
		return Doc{}, false
	}
	res, err := l.run(ctx, t, args, extool.RunOptions{
		Dir:       extool.NeutralDir(),
		Env:       extool.ChildEnv(l.roots, "MANPAGER=cat", "PAGER=cat"),
		Timeout:   runTimeout,
		MaxOutput: maxOutput,
	})
	if ctx.Err() != nil {
		return Doc{}, false // cancelled: nothing is learned, nothing is cached
	}
	// A page longer than the cap still has its first lines.
	if capped := errors.Is(err, extool.ErrOutputCap); err == nil && res.Exit == 0 || capped {
		if d, ok := parse(name, res.Stdout); ok {
			l.store(k, d, true)
			return d, true
		}
	}
	l.store(k, Doc{}, false)
	return Doc{}, false
}
