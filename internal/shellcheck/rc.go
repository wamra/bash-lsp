package shellcheck

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/wamra/bash-lsp/internal/extool"
)

// A repo's .shellcheckrc is not given to ShellCheck (it runs in a neutral
// directory, so it finds none): its keys can make ShellCheck read files
// anywhere (external-sources, source-path). The server reads the file itself
// and applies the two keys that only narrow or widen the report: disable
// (findings dropped after parsing) and enable (optional checks, passed on the
// command line when the name is plain). shell, external-sources, source-path
// and any other key are not used.
//
// The grammar is the one ShellCheck 0.11.0 reads for those keys (verified with
// --rcfile on files of each shape): a # starts a comment; a value may be in
// single or double quotes; one line may hold several key=value words, separated by space or tab; the
// disable value is a comma-separated list of codes (SC2086 or 2086), ranges
// (SC2000-SC2010) or the lower-case word all. Where ShellCheck rejects a line
// (spaces around = or after a comma, an empty entry, a bad code, an
// unbalanced quote, a lone carriage return, a key with a character other than
// letters and hyphens) it ignores the whole file; the server then applies
// nothing from that file and says so. An inverted range disables nothing.
// Unknown keys are ignored, as ShellCheck does. Unlike ShellCheck, which uses
// only the nearest file, the files up to the workspace root are all read
// (disabled codes are the union).

const maxRCBytes = 64 << 10

var (
	disableEntry = regexp.MustCompile(`^(?:SC)?(\d{1,6})(?:-(?:SC)?(\d{1,6}))?$`)
	enableName   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

type rcInfo struct {
	ranges [][2]int
	all    bool
	enable []string
	notes  []string
}

func (r rcInfo) disabled(code int) bool {
	if r.all {
		return true
	}
	for _, rg := range r.ranges {
		if code >= rg[0] && code <= rg[1] {
			return true
		}
	}
	return false
}

// readRC reads the .shellcheckrc files in the directory of path and its
// parents up to the workspace root that holds it. The file and the roots are
// compared on disk (links, case variants and other spellings of one place
// match). The files are merged: disabled codes are the union, enabled checks
// are listed nearest file first. A file outside every root reads none and
// the result says so. A file that is not a regular file, or is a link that
// leads outside the root, or is over 64 KiB, is skipped.
func readRC(path string, roots []string) rcInfo {
	var info rcInfo
	if len(roots) == 0 {
		return info
	}
	dir := filepath.Dir(path)
	var root string
	for _, r := range roots {
		if extool.Inside(dir, r) && len(filepath.Clean(r)) > len(root) {
			root = filepath.Clean(r)
		}
	}
	if root == "" {
		info.notes = append(info.notes, path+": outside every workspace folder; its .shellcheckrc was not read")
		return info
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return info
	}
	seen := map[string]bool{}
	for d := dir; ; d = filepath.Dir(d) {
		info.addFile(filepath.Join(d, ".shellcheckrc"), root, seen)
		if fi, err := os.Stat(d); (err == nil && os.SameFile(fi, rootInfo)) || d == root || filepath.Dir(d) == d {
			break
		}
	}
	return info
}

// openRCFile opens file for reading if it is a regular file whose real
// location (a link in the file or in any directory above it resolved) is
// inside root. A missing file is not an error and gives no reason; f is nil
// with a reason, which quotes no file content, for every other file that is
// not read.
func openRCFile(file, root string) (f *os.File, why string) {
	fi, err := os.Lstat(file)
	if err != nil {
		return nil, ""
	}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		return nil, file + ": a link that cannot be resolved (its target is missing, or the links loop); not followed"
	}
	if !extool.Inside(resolved, root) {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, file + ": a link that leads outside the workspace folder; not followed"
		}
		return nil, file + ": reached through a directory link that leads outside the workspace folder; not read"
	}
	if f, err = openRC(resolved); err != nil {
		return nil, file + ": not a regular file or not readable; not read"
	}
	return f, ""
}

// openRC opens path without blocking or following a link, and returns it
// only when the opened descriptor is a regular file.
func openRC(path string) (*os.File, error) {
	f, err := openNoBlock(path)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a regular file")
	}
	return f, nil
}

func (info *rcInfo) addFile(file, root string, seen map[string]bool) {
	f, why := openRCFile(file, root)
	if f == nil {
		if why != "" {
			info.notes = append(info.notes, why)
		}
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRCBytes+1))
	if err != nil || len(data) > maxRCBytes {
		info.notes = append(info.notes, file+": not read (over 64 KiB or unreadable)")
		return
	}
	var one rcInfo
	lines := strings.Split(string(data), "\n")
	for n, line := range lines {
		if n < len(lines)-1 {
			line = strings.TrimSuffix(line, "\r") // a CR is only read before the line feed
		}
		if err := one.addLine(line, seen); err != nil {
			why := "ShellCheck ignores such a file, so nothing from it is applied"
			if errors.Is(err, errOddSpace) {
				why = "ShellCheck reads this differently from the server, so nothing from the file is applied"
			}
			info.notes = append(info.notes, fmt.Sprintf("%s:%d: %v; %s", file, n+1, err, why))
			return
		}
	}
	info.ranges = append(info.ranges, one.ranges...)
	info.all = info.all || one.all
	info.enable = append(info.enable, one.enable...)
	info.notes = append(info.notes, one.notes...)
}

var (
	errBadRC    = errors.New("not valid rc syntax")
	errOddSpace = errors.New("a space-like character other than space and tab")
)

// addLine parses one line (without its line feed, and without the CR of a
// CRLF ending); an error means ShellCheck rejects the file, or reads the line
// in a way the server does not follow. Words are separated by space and tab
// only: ShellCheck rejects a lone CR anywhere, a key that is not letters and
// hyphens, and a space-like character (vertical tab, form feed, NEL,
// ideographic space and the like) at the start of a word, and takes it
// elsewhere in ways that differ.
func (info *rcInfo) addLine(line string, seen map[string]bool) error {
	if strings.IndexByte(line, '\r') >= 0 {
		return errBadRC
	}
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	for _, r := range line {
		if r != ' ' && r != '\t' && unicode.IsSpace(r) {
			return errOddSpace
		}
	}
	for _, word := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' }) {
		key, val, ok := strings.Cut(word, "=")
		if !ok || key == "" || strings.IndexFunc(key, func(r rune) bool { return r != '-' && !unicode.IsLetter(r) }) >= 0 {
			return errBadRC
		}
		if len(val) > 0 && (val[0] == '"' || val[0] == '\'') {
			if len(val) < 2 || val[len(val)-1] != val[0] {
				return errBadRC
			}
			val = val[1 : len(val)-1]
		}
		switch key {
		case "disable":
			for _, e := range strings.Split(val, ",") {
				if err := info.addDisable(e); err != nil {
					return err
				}
			}
		case "enable":
			for _, e := range strings.Split(val, ",") {
				switch {
				case e == "":
					return errBadRC
				case e != "all" && !enableName.MatchString(e):
					info.notes = append(info.notes, fmt.Sprintf("enable entry %q ignored (not a plain check name)", e))
				case !seen[e]:
					seen[e] = true
					info.enable = append(info.enable, e)
				}
			}
		}
	}
	return nil
}

func (info *rcInfo) addDisable(e string) error {
	if e == "all" {
		info.all = true
		return nil
	}
	m := disableEntry.FindStringSubmatch(e)
	if m == nil {
		return errBadRC
	}
	lo, _ := strconv.Atoi(m[1])
	hi := lo
	if m[2] != "" {
		hi, _ = strconv.Atoi(m[2])
	}
	if hi >= lo && len(info.ranges) < 1000 {
		info.ranges = append(info.ranges, [2]int{lo, hi})
	}
	return nil
}
