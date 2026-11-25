package analysis

import "testing"

func TestCompleteUnterminatedFunctionKeepsLocals(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "f() {\n  local aa\n  echo $a▮"}, "a.sh")
	if _, ok := has(items, "aa"); !ok {
		t.Fatalf("aa missing at EOF: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "g1=1\nf() {\n local aa\n echo $▮"}, "a.sh")
	for _, want := range []string{"aa", "g1"} {
		if _, ok := has(items, want); !ok {
			t.Fatalf("%s missing: %v", want, labels(items))
		}
	}
	// The closed form, with the cursor inside and after the function.
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "f() {\n local aa\n echo $▮\n}\n"}, "a.sh")
	if _, ok := has(items, "aa"); !ok {
		t.Fatalf("closed, inside: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "f() {\n local aa\n}\necho $▮"}, "a.sh")
	if _, ok := has(items, "aa"); ok {
		t.Fatalf("closed, after the function at EOF: aa leaked: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "( z=1 ); echo $▮"}, "a.sh")
	if _, ok := has(items, "z"); ok {
		t.Fatalf("after a closed subshell at EOF: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "( z=1; echo $▮"}, "a.sh")
	if _, ok := has(items, "z"); !ok {
		t.Fatalf("unterminated subshell: %v", labels(items))
	}
}

func TestCompleteAfterByteOrderMark(t *testing.T) {
	ctx, _, items, _ := compAt(t, map[string]string{"a.sh": "\xef\xbb\xbfpri▮"}, "a.sh")
	if ctx != CtxCommand {
		t.Fatalf("ctx %d", ctx)
	}
	it, ok := has(items, "printf")
	if !ok {
		t.Fatalf("printf missing: %v", labels(items))
	}
	if it.Replace.Start.Col != 4 || it.Replace.End.Col != 7 { // the BOM is three bytes
		t.Fatalf("replace %+v", it.Replace)
	}
}

func TestCompleteInsideArithmetic(t *testing.T) {
	for _, src := range []string{"g1=1\necho $((g▮", "g1=1\n(( g▮", "g1=1\n(( x = 2 + g▮", "g1=1\nx=$(( g▮"} {
		ctx, _, items, _ := compAt(t, map[string]string{"a.sh": src}, "a.sh")
		if ctx != CtxVariable {
			t.Errorf("%q: ctx %d, want variable", src, ctx)
			continue
		}
		if _, ok := has(items, "g1"); !ok {
			t.Errorf("%q: g1 missing: %v", src, labels(items))
		}
		if _, ok := has(items, "printf"); ok {
			t.Errorf("%q: a builtin offered", src)
		}
	}
	// After the arithmetic closes, a command word is a command word.
	ctx, _, _, _ := compAt(t, map[string]string{"a.sh": "(( x = 1 )); pri▮"}, "a.sh")
	if ctx != CtxCommand {
		t.Errorf("after the arithmetic command: ctx %d", ctx)
	}
	ctx, _, _, _ = compAt(t, map[string]string{"a.sh": "echo $((1+2)) pri▮"}, "a.sh")
	if ctx != CtxArgument {
		t.Errorf("after an arithmetic expansion: ctx %d", ctx)
	}
}

func TestCompleteSkipsTemporaryAssignments(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "FOOTMP=1 true\necho $FOOT▮"}, "a.sh")
	if _, ok := has(items, "FOOTMP"); ok {
		t.Fatalf("a variable that lived for one command is offered: %v", labels(items))
	}
}

func TestCompleteReplaceCoversTheTypedPartOnly(t *testing.T) {
	// The replace range ends at the cursor: text after it stays, so completing
	// "$g" followed by "g" to $gname gives $gnameg.
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "gname=1\necho $g▮g"}, "a.sh")
	it, ok := has(items, "gname")
	if !ok || it.Replace.Start.Col != 7 || it.Replace.End.Col != 8 {
		t.Fatalf("%+v %v", it.Replace, ok)
	}
}

func TestCompleteUnterminatedFunctionWithTextAfterTheCursor(t *testing.T) {
	for name, tail := range map[string]string{"newline": "\n", "crlf": "\r\n", "blank lines": "\n\n  \n", "spaces": "   ", "none": ""} {
		_, _, items, _ := compAt(t, map[string]string{"a.sh": "f() {\n  local aa\n  echo $a▮" + tail}, "a.sh")
		if _, ok := has(items, "aa"); !ok {
			t.Errorf("%s: aa missing: %v", name, labels(items))
		}
	}
	// A closed function followed by a newline still ends at its brace.
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "f() {\n  local aa\n}\necho $a▮\n"}, "a.sh")
	if _, ok := has(items, "aa"); ok {
		t.Errorf("closed: aa leaked: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "( z=1 ); echo $▮\n"}, "a.sh")
	if _, ok := has(items, "z"); ok {
		t.Errorf("closed subshell: z leaked: %v", labels(items))
	}
}
