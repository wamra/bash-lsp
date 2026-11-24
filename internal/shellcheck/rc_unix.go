//go:build !windows

package shellcheck

import (
	"os"
	"syscall"
)

// openNoBlock opens an rc file for reading without blocking and without following
// a link in the last component, so a FIFO or a link swapped in after the
// checks cannot block the server or lead outside the workspace.
func openNoBlock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
