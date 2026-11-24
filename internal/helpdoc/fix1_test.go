package helpdoc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/extool"
)

func TestParseManOptionLineEndingInOneSpace(t *testing.T) {
	for _, ws := range []string{" ", "\t", " "} {
		page := "SYNOPSIS\n     x [-A]\n\nDESCRIPTION\n     -A" + ws + "\n     -B" + ws
		d, ok := ParseMan("x", []byte(page))
		if !ok || len(d.Options) == 0 || d.Options[0].Flag != "-A" {
			t.Errorf("%q: %+v %v", ws, d, ok)
		}
	}
}

func checkDocCaps(t *testing.T, d Doc) {
	t.Helper()
	if len(d.Synopsis) > maxSynopsis || len(d.Options) > maxOptions {
		t.Fatalf("caps broken: synopsis %d bytes, %d options", len(d.Synopsis), len(d.Options))
	}
	for _, o := range d.Options {
		if len(o.Text) > maxOptionText {
			t.Fatalf("option text of %d bytes", len(o.Text))
		}
	}
}

func FuzzParseMan(f *testing.F) {
	f.Add(fixture0("man-bsd.txt"))
	f.Add([]byte("SYNOPSIS\n     x [-A]\n\nDESCRIPTION\n     -A \n"))
	f.Add([]byte("SYNOPSIS\n     x\n     -A\t\n     -B \n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if d, ok := ParseMan("x", b); ok {
			checkDocCaps(t, d)
		}
	})
}

func FuzzParseHelp(f *testing.F) {
	f.Add(fixture0("help-read.txt"))
	f.Add([]byte("x: x [-ab] [-c d] [--long arg] [[-]\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if d, ok := ParseHelp("x", b); ok {
			checkDocCaps(t, d)
		}
	})
}

func TestTimeoutAndRunErrorsAreNotCached(t *testing.T) {
	skipWindows(t)
	n := 0
	f := &fakeRun{reply: func(c call) (extool.Result, error) {
		n++
		if n == 1 {
			return extool.Result{Exit: -1}, extool.ErrTimeout
		}
		if n == 2 {
			return extool.Result{Exit: -1}, errors.New("boom")
		}
		return helpReply(c)
	}}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	for i := 0; i < 2; i++ {
		if _, ok := l.Builtin(context.Background(), "read"); ok {
			t.Fatal("a failed run gave a doc")
		}
		if _, _, known := l.Cached("builtin", "read"); known {
			t.Fatalf("run %d: a failed run was cached", i+1)
		}
	}
	if _, ok := l.Builtin(context.Background(), "read"); !ok || f.count() != 3 {
		t.Fatalf("third lookup: ok %v, %d runs", ok, f.count())
	}
}

func TestParserPanicIsACachedMiss(t *testing.T) {
	skipWindows(t)
	f := &fakeRun{reply: helpReply}
	l := newLookup(t, f, defaults(), []string{toolDir(t, "bash", "man")}, []string{t.TempDir()})
	l.resolve(&l.bash, "bash", "")
	boom := func(string, []byte) (Doc, bool) { panic("bad page") }
	if _, ok := l.lookup(context.Background(), key("builtin", "read"), l.bash.tool, nil, boom, "read"); ok {
		t.Fatal("a panicking parser gave a doc")
	}
	if _, found, known := l.Cached("builtin", "read"); found || !known {
		t.Fatalf("found %v known %v", found, known)
	}
}

func TestBadNamesReachTheNameGate(t *testing.T) {
	skipWindows(t)
	long := "a" + strings.Repeat("b", maxName) // matches the pattern, one byte too long
	bad := []string{"-n", ";ls", "$(touch x)", ".hidden", "-", "a b", "tool\n", "a*b", "ünï", long}
	files := []string{"bash", "man"}
	for _, n := range bad {
		if len(n) <= 255 {
			files = append(files, n)
		}
	}
	dir := toolDir(t, files...)
	for _, name := range bad {
		f := &fakeRun{reply: helpReply}
		var logs []string
		l := New(f.run, defaults(), extool.NewProfile([]string{dir}), []string{t.TempDir()})
		l.SetLogger(func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) })
		if _, ok := l.Command(context.Background(), name); ok {
			t.Errorf("Command(%q) gave a doc", name)
		}
		if l.Allowed("command", name) || f.count() != 0 {
			t.Errorf("%q: allowed or spawned", name)
		}
		if len(logs) == 0 || !strings.Contains(logs[0], "plain command word") {
			t.Errorf("%q was stopped by something other than the name gate: %q", name, logs)
		}
	}
}

func TestWindowsBranchStartsNothing(t *testing.T) {
	old := onWindows
	onWindows = true
	t.Cleanup(func() { onWindows = old })
	f := &fakeRun{reply: helpReply}
	dir := toolDir(t, "bash", "man", "sample")
	l := newLookup(t, f, defaults(), []string{dir}, []string{t.TempDir()})
	if _, ok := l.Builtin(context.Background(), "read"); ok {
		t.Error("Builtin on Windows")
	}
	if _, ok := l.Command(context.Background(), "sample"); ok {
		t.Error("Command on Windows")
	}
	if help, man, why := l.Available(); help || man || !strings.Contains(why, "Windows") {
		t.Errorf("Available = %v %v %q", help, man, why)
	}
	for _, line := range l.Lines() {
		if !strings.Contains(line, "unavailable (not supported on Windows)") {
			t.Errorf("line %q", line)
		}
	}
	if f.count() != 0 {
		t.Errorf("%d spawns", f.count())
	}
}
