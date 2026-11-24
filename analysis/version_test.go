package analysis

import "testing"

func TestParseVersionRange(t *testing.T) {
	good := map[string]VersionRange{
		"3.2-5.3":     {BashVersion{3, 2}, BashVersion{5, 3}},
		"3.2–5.3":     {BashVersion{3, 2}, BashVersion{5, 3}},
		" 4.0 - 4.0 ": {BashVersion{4, 0}, BashVersion{4, 0}},
		"4.4-5.10":    {BashVersion{4, 4}, BashVersion{5, 10}},
	}
	for in, want := range good {
		got, err := ParseVersionRange(in)
		if err != nil || got != want {
			t.Errorf("ParseVersionRange(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"5.3-3.2", "3.2", "", "3-5", "3.2-", "-5.3", "a.b-c.d", "3.2-5.3-6.0", "3.2.1-5.3", "3.-5.3", "99999999999.1-99999999999.2"} {
		if r, err := ParseVersionRange(in); err == nil {
			t.Errorf("ParseVersionRange(%q) = %v, want error", in, r)
		}
	}
}

func TestDefaultTargetAndSeverity(t *testing.T) {
	if DefaultTarget.Min != (BashVersion{3, 2}) || DefaultTarget.Max != (BashVersion{5, 3}) {
		t.Fatal(DefaultTarget)
	}
	if SevError != 1 || SevWarning != 2 || SevInfo != 3 || SevHint != 4 {
		t.Fatal("severities must carry the LSP values")
	}
	if DefaultTarget.String() != "3.2-5.3" {
		t.Fatal(DefaultTarget.String())
	}
}
