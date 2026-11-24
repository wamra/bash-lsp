package helpdoc

import (
	"slices"
	"strings"
	"testing"
)

const manGrep = `GREP(1)

SYNOPSIS
     grep [-A num] pattern [file ...]

DESCRIPTION
     The grep utility searches files.

     -A num, --after-context=num
	     Print num lines of trailing context after each match.  See also
	     the -B and -C options.

     -i, --ignore-case
	     Perform case insensitive matching.

     -e pattern, --regexp=pattern
	     Specify a pattern used during the search.
`

const manBash = `BASH(1)

SYNOPSIS
       bash [options] [command_string | file]

OPTIONS
       -c string If the -c option is present, then commands are read from
		 string.  If there are arguments after the string, they are
		 assigned to the positional parameters, starting with $0.
       -v        Print shell input lines as they are read.
       -i invert the sense of matching.
`

func TestParseManAliasedOptionWithArgument(t *testing.T) {
	d, ok := ParseMan("grep", []byte(manGrep))
	if !ok {
		t.Fatal("no doc")
	}
	var a Option
	for _, o := range d.Options {
		if o.Flag == "-A" {
			a = o
		}
	}
	if a.Arg != "num" || !strings.HasPrefix(a.Text, "Print num lines of trailing context") {
		t.Fatalf("-A: %+v", a)
	}
	if got := flagsOf(d); !slices.Contains(got, "-i") || !slices.Contains(got, "-e pattern") {
		t.Fatalf("options %q", got)
	}
}

func TestParseManArgumentBeforeTextOnOneSpace(t *testing.T) {
	d, ok := ParseMan("bash", []byte(manBash))
	if !ok {
		t.Fatal("no doc")
	}
	byFlag := map[string]Option{}
	for _, o := range d.Options {
		byFlag[o.Flag] = o
	}
	if c := byFlag["-c"]; c.Arg != "string" || !strings.HasPrefix(c.Text, "If the -c option is present") {
		t.Fatalf("-c: %+v", c)
	}
	// A lower-case word that starts the description is not an argument.
	if i := byFlag["-i"]; i.Arg != "" || !strings.HasPrefix(i.Text, "invert the sense") {
		t.Fatalf("-i: %+v", i)
	}
	if v := byFlag["-v"]; v.Arg != "" || !strings.HasPrefix(v.Text, "Print shell input lines") {
		t.Fatalf("-v: %+v", v)
	}
}

func TestParseHelpAlternativeOptions(t *testing.T) {
	d, ok := ParseHelp("cd", []byte("cd: cd [-L|-P] [dir]\n    Change the current directory to DIR.\n"))
	if !ok {
		t.Fatal("no doc")
	}
	if got := flagsOf(d); !slices.Equal(got, []string{"-L", "-P"}) {
		t.Fatalf("options %q", got)
	}
	d, _ = ParseHelp("wait", []byte("wait: wait [-n] [-p var|--flag arg] [id ...]\n    text\n"))
	if got := flagsOf(d); !slices.Contains(got, "-n") {
		t.Fatalf("options %q", got)
	}
}

func TestParseHelpCompoundCommandTopics(t *testing.T) {
	d, ok := ParseHelp("[[", []byte("[[ ... ]]: [[ expression ]]\n    Returns a status of 0 or 1.\n"))
	if !ok || d.Synopsis != "[[ expression ]]" || d.Name != "[[" {
		t.Fatalf("[[: %+v %v", d, ok)
	}
	d, ok = ParseHelp("{", []byte("{ ... }: { COMMANDS ; }\n    Run a set of commands in a group.\n"))
	if !ok || d.Synopsis != "{ COMMANDS ; }" {
		t.Fatalf("{: %+v %v", d, ok)
	}
	d, ok = ParseHelp("((", []byte("(( ... )): (( expression ))\n    The EXPRESSION is evaluated.\n"))
	if !ok || d.Synopsis != "(( expression ))" {
		t.Fatalf("((: %+v %v", d, ok)
	}
	// A topic whose name merely starts with the word is not it.
	if _, ok := ParseHelp("for", []byte("format: format [x]\n")); ok {
		t.Fatal("a longer topic matched")
	}
	// The plain form keeps working next to a compound one.
	if d, ok := ParseHelp("for", []byte("for: for NAME [in WORDS ... ;] do COMMANDS; done\n    text\n")); !ok || !strings.HasPrefix(d.Synopsis, "for NAME") {
		t.Fatalf("for: %+v %v", d, ok)
	}
}
