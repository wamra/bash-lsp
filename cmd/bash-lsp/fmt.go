package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wamra/bash-lsp/format"
	"github.com/wamra/bash-lsp/internal/config"
)

const fmtUsage = "usage: bash-lsp fmt [-w] [--root DIR] [--] FILE..."

// maxFmtBytes is the largest file fmt reads; format refuses anything over it
// with the same reason, but the file is not read whole first.
const maxFmtBytes = 2 << 20

// cmdFmt formats shell files with the in-process formatter, with the options
// of format.OptionsFor (.editorconfig, then settings, then shfmt's defaults)
// for each file. The root of the settings and of the .editorconfig lookup is
// found by findRoot from the current directory (--root wins); a file outside it
// reads only its own directory's .editorconfig.
//
// One file without -w: the formatted text on stdout, exit 0. Several files
// without -w: the names of the files that would change on stdout, exit 1 if
// there are any. With -w each changed file is replaced atomically, keeping
// its mode, and nothing is printed. A file that cannot be formatted (syntax
// error, zsh, over the size cap) is reported on stderr, left alone, and makes
// the exit code 1; the other files are still done.
func cmdFmt(args []string, stdout, stderr io.Writer) int {
	write, root := false, ""
	var files []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			files = append(files, args[i+1:]...)
			break
		}
		switch a := args[i]; {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, fmtHelp)
			return 0
		case a == "-w":
			write = true
		case a == "--root":
			if i+1 >= len(args) {
				return fmtUsageError(stderr, "--root needs a directory")
			}
			i++
			root = args[i]
		case strings.HasPrefix(a, "-"):
			return fmtUsageError(stderr, fmt.Sprintf("unknown flag %q for fmt", a))
		default:
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		return fmtUsageError(stderr, "fmt needs at least one FILE")
	}
	absRoot, _ := findRoot(nil, root)
	st := config.Load(absRoot, nil)
	for _, w := range st.Warnings {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	code, changed := 0, 0
	noted := map[string]bool{}
	for _, name := range files {
		out, orig, ok := fmtOne(name, absRoot, st, stderr, noted)
		if !ok {
			code = 1
			continue
		}
		differs := !bytes.Equal(out, orig)
		switch {
		case write:
			if differs {
				if err := writable(name); err != nil {
					fmt.Fprintf(stderr, "bash-lsp: %s: not writable, left as it is: %v\n", name, err)
					code = 1
				} else if err := writeFileAtomic(name, out); err != nil {
					fmt.Fprintf(stderr, "bash-lsp: %s: %v\n", name, err)
					code = 1
				}
			}
		case len(files) == 1:
			stdout.Write(out)
		case differs:
			fmt.Fprintln(stdout, name)
			changed++
		}
	}
	if changed > 0 {
		code = 1
	}
	return code
}

const fmtHelp = fmtUsage + `

Formats with the in-process printer. One file without -w prints the text;
several list the files that would change (exit 1 if any); -w rewrites them.
A file that cannot be formatted is reported and left alone (exit 1).

-w writes a temporary file and renames it over the original: the mode bits are
kept; the owner, extended attributes, hard links and setuid or sticky bits are
not, and an edit made between the read and the rename is lost.
A CRLF file is read as line-terminated; the output keeps CRLF.
$[ ] and for ((;;)) { } are refused. Backquotes are printed as $( ).
`

func fmtUsageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "bash-lsp: %s\n%s\n", msg, fmtUsage)
	return 2
}

// fmtOne reads and formats one file. ok is false when the file was reported
// instead.
func fmtOne(name, root string, st config.Settings, stderr io.Writer, noted map[string]bool) (out, orig []byte, ok bool) {
	real, err := filepath.Abs(name)
	if err == nil {
		real, err = filepath.EvalSymlinks(real)
	}
	if err != nil {
		fmt.Fprintf(stderr, "bash-lsp: %s: %v\n", name, err)
		return nil, nil, false
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Fprintf(stderr, "bash-lsp: %s: outside the root %s (use --root); not formatted\n", name, root)
		return nil, nil, false
	}
	limit := int64(maxFmtBytes)
	if st.MaxFileBytes > 0 && st.MaxFileBytes < limit {
		limit = st.MaxFileBytes
	}
	info, err := os.Stat(real)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "bash-lsp: %s: %v\n", name, err)
		return nil, nil, false
	case !info.Mode().IsRegular():
		fmt.Fprintf(stderr, "bash-lsp: %s: not a regular file\n", name)
		return nil, nil, false
	case info.Size() > limit:
		fmt.Fprintf(stderr, "bash-lsp: %s: file is %d bytes, over the %d byte limit\n", name, info.Size(), limit)
		return nil, nil, false
	}
	orig, err = os.ReadFile(real)
	if err != nil {
		fmt.Fprintf(stderr, "bash-lsp: %s: %v\n", name, err)
		return nil, nil, false
	}
	abs := real
	for _, n := range format.EditorconfigNotes(abs, root) {
		if !noted[n] {
			noted[n] = true
			fmt.Fprintf(stderr, "bash-lsp: editorconfig: %s\n", n)
		}
	}
	out, err = format.Source(name, orig, format.OptionsFor(abs, root, st.Format, nil))
	if err != nil {
		fmt.Fprintf(stderr, "bash-lsp: %s: %v\n", name, err)
		return nil, nil, false
	}
	return out, orig, true
}

// writable reports whether the file at path (the target of a link) can be
// opened for writing by this user. Nothing is written.
func writable(path string) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// writeFileAtomic replaces the file at path (the target, if path is a link)
// with data: a temporary file in the same directory with the same permission
// bits, written, synced and renamed over it.
func writeFileAtomic(path string, data []byte) error {
	tmp, target, err := stageFile(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// stageFile writes data to a temporary file beside path (the target, if path
// is a link) with the permission bits of the file, synced, and returns its
// name and the file it is meant to replace. On an error no temporary file is
// left.
func stageFile(path string, data []byte) (tmpName, target string, err error) {
	if real, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = real
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bash-lsp-fmt-*")
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		return "", "", err
	}
	if _, err = tmp.Write(data); err != nil {
		return "", "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", "", err
	}
	if err = tmp.Close(); err != nil {
		return "", "", err
	}
	return tmp.Name(), path, nil
}
