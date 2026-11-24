package analysis

import (
	"fmt"
	"strconv"
	"strings"
)

// Severity is a diagnostic severity, with the values the language server
// protocol uses.
type Severity uint8

const (
	SevError Severity = iota + 1
	SevWarning
	SevInfo
	SevHint
)

// BashVersion is a bash release series, such as 3.2 or 5.3.
type BashVersion struct{ Major, Minor int }

// Less reports whether v is an earlier release series than o.
func (v BashVersion) Less(o BashVersion) bool {
	return v.Major < o.Major || (v.Major == o.Major && v.Minor < o.Minor)
}

func (v BashVersion) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// VersionRange is the span of bash versions a script must run on.
type VersionRange struct{ Min, Max BashVersion }

func (r VersionRange) String() string { return r.Min.String() + "-" + r.Max.String() }

// DefaultTarget is the target range when no setting names one.
var DefaultTarget = VersionRange{BashVersion{3, 2}, BashVersion{5, 3}}

// ParseVersionRange reads "3.2-5.3" (a hyphen or an en dash between two
// MAJOR.MINOR versions). Min must not exceed Max.
func ParseVersionRange(s string) (VersionRange, error) {
	body := strings.ReplaceAll(strings.TrimSpace(s), "–", "-")
	lo, hi, ok := strings.Cut(body, "-")
	if !ok {
		return VersionRange{}, fmt.Errorf("version range %q: want MIN-MAX, such as 3.2-5.3", s)
	}
	min, err := parseBashVersion(lo)
	if err != nil {
		return VersionRange{}, fmt.Errorf("version range %q: %w", s, err)
	}
	max, err := parseBashVersion(hi)
	if err != nil {
		return VersionRange{}, fmt.Errorf("version range %q: %w", s, err)
	}
	if max.Less(min) {
		return VersionRange{}, fmt.Errorf("version range %q: minimum %s is above maximum %s", s, min, max)
	}
	return VersionRange{min, max}, nil
}

func parseBashVersion(s string) (BashVersion, error) {
	s = strings.TrimSpace(s)
	maj, min, ok := strings.Cut(s, ".")
	if !ok {
		return BashVersion{}, fmt.Errorf("version %q: want MAJOR.MINOR", s)
	}
	a, errA := parseSmall(maj)
	b, errB := parseSmall(min)
	if errA != nil || errB != nil {
		return BashVersion{}, fmt.Errorf("version %q: want MAJOR.MINOR with digits only", s)
	}
	return BashVersion{a, b}, nil
}

// parseSmall reads one to four ASCII digits.
func parseSmall(s string) (int, error) {
	if s == "" || len(s) > 4 {
		return 0, fmt.Errorf("bad number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad number")
		}
	}
	return strconv.Atoi(s)
}
