package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runFmt(t *testing.T, dir string, args ...string) (code int, out, errs string) {
	t.Helper()
	t.Setenv("BASH_LSP_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	t.Chdir(dir)
	var o, e bytes.Buffer
	code = run(append([]string{"fmt"}, args...), strings.NewReader(""), &o, &e)
	return code, o.String(), e.String()
}

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestFmtPrintsFormattedText(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.sh"), "if true;then echo   a;fi\n", 0o644)
	code, out, errs := runFmt(t, dir, "a.sh")
	if code != 0 || out != "if true; then echo a; fi\n" || errs != "" {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.sh")); string(b) != "if true;then echo   a;fi\n" {
		t.Fatalf("the file changed without -w: %q", b)
	}
}

func TestFmtWriteIsAtomicAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.sh")
	write(t, p, "#!/bin/sh\nif true;then echo   a;fi\n", 0o750)
	code, out, errs := runFmt(t, dir, "-w", "run.sh")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "#!/bin/sh\nif true; then echo a; fi\n" {
		t.Fatalf("got %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o750 {
		t.Fatalf("mode %v", st.Mode())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temporary files left behind: %v", ents)
	}
	// An unchanged file is not touched.
	before, _ := os.Stat(p)
	if code, _, _ := runFmt(t, dir, "-w", "run.sh"); code != 0 {
		t.Fatal(code)
	}
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged file was rewritten")
	}
	// A symlink keeps being a symlink: its target is rewritten.
	target := filepath.Join(dir, "t.sh")
	write(t, target, "echo   x\n", 0o644)
	if err := os.Symlink("t.sh", filepath.Join(dir, "l.sh")); err != nil {
		t.Skip("no symlinks")
	}
	if code, _, errs := runFmt(t, dir, "-w", "l.sh"); code != 0 {
		t.Fatalf("exit %d %s", code, errs)
	}
	if st, _ := os.Lstat(filepath.Join(dir, "l.sh")); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if b, _ := os.ReadFile(target); string(b) != "echo x\n" {
		t.Fatalf("target %q", b)
	}
}

func TestFmtListsChangedFilesAndExitsOne(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.sh"), "echo   a\n", 0o644)
	write(t, filepath.Join(dir, "b.sh"), "echo b\n", 0o644)
	write(t, filepath.Join(dir, "c.sh"), "echo   c\n", 0o644)
	code, out, _ := runFmt(t, dir, "a.sh", "b.sh", "c.sh")
	if code != 1 || out != "a.sh\nc.sh\n" {
		t.Fatalf("exit %d out %q", code, out)
	}
	if code, out, _ := runFmt(t, dir, "b.sh", "b.sh"); code != 0 || out != "" {
		t.Fatalf("nothing to change: exit %d out %q", code, out)
	}
	for _, f := range []string{"a.sh", "c.sh"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); !strings.Contains(string(b), "   ") {
			t.Fatalf("%s was written without -w", f)
		}
	}
}

func TestFmtBrokenFileIsReportedAndNotWritten(t *testing.T) {
	dir := t.TempDir()
	broken := "if true; then\n  echo   a\n"
	write(t, filepath.Join(dir, "bad.sh"), broken, 0o644)
	write(t, filepath.Join(dir, "ok.sh"), "echo   a\n", 0o644)
	code, out, errs := runFmt(t, dir, "-w", "bad.sh", "ok.sh")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errs, "bad.sh") || !strings.Contains(errs, "syntax error") || out != "" {
		t.Fatalf("out %q err %q", out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bad.sh")); string(b) != broken {
		t.Fatalf("the broken file was written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ok.sh")); string(b) != "echo a\n" {
		t.Fatalf("the good file next to it was not formatted: %q", b)
	}
	// Alone and without -w: nothing on stdout.
	if code, out, errs := runFmt(t, dir, "bad.sh"); code != 1 || out != "" || !strings.Contains(errs, "bad.sh") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
}

func TestFmtUsesEditorconfigAndSettings(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".editorconfig"), "[*.sh]\nindent_style=space\nindent_size=2\n", 0o644)
	write(t, filepath.Join(dir, "a.sh"), "if a;then\nb\nfi\n", 0o644)
	if code, out, _ := runFmt(t, dir, "a.sh"); code != 0 || out != "if a; then\n  b\nfi\n" {
		t.Fatalf("exit %d out %q", code, out)
	}
	write(t, filepath.Join(dir, ".editorconfig"), "", 0o644)
	write(t, filepath.Join(dir, ".bash-lsp.toml"), "[format]\nindent = 4\n", 0o644)
	if code, out, errs := runFmt(t, dir, "a.sh"); code != 0 || out != "if a; then\n    b\nfi\n" {
		t.Fatalf("settings: exit %d out %q err %q", code, out, errs)
	}
}

func TestFmtUsageAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runFmt(t, dir); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("no files: exit %d %q", code, errs)
	}
	if code, _, errs := runFmt(t, dir, "--bogus", "a.sh"); code != 2 {
		t.Fatalf("bad flag: exit %d %q", code, errs)
	}
	if code, out, errs := runFmt(t, dir, "nope.sh"); code != 1 || out != "" || !strings.Contains(errs, "nope.sh") {
		t.Fatalf("missing: exit %d out %q err %q", code, out, errs)
	}
	if code, _, errs := runFmt(t, dir, "."); code != 1 || !strings.Contains(errs, "regular file") {
		t.Fatalf("directory: exit %d err %q", code, errs)
	}
}

func TestFmtRefusesReadOnlyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ro.sh")
	write(t, p, "echo   a\n", 0o444)
	write(t, filepath.Join(dir, "ok.sh"), "echo   a\n", 0o644)
	code, _, errs := runFmt(t, dir, "-w", "ro.sh", "ok.sh")
	if code != 1 || !strings.Contains(errs, "ro.sh") || !strings.Contains(errs, "not writable") {
		t.Fatalf("exit %d err %q", code, errs)
	}
	if b, _ := os.ReadFile(p); string(b) != "echo   a\n" {
		t.Fatalf("read-only file rewritten: %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o444 {
		t.Fatalf("mode %v", st.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ok.sh")); string(b) != "echo a\n" {
		t.Fatalf("the other file was not formatted: %q", b)
	}
}

func TestFmtRefusesPathOutsideRoot(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(outer, "out.sh"), "echo   a\n", 0o644)
	write(t, filepath.Join(root, "in.sh"), "echo   a\n", 0o644)
	code, out, errs := runFmt(t, root, "-w", "../out.sh", "in.sh")
	if code != 1 || !strings.Contains(errs, "outside") || out != "" {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(outer, "out.sh")); string(b) != "echo   a\n" {
		t.Fatalf("outside file written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "in.sh")); string(b) != "echo a\n" {
		t.Fatalf("inside file: %q", b)
	}
	// --root names the directory that bounds it.
	if code, _, errs := runFmt(t, root, "-w", "--root", outer, "../out.sh"); code != 0 {
		t.Fatalf("--root: exit %d %q", code, errs)
	}
	// A link that leaves the root is outside it.
	if err := os.Symlink(filepath.Join(outer, "out.sh"), filepath.Join(root, "link.sh")); err == nil {
		write(t, filepath.Join(outer, "out.sh"), "echo   b\n", 0o644)
		if code, _, errs := runFmt(t, root, "-w", "link.sh"); code != 1 || !strings.Contains(errs, "outside") {
			t.Fatalf("link: exit %d %q", code, errs)
		}
	}
}

func TestFmtDoubleDashEndsOptions(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "-w"), "echo   a\n", 0o644)
	code, out, errs := runFmt(t, dir, "--", "-w")
	if code != 0 || out != "echo a\n" {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	if code, _, errs := runFmt(t, dir, "-w", "--", "-w"); code != 0 {
		t.Fatalf("exit %d %q", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "-w")); string(b) != "echo a\n" {
		t.Fatalf("%q", b)
	}
}

func TestFmtHonoursMaxFileBytes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".bash-lsp.toml"), "[limits]\nmax_file_bytes = 20\n", 0o644)
	write(t, filepath.Join(dir, "big.sh"), "echo   a\necho   b\necho   c\n", 0o644)
	write(t, filepath.Join(dir, "small.sh"), "echo   a\n", 0o644)
	code, out, errs := runFmt(t, dir, "-w", "big.sh", "small.sh")
	if code != 1 || !strings.Contains(errs, "big.sh") || !strings.Contains(errs, "limit") || out != "" {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "big.sh")); !strings.Contains(string(b), "   ") {
		t.Fatalf("over-limit file written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "small.sh")); string(b) != "echo a\n" {
		t.Fatalf("small: %q", b)
	}
}

func TestFmtHelpNamesTheLimits(t *testing.T) {
	code, out, _ := runFmt(t, t.TempDir(), "-h")
	if code != 0 || !strings.Contains(out, "temporary file") || !strings.Contains(out, "CRLF") || !strings.Contains(out, "$[ ]") {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestFmtClampsEditorconfigIndent(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".editorconfig"), "[*.sh]\nindent_style=space\nindent_size=10000000\n", 0o644)
	write(t, filepath.Join(dir, "a.sh"), "if a;then\nb\nfi\n", 0o644)
	code, out, errs := runFmt(t, dir, "a.sh")
	if code != 0 || out != "if a; then\n"+strings.Repeat(" ", 16)+"b\nfi\n" || !strings.Contains(errs, "clamped to 16") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
}
