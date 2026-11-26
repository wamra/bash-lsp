package query

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if _, err := PreviewRename(s, root, "../x.sh:1:1", "h", 0); !errors.As(err, &r) || r.Code != "outside-root" || !strings.Contains(r.Error(), "outside the root") {
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

func TestRefusalTextRedactsAndStaysOnOneLine(t *testing.T) {
	const tok = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	s, root := world(t, map[string]string{"a.sh": "f() { :; }\nf\n"})
	for _, c := range [][2]string{{"f", tok + "!"}, {tok, "h"}, {"../" + tok + ".sh:1:1", "h"}} {
		_, err := PreviewRename(s, root, c[0], c[1], 0)
		var r *RenameRefusedError
		if !errors.As(err, &r) {
			t.Fatalf("%v: %v", c, err)
		}
		if strings.Contains(r.Error(), "ghp_") || strings.Contains(r.Error(), tok[:20]) {
			t.Errorf("%v: secret in %q", c, r.Error())
		}
	}
	e := &RenameRefusedError{Code: "name-conflict", Reason: "line one\nline two\r " + tok, Sites: []Site{{Path: "a.sh", Line: 1, Col: 2}}}
	if got := e.Error(); strings.Count(got, "\n") != 1 || strings.Contains(got, "ghp_") || !strings.HasSuffix(got, "\nsite  a.sh:1:2") {
		t.Errorf("%q", got)
	}
}

func TestRefusalTextDropsWholeSiteLines(t *testing.T) {
	e := &RenameRefusedError{Code: "name-conflict", Reason: "r"}
	for i := 0; i < 1000; i++ {
		e.Sites = append(e.Sites, Site{Path: "some/long/path/to/a/file.sh", Line: i + 1, Col: 1})
	}
	got := e.Error()
	if len(got) > 4100 {
		t.Fatalf("%d bytes", len(got))
	}
	lines := strings.Split(got, "\n")
	for _, l := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(l, "site  some/long/path/to/a/file.sh:") || !strings.HasSuffix(l, ":1") && !strings.Contains(l, ":1") {
			t.Fatalf("a site line is cut: %q", l)
		}
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "more  ") {
		t.Errorf("last line %q", last)
	}
}

func TestRefusalDoesNotRepeatItsCode(t *testing.T) {
	e := &RenameRefusedError{Code: "dynamic-scope", Reason: "dynamic scope: a function"}
	if got := e.Error(); got != "refused: dynamic-scope: a function" {
		t.Errorf("%q", got)
	}
}

func TestReservedNameReasonForAVariable(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() {\n  local v=1\n  echo \"$v\"\n}\ng() { :; }\n"})
	_, err := PreviewRename(s, root, "a.sh:2:9", "if", 0)
	var r *RenameRefusedError
	if !errors.As(err, &r) || r.Code != "reserved-name" || !strings.Contains(r.Reason, "accepts it as a variable name") {
		t.Fatalf("variable: %v", err)
	}
	_, err = PreviewRename(s, root, "g", "if", 0)
	if !errors.As(err, &r) || r.Code != "reserved-name" || !strings.Contains(r.Reason, "builtin or keyword") {
		t.Fatalf("function: %v", err)
	}
}

func TestCheckFileMeta(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() { :; }\n"})
	_ = s
	p := filepath.Join(root, "a.sh")
	if _, issue := CheckFileMeta(root, p, "a.sh"); issue != nil {
		t.Fatalf("%+v", issue)
	}
	if runtime.GOOS == "windows" {
		// Owner, group and link count come from the Unix file identity, which
		// Windows does not have: the check skips them (README, rename --apply).
		return
	}
	old := effectiveUID
	defer func() { effectiveUID = old }()
	effectiveUID = func() int { return os.Geteuid() + 1 }
	if _, issue := CheckFileMeta(root, p, "a.sh"); issue == nil || issue.Code != IssueNotOwner {
		t.Errorf("another user's file: %+v", issue)
	}
	effectiveUID = func() int { return 0 } // root may rewrite and chown
	if _, issue := CheckFileMeta(root, p, "a.sh"); issue != nil {
		t.Errorf("root: %+v", issue)
	}
	effectiveUID = old
	if err := os.Link(p, filepath.Join(root, "b.sh")); err == nil {
		if _, issue := CheckFileMeta(root, p, "a.sh"); issue == nil || issue.Code != IssueHardLink {
			t.Errorf("hard link: %+v", issue)
		}
	}
}

func TestCheckFileBytes(t *testing.T) {
	for _, c := range []struct {
		b, want []byte
		code    string
	}{
		{[]byte("a\x00"), nil, IssueNUL},
		{[]byte("a\xff"), nil, IssueInvalidUTF8},
		{[]byte("a"), []byte("b"), IssueStale},
		{[]byte("a"), []byte("a"), ""},
		{[]byte("a"), nil, ""},
	} {
		got := CheckFileBytes("a.sh", c.b, c.want)
		if (got == nil) != (c.code == "") || got != nil && got.Code != c.code {
			t.Errorf("%q %q: %+v want %q", c.b, c.want, got, c.code)
		}
	}
}

func TestCheckFileMetaGroup(t *testing.T) {
	_, root := world(t, map[string]string{"a.sh": "f() { :; }\n"})
	p := root + "/a.sh"
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _, gid, ok := fileIdentity(fi)
	if !ok {
		t.Skip("no group on this system")
	}
	oldG, oldS, oldU := effectiveGID, supplementaryGroups, effectiveUID
	defer func() { effectiveGID, supplementaryGroups, effectiveUID = oldG, oldS, oldU }()
	effectiveUID = func() int { return int(fileUID(fi)) } // the owner
	effectiveGID = func() int { return int(gid) + 1 }     // not in the file's group
	supplementaryGroups = func() ([]int, error) { return nil, nil }
	// a path whose directory is not readable has no group to inherit
	gone := root + "/nodir/a.sh"
	if canSetGroup(gid, gone) {
		t.Errorf("a group the user is not in, no directory to inherit it from")
	}
	supplementaryGroups = func() ([]int, error) { return []int{int(gid)}, nil }
	if !canSetGroup(gid, gone) {
		t.Errorf("a supplementary group")
	}
	supplementaryGroups = func() ([]int, error) { return nil, nil }
	effectiveGID = func() int { return int(gid) }
	if !canSetGroup(gid, gone) {
		t.Errorf("the user's own group")
	}
	// the directory's group is the file's: new files there get it
	effectiveGID = func() int { return int(gid) + 1 }
	if !canSetGroup(gid, p) {
		t.Errorf("the group of the directory")
	}
}

func fileUID(fi os.FileInfo) uint32 { _, uid, _, _ := fileIdentity(fi); return uid }

// A preview opens nothing for writing: a read-only file is found by the
// permission check.
func TestCheckFileMetaReadOnlyWithoutOpening(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	_, root := world(t, map[string]string{"a.sh": "f() { :; }\n"})
	p := root + "/a.sh"
	os.Chmod(p, 0o444)
	if _, issue := CheckFileMeta(root, p, "a.sh"); issue == nil || issue.Code != IssueReadOnly {
		t.Errorf("%+v", issue)
	}
}

// A file whose group is neither the user's, nor a group the user is in, nor
// the group of its directory is refused as not-owner.
func TestCheckFileMetaRefusesAGroupItCannotKeep(t *testing.T) {
	_, root := world(t, map[string]string{"a.sh": "f() { :; }\n"})
	p := root + "/a.sh"
	di, _ := os.Lstat(root)
	_, _, dirGid, ok := fileIdentity(di)
	groups, _ := os.Getgroups()
	if !ok || os.Geteuid() == 0 {
		t.Skip("needs a unix user")
	}
	other := -1
	for _, g := range groups {
		if uint32(g) != dirGid {
			other = g
			break
		}
	}
	if other < 0 || os.Chown(p, -1, other) != nil {
		t.Skip("no second group to give the file")
	}
	oldG, oldS := effectiveGID, supplementaryGroups
	defer func() { effectiveGID, supplementaryGroups = oldG, oldS }()
	effectiveGID = func() int { return other + 1 }
	supplementaryGroups = func() ([]int, error) { return nil, nil }
	if _, issue := CheckFileMeta(root, p, "a.sh"); issue == nil || issue.Code != IssueNotOwner || !strings.Contains(issue.Reason, "group") {
		t.Errorf("%+v", issue)
	}
	supplementaryGroups = func() ([]int, error) { return []int{other}, nil }
	if _, issue := CheckFileMeta(root, p, "a.sh"); issue != nil {
		t.Errorf("with the group: %+v", issue)
	}
}
