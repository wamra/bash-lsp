package extool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeModes are the behaviours of the test binary when it is re-executed as a
// fake tool (WB_FAKE_TOOL=<mode>); run_unix_test.go adds the unix-only ones.
var fakeModes = map[string]func(){
	"sleep": func() { time.Sleep(time.Hour) },
	"flood": func() {
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(9)
			}
		}
	},
	"json": func() { fmt.Print(`{"comments":[]}`); os.Exit(1) },
	"stderr": func() {
		fmt.Fprint(os.Stderr, strings.Repeat("e", 200<<10))
	},
	"env":   func() { fmt.Print(strings.Join(os.Environ(), "\n")) },
	"argv":  func() { fmt.Print(strings.Join(os.Args[1:], "\n")) },
	"stdin": func() { b, _ := io.ReadAll(os.Stdin); os.Stdout.Write(b) },
	"exit3": func() { fmt.Fprint(os.Stderr, "boom\nsecond"); os.Exit(3) },
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("WB_FAKE_TOOL"); mode != "" {
		f, ok := fakeModes[mode]
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown fake mode", mode)
			os.Exit(64)
		}
		f()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fake is the test binary as a Tool, to be run with WB_FAKE_TOOL in its Env.
func fake(t *testing.T) Tool {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Tool{Name: "fake", Path: exe}
}

func mode(m string) []string { return []string{"WB_FAKE_TOOL=" + m} }

func TestRunTimeout(t *testing.T) {
	const limit = 500 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("sleep"), Timeout: limit})
	took := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if took > slack(2*limit) {
		t.Errorf("took %v, more than twice the %v timeout", took, limit)
	}
}

func TestRunOutputCap(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("flood"), Timeout: 20 * time.Second, MaxOutput: 10 << 20})
	if !errors.Is(err, ErrOutputCap) {
		t.Fatalf("err = %v, want ErrOutputCap", err)
	}
	if len(res.Stdout) > 10<<20 {
		t.Errorf("kept %d bytes, over the cap", len(res.Stdout))
	}
	if time.Since(start) > slack(10*time.Second) {
		t.Errorf("a flood took %v to stop", time.Since(start))
	}
}

func TestRunExit1WithJSONIsNotAnError(t *testing.T) {
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("json"), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Exit != 1 || string(res.Stdout) != `{"comments":[]}` {
		t.Errorf("exit %d, stdout %q", res.Exit, res.Stdout)
	}
}

func TestRunStderrCapped(t *testing.T) {
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: mode("stderr"), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(res.Stderr) != MaxStderr || res.Stderr[0] != 'e' {
		t.Errorf("stderr is %d bytes, want %d", len(res.Stderr), MaxStderr)
	}
	res, _ = Run(context.Background(), fake(t), nil, RunOptions{Env: mode("exit3"), Timeout: 10 * time.Second})
	if res.Exit != 3 || string(res.Stderr) != "boom\nsecond" {
		t.Errorf("exit %d stderr %q", res.Exit, res.Stderr)
	}
}

func TestRunEnvIsExactlyWhatIsGiven(t *testing.T) {
	t.Setenv("WB_SECRET", "x")
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Env: []string{"WB_FAKE_TOOL=env", "A=1"}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Stdout), "WB_SECRET") {
		t.Error("the parent's environment leaked")
	}
	got := strings.Split(string(res.Stdout), "\n")
	slices.Sort(got)
	got = slices.DeleteFunc(got, func(s string) bool { // the runtime may add its own on some platforms
		return runtime.GOOS == "windows" && strings.HasPrefix(strings.ToUpper(s), "SYSTEMROOT=")
	})
	if want := []string{"A=1", "WB_FAKE_TOOL=env"}; !slices.Equal(got, want) {
		t.Errorf("environment = %q, want %q", got, want)
	}
}

func TestRunNilEnvInherits(t *testing.T) {
	t.Setenv("WB_FAKE_TOOL", "env")
	t.Setenv("WB_MARK", "present")
	res, err := Run(context.Background(), fake(t), nil, RunOptions{Timeout: 10 * time.Second})
	if err != nil || !strings.Contains(string(res.Stdout), "WB_MARK=present") {
		t.Errorf("err %v, stdout %q", err, res.Stdout)
	}
}

// Arguments reach the child as they are: no shell sees them, so metacharacters
// and option-looking text stay data; stdin carries content.
func TestRunNoShellAndStdin(t *testing.T) {
	args := []string{"$(touch pwned)", "; touch pwned", "--version", "`id`", "a b", ""}
	dir := t.TempDir()
	res, err := Run(context.Background(), fake(t), args, RunOptions{Dir: dir, Env: mode("argv"), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(string(res.Stdout), "\n"); !slices.Equal(got, args) {
		t.Errorf("args = %q, want %q", got, args)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("a shell ran an argument")
	}
	in := []byte("--help\n$(touch pwned)\n")
	res, err = Run(context.Background(), fake(t), nil, RunOptions{Dir: dir, Stdin: in, Env: mode("stdin"), Timeout: 10 * time.Second})
	if err != nil || !bytes.Equal(res.Stdout, in) {
		t.Errorf("err %v, stdin echo %q", err, res.Stdout)
	}
}

func TestRunCancelKillsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Run(ctx, fake(t), nil, RunOptions{Env: mode("sleep"), Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > slack(3*time.Second) {
		t.Errorf("a cancelled request took %v to return", time.Since(start))
	}
}

func TestRunLeavesNoGoroutines(t *testing.T) {
	settle := func() int {
		var n int
		for i := 0; i < 50; i++ {
			runtime.GC()
			n = runtime.NumGoroutine()
			time.Sleep(20 * time.Millisecond)
		}
		return n
	}
	before := settle()
	for _, m := range []string{"sleep", "flood", "json"} {
		Run(context.Background(), fake(t), nil, RunOptions{Env: mode(m), Timeout: 300 * time.Millisecond, MaxOutput: 1 << 20, Stdin: bytes.Repeat([]byte("s"), 1<<20)})
	}
	var after int
	for i := 0; i < 100; i++ {
		after = runtime.NumGoroutine()
		if after <= before {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutines: %d before, %d after", before, after)
}

func TestRunRefusesBadTool(t *testing.T) {
	for _, tool := range []Tool{{}, {Name: "x", Path: "relative/tool"}, {Name: "x", Path: "tool"}} {
		if _, err := Run(context.Background(), tool, nil, RunOptions{}); err == nil {
			t.Errorf("Run(%+v) started a program", tool)
		}
	}
}

// --- Find ---

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A script named like the tool inside a workspace root, first on PATH, is
// refused and never started.
func TestFindRefusesInRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the planted program is a /bin/sh script; Windows has no equivalent of an executable script")
	}
	root, other := t.TempDir(), t.TempDir()
	marker := filepath.Join(root, "marker")
	script(t, root, "shellcheck", "#!/bin/sh\ntouch '"+marker+"'\n")
	script(t, other, "shellcheck", "#!/bin/sh\nexit 0\n")
	prof := NewProfile([]string{root, other}) // PATH=<root>:<other>
	tool, ref, ok := Find("shellcheck", "", prof, []string{root})
	if ok || ref == nil {
		t.Fatalf("Find = %+v, %v, %v; want a refusal", tool, ref, ok)
	}
	if ref.Name != "shellcheck" || ref.Reason == "" || !strings.Contains(ref.Reason, "workspace") {
		t.Errorf("refusal = %+v", ref)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the planted script ran")
	}
	// The same directory order without the root as a workspace folder is fine.
	if tool, ref, ok := Find("shellcheck", "", prof, nil); !ok || ref != nil || tool.Path == "" {
		t.Errorf("Find with no roots = %+v, %v, %v", tool, ref, ok)
	}
}

func TestFindConfiguredWinsAndIsResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root, other, third := t.TempDir(), t.TempDir(), t.TempDir()
	real := script(t, other, "sc-real", "#!/bin/sh\n")
	script(t, third, "shellcheck", "#!/bin/sh\n")
	link := filepath.Join(other, "sc-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	prof := NewProfile([]string{third})
	tool, ref, ok := Find("shellcheck", link, prof, []string{root})
	want, _ := filepath.EvalSymlinks(real)
	if !ok || ref != nil || tool.Path != want || tool.Name != "shellcheck" {
		t.Fatalf("Find = %+v, %v, %v; want %s", tool, ref, ok, want)
	}
	// A bare configured name is looked up in the profile.
	if tool, _, ok = Find("shellcheck", "sc-real", NewProfile([]string{other}), nil); !ok || tool.Path != want {
		t.Errorf("bare configured name: %+v %v", tool, ok)
	}
	// A configured name that is not found does not fall back to the default name.
	if _, ref, ok = Find("shellcheck", "no-such-tool", prof, nil); ok || ref != nil {
		t.Errorf("missing configured name: %v %v", ref, ok)
	}
	// Not found at all.
	if _, ref, ok = Find("nothing-here", "", prof, nil); ok || ref != nil {
		t.Errorf("missing: %v %v", ref, ok)
	}
	// A nil profile finds nothing rather than panicking or reading $PATH.
	if _, _, ok = Find("shellcheck", "", nil, nil); ok {
		t.Error("nil profile found a program")
	}
}

func TestFindRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes and symlinks differ on Windows")
	}
	root, out := t.TempDir(), t.TempDir()
	inRoot := script(t, filepath.Join(root), "tool", "#!/bin/sh\n")
	outside := script(t, out, "tool", "#!/bin/sh\n")
	link := filepath.Join(out, "to-root")
	if err := os.Symlink(inRoot, link); err != nil {
		t.Fatal(err)
	}
	back := filepath.Join(root, "bin-link") // a link in the root that leads out
	if err := os.Symlink(outside, back); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(out, "data")
	os.WriteFile(data, []byte("x"), 0o644)
	for name, configured := range map[string]string{
		"in root":                 inRoot,
		"link from outside to in": link,
		"link in root to outside": back,
		"relative path":           "./tool",
		"relative with dir":       "bin/tool",
		"not executable":          data,
		"missing path":            filepath.Join(out, "absent"),
		"directory":               out,
	} {
		tool, ref, ok := Find("tool", configured, nil, []string{root})
		if ok || ref == nil || ref.Reason == "" {
			t.Errorf("%s: Find = %+v, %v, %v; want a refusal", name, tool, ref, ok)
		}
	}
	// A root given through a symlink is still a root.
	alias := filepath.Join(out, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, ref, ok := Find("tool", inRoot, nil, []string{alias}); ok || ref == nil {
		t.Error("a symlinked root does not contain its tools")
	}
	// An outside program is fine with the root present.
	if _, ref, ok := Find("tool", outside, nil, []string{root}); !ok || ref != nil {
		t.Errorf("outside tool: %v %v", ref, ok)
	}
}
