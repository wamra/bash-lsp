package shellcheck

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A repo's .shellcheckrc is not given to ShellCheck (it runs in a neutral
// directory, so it finds none): its keys can make ShellCheck read files
// anywhere (external-sources, source-path). The server reads the file itself
// and applies the two keys that only narrow or widen the report: disable
// (findings dropped after parsing) and enable (optional checks, passed on the
// command line when the name is plain). shell, external-sources, source-path
// and any other key are not used.

const maxRCBytes = 64 << 10

var (
	disableEntry = regexp.MustCompile(`^(?i:sc)?(\d{1,6})(?:-(?i:sc)?(\d{1,6}))?$`)
	enableName   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

type rcInfo struct {
	ranges [][2]int
	enable []string
	notes  []string
}

func (r rcInfo) disabled(code int) bool {
	for _, rg := range r.ranges {
		if code >= rg[0] && code <= rg[1] {
			return true
		}
	}
	return false
}

// readRC reads the .shellcheckrc files in the directory of path and its
// parents up to the workspace root that holds it (the lookup ShellCheck does,
// stopped at the root). The files are merged: disabled codes are the union,
// enabled checks are listed nearest file first. A file outside every root
// reads none. A file that is not a regular file or is over 64 KiB is skipped.
func readRC(path string, roots []string) rcInfo {
	var info rcInfo
	dir := filepath.Dir(path)
	root := ""
	for _, r := range roots {
		if rel, err := filepath.Rel(r, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if len(r) > len(root) {
				root = r
			}
		}
	}
	if root == "" {
		return info
	}
	seen := map[string]bool{}
	for d := dir; ; d = filepath.Dir(d) {
		info.addFile(filepath.Join(d, ".shellcheckrc"), seen)
		if d == filepath.Clean(root) || filepath.Dir(d) == d {
			break
		}
	}
	return info
}

func (info *rcInfo) addFile(file string, seen map[string]bool) {
	fi, err := os.Stat(file)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRCBytes+1))
	if err != nil || len(data) > maxRCBytes {
		info.notes = append(info.notes, file+": not read (over 64 KiB or unreadable)")
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		switch strings.TrimSpace(key) {
		case "disable":
			for _, e := range strings.Split(val, ",") {
				if e = strings.TrimSpace(e); e != "" {
					info.addDisable(file, e)
				}
			}
		case "enable":
			for _, e := range strings.Split(val, ",") {
				e = strings.TrimSpace(e)
				switch {
				case e == "":
				case e != "all" && !enableName.MatchString(e):
					info.notes = append(info.notes, fmt.Sprintf("%s: enable entry %q ignored (not a plain check name)", file, e))
				case !seen[e]:
					seen[e] = true
					info.enable = append(info.enable, e)
				}
			}
		}
	}
}

func (info *rcInfo) addDisable(file, e string) {
	m := disableEntry.FindStringSubmatch(e)
	if m == nil {
		info.notes = append(info.notes, fmt.Sprintf("%s: disable entry %q ignored (not a code or a range of codes)", file, e))
		return
	}
	lo, _ := strconv.Atoi(m[1])
	hi := lo
	if m[2] != "" {
		hi, _ = strconv.Atoi(m[2])
	}
	if hi < lo || len(info.ranges) >= 1000 {
		info.notes = append(info.notes, fmt.Sprintf("%s: disable entry %q ignored", file, e))
		return
	}
	info.ranges = append(info.ranges, [2]int{lo, hi})
}
