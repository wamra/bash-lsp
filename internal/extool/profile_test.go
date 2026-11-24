package extool

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProfileExecutableOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are a Unix rule; PATHEXT is tested separately")
	}
	dir := t.TempDir()
	foo := write(t, dir, "foo", 0o755)
	write(t, dir, "bar", 0o644)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewProfile([]string{dir})
	if got, ok := p.Lookup("foo"); !ok || got != foo {
		t.Fatalf("Lookup(foo) = %q, %v; want %q, true", got, ok, foo)
	}
	for _, name := range []string{"bar", "sub", "missing", "", "foo/bar", "../foo"} {
		if got, ok := p.Lookup(name); ok {
			t.Errorf("Lookup(%q) = %q, true; want not found", name, got)
		}
	}
}

func TestProfileRelativeDirSkippedAndReported(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "foo", 0o755)
	p := NewProfile([]string{".", "bin", "", dir})
	if _, ok := p.Lookup("foo"); !ok {
		t.Fatal("foo in the absolute directory was not found")
	}
	sk := p.Skipped()
	if len(sk) != 3 {
		t.Fatalf("Skipped() = %q; want three entries", sk)
	}
	for i, dirName := range []string{".", "bin", ""} {
		if !strings.HasPrefix(sk[i], dirName+": ") || !strings.Contains(sk[i], "relative") {
			t.Errorf("Skipped()[%d] = %q; want %q: relative directory", i, sk[i], dirName)
		}
	}
}

func TestProfileFirstDirectoryWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are a Unix rule")
	}
	a, b := t.TempDir(), t.TempDir()
	pa := write(t, a, "tool", 0o755)
	write(t, b, "tool", 0o755)
	if got, _ := NewProfile([]string{a, b}).Lookup("tool"); got != pa {
		t.Errorf("Lookup(tool) = %q; want the first directory's %q", got, pa)
	}
}

func TestProfileSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir, other := t.TempDir(), t.TempDir()
	real := write(t, other, "real", 0o755)
	data := write(t, other, "data", 0o644)
	for name, target := range map[string]string{"ok": real, "notexec": data, "dangling": filepath.Join(other, "gone"), "dirlink": other} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	p := NewProfile([]string{dir})
	if got, ok := p.Lookup("ok"); !ok || got != filepath.Join(dir, "ok") {
		t.Errorf("Lookup(ok) = %q, %v; want the link in the profile directory", got, ok)
	}
	for _, name := range []string{"notexec", "dangling", "dirlink"} {
		if _, ok := p.Lookup(name); ok {
			t.Errorf("%s counts as a command", name)
		}
	}
}

func TestProfileLimits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are a Unix rule")
	}
	dirs := make([]string, 0, 66)
	for i := 0; i < 66; i++ {
		d := t.TempDir()
		write(t, d, "cmd"+string(rune('a'+i%26))+string(rune('a'+i/26)), 0o755)
		dirs = append(dirs, d)
	}
	p := NewProfile(dirs)
	if p.Len() != 64 {
		t.Errorf("Len() = %d; want 64 (one name per directory read)", p.Len())
	}
	if sk := p.Skipped(); len(sk) != 2 || !strings.Contains(sk[0], "more than 64 directories") {
		t.Errorf("Skipped() = %q; want the two directories past 64", sk)
	}
	defer func(n int) { maxProfileNames = n }(maxProfileNames)
	maxProfileNames = 200
	big := t.TempDir()
	for i := 0; i < maxProfileNames+10; i++ {
		if err := os.WriteFile(filepath.Join(big, "n"+itoa(i)), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p = NewProfile([]string{big})
	if p.Len() != maxProfileNames {
		t.Errorf("Len() = %d; want the cap %d", p.Len(), maxProfileNames)
	}
	if sk := p.Skipped(); len(sk) != 1 || !strings.Contains(sk[0], "more than 200 names") {
		t.Errorf("Skipped() = %q; want one name-limit entry", sk)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestProfileFromEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are a Unix rule")
	}
	dir := t.TempDir()
	write(t, dir, "envtool", 0o755)
	t.Setenv("PATH", "relative"+string(os.PathListSeparator)+dir+string(os.PathListSeparator))
	p := ProfileFromEnv()
	if _, ok := p.Lookup("envtool"); !ok {
		t.Error("envtool from $PATH not found")
	}
	if sk := p.Skipped(); len(sk) != 2 { // "relative" and the empty last entry
		t.Errorf("Skipped() = %q; want the relative and the empty entry", sk)
	}
	t.Setenv("PATH", "")
	if p := ProfileFromEnv(); p.Len() != 0 || p.Commands() != nil {
		t.Error("an empty PATH must give an empty profile and a nil Commands")
	}
}

func TestNilProfile(t *testing.T) {
	var p *Profile
	if _, ok := p.Lookup("ls"); ok || p.Len() != 0 || p.Skipped() != nil || p.Commands() != nil {
		t.Error("a nil Profile must answer nothing")
	}
}

// NewProfile only lists the directories; a name is checked on its first
// Lookup, and the answer is kept.
func TestProfileChecksOnFirstLookup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are a Unix rule")
	}
	dir := t.TempDir()
	p := write(t, dir, "late", 0o644)
	prof := NewProfile([]string{dir})
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := prof.Lookup("late"); !ok || got != p {
		t.Fatalf("Lookup(late) = %q, %v; the file was made executable before its first lookup", got, ok)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := prof.Lookup("late"); !ok {
		t.Error("the first answer was not kept")
	}
}

// A non-executable file of a name does not hide a program of that name later
// in the list, as in a shell.
func TestProfileSkipsNonProgramsOfTheSameName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits are a Unix rule")
	}
	a, b := t.TempDir(), t.TempDir()
	write(t, a, "tool", 0o644)
	pb := write(t, b, "tool", 0o755)
	if got, ok := NewProfile([]string{a, b}).Lookup("tool"); !ok || got != pb {
		t.Errorf("Lookup(tool) = %q, %v; want %q", got, ok, pb)
	}
}

// fakeDir is a directory reader for tests: next gives each chunk.
type fakeDir struct {
	next   func(n int) ([]os.DirEntry, error)
	closed *bool
}

func (f fakeDir) ReadDir(n int) ([]os.DirEntry, error) { return f.next(n) }
func (f fakeDir) Close() error {
	if f.closed != nil {
		*f.closed = true
	}
	return nil
}

type fakeEntry struct{ name string }

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return false }
func (e fakeEntry) Type() os.FileMode          { return 0 }
func (e fakeEntry) Info() (os.FileInfo, error) { return nil, os.ErrInvalid }

func useReader(t *testing.T, f func(dir string) (dirReader, error)) {
	t.Helper()
	old := openDir
	openDir = f
	t.Cleanup(func() { openDir = old })
}

// A directory on a hung mount does not hold the profile: it is skipped after
// the per-directory time bound and the other directories are still read.
func TestProfileHungDirectoryIsSkipped(t *testing.T) {
	good := t.TempDir()
	write(t, good, "tool", 0o755)
	hung := filepath.Join(t.TempDir(), "hung")
	if runtime.GOOS == "windows" {
		hung = `C:\hung`
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	useReader(t, func(dir string) (dirReader, error) {
		if dir == hung {
			return fakeDir{next: func(int) ([]os.DirEntry, error) { <-release; return nil, io.EOF }}, nil
		}
		return osOpenDir(dir)
	})
	defer func(d time.Duration) { profileDirTimeout = d }(profileDirTimeout)
	profileDirTimeout = 100 * time.Millisecond
	start := time.Now()
	p := NewProfile([]string{hung, good})
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("NewProfile took %v with a hung directory", el)
	}
	if _, ok := p.Lookup("tool"); !ok && runtime.GOOS != "windows" {
		t.Error("the directory after the hung one was not read")
	}
	if sk := p.Skipped(); len(sk) != 1 || !strings.HasPrefix(sk[0], hung+": ") || !strings.Contains(sk[0], "timed out") {
		t.Errorf("Skipped() = %q; want the hung directory with a time-out reason", sk)
	}
}

// A directory is read in chunks and reading stops at the name cap: a reader
// that never ends is not drained.
func TestProfileReadsInChunksAndStopsAtCap(t *testing.T) {
	defer func(n int) { maxProfileNames = n }(maxProfileNames)
	maxProfileNames = 500
	dir := filepath.Join(t.TempDir(), "endless")
	if runtime.GOOS == "windows" {
		dir = `C:\endless`
	}
	asked, calls, closed := 0, 0, false
	useReader(t, func(string) (dirReader, error) {
		return fakeDir{closed: &closed, next: func(n int) ([]os.DirEntry, error) {
			calls++
			if n <= 0 || n > profileChunk {
				t.Errorf("chunk size %d; want 1..%d", n, profileChunk)
			}
			var out []os.DirEntry
			for i := 0; i < n; i++ {
				asked++
				out = append(out, fakeEntry{"n" + itoa(asked)})
			}
			return out, nil
		}}, nil
	})
	p := NewProfile([]string{dir})
	if asked > 2*maxProfileNames+profileChunk {
		t.Errorf("%d entries read for a cap of %d", asked, maxProfileNames)
	}
	if p.Len() != 500 {
		t.Errorf("Len() = %d; want the cap 500", p.Len())
	}
	if sk := p.Skipped(); len(sk) != 1 || !strings.Contains(sk[0], "more than 500 names") {
		t.Errorf("Skipped() = %q", sk)
	}
	if !closed {
		t.Error("the directory was not closed")
	}
}

func TestProfileDirsCountsSkippedToo(t *testing.T) {
	good := t.TempDir()
	p := NewProfile([]string{good, "relative", filepath.Join(good, "missing")})
	if p.Dirs() != 3 || len(p.Skipped()) != 2 {
		t.Errorf("Dirs() = %d, Skipped() = %q; want 3 and two entries", p.Dirs(), p.Skipped())
	}
	var nilp *Profile
	if nilp.Dirs() != 0 {
		t.Error("Dirs() of a nil profile")
	}
}
