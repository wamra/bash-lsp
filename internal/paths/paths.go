// Package paths holds the lexical path rules the packages share, where
// filepath's answer depends on the platform in a way that matters here.
package paths

import (
	"runtime"
	"strings"
)

// Rooted reports whether p does not name a place relative to the directory it
// is read against: it starts at a root. On every platform a leading "/" is a
// root. On Windows filepath.IsAbs is stricter, since it needs a volume, and a
// path such as "/etc/os-release", `\etc` or `C:x` is not "absolute" but is
// not relative to a directory either: joined to one it would name a place
// the caller never meant. Rooted treats all of them as rooted.
func Rooted(p string) bool { return rooted(p, runtime.GOOS == "windows") }

func rooted(p string, windows bool) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	if !windows {
		return false
	}
	return strings.HasPrefix(p, `\`) || len(p) >= 2 && p[1] == ':' && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z')
}
