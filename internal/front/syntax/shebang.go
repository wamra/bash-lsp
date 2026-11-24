// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package syntax

import "regexp"

// shebangRe and Shebang are copied verbatim from mvdan.cc/sh/v3/fileutil
// (file.go, at the commit listed in UPGRADING.md), because the printer calls
// Shebang and this repository imports nothing from the upstream module.
var shebangRe = regexp.MustCompile(`^#![ \t]*/(usr/)?bin/(env[ \t]+)?(sh|dash|bash|mksh|bats|zsh)(\s|$)`)

// Shebang parses a "#!" sequence from the beginning of the input bytes,
// and returns the shell that it points to.
//
// For instance, it returns "sh" for "#!/bin/sh",
// and "bash" for "#!/usr/bin/env bash".
func Shebang(bs []byte) string {
	m := shebangRe.FindSubmatch(bs)
	if m == nil {
		return ""
	}
	return string(m[3])
}
