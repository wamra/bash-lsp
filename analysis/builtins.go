package analysis

import "sort"

// The builtin list: bash builtins and reserved words. A command with one of
// these names and no function or alias of that name in view gets no call edge
// and no name-match-outside-closure candidate.
//
// Generated on 2026-11-23 on macOS 26.5.1 from the installed bash:
//
//	$ /bin/bash --version
//	GNU bash, version 3.2.57(1)-release (arm64-apple-darwin25)
//	$ /bin/bash -c 'compgen -b'   # 58 builtins: the "bash 3.2 compgen -b" group below
//	$ /bin/bash -c 'compgen -k'   # 21 reserved words: the "bash 3.2 compgen -k" group below
//
// Cross-checked against `man bash` (GNU Bash-3.2, 2006 September 28), section
// SHELL BUILTIN COMMANDS: every compgen -b name has an entry there except
// `true` and `false`, which that section names only in its introduction ("the
// :, true, false, and test builtins do not accept options"). `dash` (`man dash`,
// section Builtins) lists no builtin missing from bash 3.2's list.
//
// Newer bash: no bash newer than 3.2 and no bash 4 or 5 manual is installed on
// the generating machine (no /opt/homebrew/bin/bash or /usr/local/bin/bash;
// /usr/share/doc/bash/bashref.html and every bash.1 under the Command Line
// Tools SDKs are 3.2). The additions below come from sources readable there:
//
//   - compopt, mapfile, readarray: the IsBuiltin list of mvdan.cc/sh/v3/interp
//     (interp/builtin.go, lines 88, 99 and 100, module version
//     v3.14.1-0.20260928185915-0e0eb36bb6dd in the Go module cache, the upstream
//     commit the vendored parser was copied from), in its group "Bash built-ins
//     which are not present in POSIX, obtained in September 2025 from
//     https://man.archlinux.org/man/bash.1.en#SHELL_BUILTIN_COMMANDS". A
//     secondary source: a list in code quoting the bash 5 manual.
//   - coproc: the vendored parser's IsKeyword (internal/front/syntax/parser.go,
//     line 976), "copied from the bash 5.1 source code, file y.tab.c".
//
// Deliberately left out: `newgrp`, which mvdan's list carries as a POSIX
// regular built-in but bash does not provide (it is not in compgen -b or the
// bash manual). `wait -n` is an option of `wait`, not a builtin.
//
// Known gap: any builtin of bash 4 or 5 that neither source above names is
// missing; it cannot be checked on the generating machine. mksh is not
// installed there and contributed nothing.
var builtins = map[string]bool{}

func init() {
	for _, group := range [][]string{
		{ // bash 3.2 compgen -b
			".", ":", "[", "alias", "bg", "bind", "break", "builtin", "caller", "cd", "command", "compgen",
			"complete", "continue", "declare", "dirs", "disown", "echo", "enable", "eval", "exec", "exit",
			"export", "false", "fc", "fg", "getopts", "hash", "help", "history", "jobs", "kill", "let",
			"local", "logout", "popd", "printf", "pushd", "pwd", "read", "readonly", "return", "set",
			"shift", "shopt", "source", "suspend", "test", "times", "trap", "true", "type", "typeset",
			"ulimit", "umask", "unalias", "unset", "wait",
		},
		{ // bash 3.2 compgen -k
			"if", "then", "else", "elif", "fi", "case", "esac", "for", "select", "while", "until", "do",
			"done", "in", "function", "time", "{", "}", "!", "[[", "]]",
		},
		{"compopt", "mapfile", "readarray"}, // mvdan.cc/sh/v3/interp IsBuiltin (bash 5 manual)
		{"coproc"},                          // vendored parser IsKeyword (bash 5.1 y.tab.c)
	} {
		for _, w := range group {
			builtins[w] = true
		}
	}
}

// IsBuiltin reports whether name is a bash builtin or reserved word (see the
// list's sources above).
func IsBuiltin(name string) bool { return builtins[name] }

// BuiltinNames lists the builtins and reserved words, sorted.
func BuiltinNames() []string {
	out := make([]string, 0, len(builtins))
	for n := range builtins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
