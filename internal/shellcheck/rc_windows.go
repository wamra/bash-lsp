//go:build windows

package shellcheck

import "os"

// openNoBlock opens an rc file for reading; the caller checks what was opened.
func openNoBlock(path string) (*os.File, error) { return os.Open(path) }
