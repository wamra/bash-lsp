package query

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func renameWorld(t *testing.T) (*RenameDoc, string) {
	t.Helper()
	s, root := world(t, map[string]string{
		"main.sh": "#!/usr/bin/env bash\nsource ./lib.sh\nf() { :; }\nf\ncmd=f\n\"$cmd\"\n",
		"lib.sh":  "g() { f; }\n",
	})
	d, err := PreviewRename(s, root, "f", "h", 0)
	if err != nil {
		t.Fatal(err)
	}
	return d, root
}

func TestPreviewRenameShape(t *testing.T) {
	d, _ := renameWorld(t)
	if d.Old != "f" || d.New != "h" || d.Symbol != "function f" || d.Generation == 0 {
		t.Fatalf("header %+v", d)
	}
	var got []string
	for _, e := range d.Edits {
		got = append(got, fmt.Sprintf("%s:%d:%d", e.Path, e.Line, e.Col))
	}
	if strings.Join(got, " ") != "lib.sh:1:7 main.sh:3:1 main.sh:4:1" {
		t.Errorf("edits %v", got)
	}
	if len(d.Excluded) != 2 || d.Excluded[0].Line != 5 || d.Excluded[0].Resolution == "" || d.Excluded[0].Reason == "" || d.Excluded[0].Text == "" {
		t.Errorf("excluded %+v", d.Excluded)
	}
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"generation"`, `"symbol"`, `"old"`, `"new"`, `"edits"`, `"excluded"`, `"others"`, `"warnings"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON lacks %s:\n%s", k, b)
		}
	}
	if strings.Contains(string(b), `"truncated"`) {
		t.Errorf("truncated is set:\n%s", b)
	}
	for _, k := range []string{`"edits": []`, `"others": null`, `"excluded": null`, `"warnings": null`} {
		if strings.Contains(string(b), k) {
			t.Errorf("empty list is not []: %s", k)
		}
	}
}

func TestPreviewRenameCut(t *testing.T) {
	var src strings.Builder
	src.WriteString("f() { :; }\n")
	for i := 0; i < 30; i++ {
		src.WriteString("f\n")
	}
	s, root := world(t, map[string]string{"a.sh": src.String()})
	d, err := PreviewRename(s, root, "f", "h", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Edits) != 10 || !d.Truncated {
		t.Fatalf("%d edits, truncated %v", len(d.Edits), d.Truncated)
	}
	if d.Len() != 10 {
		t.Errorf("Len %d", d.Len())
	}
	c := d.Cut(3).(*RenameDoc)
	if len(c.Edits) != 3 || !c.Truncated {
		t.Errorf("Cut: %d edits, truncated %v", len(c.Edits), c.Truncated)
	}
}

// The excluded text is redacted before it is cut: a secret that straddles
// the cut is not left as a piece the patterns miss.
func TestRenameDocRedactsThenCuts(t *testing.T) {
	const tok = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	s, root := world(t, map[string]string{"a.sh": "f() { :; }\n\"f\"\n"})
	plan, err := PlanRename(s, root, "f", "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Excluded) != 1 {
		t.Fatalf("excluded %+v", plan.Excluded)
	}
	for _, c := range []struct{ name, text string }{
		{"starts inside the window", strings.Repeat("\u00e9", 70) + tok + strings.Repeat("\u00fc", 300)},
		{"straddles the cut", strings.Repeat("x", 190) + tok + strings.Repeat("y", 50)},
		{"whole", tok},
	} {
		plan.Excluded[0].Text = c.text
		d := RenameDocOf(s, root, plan, 0)
		got := d.Excluded[0].Text
		if strings.Contains(got, "ghp_") || strings.Contains(got, tok[:12]) {
			t.Errorf("%s: secret in %q", c.name, got)
		}
		if len(got) > 200 || !utf8.ValidString(got) {
			t.Errorf("%s: %d bytes, valid %v", c.name, len(got), utf8.ValidString(got))
		}
		if got == "" {
			t.Errorf("%s: empty", c.name)
		}
	}
}

func TestRenameDocClipsLongTextOnARuneBoundary(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() { :; }\n\"f\"\n"})
	plan, err := PlanRename(s, root, "f", "h")
	if err != nil {
		t.Fatal(err)
	}
	plan.Excluded[0].Text = strings.Repeat("\u00fc", 300)
	got := RenameDocOf(s, root, plan, 0).Excluded[0].Text
	if n := len(got); n != 200 || !utf8.ValidString(got) {
		t.Errorf("text %d bytes", n)
	}
	plan.Excluded[0].Text = "a" + strings.Repeat("\u00fc", 300)
	got = RenameDocOf(s, root, plan, 0).Excluded[0].Text
	if n := len(got); n != 199 || !utf8.ValidString(got) { // 1 + 99 two-byte runes, the 100th does not fit
		t.Errorf("text %d bytes", n)
	}
	plan.Excluded[0].Text = "ab" + strings.Repeat("\u00fc", 300)
	got = RenameDocOf(s, root, plan, 0).Excluded[0].Text
	if n := len(got); n != 200 || !utf8.ValidString(got) {
		t.Errorf("text %d bytes", n)
	}
}

func TestPreviewRenameRefusal(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() { :; }\ng() { :; }\nf\n"})
	_, err := PreviewRename(s, root, "f", "g", 0)
	var r *RenameRefusedError
	if !errors.As(err, &r) || r.Code != "name-conflict" {
		t.Fatalf("err %v", err)
	}
	if !strings.HasPrefix(r.Error(), "refused: name-conflict: ") {
		t.Errorf("text %q", r.Error())
	}
	if _, err := PreviewRename(s, root, "nosuch", "h", 0); !errors.As(err, &r) || r.Code != "not-a-symbol" {
		t.Errorf("err %v", err)
	}
	// a file part outside the root is refused before the snapshot is asked
	if _, err := PreviewRename(s, root, "../x.sh:1:1", "h", 0); err == nil || errors.As(err, &r) {
		t.Errorf("err %v", err)
	}
}

func TestPreviewRenameByPosition(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() {\n  local v=1\n  echo \"$v\"\n}\n"})
	d, err := PreviewRename(s, root, "a.sh:2:9", "w", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Symbol != "variable v" || len(d.Edits) != 2 {
		t.Errorf("%+v", d)
	}
	if _, err := PreviewRename(s, root, "a.sh:99:1", "w", 0); err == nil {
		t.Error("a line past the end is accepted")
	}
}
