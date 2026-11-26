package analysis

import "testing"

func TestRootedOn(t *testing.T) {
	for _, c := range []struct {
		p             string
		unix, windows bool
	}{
		{"/etc/os-release", true, true},
		{"//host/share/x", true, true},
		{`\\host\share\x`, false, true},
		{`\etc`, false, true},
		{`C:\Users\x\a.sh`, false, true},
		{`C:/Users/x/a.sh`, false, true},
		{"lib/a.sh", false, false},
		{"./a.sh", false, false},
		{"", false, false},
		{"~/a.sh", false, false},
		{"1:a", false, false},
	} {
		if got := rootedOn(c.p, false); got != c.unix {
			t.Errorf("rootedOn(%q, unix) = %v, want %v", c.p, got, c.unix)
		}
		if got := rootedOn(c.p, true); got != c.windows {
			t.Errorf("rootedOn(%q, windows) = %v, want %v", c.p, got, c.windows)
		}
	}
}
