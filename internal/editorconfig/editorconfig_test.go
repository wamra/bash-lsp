package editorconfig

import (
	"reflect"
	"strings"
	"testing"
)

// files is an in-memory file system for Lookup.
type files map[string]string

func (f files) read(p string) ([]byte, bool) {
	s, ok := f[p]
	return []byte(s), ok
}

func TestLookupIndent(t *testing.T) {
	fs := files{"/ws/.editorconfig": "[*.sh]\nindent_style=space\nindent_size=2\n"}
	got := Lookup("/ws/a/run.sh", "/ws", fs.read)
	want := map[string]string{"indent_style": "space", "indent_size": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := Lookup("/ws/a/run.py", "/ws", fs.read); len(got) != 0 {
		t.Fatalf("*.sh matched run.py: %v", got)
	}
}

func TestLookupPrecedenceAndRoot(t *testing.T) {
	fs := files{
		"/.editorconfig":          "[*]\nindent_style=tab\nbinary_next_line=true\n",
		"/ws/.editorconfig":       "root = true\n[*]\nindent_style=space\nindent_size=4\n",
		"/ws/sub/.editorconfig":   "[*.sh]\nindent_size=2\n[run.sh]\nsimplify=true\n",
		"/ws/sub/x/.editorconfig": "[*]\nindent_size = unset\n",
	}
	got := Lookup("/ws/sub/run.sh", "/ws", fs.read)
	want := map[string]string{"indent_style": "space", "indent_size": "2", "simplify": "true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// "unset" removes a property set farther up; the closer file wins.
	got = Lookup("/ws/sub/x/a.sh", "/ws", fs.read)
	if _, ok := got["indent_size"]; ok || got["indent_style"] != "space" {
		t.Fatalf("unset: %v", got)
	}
	// root = true stops the walk, and so does the workspace root.
	if _, ok := Lookup("/ws/sub/run.sh", "/ws", fs.read)["binary_next_line"]; ok {
		t.Fatal("read /.editorconfig past root = true")
	}
	fs["/ws/.editorconfig"] = "[*]\nindent_style=space\n"
	if _, ok := Lookup("/ws/sub/run.sh", "/ws", fs.read)["binary_next_line"]; ok {
		t.Fatal("read /.editorconfig above the workspace root")
	}
}

func TestLookupOutsideRootReadsOwnDirectoryOnly(t *testing.T) {
	fs := files{"/other/.editorconfig": "[*]\nindent_style=tab\n", "/.editorconfig": "[*]\nsimplify=true\n"}
	got := Lookup("/other/a.sh", "/ws", fs.read)
	if !reflect.DeepEqual(got, map[string]string{"indent_style": "tab"}) {
		t.Fatalf("got %v", got)
	}
}

func TestGlobs(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"*", "a/b/c.txt", true},
		{"*.sh", "a/b/c.sh", true},
		{"*.sh", "c.sh", true},
		{"*.sh", "c.shx", false},
		{"*.{sh,bash}", "x.bash", true},
		{"*.{sh,bash}", "x.zsh", false},
		{"**", "a/b", true},
		{"**.sh", "a/b/c.sh", true},
		{"scripts/*.sh", "scripts/a.sh", true},
		{"scripts/*.sh", "scripts/deep/a.sh", false},
		{"scripts/**.sh", "scripts/deep/a.sh", true},
		{"/top.sh", "top.sh", true},
		{"/top.sh", "a/top.sh", false},
		{"Makefile", "a/Makefile", true},
		{"lib/*", "a/lib/x", false},
	} {
		re, ok := compile(c.pat)
		if !ok {
			t.Errorf("%q not supported", c.pat)
			continue
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%q on %q: %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestUnsupportedGlobsAreIgnoredAndReported(t *testing.T) {
	fs := files{"/ws/.editorconfig": "[*.s?]\nindent_style=tab\n[[ab].sh]\nindent_style=tab\n[{1..3}.sh]\nindent_style=tab\n[*.sh]\nindent_style=space\n"}
	props, notes := LookupNotes("/ws/a.sh", "/ws", fs.read)
	if props["indent_style"] != "space" {
		t.Fatalf("got %v", props)
	}
	if len(notes) != 3 {
		t.Fatalf("notes %q, want 3", notes)
	}
	for _, n := range notes {
		if !strings.Contains(n, "/ws/.editorconfig:") || !strings.Contains(n, "not supported") {
			t.Errorf("note %q", n)
		}
	}
}

func TestKeysAndValuesAreLowerCasedAndComments(t *testing.T) {
	fs := files{"/ws/.editorconfig": "# c\n; c\n\n[*]\nIndent_Style = Space\n  indent_size=3  \n# indent_size=9\nbogus line without equals\n"}
	got := Lookup("/ws/a.sh", "/ws", fs.read)
	want := map[string]string{"indent_style": "space", "indent_size": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadFileLimits(t *testing.T) {
	big := "[*]\nindent_size=2\n" + strings.Repeat("# x\n", maxFileBytes)
	fs := files{"/ws/.editorconfig": big}
	if got := Lookup("/ws/a.sh", "/ws", fs.read); len(got) != 0 {
		t.Fatalf("a file over %d bytes was read: %v", maxFileBytes, got)
	}
}

// A Windows path (backslashes, a drive letter) finds the same sections as the
// same path in slash form.
func TestLookupWindowsPaths(t *testing.T) {
	fs := files{
		"C:/ws/.editorconfig":     "root = true\n[*]\nindent_style=space\n[b.sh]\nindent_style=tab\n[sub/*.sh]\nsimplify=true\n",
		"C:/ws/sub/.editorconfig": "[*.sh]\nindent_size=2\n",
		"/.editorconfig":          "[*]\nbinary_next_line=true\n",
	}
	for _, c := range []struct {
		file, root string
		want       map[string]string
	}{
		{`C:\ws\b.sh`, `C:\ws`, map[string]string{"indent_style": "tab"}},
		{`C:/ws/b.sh`, `C:/ws`, map[string]string{"indent_style": "tab"}},
		{`C:\ws\sub\a.sh`, `C:\ws`, map[string]string{"indent_style": "space", "simplify": "true", "indent_size": "2"}},
		{`C:\ws\sub\a.sh`, `C:\ws\`, map[string]string{"indent_style": "space", "simplify": "true", "indent_size": "2"}},
	} {
		if got := Lookup(c.file, c.root, fs.read); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (root %s): got %v, want %v", c.file, c.root, got, c.want)
		}
	}
}
