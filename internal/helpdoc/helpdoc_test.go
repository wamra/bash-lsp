package helpdoc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func flagsOf(d Doc) []string {
	var out []string
	for _, o := range d.Options {
		out = append(out, strings.TrimSpace(o.Flag+" "+o.Arg))
	}
	return out
}

func TestParseHelpPicksExactTopic(t *testing.T) {
	out := fixture(t, "help-read.txt")
	d, ok := ParseHelp("read", out)
	if !ok {
		t.Fatal("no doc for read")
	}
	if d.Name != "read" || d.Source != "help" || !strings.HasPrefix(d.Synopsis, "read [-ers] [-u fd]") {
		t.Fatalf("doc %+v", d)
	}
	if strings.Contains(d.Synopsis, "readonly") {
		t.Fatalf("the readonly section leaked into the synopsis: %q", d.Synopsis)
	}
	want := []string{"-e", "-r", "-s", "-u fd", "-t timeout", "-p prompt", "-a array", "-n nchars", "-d delim"}
	if got := flagsOf(d); !slices.Equal(got, want) {
		t.Fatalf("options %q, want %q", got, want)
	}
	if _, ok := ParseHelp("rea", out); ok {
		t.Fatal("a prefix matched a topic")
	}
	if ro, ok := ParseHelp("readonly", out); !ok || !strings.HasPrefix(ro.Synopsis, "readonly [-af]") {
		t.Fatalf("readonly: %+v %v", ro, ok)
	}
	if _, ok := ParseHelp("read", nil); ok {
		t.Fatal("empty output gave a doc")
	}
}

func TestStripOverstrike(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"N\bNA\bAM\bME\bE", "NAME"},
		{"_\bf_\bo_\bo", "foo"},
		{"plain text", "plain text"},
		{"•\b• item", "• item"},
		{"–\b– dash", "– dash"},
		{"\bx", "x"},
		{"", ""},
	} {
		if got := string(StripOverstrike([]byte(tc.in))); got != tc.want {
			t.Errorf("StripOverstrike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseManOptions(t *testing.T) {
	d, ok := ParseMan("sample", fixture(t, "man-bsd.txt"))
	if !ok {
		t.Fatal("no doc")
	}
	if d.Name != "sample" || d.Source != "man" {
		t.Fatalf("doc %+v", d)
	}
	if want := "sample [-@ABc] [--color=when]\n[-D format] [file ...]"; d.Synopsis != want {
		t.Fatalf("synopsis %q, want %q", d.Synopsis, want)
	}
	want := []Option{
		{"-@", "", "Show invented extra keys."},
		{"-A", "", "Include invented entries."},
		{"-D", "format", "Use the given invented format."},
		{"--color", "when", "Colour output when asked."},
	}
	if !slices.Equal(d.Options, want) {
		t.Fatalf("options %+v\nwant    %+v", d.Options, want)
	}
	if _, ok := ParseMan("sample", []byte("no sections here")); ok {
		t.Fatal("a page without SYNOPSIS gave a doc")
	}
}

func TestParseManStripsOverstrike(t *testing.T) {
	page := "S\bSA\bAM\bMP\bPL\bLE\bE(1)\n\nN\bNA\bAM\bME\bE\n     s\bsa\bam\bmp\bpl\ble\be – x\n\nS\bSY\bYN\bNO\bOP\bPS\bSI\bIS\bS\n     s\bsa\bam\bmp\bpl\ble\be [-\b-q\bq]\n\nD\bDE\bES\bSC\bCR\bRI\bIP\bPT\bTI\bIO\bON\bN\n     x\n"
	d, ok := ParseMan("sample", []byte(page))
	if !ok || d.Synopsis != "sample [-q]" {
		t.Fatalf("%+v %v", d, ok)
	}
}

// ---- Lookup ----

type call struct {
	tool extool.Tool
	args []string
	opts extool.RunOptions
	ctx  context.Context
}

type fakeRun struct {
	mu    sync.Mutex
	calls []call
	reply func(c call) (extool.Result, error)
}

func (f *fakeRun) run(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
	c := call{tl, slices.Clone(args), o, ctx}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(c)
	}
	return extool.Result{Exit: 1}, nil
}

func (f *fakeRun) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helpers are not used on Windows")
	}
}

// toolDir makes a directory holding executable files named names.
func toolDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func defaults() config.Settings { return config.Merge(nil, nil, nil) }

func newLookup(t *testing.T, f *fakeRun, st config.Settings, dirs []string, roots []string) *Lookup {
	t.Helper()
	l := New(f.run, st, extool.NewProfile(dirs), roots)
	l.SetLogger(func(string, ...any) {})
	return l
}

func helpReply(c call) (extool.Result, error) {
	if filepath.Base(c.tool.Path) == "bash" {
		return extool.Result{Stdout: fixture0("help-read.txt")}, nil
	}
	return extool.Result{Stdout: fixture0("man-bsd.txt")}, nil
}

func fixture0(name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}
	return b
}

func TestNoSpawnForPrefix(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	if _, ok := l.Builtin(context.Background(), "rea"); ok {
		t.Fatal("a prefix gave a doc")
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d spawns for a prefix", n)
	}
}

func TestNoSpawnForBadNames(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: helpReply}
	dir := toolDir(t, "bash", "man", "tool")
	l := newLookup(t, f, defaults(), []string{dir}, []string{t.TempDir()})
	for _, name := range []string{"$(touch x)", "-n", ";ls", "../x", "./tool", "", " ", "a b", "a\x00b", "tool\n", ".hidden", "-"} {
		if _, ok := l.Command(context.Background(), name); ok {
			t.Errorf("Command(%q) gave a doc", name)
		}
		if _, ok := l.Builtin(context.Background(), name); ok {
			t.Errorf("Builtin(%q) gave a doc", name)
		}
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d spawns for bad names: %+v", n, f.calls)
	}
}

func TestNoSpawnForUnknownCommand(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	if _, ok := l.Command(context.Background(), "not-on-the-path"); ok || f.count() != 0 {
		t.Fatalf("a name not in the profile spawned %d times", f.count())
	}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestArgvLiteral(t *testing.T) {
	skipWindows(t)
	t.Setenv("BASH_ENV", "/tmp/should-not-reach-the-child")
	t.Setenv("ENV", "/tmp/nor-this")
	f := &fakeRun{reply: helpReply}
	dir := toolDir(t, "bash", "man", "sample")
	l := newLookup(t, f, defaults(), []string{dir}, []string{t.TempDir()})
	d, ok := l.Builtin(context.Background(), "read")
	if !ok || d.Source != "help" || d.Name != "read" {
		t.Fatalf("Builtin(read) = %+v %v", d, ok)
	}
	if f.count() != 1 {
		t.Fatalf("%d calls, want 1", f.count())
	}
	c := f.calls[0]
	if c.tool.Path != resolved(t, filepath.Join(dir, "bash")) {
		t.Errorf("program %q", c.tool.Path)
	}
	want := []string{"--noprofile", "--norc", "-c", `help -- "$1"`, "bash", "read"}
	if !slices.Equal(c.args, want) {
		t.Errorf("argv %q, want %q", c.args, want)
	}
	if !slices.Contains(c.opts.Env, "MANPAGER=cat") || !slices.Contains(c.opts.Env, "PAGER=cat") {
		t.Errorf("env lacks MANPAGER=cat and PAGER=cat: %q", c.opts.Env)
	}
	for _, kv := range c.opts.Env {
		if strings.HasPrefix(kv, "BASH_ENV=") || strings.HasPrefix(kv, "ENV=") {
			t.Errorf("child env carries %s", kv)
		}
	}
	if c.opts.Dir != extool.NeutralDir() || c.opts.Timeout != 2*time.Second || c.opts.MaxOutput != 256<<10 {
		t.Errorf("options %+v", c.opts)
	}

	d, ok = l.Command(context.Background(), "sample")
	if !ok || d.Source != "man" {
		t.Fatalf("Command(sample) = %+v %v", d, ok)
	}
	c = f.calls[1]
	if c.tool.Path != resolved(t, filepath.Join(dir, "man")) || !slices.Equal(c.args, []string{"--", "sample"}) {
		t.Errorf("man call %+v", c)
	}
}

func TestChildPathLosesRootEntries(t *testing.T) {
	skipWindows(t)
	root := t.TempDir()
	inside := filepath.Join(root, "bin")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", inside+string(os.PathListSeparator)+"/usr/bin")
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{root})
	l.Builtin(context.Background(), "read")
	if f.count() != 1 {
		t.Fatalf("%d calls", f.count())
	}
	for _, kv := range f.calls[0].opts.Env {
		if strings.HasPrefix(kv, "PATH=") && strings.Contains(kv, inside) {
			t.Errorf("child PATH keeps a workspace entry: %s", kv)
		}
	}
}

func TestHelpProgramInsideRootRefused(t *testing.T) {
	skipWindows(t)
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"bash", "man", "sample"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{bin}, []string{root})
	if _, ok := l.Builtin(context.Background(), "read"); ok {
		t.Error("a bash inside the workspace was used")
	}
	if _, ok := l.Command(context.Background(), "sample"); ok {
		t.Error("a man or a command inside the workspace was used")
	}
	if n := f.count(); n != 0 {
		t.Errorf("%d spawns", n)
	}
	help, man, why := l.Available()
	if help || man || !strings.Contains(why, "inside the workspace folder") {
		t.Errorf("Available = %v %v %q", help, man, why)
	}
	for _, line := range l.Lines() {
		if !strings.Contains(line, "refused") {
			t.Errorf("line %q does not say refused", line)
		}
	}
}

func TestCommandInsideRootRefused(t *testing.T) {
	skipWindows(t)
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sample"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man"), bin}, []string{root})
	if _, ok := l.Command(context.Background(), "sample"); ok || f.count() != 0 {
		t.Fatalf("a command found only inside the workspace spawned %d times", f.count())
	}
}

func TestRepoHelpDisabledHonoured(t *testing.T) {
	skipWindows(t)
	repo := []byte("[help]\nenabled = false\nbash_path = \"/bin/sh\"\n")
	st := config.Merge(nil, repo, nil)
	if st.Help.Enabled || st.Help.BashPath != "" {
		t.Fatalf("settings %+v", st.Help)
	}
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, st, []string{toolDir(t, "bash", "man", "sample")}, []string{t.TempDir()})
	if _, ok := l.Builtin(context.Background(), "read"); ok {
		t.Error("Builtin ran with help disabled")
	}
	if _, ok := l.Command(context.Background(), "sample"); ok {
		t.Error("Command ran with help disabled")
	}
	if n := f.count(); n != 0 {
		t.Errorf("%d spawns", n)
	}
	if help, man, _ := l.Available(); help || man {
		t.Error("Available is true with help disabled")
	}
	for _, line := range l.Lines() {
		if !strings.HasSuffix(line, "disabled (help.enabled = false)") {
			t.Errorf("line %q", line)
		}
	}
}

func TestCacheCapped(t *testing.T) {
	skipWindows(t)
	names := make([]string, 0, maxCache+1)
	for i := 0; i <= maxCache; i++ {
		names = append(names, fmt.Sprintf("tool%04d", i))
	}
	f := &fakeRun{}
	dir := toolDir(t, append([]string{"bash", "man"}, names...)...)
	var mu sync.Mutex
	var logs []string
	l := New(f.run, defaults(), extool.NewProfile([]string{dir}), []string{t.TempDir()})
	l.SetLogger(func(format string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(format, a...)); mu.Unlock() })
	for _, n := range names {
		l.Command(context.Background(), n)
	}
	if got := l.cacheLen(); got != maxCache {
		t.Fatalf("cache holds %d names, want %d", got, maxCache)
	}
	if f.count() != len(names) {
		t.Fatalf("%d spawns for %d names", f.count(), len(names))
	}
	// A name past the cap is looked up again, a cached one is not.
	l.Command(context.Background(), names[len(names)-1])
	l.Command(context.Background(), names[0])
	if f.count() != len(names)+1 {
		t.Fatalf("%d spawns, want %d", f.count(), len(names)+1)
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, s := range logs {
		if strings.Contains(s, "cache") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d cache log lines, want 1: %q", n, logs)
	}
}

func TestMissesAreCachedAndCancelsAreNot(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{} // exit 1, no output: a miss
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	for range 3 {
		if _, ok := l.Builtin(context.Background(), "read"); ok {
			t.Fatal("a miss gave a doc")
		}
	}
	if f.count() != 1 {
		t.Fatalf("%d spawns for one repeated miss", f.count())
	}
	if _, found, known := l.Cached("builtin", "read"); found || !known {
		t.Fatalf("Cached = found %v known %v", found, known)
	}
	if _, _, known := l.Cached("builtin", "printf"); known {
		t.Fatal("Cached knows a name never asked")
	}

	ctx, cancel := context.WithCancel(context.Background())
	g := &fakeRun{reply: func(c call) (extool.Result, error) {
		cancel()
		<-c.ctx.Done()
		return extool.Result{Exit: -1}, c.ctx.Err()
	}}
	l = newLookup(t, g, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	if _, ok := l.Builtin(ctx, "printf"); ok {
		t.Fatal("a cancelled lookup gave a doc")
	}
	if _, _, known := l.Cached("builtin", "printf"); known {
		t.Fatal("a cancelled lookup was cached as a miss")
	}
}

func TestOutputCapStillParsesTheTop(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: func(c call) (extool.Result, error) {
		return extool.Result{Stdout: fixture0("man-bsd.txt")}, extool.ErrOutputCap
	}}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man", "sample")}, []string{t.TempDir()})
	d, ok := l.Command(context.Background(), "sample")
	if !ok || !strings.HasPrefix(d.Synopsis, "sample ") {
		t.Fatalf("%+v %v", d, ok)
	}
}

func TestRunErrorIsAMiss(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: func(c call) (extool.Result, error) { return extool.Result{Exit: -1}, errors.New("boom") }}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	if _, ok := l.Builtin(context.Background(), "read"); ok {
		t.Fatal("an error gave a doc")
	}
}

func TestSemaphoreOfTwo(t *testing.T) {
	skipWindows(t)
	var mu sync.Mutex
	cur, peak := 0, 0
	release := make(chan struct{})
	f := &fakeRun{reply: func(c call) (extool.Result, error) {
		mu.Lock()
		cur++
		peak = max(peak, cur)
		mu.Unlock()
		<-release
		mu.Lock()
		cur--
		mu.Unlock()
		return extool.Result{Exit: 1}, nil
	}}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	var wg sync.WaitGroup
	for _, n := range []string{"read", "printf", "echo", "test", "cd"} {
		wg.Add(1)
		go func() { defer wg.Done(); l.Builtin(context.Background(), n) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		p := peak
		mu.Unlock()
		if p >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // time for a third to start, if it could
	close(release)
	wg.Wait()
	if peak != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak)
	}
}

func TestLinesAvailableAndAbsent(t *testing.T) {
	skipWindows(t)
	dir := toolDir(t, "bash", "man")
	l := newLookup(t, &fakeRun{}, defaults(), []string{dir}, []string{t.TempDir()})
	lines := l.Lines()
	if len(lines) != 2 ||
		lines[0] != "help: available ("+resolved(t, filepath.Join(dir, "bash"))+")" ||
		lines[1] != "man: available ("+resolved(t, filepath.Join(dir, "man"))+")" {
		t.Fatalf("lines %q", lines)
	}
	if help, man, why := l.Available(); !help || !man || why != "" {
		t.Fatalf("Available = %v %v %q", help, man, why)
	}
	l = newLookup(t, &fakeRun{}, defaults(), []string{t.TempDir()}, nil)
	if got := l.Lines(); !slices.Equal(got, []string{"help: absent", "man: absent"}) {
		t.Fatalf("lines %q", got)
	}
}

func TestBuiltinOnlyForBuiltins(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man", "sample")}, []string{t.TempDir()})
	if _, ok := l.Builtin(context.Background(), "sample"); ok || f.count() != 0 {
		t.Fatal("Builtin looked up a non-builtin")
	}
}
