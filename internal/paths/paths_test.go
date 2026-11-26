package paths

import "testing"

func TestRooted(t *testing.T) {
	for _, c := range []struct {
		p                string
		unix, windowsOut bool
	}{
		{"/etc/os-release", true, true},
		{"//host/share/x", true, true},
		{`\\host\share\x`, false, true},
		{`\etc`, false, true},
		{`C:\Users\x\a.sh`, false, true},
		{`C:/Users/x/a.sh`, false, true},
		{`d:x`, false, true},
		{"lib/a.sh", false, false},
		{"./a.sh", false, false},
		{"", false, false},
		{"~/a.sh", false, false},
		{"a:b", false, true}, // a drive-relative path on Windows
		{"1:a", false, false},
		{"$HOME/a", false, false},
	} {
		if got := rooted(c.p, false); got != c.unix {
			t.Errorf("rooted(%q, unix) = %v, want %v", c.p, got, c.unix)
		}
		if got := rooted(c.p, true); got != c.windowsOut {
			t.Errorf("rooted(%q, windows) = %v, want %v", c.p, got, c.windowsOut)
		}
	}
}
