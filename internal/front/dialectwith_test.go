package front

import "testing"

func TestDetectWithFallback(t *testing.T) {
	cases := []struct {
		name, path, src string
		fallback, want  Dialect
	}{
		{"no ext, no shebang", "script", "echo hi\n", POSIX, POSIX},
		{"no ext, no shebang, default", "script", "echo hi\n", Bash, Bash},
		{".sh, no shebang", "a.sh", "echo hi\n", POSIX, POSIX},
		{".sh, bash shebang stays bash", "a.sh", "#!/bin/bash\necho hi\n", POSIX, Bash},
		{".sh, sh shebang", "a.sh", "#!/bin/sh\necho hi\n", Bash, POSIX},
		{"no ext, bash shebang stays bash", "script", "#!/usr/bin/env bash\n", POSIX, Bash},
		{"directive wins", "a.sh", "# shellcheck shell=bash\n", POSIX, Bash},
		{".bash stays bash", "a.bash", "echo hi\n", POSIX, Bash},
		{".ksh stays mksh", "a.ksh", "echo hi\n", POSIX, Mksh},
		{".bats stays bats", "a.bats", "echo hi\n", POSIX, Bats},
		{"other ext takes the fallback", "notes.txt", "echo hi\n", Mksh, Mksh},
		{"zsh stays zsh", "a.zsh", "echo hi\n", POSIX, Zsh},
	}
	for _, c := range cases {
		if got := DetectWith(c.path, []byte(c.src), c.fallback); got != c.want {
			t.Errorf("%s: DetectWith(%q, fallback %v) = %v, want %v", c.name, c.path, c.fallback, got, c.want)
		}
	}
}

func TestDetectIsDetectWithBash(t *testing.T) {
	for _, c := range []struct{ path, src string }{
		{"a.sh", "echo\n"}, {"x", ""}, {"x", "#!/bin/dash\n"}, {"a.ksh", ""}, {"a.txt", ""}, {"a.bats", ""},
	} {
		if Detect(c.path, []byte(c.src)) != DetectWith(c.path, []byte(c.src), Bash) {
			t.Errorf("Detect(%q) != DetectWith(.., Bash)", c.path)
		}
	}
}
