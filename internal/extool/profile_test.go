package extool

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
