package shellcheck

import (
	"os"
	"time"
)

// onCI reports whether the tests run on a CI service (GitHub Actions sets
// CI=true).
func onCI() bool { return os.Getenv("CI") != "" }

// slack scales a wall-clock budget. The budgets were set on the development
// machine; a hosted runner is a shared two-core VM, 1.2x to 3x slower on
// these tests (ubuntu-latest and windows-latest in the first CI run), so on CI
// they are four times as long. A regression that matters here is a change of
// order of magnitude, which the wider budget still catches.
func slack(d time.Duration) time.Duration {
	if onCI() {
		return 4 * d
	}
	return d
}
