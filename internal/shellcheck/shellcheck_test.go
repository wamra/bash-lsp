package shellcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// report prints one ShellCheck finding whose message is msg: the fake tools
// use it to tell the test what they saw.
func report(msg string) {
	b, _ := json.Marshal(msg)
	fmt.Printf(`{"comments":[{"file":"-","line":1,"endLine":1,"column":1,"endColumn":2,"level":"info","code":1,"message":%s,"fix":null}]}`, b)
}

// The test binary re-executed as a fake ShellCheck (WB_FAKE_TOOL=<mode>).
var fakeModes = map[string]func(){
	"json":  func() { fmt.Print(os.Getenv("WB_FAKE_OUT")); os.Exit(1) },
	"clean": func() { fmt.Print(`{"comments":[]}`) },
	"env":   func() { report(strings.Join(os.Environ(), "\n")) },
	"argv": func() {
		b, _ := io.ReadAll(os.Stdin)
		wd, _ := os.Getwd()
		report(fmt.Sprintf("%s\x00%s\x00%s", strings.Join(os.Args[1:], "\n"), wd, b))
	},
	"exit2": func() { fmt.Fprint(os.Stderr, "\nfirst line\nsecond line"); os.Exit(2) },
	"exit2secret": func() {
		fmt.Fprint(os.Stderr, "bad option token=hunter2hunter2 ghp_0123456789abcdefghijklmnopqrstuvwxyzAB\nsecond")
		os.Exit(2)
	},
	"version": func() { fmt.Print("ShellCheck - shell script analysis tool\nversion: 9.9.9\nlicense: GPL\n") },
	"flood": func() {
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(9)
			}
		}
	},
	"sleep": func() { time.Sleep(time.Hour) },
	"gate": func() { // count concurrent runs through a shared directory
		dir := os.Getenv("WB_GATE")
		me := filepath.Join(dir, fmt.Sprint(os.Getpid()))
		os.WriteFile(me, nil, 0o644)
		time.Sleep(300 * time.Millisecond)
		ents, _ := os.ReadDir(dir)
		report(fmt.Sprint(len(ents)))
		os.Remove(me)
	},
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("WB_FAKE_TOOL"); mode != "" {
		f, ok := fakeModes[mode]
		if !ok {
			os.Exit(64)
		}
		f()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeRunner is a Runner whose tool is the test binary in the given mode;
// extra is passed through the environment filter (test hook).
func fakeRunner(t *testing.T, mode string, o Options, extra ...string) *Runner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := New(extool.Tool{Name: "shellcheck", Path: exe}, o)
	r.extraEnv = append([]string{"WB_FAKE_TOOL=" + mode}, extra...)
	return r
}

func bashFile(path, src string) *tree.File { return parse.FileAs(path, []byte(src), tree.Bash) }

func TestCheckDropsGHCRTS(t *testing.T) {
	t.Setenv("GHCRTS", "-M1m")
	t.Setenv("SHELLCHECK_OPTS", "-e SC2086")
	t.Setenv("WB_SECRET", "s")
	r := fakeRunner(t, "env", Options{})
	ds, err := r.Check(context.Background(), bashFile(filepath.Join(t.TempDir(), "a.sh"), "echo hi\n"))
	if err != nil || len(ds) != 1 {
		t.Fatalf("Check = %v, %v", ds, err)
	}
	seen := ds[0].Message
	for _, name := range []string{"GHCRTS=", "SHELLCHECK_OPTS=", "WB_SECRET="} {
		if strings.Contains(seen, name) {
			t.Errorf("the tool saw %s", name)
		}
	}
	if !strings.Contains(seen, "WB_FAKE_TOOL=env") || !strings.Contains(seen, "PATH=") {
		t.Errorf("the tool's environment lost what it needs:\n%s", seen)
	}
}

func TestCheckArgsDirAndStdin(t *testing.T) {
	dir := t.TempDir()
	r := fakeRunner(t, "argv", Options{})
	src := "--version\n$(touch pwned)\n"
	f := bashFile(filepath.Join(dir, "sub", "--evil name.sh"), src)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	ds, err := r.Check(context.Background(), f)
	if err != nil || len(ds) != 1 {
		t.Fatalf("Check = %v, %v", ds, err)
	}
	seen := ds[0].Message
	parts := strings.SplitN(seen, "\x00", 3)
	if len(parts) != 3 {
		t.Fatalf("report %q", seen)
	}
	if want := "--format=json1\n--shell=bash\n--exclude=SC1090,SC1091\n-"; parts[0] != want {
		t.Errorf("args = %q, want %q", parts[0], want)
	}
	if parts[1] != extool.NeutralDir() {
		t.Errorf("cwd = %s, want the neutral directory %s", parts[1], extool.NeutralDir())
	}
	if parts[2] != src {
		t.Errorf("stdin = %q, want %q", parts[2], src)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "pwned")); err == nil {
		t.Error("analysed text was executed")
	}
}

func TestCheckShellFlagPerDialect(t *testing.T) {
	for d, want := range map[tree.Dialect]string{tree.Bash: "bash", tree.Bats: "bash", tree.POSIX: "sh", tree.Mksh: "ksh"} {
		got, ok := ShellFlag(d)
		if !ok || got != want {
			t.Errorf("ShellFlag(%v) = %q, %v; want %q", d, got, ok, want)
		}
	}
	for _, d := range []tree.Dialect{tree.Zsh, 0, 99} {
		if s, ok := ShellFlag(d); ok {
			t.Errorf("ShellFlag(%v) = %q, true", d, s)
		}
	}
}

// A file that is skipped or has a dialect ShellCheck lacks is never sent.
func TestCheckSendsNothingForSkippedOrZsh(t *testing.T) {
	r := New(extool.Tool{Name: "shellcheck", Path: "/nonexistent/never-started"}, Options{})
	skipped := bashFile("/x/a.sh", "echo $a\n")
	skipped.Skipped = "over the limit"
	zsh := parse.FileAs("/x/b.zsh", []byte("echo $a\n"), tree.Zsh)
	for _, f := range []*tree.File{skipped, zsh, nil} {
		ds, err := r.Check(context.Background(), f)
		if err != nil || len(ds) != 0 {
			t.Errorf("Check = %v, %v; want nothing and no error", ds, err)
		}
	}
}

func TestCheckExitCodes(t *testing.T) {
	f := bashFile("/x/a.sh", "echo hi\n")
	// 0 and 1 are success.
	if _, err := fakeRunner(t, "clean", Options{}).Check(context.Background(), f); err != nil {
		t.Errorf("exit 0: %v", err)
	}
	r := fakeRunner(t, "json", Options{}, `WB_FAKE_OUT={"comments":[]}`)
	if _, err := r.Check(context.Background(), f); err != nil {
		t.Errorf("exit 1: %v", err)
	}
	// 2 is an error with the first non-empty stderr line.
	_, err := fakeRunner(t, "exit2", Options{}).Check(context.Background(), f)
	if err == nil || !strings.Contains(err.Error(), "first line") || strings.Contains(err.Error(), "second line") {
		t.Errorf("exit 2: %v", err)
	}
	// A credential-looking value in that line is redacted.
	_, err = fakeRunner(t, "exit2secret", Options{}).Check(context.Background(), f)
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "ghp_") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("exit 2 with a secret: %v", err)
	}
	// Garbage on stdout is an error, not a panic.
	r = fakeRunner(t, "json", Options{}, "WB_FAKE_OUT=not json")
	if _, err := r.Check(context.Background(), f); err == nil {
		t.Error("garbage output: no error")
	}
}

func TestCheckBoundsFloodAndHang(t *testing.T) {
	f := bashFile("/x/a.sh", "echo hi\n")
	_, err := fakeRunner(t, "flood", Options{MaxOutput: 1 << 20}).Check(context.Background(), f)
	if !errors.Is(err, extool.ErrOutputCap) {
		t.Errorf("flood: %v", err)
	}
	start := time.Now()
	_, err = fakeRunner(t, "sleep", Options{Timeout: 400 * time.Millisecond}).Check(context.Background(), f)
	if !errors.Is(err, extool.ErrTimeout) || time.Since(start) > time.Second {
		t.Errorf("hang: %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	if _, err = fakeRunner(t, "sleep", Options{}).Check(ctx, f); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}

// At most Concurrency children run at once; a waiting Check gives up when its
// context ends.
func TestCheckConcurrencyLimit(t *testing.T) {
	gate := t.TempDir()
	r := fakeRunner(t, "gate", Options{Concurrency: 2}, "WB_GATE="+gate)
	var peak atomic.Int64
	note := func(msg string) {
		var n int64
		fmt.Sscan(msg, &n)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				return
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ds, err := r.Check(context.Background(), bashFile("/x/a.sh", "echo\n"))
			if err != nil || len(ds) != 1 {
				t.Errorf("Check = %v, %v", ds, err)
				return
			}
			note(ds[0].Message)
		}()
	}
	wg.Wait()
	if p := peak.Load(); p < 1 || p > 2 {
		t.Errorf("peak concurrency %d, want 1 or 2", p)
	}

	one := fakeRunner(t, "sleep", Options{Concurrency: 1, Timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := one.Check(ctx, bashFile("/x/a.sh", "echo\n")); done <- err }()
	time.Sleep(200 * time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, err := one.Check(ctx2, bashFile("/x/a.sh", "echo\n")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a waiting Check: %v", err)
	}
	cancel()
	<-done
}

func TestVersion(t *testing.T) {
	v, err := fakeRunner(t, "version", Options{}).Version(context.Background())
	if err != nil || v != "9.9.9" {
		t.Errorf("Version = %q, %v", v, err)
	}
	if _, err := fakeRunner(t, "clean", Options{}).Version(context.Background()); err == nil {
		t.Error("output with no version line is accepted")
	}
}

func TestOptionDefaults(t *testing.T) {
	r := New(extool.Tool{}, Options{})
	if r.opt.Timeout != 10*time.Second || r.opt.MaxOutput != 4<<20 || r.opt.Concurrency != 2 || cap(r.sem) != 2 {
		t.Errorf("defaults = %+v", r.opt)
	}
}

// --- Parse, on output captured from ShellCheck 0.11.0 ---

func fixture(t *testing.T, name string) (*tree.File, []byte) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "shellcheck", name+".sh"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join("..", "..", "testdata", "shellcheck", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return bashFile("/w/"+name+".sh", string(src)), out
}

// applyEdits applies edits (non-overlapping, at distinct offsets) to src.
func applyEdits(src []byte, edits []analysis.TextEdit) string {
	es := slices.Clone(edits)
	slices.SortFunc(es, func(a, b analysis.TextEdit) int { return b.Range.Start.Offset - a.Range.Start.Offset })
	out := slices.Clone(src)
	for _, e := range es {
		out = append(out[:e.Range.Start.Offset], append([]byte(e.NewText), out[e.Range.End.Offset:]...)...)
	}
	return string(out)
}

func TestParseEmojiLine(t *testing.T) {
	f, out := fixture(t, "emoji")
	ds, err := Parse(f, out)
	if err != nil || len(ds) != 1 {
		t.Fatalf("Parse = %v, %v", ds, err)
	}
	d := ds[0]
	if d.Rule != "SC2086" || d.Severity != analysis.SevInfo || d.Origin != "shellcheck" || d.Message == "" {
		t.Errorf("diagnostic = %+v", d)
	}
	// ShellCheck counts code points: `echo "😀" $x` has $x at columns 10-12; the
	// emoji is 4 bytes, so the byte range starts 3 bytes later and the UTF-16
	// range 1 unit later.
	if got := string(f.Src[d.Range.Start.Offset:d.Range.End.Offset]); got != "$x" {
		t.Fatalf("range covers %q", got)
	}
	if d.Range.Start.Line != 3 || d.Range.Start.Col != 13 || d.Range.End.Col != 15 {
		t.Errorf("range = %+v", d.Range)
	}
	if l, c := f.ToUTF16(d.Range.Start); l != 2 || c != 10 {
		t.Errorf("UTF-16 start = %d:%d, want 2:10", l, c)
	}
	if l, c := f.ToUTF16(d.Range.End); l != 2 || c != 12 {
		t.Errorf("UTF-16 end = %d:%d, want 2:12", l, c)
	}
	if len(d.Fixes) != 1 || d.Fixes[0].Title != "ShellCheck: apply fix" || d.Fixes[0].Safe {
		t.Fatalf("fixes = %+v", d.Fixes)
	}
	if got, want := applyEdits(f.Src, d.Fixes[0].Edits), "#!/bin/bash\nx=$1\necho \"😀\" \"$x\"\n"; got != want {
		t.Errorf("fixed text = %q, want %q", got, want)
	}
}

func TestParseTabLine(t *testing.T) {
	f, out := fixture(t, "tab")
	ds, err := Parse(f, out)
	if err != nil || len(ds) != 2 {
		t.Fatalf("Parse = %v, %v", ds, err)
	}
	// A tab is one column.
	for _, d := range ds {
		if got := string(f.Src[d.Range.Start.Offset:d.Range.End.Offset]); got != "$x" {
			t.Errorf("line %d: range covers %q", d.Range.Start.Line, got)
		}
		if got := applyEdits(f.Src, d.Fixes[0].Edits); strings.Count(got, `"$x"`) != 1 {
			t.Errorf("fixed text = %q", got)
		}
	}
}

func TestParseCRLFFile(t *testing.T) {
	f, out := fixture(t, "crlf")
	if !bytes.Contains(f.Src, []byte("\r\n")) {
		t.Fatal("the fixture lost its carriage returns")
	}
	ds, err := Parse(f, out)
	if err != nil {
		t.Fatal(err)
	}
	var quoting, cr int
	for _, d := range ds {
		switch d.Rule {
		case "SC2086":
			quoting++
			if got := string(f.Src[d.Range.Start.Offset:d.Range.End.Offset]); got != "$x" {
				t.Errorf("SC2086 covers %q", got)
			}
		case "SC1017":
			cr++
			if d.Severity != analysis.SevError {
				t.Errorf("SC1017 severity %v", d.Severity)
			}
			if got := string(f.Src[d.Range.Start.Offset:min(d.Range.End.Offset+1, len(f.Src))]); !strings.HasPrefix(got, "\r") {
				t.Errorf("SC1017 at %+v is not at a carriage return: %q", d.Range, got)
			}
		}
	}
	if quoting != 2 || cr != 4 {
		t.Errorf("SC2086 x%d, SC1017 x%d; want 2 and 4", quoting, cr)
	}
}

func TestParseCodesLevelsAndFiles(t *testing.T) {
	f, out := fixture(t, "fix")
	ds, err := Parse(f, out)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, d := range ds {
		codes = append(codes, fmt.Sprintf("%s/%d", d.Rule, d.Severity))
		if d.Rule == "SC2154" && len(d.Fixes) != 0 {
			t.Error("a finding with fix null has a fix")
		}
	}
	if want := []string{"SC2086/3", "SC2154/2", "SC2086/3"}; !slices.Equal(codes, want) {
		t.Errorf("codes = %v, want %v", codes, want)
	}
	for level, want := range map[string]analysis.Severity{"error": 1, "warning": 2, "info": 3, "style": 4, "weird": 3} {
		js := fmt.Sprintf(`{"comments":[{"file":"-","line":1,"endLine":1,"column":1,"endColumn":2,"level":%q,"code":2000,"message":"m","fix":null}]}`, level)
		ds, err := Parse(f, []byte(js))
		if err != nil || len(ds) != 1 || ds[0].Severity != want {
			t.Errorf("level %s: %v %v", level, ds, err)
		}
	}
	// Only comments about the standard input count.
	js := `{"comments":[{"file":"other.sh","line":1,"endLine":1,"column":1,"endColumn":2,"level":"error","code":2000,"message":"m","fix":null}]}`
	if ds, err := Parse(f, []byte(js)); err != nil || len(ds) != 0 {
		t.Errorf("other file: %v %v", ds, err)
	}
	// Positions beyond the file are clamped; never a panic.
	js = `{"comments":[{"file":"-","line":99,"endLine":100,"column":50,"endColumn":60,"level":"error","code":2000,"message":"m","fix":{"replacements":[{"line":0,"endLine":-3,"column":0,"endColumn":9999,"replacement":"x","precedence":1,"insertionPoint":"afterEnd"}]}}]}`
	ds, err = Parse(f, []byte(js))
	if err != nil || len(ds) != 1 || ds[0].Range.End.Offset > len(f.Src) {
		t.Errorf("clamp: %v %v", ds, err)
	}
	for _, bad := range []string{"", "x", `{"comments":`, `[]`} {
		if _, err := Parse(f, []byte(bad)); err == nil && bad != `[]` {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

// --- against the installed ShellCheck ---

// isolateHome points HOME (USERPROFILE on Windows) and XDG_CONFIG_HOME at an empty directory: a test
// that runs the real ShellCheck must not read the developer's own rc.
func isolateHome(t testing.TB) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func realRunner(t *testing.T) *Runner {
	t.Helper()
	isolateHome(t)
	tool, ref, ok := extool.Find("shellcheck", "", extool.ProfileFromEnv(), nil)
	if !ok {
		t.Skipf("shellcheck is not installed (refusal: %v)", ref)
	}
	return New(tool, Options{})
}

// analyse returns the native findings of path as the workspace would.
func analyse(t *testing.T, root, path string) (*tree.File, []analysis.Diagnostic) {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".sh") {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			files[p] = b
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	s := analysis.Analyze(files, analysis.Options{Root: root})
	return parse.File(path, files[path]), s.Diagnostics(path, analysis.DiagOptions{})
}

func TestShellCheckOnLintFixture(t *testing.T) {
	r := realRunner(t)
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "lint-fixture"))
	path := filepath.Join(root, "main.sh")
	f, native := analyse(t, root, path)
	sc, err := r.Check(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	merged := analysis.Merge(native, sc)
	if !slices.ContainsFunc(native, func(d analysis.Diagnostic) bool { return d.Rule == "WB1401" }) {
		t.Error("the native analysis found no WB1401 to de-duplicate: the test proves nothing")
	}
	text := func(d analysis.Diagnostic) string { return string(f.Src[d.Range.Start.Offset:d.Range.End.Offset]) }
	lineOf := func(d analysis.Diagnostic) string {
		return strings.Split(string(f.Src), "\n")[d.Range.Start.Line-1]
	}
	var sc2086, sc2154 []analysis.Diagnostic
	for _, d := range merged {
		switch d.Rule {
		case "WB1401":
			t.Errorf("WB1401 survived the merge: %+v", d)
		case "SC2086":
			sc2086 = append(sc2086, d)
		case "SC2154":
			sc2154 = append(sc2154, d)
		}
	}
	if len(sc2086) != 2 {
		t.Fatalf("SC2086 count = %d, want 2: %+v", len(sc2086), merged)
	}
	if !strings.Contains(lineOf(sc2086[0]), "rm $f") || !strings.Contains(lineOf(sc2086[1]), "echo $undefined_thing") {
		t.Errorf("SC2086 on lines %q and %q", lineOf(sc2086[0]), lineOf(sc2086[1]))
	}
	if len(sc2154) != 1 || text(sc2154[0]) != "$undefined_thing" {
		t.Fatalf("SC2154 = %+v", sc2154)
	}
	for i, want := range []string{`"$f"`, `"$undefined_thing"`} {
		d := sc2086[i]
		if len(d.Fixes) != 1 {
			t.Fatalf("SC2086 #%d has %d fixes", i, len(d.Fixes))
		}
		fixed := applyEdits(f.Src, d.Fixes[0].Edits)
		if !strings.Contains(fixed, want) || strings.Count(fixed, want) != 1 {
			t.Errorf("fix #%d gives %q, want it to contain %s", i, fixed, want)
		}
	}
}

func TestNoSC1091OnResolvedSource(t *testing.T) {
	r := realRunner(t)
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "corpus"))
	path := filepath.Join(root, "main.sh")
	f, native := analyse(t, root, path)
	sc, err := r.Check(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range analysis.Merge(native, sc) {
		if d.Rule == "SC1091" || d.Rule == "WB1101" {
			t.Errorf("unexpected %s at line %d: %s", d.Rule, d.Range.Start.Line, d.Message)
		}
	}
	if v, err := r.Version(context.Background()); err != nil || v == "" {
		t.Errorf("Version = %q, %v", v, err)
	}
}

// BenchmarkCheck times one real ShellCheck run on the evidence fixture (process
// start, parse of the report); it skips when ShellCheck is not installed.
func BenchmarkCheck(b *testing.B) {
	tool, _, ok := extool.Find("shellcheck", "", extool.ProfileFromEnv(), nil)
	if !ok {
		b.Skip("shellcheck is not installed")
	}
	isolateHome(b)
	r := New(tool, Options{})
	path, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "lint-fixture", "main.sh"))
	src, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	f := parse.File(path, src)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Check(context.Background(), f); err != nil {
			b.Fatal(err)
		}
	}
}

// --- the workspace does not steer what runs or what is read ---

// writeFile writes name under dir (creating directories) and returns its path.
func writeFile(t *testing.T, dir, name, text string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// A wrapper script outside the workspace starts `bash` through PATH; a `bash`
// in the analysed file's directory must not run, whatever the PATH entries.
func TestCheckNoWorkspaceProgramThroughPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the planted programs are /bin/sh and env scripts")
	}
	exe, _ := os.Executable()
	for _, pathVar := range []string{":/usr/bin:/bin", ".:/usr/bin:/bin", "./sub:/usr/bin:/bin", "../sub:/usr/bin:/bin", "@WS@:/usr/bin:/bin"} {
		base := t.TempDir()
		ws := filepath.Join(base, "ws")
		marker := filepath.Join(base, "marker")
		wrapper := writeFile(t, base, "tools/shellcheck", "#!/usr/bin/env bash\nexec '"+exe+"' \"$@\"\n", 0o755)
		writeFile(t, ws, "sub/bash", "#!/bin/sh\necho ran > '"+marker+"'\nexec /bin/bash \"$@\"\n", 0o755)
		t.Setenv("PATH", strings.ReplaceAll(pathVar, "@WS@", filepath.Join(ws, "sub")))
		r := New(extool.Tool{Name: "shellcheck", Path: wrapper}, Options{Roots: []string{ws}})
		r.extraEnv = []string{"WB_FAKE_TOOL=clean"}
		f := bashFile(filepath.Join(ws, "sub", "a.sh"), "echo hi\n")
		if _, err := r.Check(context.Background(), f); err != nil {
			t.Errorf("PATH=%q: %v", pathVar, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("PATH=%q: the workspace's bash ran", pathVar)
		}
	}
}

// The root as a case variant on PATH is still the root.
func TestCheckNoWorkspaceProgramThroughCaseVariantPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the planted programs are /bin/sh and env scripts")
	}
	base := t.TempDir()
	ws := filepath.Join(base, "WS")
	os.MkdirAll(ws, 0o755)
	a, errA := os.Stat(ws)
	b, errB := os.Stat(filepath.Join(base, "ws"))
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		t.Skip("the volume is case-sensitive")
	}
	exe, _ := os.Executable()
	marker := filepath.Join(base, "marker")
	wrapper := writeFile(t, base, "tools/shellcheck", "#!/usr/bin/env bash\nexec '"+exe+"' \"$@\"\n", 0o755)
	writeFile(t, ws, "bash", "#!/bin/sh\necho ran > '"+marker+"'\nexec /bin/bash \"$@\"\n", 0o755)
	t.Setenv("PATH", filepath.Join(base, "ws")+":/usr/bin:/bin")
	r := New(extool.Tool{Name: "shellcheck", Path: wrapper}, Options{Roots: []string{ws}})
	r.extraEnv = []string{"WB_FAKE_TOOL=clean"}
	if _, err := r.Check(context.Background(), bashFile(filepath.Join(ws, "a.sh"), "echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the workspace's bash ran")
	}
}

func TestReadRC(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".shellcheckrc", "# comment\nshell=sh\nexternal-sources=true\nsource-path=/etc\ndisable=SC2086,SC2000-SC2002\nenable=require-variable-braces\n", 0o644)
	writeFile(t, root, "sub/.shellcheckrc", "disable=2154\nenable=all,Bad;Name,-x\nfoo=bar\n", 0o644)
	rc := readRC(filepath.Join(root, "sub", "deep", "a.sh"), []string{root})
	for _, code := range []int{2086, 2000, 2001, 2002, 2154} {
		if !rc.disabled(code) {
			t.Errorf("SC%d not disabled", code)
		}
	}
	if rc.disabled(2003) || rc.disabled(1091) {
		t.Error("an unlisted code is disabled")
	}
	if want := []string{"all", "require-variable-braces"}; !slices.Equal(rc.enable, want) {
		t.Errorf("enable = %v, want %v", rc.enable, want)
	}
	joined := strings.Join(rc.notes, "\n")
	for _, w := range []string{"Bad;Name", "-x"} {
		if !strings.Contains(joined, w) {
			t.Errorf("no notice about %q: %v", w, rc.notes)
		}
	}
	// A rc that is not a regular file is not read (a FIFO would block).
	if runtime.GOOS != "windows" {
		d := t.TempDir()
		if err := mkfifo(filepath.Join(d, ".shellcheckrc")); err == nil {
			readRC(filepath.Join(d, "a.sh"), []string{d}) // must return
		}
	}
}

// The rc grammar as ShellCheck 0.11.0 reads it, for the keys the server uses
// (checked with --rcfile on files of these shapes): a # starts a comment,
// values may be quoted, several key=value words may share a line, disable=all
// disables everything. A line it rejects (spaces around = or after a comma, an
// empty entry, a bad code, upper-case ALL, an unbalanced quote) makes it ignore
// the whole file, so the server applies nothing from such a file.
func TestReadRCGrammar(t *testing.T) {
	for _, c := range []struct {
		text    string
		off     []int // codes that must be disabled
		on      []int // codes that must not be
		all     bool
		invalid bool
	}{
		{text: "disable=SC2086\n", off: []int{2086}, on: []int{2154}},
		{text: "disable=SC2086 # why\n", off: []int{2086}, on: []int{2154}},
		{text: "disable=SC2086#why\n", off: []int{2086}, on: []int{2154}},
		{text: "disable=\"SC2086\"\n", off: []int{2086}},
		{text: "disable='SC2086,SC2154' # x\n", off: []int{2086, 2154}},
		{text: "disable=SC2086 disable=SC2154\n", off: []int{2086, 2154}},
		{text: "disable=SC2086\ndisable=SC2154\n", off: []int{2086, 2154}},
		{text: "  disable=SC2086   \n", off: []int{2086}},
		{text: "disable=SC2086\r\n", off: []int{2086}},
		{text: "disable=2086,SC2000-SC2002\n", off: []int{2086, 2000, 2002}, on: []int{2003}},
		{text: "disable=all\n", all: true},
		{text: "disable=SC2154-SC2000\n", on: []int{2154}}, // an inverted range disables nothing
		{text: "foo=bar\ndisable=SC2086\n", off: []int{2086}},
		{text: "disable=ALL\n", invalid: true},
		{text: "disable=sc2086x\n", invalid: true},
		{text: "disable = SC2086\n", invalid: true},
		{text: "disable=SC2086, SC2154\n", invalid: true},
		{text: "disable=SC2086,\n", invalid: true},
		{text: "disable=SC2086,,SC2154\n", invalid: true},
		{text: "disable=SC2086 junk\n", invalid: true},
		{text: "disable=\"SC2086\n", invalid: true},
		{text: "disable=junk\ndisable=SC2154\n", invalid: true},
		{text: "enable=a,b c\ndisable=SC2154\n", invalid: true},
	} {
		root := t.TempDir()
		writeFile(t, root, ".shellcheckrc", c.text, 0o644)
		rc := readRC(filepath.Join(root, "a.sh"), []string{root})
		for _, code := range c.off {
			if !rc.disabled(code) {
				t.Errorf("%q: SC%d not disabled", c.text, code)
			}
		}
		for _, code := range c.on {
			if rc.disabled(code) {
				t.Errorf("%q: SC%d disabled", c.text, code)
			}
		}
		if c.all && !(rc.disabled(1) && rc.disabled(2086) && rc.disabled(9999)) {
			t.Errorf("%q: not everything disabled", c.text)
		}
		if c.invalid && (rc.disabled(2154) || rc.disabled(2086) || len(rc.notes) == 0) {
			t.Errorf("%q: an invalid file was applied or went unnoticed (%v)", c.text, rc.notes)
		}
	}
}

// The file's path may be spelled differently from the root (a symlinked
// root, a case variant); the rc is still found. A file outside every root
// says so.
func TestReadRCPathSpelling(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeFile(t, real, ".shellcheckrc", "disable=SC2086\n", 0o644)
	writeFile(t, real, "sub/a.sh", "x\n", 0o644)
	if runtime.GOOS != "windows" {
		alias := filepath.Join(base, "alias")
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		if rc := readRC(filepath.Join(real, "sub", "a.sh"), []string{alias}); !rc.disabled(2086) {
			t.Error("a symlinked root hides the rc")
		}
		if rc := readRC(filepath.Join(alias, "sub", "a.sh"), []string{real}); !rc.disabled(2086) {
			t.Error("a file reached through a link hides the rc")
		}
	}
	if up := strings.ToUpper(real); up != real {
		if _, err := os.Stat(up); err == nil {
			if rc := readRC(filepath.Join(up, "sub", "a.sh"), []string{real}); !rc.disabled(2086) {
				t.Error("a case variant hides the rc")
			}
		}
	}
	out := readRC(filepath.Join(t.TempDir(), "a.sh"), []string{real})
	if out.disabled(2086) || len(out.notes) != 1 || !strings.Contains(out.notes[0], "outside") {
		t.Errorf("a file outside the roots: %+v", out)
	}
	if none := readRC(filepath.Join(t.TempDir(), "a.sh"), nil); len(none.notes) != 0 {
		t.Errorf("no roots, yet notes: %v", none.notes)
	}
}

// A .shellcheckrc that is a link leading outside the root is not followed and
// its text is never echoed; one that is not a regular file is skipped without
// blocking.
func TestReadRCSymlinksAndFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links and FIFOs")
	}
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	writeFile(t, base, "outside/rc", "disable=SC2086\nenable=Secret Token 123\nenable=Secret;Token\n", 0o644)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", "rc"), filepath.Join(root, ".shellcheckrc")); err != nil {
		t.Fatal(err)
	}
	rc := readRC(filepath.Join(root, "a.sh"), []string{root})
	if rc.disabled(2086) || len(rc.enable) != 0 {
		t.Error("an rc outside the root was followed")
	}
	if all := strings.Join(rc.notes, "\n"); strings.Contains(all, "Secret") || !strings.Contains(all, "link") {
		t.Errorf("notes = %q", rc.notes)
	}
	// A link that stays inside the root is read.
	writeFile(t, root, "conf/real.rc", "disable=SC2154\n", 0o644)
	os.MkdirAll(filepath.Join(root, "in"), 0o755)
	if err := os.Symlink(filepath.Join(root, "conf", "real.rc"), filepath.Join(root, "in", ".shellcheckrc")); err != nil {
		t.Fatal(err)
	}
	if rc := readRC(filepath.Join(root, "in", "a.sh"), []string{root}); !rc.disabled(2154) {
		t.Error("an rc linked inside the root was not read")
	}
	// openRC never blocks on a FIFO and refuses it.
	fifo := filepath.Join(base, "fifo")
	if err := mkfifo(fifo); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, err := openRC(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was opened as an rc")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("openRC blocked on a FIFO")
	}
}

func TestParseHugeColumnOnASCIILine(t *testing.T) {
	f := bashFile("/x/a.sh", "echo hello\necho $a\n")
	js := `{"comments":[{"file":"-","line":2,"endLine":2,"column":9223372036854775807,"endColumn":9223372036854775807,"level":"info","code":1,"message":"m","fix":null}]}`
	ds, err := Parse(f, []byte(js))
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	wantEnd := strings.Index(string(f.Src), "\n") + 1 + len("echo $a")
	if ds[0].Range.Start.Offset != wantEnd || ds[0].Range.End.Offset != wantEnd {
		t.Errorf("range %+v, want both at %d (the end of the line)", ds[0].Range, wantEnd)
	}
}

// The repo rc's enable list reaches ShellCheck as one --enable argument;
// disable, shell, external-sources and source-path never do.
func TestCheckPassesOnlyEnableFromRC(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".shellcheckrc", "enable=require-variable-braces,Bad;Name\nexternal-sources=true\nsource-path=/etc\nshell=sh\ndisable=SC2086\n", 0o644)
	r := fakeRunner(t, "argv", Options{Roots: []string{root}})
	ds, notes, err := r.CheckNotes(context.Background(), bashFile(filepath.Join(root, "a.sh"), "echo\n"))
	if err != nil || len(ds) != 1 {
		t.Fatalf("CheckNotes = %v, %v", ds, err)
	}
	args := strings.SplitN(ds[0].Message, "\x00", 2)[0]
	want := "--format=json1\n--shell=bash\n--exclude=SC1090,SC1091\n--enable=require-variable-braces\n-"
	if args != want {
		t.Errorf("args = %q, want %q", args, want)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, "\n"), "Bad;Name") {
		t.Errorf("notes = %v", notes)
	}
}

func TestCheckAppliesRCDisableAfterParsing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".shellcheckrc", "disable=SC2086\n", 0o644)
	out := `{"comments":[` +
		`{"file":"-","line":1,"endLine":1,"column":1,"endColumn":2,"level":"info","code":2086,"message":"a","fix":null},` +
		`{"file":"-","line":1,"endLine":1,"column":1,"endColumn":2,"level":"info","code":2154,"message":"b","fix":null}]}`
	r := fakeRunner(t, "json", Options{Roots: []string{root}}, "WB_FAKE_OUT="+out)
	ds, err := r.Check(context.Background(), bashFile(filepath.Join(root, "a.sh"), "echo\n"))
	if err != nil || len(ds) != 1 || ds[0].Rule != "SC2154" {
		t.Errorf("Check = %v, %v; want only SC2154", ds, err)
	}
}

func realChecker(t *testing.T, roots ...string) *Runner {
	t.Helper()
	isolateHome(t)
	tool, _, ok := extool.Find("shellcheck", "", extool.ProfileFromEnv(), nil)
	if !ok {
		t.Skip("shellcheck is not installed")
	}
	return New(tool, Options{Roots: roots, Timeout: 20 * time.Second})
}

func rules(ds []analysis.Diagnostic) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Rule)
	}
	return out
}

// A repo .shellcheckrc with external-sources and a source-path outside the
// workspace does not make ShellCheck read outside it, and `source /dev/zero`
// does not make it grow: it returns long before its timeout, with findings.
func TestCheckRepoRCCannotReachOutside(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	writeFile(t, outside, "defs.sh", "probe_vax=1\n", 0o644)
	writeFile(t, ws, ".shellcheckrc", "external-sources=true\nsource-path="+outside+"\n", 0o644)
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := realChecker(t, ws)
	f := bashFile(filepath.Join(ws, "sub", "a.sh"), "#!/bin/bash\nsource defs.sh\necho \"$probe_var\"\n")
	ds, err := r.Check(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if strings.Contains(d.Message, "probe_vax") {
			t.Errorf("an outside identifier leaked: %s", d.Message)
		}
	}
	if !slices.Contains(rules(ds), "SC2154") {
		t.Errorf("SC2154 is gone, so the outside file was read: %v", rules(ds))
	}
	zero := bashFile(filepath.Join(ws, "sub", "z.sh"), "#!/bin/bash\nx=$1\nsource /dev/zero\necho $x\n")
	start := time.Now()
	ds, err = r.Check(context.Background(), zero)
	if err != nil || !slices.Contains(rules(ds), "SC2086") {
		t.Fatalf("source /dev/zero: %v, %v", rules(ds), err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("source /dev/zero took %v", took)
	}
}

func TestCheckRepoRCDisableAndUserRC(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	writeFile(t, ws, ".shellcheckrc", "disable=SC2086\n", 0o644)
	src := "#!/bin/bash\nx=$1\necho $x\necho $undefined_one\n"
	r := realChecker(t, ws)
	ds, err := r.Check(context.Background(), bashFile(filepath.Join(ws, "a.sh"), src))
	if err != nil || slices.Contains(rules(ds), "SC2086") || !slices.Contains(rules(ds), "SC2154") {
		t.Errorf("repo disable: %v, %v", rules(ds), err)
	}
	// The user's own rc (HOME) still applies.
	home := t.TempDir()
	writeFile(t, home, ".shellcheckrc", "disable=SC2154\n", 0o644)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	ds, err = r.Check(context.Background(), bashFile(filepath.Join(ws, "a.sh"), src))
	if err != nil || slices.Contains(rules(ds), "SC2154") {
		t.Errorf("user rc: %v, %v", rules(ds), err)
	}
}

// ShellCheck cannot follow sources now; SC1090 and SC1091 never come from it
// (the native WB1101 reports what the server cannot resolve).
func TestNoSC1090FromShellCheck(t *testing.T) {
	r := realChecker(t)
	src := "#!/bin/bash\nHERE=$(dirname \"$0\")\nsource \"$HERE/lib/util.sh\"\nsource ./nope.sh\n"
	ds, err := r.Check(context.Background(), bashFile("/x/a.sh", src))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Rule == "SC1090" || d.Rule == "SC1091" {
			t.Errorf("%s from ShellCheck: %s", d.Rule, d.Message)
		}
	}
}

// --- Parse bounds ---

func TestParseRequiresCommentsArray(t *testing.T) {
	f := bashFile("/x/a.sh", "echo $a\n")
	for _, bad := range []string{`{}`, `null`, `[]`, `{"comments":null}`, `{"x":"comments"}`, `{"comments":{}}`, `{"comments":"x"}`} {
		if ds, err := Parse(f, []byte(bad)); err == nil {
			t.Errorf("Parse(%s) = %v, nil error", bad, ds)
		}
	}
	if ds, err := Parse(f, []byte(`{"comments":[]}`)); err != nil || len(ds) != 0 {
		t.Errorf("empty report: %v, %v", ds, err)
	}
}

// One rule for a bad range: the end is moved up to the start, both inside the file.
func TestParseClampsRanges(t *testing.T) {
	f := bashFile("/x/a.sh", "echo hello\necho $a\n")
	cases := map[string]string{
		"inverted": `"line":2,"endLine":1,"column":6,"endColumn":2`,
		"negative": `"line":-5,"endLine":999,"column":-3,"endColumn":99999`,
		"far":      `"line":50,"endLine":60,"column":9,"endColumn":9`,
	}
	for name, pos := range cases {
		js := `{"comments":[{"file":"-",` + pos + `,"level":"info","code":1,"message":"m","fix":{"replacements":[{` + pos + `,"replacement":"x"}]}}]}`
		ds, err := Parse(f, []byte(js))
		if err != nil || len(ds) != 1 {
			t.Fatalf("%s: %v %v", name, ds, err)
		}
		rs := []tree.Range{ds[0].Range, ds[0].Fixes[0].Edits[0].Range}
		for _, r := range rs {
			if r.Start.Offset > r.End.Offset || r.Start.Offset < 0 || r.End.Offset > len(f.Src) {
				t.Errorf("%s: range %+v is not ordered inside the file", name, r)
			}
		}
	}
}

// Mapping columns costs time linear in the report: three doublings of a report
// of findings on one long line grow by at most 2.5 per doubling on average
// ((t4/t1)^(1/3), each time the best of five runs). Linear gives 2, quadratic
// 4; single doublings scatter under load and are logged, not asserted.
func TestParseGrowth(t *testing.T) {
	for _, pad := range []string{"a", "é"} {
		line := "echo " + strings.Repeat(pad, 150000) + "\n"
		f := bashFile("/x/a.sh", line)
		timeFor := func(n int) time.Duration {
			var b strings.Builder
			b.WriteString(`{"comments":[`)
			for i := 0; i < n; i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`{"file":"-","line":1,"endLine":1,"column":149990,"endColumn":149999,"level":"info","code":2086,"message":"m","fix":null}`)
			}
			b.WriteString(`]}`)
			out := []byte(b.String())
			best := time.Hour
			for i := 0; i < 5; i++ {
				start := time.Now()
				if _, err := Parse(f, out); err != nil {
					t.Fatal(err)
				}
				best = min(best, time.Since(start))
			}
			return best
		}
		var times []time.Duration
		for _, n := range []int{10000, 20000, 40000, 80000} {
			times = append(times, timeFor(n))
		}
		t.Logf("pad %q: %v", pad, times)
		if overall := math.Cbrt(float64(times[3]) / float64(max(times[0], time.Millisecond))); overall > 2.5 {
			t.Errorf("pad %q: grew x%.2f per doubling over three doublings (%v)", pad, overall, times)
		}
	}
}

// --- rc hardening: nothing is applied that ShellCheck 0.11.0 would not apply ---

// ShellCheck ignores a whole rc file for a lone carriage return (a CR is only
// accepted before the line feed), for a key with a character other than a
// letter or a hyphen, and for a space-like character at the start of a word.
// The server applies nothing from such a file, and nothing from a file with a
// space-like character other than space and tab between words (ShellCheck
// would apply only part of it).
func TestReadRCRejectsWhatShellCheckRejects(t *testing.T) {
	for _, text := range []string{
		"disable=SC2086\rdisable=SC2154\n",
		"disable=SC2086\r",
		"disable=SC2086\r\r\n",
		"# c\rdisable=SC2154\n",
		"disable=SC2086 # c\rd\n",
		"disable=SC2086\vdisable=SC2154\n",
		"disable=SC2086\fdisable=SC2154\n",
		"disable=SC2086\u0085disable=SC2154\n",
		"disable=SC2086　disable=SC2154\n",
		"disable=SC2086 disable=SC2154\n",
		"disable=SC2086\n\vdisable=SC2154\n",
		"disable=SC2086\n　disable=SC2154\n",
		"a.b=1\ndisable=SC2154\n",
		"foo_bar=1\ndisable=SC2154\n",
		"foo1=1\ndisable=SC2154\n",
	} {
		root := t.TempDir()
		writeFile(t, root, ".shellcheckrc", text, 0o644)
		rc := readRC(filepath.Join(root, "a.sh"), []string{root})
		if rc.disabled(2154) || rc.disabled(2086) || len(rc.notes) == 0 {
			t.Errorf("%q: applied or unnoticed (%v)", text, rc.notes)
		}
	}
	// A hyphenated or capitalised key is fine, as is CRLF.
	root := t.TempDir()
	writeFile(t, root, ".shellcheckrc", "source-path=x\nFoo=1\ndisable=SC2154\r\n# c\r\ndisable=SC2086\r\n", 0o644)
	if rc := readRC(filepath.Join(root, "a.sh"), []string{root}); !rc.disabled(2154) || !rc.disabled(2086) {
		t.Errorf("a valid CRLF file was not applied: %+v", rc)
	}
}

// A link that cannot be resolved is not called a link that leads outside; a
// file that is not regular (FIFO, directory) is noticed, not skipped silently.
func TestReadRCNotices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links and FIFOs")
	}
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, ".shellcheckrc")); err != nil {
		t.Fatal(err)
	}
	rc := readRC(filepath.Join(root, "a.sh"), []string{root})
	all := strings.Join(rc.notes, "\n")
	if len(rc.notes) != 1 || strings.Contains(all, "outside") || !strings.Contains(all, "cannot be resolved") {
		t.Errorf("dangling link: %q", rc.notes)
	}
	loop := t.TempDir()
	if err := os.Symlink(filepath.Join(loop, ".shellcheckrc"), filepath.Join(loop, ".shellcheckrc")); err != nil {
		t.Fatal(err)
	}
	if rc := readRC(filepath.Join(loop, "a.sh"), []string{loop}); len(rc.notes) != 1 || strings.Contains(rc.notes[0], "outside") {
		t.Errorf("link loop: %q", rc.notes)
	}
	fifoDir := t.TempDir()
	if err := mkfifo(filepath.Join(fifoDir, ".shellcheckrc")); err != nil {
		t.Skip(err)
	}
	done := make(chan rcInfo, 1)
	go func() { done <- readRC(filepath.Join(fifoDir, "a.sh"), []string{fifoDir}) }()
	select {
	case rc := <-done:
		if len(rc.notes) != 1 || !strings.Contains(rc.notes[0], "not a regular file") {
			t.Errorf("FIFO: %q", rc.notes)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readRC blocked on a FIFO")
	}
	dirDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirDir, ".shellcheckrc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rc := readRC(filepath.Join(dirDir, "a.sh"), []string{dirDir}); len(rc.notes) != 1 || !strings.Contains(rc.notes[0], "not a regular file") {
		t.Errorf("directory: %q", rc.notes)
	}
}

// A directory inside the root that is a link to a directory outside it does
// not hand over its .shellcheckrc.
func TestReadRCSymlinkedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links")
	}
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	writeFile(t, base, "outside/.shellcheckrc", "disable=SC2086\nenable=secret-check\n", 0o644)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	rc := readRC(filepath.Join(root, "link", "a.sh"), []string{root})
	if rc.disabled(2086) || len(rc.enable) != 0 {
		t.Error("an rc in a linked directory outside the root was read")
	}
	if all := strings.Join(rc.notes, "\n"); strings.Contains(all, "secret-check") || len(rc.notes) == 0 {
		t.Errorf("notes = %q", rc.notes)
	}
	// A link to a directory inside the root is read.
	writeFile(t, root, "real/.shellcheckrc", "disable=SC2154\n", 0o644)
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if rc := readRC(filepath.Join(root, "alias", "a.sh"), []string{root}); !rc.disabled(2154) {
		t.Errorf("an rc in a directory linked inside the root was not read: %q", rc.notes)
	}
}

// A column past the end of a CRLF line is the end of the text, before the CR.
func TestParseClampedColumnOnCRLFLine(t *testing.T) {
	f := bashFile("/x/a.sh", "echo $a\r\necho b\r\n")
	js := `{"comments":[{"file":"-","line":1,"endLine":1,"column":9999,"endColumn":9999,"level":"info","code":1,"message":"m","fix":null}]}`
	ds, err := Parse(f, []byte(js))
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	if want := len("echo $a"); ds[0].Range.Start.Offset != want || ds[0].Range.End.Offset != want {
		t.Errorf("range %+v, want both at %d", ds[0].Range, want)
	}
}

// ShellCheck counts lines by "\n" only: a finding after a lone CR is at the
// line ShellCheck says, and its LSP range sits one line further on.
func TestParseFindingAfterLoneCR(t *testing.T) {
	f := bashFile("/w/cr.sh", "#!/bin/bash\n# a\rb\nf=x\nrm $f\n")
	out := []byte(`{"comments":[{"file":"-","line":4,"endLine":4,"column":4,"endColumn":6,"level":"info","code":2086,"message":"Double quote to prevent globbing and word splitting.","fix":null}]}`)
	ds, err := Parse(f, out)
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	r := ds[0].Range
	if got := string(f.Src[r.Start.Offset:r.End.Offset]); got != "$f" {
		t.Fatalf("covers %q", got)
	}
	if r.Start.Line != 4 || r.Start.Col != 4 {
		t.Errorf("tool position %d:%d, want 4:4", r.Start.Line, r.Start.Col)
	}
	if l, c := f.ToUTF16(r.Start); l != 4 || c != 3 {
		t.Errorf("LSP start (%d,%d), want (4,3)", l, c)
	}
	if l, c := f.ToUTF16(r.End); l != 4 || c != 5 {
		t.Errorf("LSP end (%d,%d), want (4,5)", l, c)
	}
}
