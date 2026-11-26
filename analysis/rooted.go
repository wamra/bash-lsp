package analysis

import (
	"runtime"
	"strings"
)

// rooted reports whether the path a script names starts at a root rather than
// below the directory it is read against. A leading "/" is a root on every
// platform. On Windows filepath.IsAbs needs a volume, so "/etc/os-release",
// `\etc` and `C:x` are not "absolute" there, and yet joined to a directory they
// would name a place the script never meant: rooted treats them as roots, so
// they are looked up as they are and found in no workspace folder.
//
// The same rule is internal/paths.Rooted; this package may not import
// internal packages (boundary_test.go).
func rooted(p string) bool { return rootedOn(p, runtime.GOOS == "windows") }

func rootedOn(p string, windows bool) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	if !windows {
		return false
	}
	return strings.HasPrefix(p, `\`) || len(p) >= 2 && p[1] == ':' && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z')
}
