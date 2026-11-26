package analysis

import (
	"path/filepath"
	"runtime"
)

// platPath is p, written as a Unix absolute path ("/w/a.sh"), as an absolute
// path of the host: unchanged on Unix; on Windows it takes the volume of the
// working directory and uses backslashes, which is what the snapshot's keys
// look like there.
func platPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	a, err := filepath.Abs(filepath.FromSlash(p))
	if err != nil {
		return p
	}
	return a
}

// slashed is p with forward slashes: how a shell script spells a path.
func slashed(p string) string { return filepath.ToSlash(p) }
