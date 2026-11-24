//go:build windows

package shellcheck

import "errors"

func mkfifo(string) error { return errors.New("no fifo on Windows") }
