package analysis

import (
	"os"
	"time"
)

// onCI reports whether the tests run on a CI service (GitHub Actions sets
// CI=true). The growth tests compare timings of a few milliseconds measured on
// a shared two-core VM, where cache and allocator effects alone were seen to
// give x2.5 to x3.2 per doubling for work that is linear on the development
// machine (a quadratic shape gives x4). A bound that close to the noise
// cannot tell the two apart there, so on CI those tests keep their
// correctness checks and their overall time bound, log the growth, and leave
// the growth assertion to runs on a development machine.
func onCI() bool { return os.Getenv("CI") != "" }

// slack scales a wall-clock budget: four times as long on CI, where a hosted
// runner is a shared two-core VM several times slower than the development
// machine on these tests. A regression that matters is a change of order of
// magnitude, which the wider budget still catches.
func slack(d time.Duration) time.Duration {
	if onCI() {
		return 4 * d
	}
	return d
}
