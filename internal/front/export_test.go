package front

import "github.com/wamra/bash-lsp/tree"

// SetCrashAnalyse lets the external test package install crashAnalyse.
func SetCrashAnalyse(fn func(*tree.File) string) { crashAnalyse = fn }
