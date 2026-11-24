//go:build windows

package extool

import "os/exec"

// procGroup is a placeholder: no job object is used, so only the child
// itself is killed on timeout or cancel (exec's default).
type procGroup struct{}

func setGroup(*exec.Cmd) procGroup { return procGroup{} }

func (procGroup) killAll() {}
