//go:build !windows

package shellcheck

import "syscall"

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }
